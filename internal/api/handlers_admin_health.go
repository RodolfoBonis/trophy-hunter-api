package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/httperr"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/promql"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/queue"
	syncpkg "github.com/RodolfoBonis/trophy-hunter-api/internal/sync"
)

// Painel do dono: GET /admin/health junta o banco (contas, syncs, fila, IA) e o Prometheus (alertas,
// erros do front, fontes externas, Web Vitals). Prometheus fora = seção marcada como indisponível,
// o resto responde igual. Ações: POST /admin/jobs/{id}/retry e POST /admin/integrations/{id}/sync.

type adminAccountDTO struct {
	ID           string     `json:"id"`
	User         string     `json:"user"`
	Platform     string     `json:"platform"`
	DisplayName  string     `json:"display_name"`
	Label        string     `json:"label"`
	Status       string     `json:"status"`
	LastSyncedAt *time.Time `json:"last_synced_at"`
	LastError    string     `json:"last_error"`
}

type adminJobCountDTO struct {
	Kind   string `json:"kind"`
	Status string `json:"status"`
	Count  int    `json:"count"`
}

type adminDeadJobDTO struct {
	ID        string     `json:"id"`
	Kind      string     `json:"kind"`
	Attempts  int        `json:"attempts"`
	Error     string     `json:"error"`
	UpdatedAt *time.Time `json:"updated_at"`
}

type adminAIUsageDTO struct {
	User     string `json:"user"`
	Owner    bool   `json:"owner"`
	Last7d   int    `json:"last_7d"`
	Last24h  int    `json:"last_24h"`
	DayLimit int    `json:"day_limit"` // 0 = sem teto
}

type adminLabeledValue struct {
	Label string  `json:"label"`
	Value float64 `json:"value"`
}

type adminProviderDTO struct {
	Provider   string  `json:"provider"`
	Calls24h   float64 `json:"calls_24h"`
	ErrorRatio float64 `json:"error_ratio_1h"`
}

type adminAlertDTO struct {
	Name     string `json:"name"`
	Severity string `json:"severity"`
}

type adminVitalDTO struct {
	Route  string  `json:"route"`
	Device string  `json:"device"`
	LCPp75 float64 `json:"lcp_p75_s"`
}

type adminPromDTO struct {
	Available    bool                `json:"available"`
	Alerts       []adminAlertDTO     `json:"alerts"`
	ClientErrors []adminLabeledValue `json:"client_errors_24h"`
	Providers    []adminProviderDTO  `json:"providers"`
	Vitals       []adminVitalDTO     `json:"vitals"`
}

type adminHealthDTO struct {
	GeneratedAt time.Time          `json:"generated_at"`
	Accounts    []adminAccountDTO  `json:"accounts"`
	Syncs24h    map[string]int     `json:"syncs_24h"`
	Queue       []adminJobCountDTO `json:"queue"`
	RecentDead  []adminDeadJobDTO  `json:"recent_dead"`
	AIUsage     []adminAIUsageDTO  `json:"ai_usage"`
	Totals      map[string]int     `json:"totals"`
	Prometheus  adminPromDTO       `json:"prometheus"`
}

func (h *Handlers) getAdminHealth(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	out := adminHealthDTO{GeneratedAt: time.Now().UTC(), Syncs24h: map[string]int{}, Totals: map[string]int{}}

	// Prometheus em paralelo (rede) enquanto o banco responde.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		out.Prometheus = h.promHealth(ctx)
	}()

	accounts, err := h.q.AdminAccounts(ctx)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	out.Accounts = make([]adminAccountDTO, 0, len(accounts))
	for _, a := range accounts {
		out.Accounts = append(out.Accounts, adminAccountDTO{
			ID: uuidStr(a.ID), User: a.UserName, Platform: a.Platform, DisplayName: a.DisplayName, Label: a.Label,
			Status: a.Status, LastSyncedAt: tptr(a.LastSyncedAt), LastError: a.LastError,
		})
	}
	runs, err := h.q.AdminSyncRuns24h(ctx)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	for _, x := range runs {
		out.Syncs24h[x.Status] = int(x.N)
	}
	counts, err := h.q.AdminJobCounts(ctx)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	out.Queue = make([]adminJobCountDTO, 0, len(counts))
	for _, c := range counts {
		out.Queue = append(out.Queue, adminJobCountDTO{Kind: c.Kind, Status: c.Status, Count: int(c.N)})
	}
	dead, err := h.q.AdminRecentDead(ctx)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	out.RecentDead = make([]adminDeadJobDTO, 0, len(dead))
	for _, d := range dead {
		out.RecentDead = append(out.RecentDead, adminDeadJobDTO{ID: uuidStr(d.ID), Kind: d.Kind, Attempts: int(d.Attempts), Error: truncate(d.LastError, 300), UpdatedAt: tptr(d.UpdatedAt)})
	}
	usage, err := h.q.AdminAIUsage(ctx)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	limit := 0
	if h.ai != nil {
		limit = h.ai.DailyLimit()
	}
	out.AIUsage = make([]adminAIUsageDTO, 0, len(usage))
	for _, u := range usage {
		d := adminAIUsageDTO{User: u.Name, Owner: u.Role == "owner", Last7d: int(u.Last7d), Last24h: int(u.Last24h), DayLimit: limit}
		if d.Owner {
			d.DayLimit = 0
		}
		out.AIUsage = append(out.AIUsage, d)
	}
	t, err := h.q.AdminTotals(ctx)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	out.Totals = map[string]int{"members": int(t.Members), "badges": int(t.Badges), "open_challenges": int(t.OpenChallenges), "wishlist_items": int(t.WishlistItems), "public_profiles": int(t.PublicProfiles)}

	wg.Wait()
	writeJSON(w, http.StatusOK, out)
}

const promNS = `namespace="trophy-hunter"`

func (h *Handlers) promHealth(ctx context.Context) adminPromDTO {
	out := adminPromDTO{Alerts: []adminAlertDTO{}, ClientErrors: []adminLabeledValue{}, Providers: []adminProviderDTO{}, Vitals: []adminVitalDTO{}}
	if h.prom == nil {
		return out
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	q := func(expr string) []promql.Sample {
		s, err := h.prom.Query(ctx, expr)
		if err != nil {
			if !errors.Is(err, promql.ErrNotConfigured) {
				h.logger.Warn("painel: consulta ao prometheus falhou", slog.String("err", err.Error()))
			}
			return nil
		}
		out.Available = true
		return s
	}
	for _, s := range q(`ALERTS{alertstate="firing",` + promNS + `}`) {
		out.Alerts = append(out.Alerts, adminAlertDTO{Name: s.Labels["alertname"], Severity: s.Labels["severity"]})
	}
	for _, s := range q(`sum by (kind) (increase(client_errors_total{` + promNS + `}[24h]))`) {
		if s.Value >= 0.5 {
			out.ClientErrors = append(out.ClientErrors, adminLabeledValue{Label: s.Labels["kind"], Value: s.Value})
		}
	}
	calls := map[string]float64{}
	for _, s := range q(`sum by (provider) (increase(external_api_requests_total{` + promNS + `}[24h]))`) {
		calls[s.Labels["provider"]] = s.Value
	}
	ratio := map[string]float64{}
	for _, s := range q(`sum by (provider) (rate(external_api_requests_total{` + promNS + `,status!~"2.*"}[1h])) / sum by (provider) (rate(external_api_requests_total{` + promNS + `}[1h]))`) {
		ratio[s.Labels["provider"]] = s.Value
	}
	for p, c := range calls {
		out.Providers = append(out.Providers, adminProviderDTO{Provider: p, Calls24h: c, ErrorRatio: ratio[p]})
	}
	sort.Slice(out.Providers, func(i, j int) bool { return out.Providers[i].Provider < out.Providers[j].Provider })
	for _, s := range q(`histogram_quantile(0.75, sum by (le, route, device) (rate(web_vitals_seconds_bucket{` + promNS + `,name="LCP"}[24h])))`) {
		out.Vitals = append(out.Vitals, adminVitalDTO{Route: s.Labels["route"], Device: s.Labels["device"], LCPp75: s.Value})
	}
	sort.Slice(out.Vitals, func(i, j int) bool { return out.Vitals[i].LCPp75 > out.Vitals[j].LCPp75 })
	return out
}

func (h *Handlers) retryJob(w http.ResponseWriter, r *http.Request) {
	id, err := parseUUIDParam(chi.URLParam(r, "id"))
	if err != nil {
		httperr.Write(w, r, httperr.New(httperr.KindBadRequest, "Bad request", "id inválido"))
		return
	}
	if h.queue == nil {
		httperr.Write(w, r, httperr.New(httperr.KindNotImplemented, "Not implemented", "fila indisponível"))
		return
	}
	if err := h.queue.Retry(r.Context(), uuidStr(id)); err != nil {
		if errors.Is(err, queue.ErrNotRetryable) {
			httperr.Write(w, r, httperr.New(httperr.KindConflict, "Conflict", "o job não existe mais ou não está parado (dead)"))
			return
		}
		writeInternal(w, r, err)
		return
	}
	h.logger.Info("painel: job reprocessado pelo dono", slog.String("job_id", uuidStr(id)))
	w.WriteHeader(http.StatusAccepted)
}

// adminSyncIntegration: o dono sincroniza a conta de qualquer membro (ex.: depois de ajudar a
// arrumar um perfil privado).
func (h *Handlers) adminSyncIntegration(w http.ResponseWriter, r *http.Request) {
	id, err := parseUUIDParam(chi.URLParam(r, "id"))
	if err != nil {
		httperr.Write(w, r, httperr.New(httperr.KindBadRequest, "Bad request", "id inválido"))
		return
	}
	if _, err := h.q.GetIntegration(r.Context(), id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httperr.Write(w, r, httperr.New(httperr.KindNotFound, "Not found", "conta não encontrada"))
			return
		}
		writeInternal(w, r, err)
		return
	}
	job, enq, err := h.syncer.EnqueuePlatformSync(r.Context(), id, syncpkg.ScopeIncremental, syncpkg.TriggerManual)
	if err != nil {
		h.writeEnqueueError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, enqueueResp(job, enq))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

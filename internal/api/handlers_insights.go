package api

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	dbgen "github.com/RodolfoBonis/trophy-hunter-api/db/gen"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/httperr"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/ranking"
)

// Limites das listas do dashboard e da fila.
const (
	rarestLimit      = 6
	completionsLimit = 8
	nextLimit        = 24
	todayTrophies    = 3
	timelineDefault  = 60
	timelineMax      = 200
	maxNotesLen      = 4000
	maxPriority      = 3
)

// allowedTags é a taxonomia de marcações (igual ao CHECK do banco).
var allowedTags = []string{"missable", "online", "grind", "difficulty", "collectible", "buggy"}

// --- GET /stats ---------------------------------------------------------------------------------

type statsGamesDTO struct {
	Total      int `json:"total"`
	Completed  int `json:"completed"`
	Almost     int `json:"almost"`
	InProgress int `json:"in_progress"`
	NotStarted int `json:"not_started"`
	Abandoned  int `json:"abandoned"`
}

type statsTrophiesDTO struct {
	Total    int             `json:"total"`
	Earned   int             `json:"earned"`
	Platinum typeCountDTO    `json:"platinum"`
	Gold     typeCountDTO    `json:"gold"`
	Silver   typeCountDTO    `json:"silver"`
	Bronze   typeCountDTO    `json:"bronze"`
	Rarity   rarityEarnedDTO `json:"rarity"`
}

type rarestDTO struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Type      string     `json:"type"`
	Icon      string     `json:"icon"`
	RarityPct float64    `json:"rarity_pct"`
	EarnedAt  *time.Time `json:"earned_at"`
	GameID    string     `json:"game_id"`
	GameTitle string     `json:"game_title"`
	Platform  string     `json:"platform"`
}

type completionDTO struct {
	GameID      string     `json:"game_id"`
	Title       string     `json:"title"`
	Platform    string     `json:"platform"`
	ExternalID  string     `json:"external_id"`
	Cover       string     `json:"cover"`
	Hero        string     `json:"hero"`
	Total       int        `json:"total"`
	CompletedAt *time.Time `json:"completed_at"`
}

type evolutionDTO struct {
	Month      string `json:"month"`
	Earned     int    `json:"earned"`
	Cumulative int    `json:"cumulative"`
}

type statsDTO struct {
	Games       statsGamesDTO    `json:"games"`
	Trophies    statsTrophiesDTO `json:"trophies"`
	Rarest      []rarestDTO      `json:"rarest"`
	Completions []completionDTO  `json:"completions"`
	Evolution   []evolutionDTO   `json:"evolution"`
}

// getStats devolve o resumo do dashboard — GET /stats. Jogos contam uma vez (conta mais avançada).
func (h *Handlers) getStats(w http.ResponseWriter, r *http.Request) {
	out, err := h.statsFor(r.Context(), currentUser(r).ID)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// statsFor monta o resumo de um usuário (o próprio, ou um amigo que compartilha — ver social).
func (h *Handlers) statsFor(ctx context.Context, uid pgtype.UUID) (statsDTO, error) {
	games, err := h.q.StatsGames(ctx, dbgen.StatsGamesParams{UserID: uid, Almost: h.almostThreshold})
	if err != nil {
		return statsDTO{}, err
	}
	tr, err := h.q.StatsTrophies(ctx, uid)
	if err != nil {
		return statsDTO{}, err
	}
	rarest, err := h.q.RarestEarned(ctx, dbgen.RarestEarnedParams{UserID: uid, Lim: rarestLimit})
	if err != nil {
		return statsDTO{}, err
	}
	comps, err := h.q.RecentCompletions(ctx, dbgen.RecentCompletionsParams{UserID: uid, Lim: completionsLimit})
	if err != nil {
		return statsDTO{}, err
	}
	evo, err := h.q.Evolution(ctx, uid)
	if err != nil {
		return statsDTO{}, err
	}

	out := statsDTO{
		Games: statsGamesDTO{
			Total: int(games.Total), Completed: int(games.Completed), Almost: int(games.Almost),
			InProgress: int(games.InProgress), NotStarted: int(games.NotStarted), Abandoned: int(games.Abandoned),
		},
		Trophies: statsTrophiesDTO{
			Total: int(tr.Total), Earned: int(tr.Earned),
			Platinum: typeCountDTO{Earned: int(tr.PlatinumEarned), Total: int(tr.PlatinumTotal)},
			Gold:     typeCountDTO{Earned: int(tr.GoldEarned), Total: int(tr.GoldTotal)},
			Silver:   typeCountDTO{Earned: int(tr.SilverEarned), Total: int(tr.SilverTotal)},
			Bronze:   typeCountDTO{Earned: int(tr.BronzeEarned), Total: int(tr.BronzeTotal)},
			Rarity: rarityEarnedDTO{
				Legendary: int(tr.LegendaryEarned), Epic: int(tr.EpicEarned), Rare: int(tr.RareEarned),
				Uncommon: int(tr.UncommonEarned), Common: int(tr.CommonEarned),
			},
		},
		Rarest:      make([]rarestDTO, 0, len(rarest)),
		Completions: make([]completionDTO, 0, len(comps)),
		Evolution:   make([]evolutionDTO, 0, len(evo)),
	}
	for _, x := range rarest {
		out.Rarest = append(out.Rarest, rarestDTO{
			ID: uuidStr(x.ID), Name: x.Name, Type: x.Type, Icon: x.Icon, RarityPct: x.RarityPct,
			EarnedAt: tptr(x.EarnedAt), GameID: uuidStr(x.GameID), GameTitle: x.GameTitle, Platform: x.Platform,
		})
	}
	for _, c := range comps {
		out.Completions = append(out.Completions, completionDTO{
			GameID: uuidStr(c.ID), Title: c.Title, Platform: c.Platform, ExternalID: c.ExternalID,
			Cover: c.Cover, Hero: c.Hero, Total: int(c.Total), CompletedAt: tptr(c.LastUnlockAt),
		})
	}
	cum := 0
	for _, e := range evo {
		cum += int(e.Earned)
		out.Evolution = append(out.Evolution, evolutionDTO{Month: e.Month, Earned: int(e.Earned), Cumulative: cum})
	}
	return out, nil
}

// --- GET /next ----------------------------------------------------------------------------------

type remainingTrophyDTO struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description *string  `json:"description"`
	Type        string   `json:"type"`
	Hidden      bool     `json:"hidden"`
	Icon        string   `json:"icon"`
	IconLocked  string   `json:"icon_locked"`
	RarityPct   *float64 `json:"rarity_pct"`
	Tags        []string `json:"tags"`
}

type nextItemDTO struct {
	GameID             string     `json:"game_id"`
	Title              string     `json:"title"`
	Platform           string     `json:"platform"`
	ExternalID         string     `json:"external_id"`
	Cover              string     `json:"cover"`
	Hero               string     `json:"hero"`
	Earned             int        `json:"earned"`
	Total              int        `json:"total"`
	Remaining          int        `json:"remaining"`
	CompletionPct      float64    `json:"completion_pct"`
	AvgRemainingRarity float64    `json:"avg_remaining_rarity"`
	MinRemainingRarity float64    `json:"min_remaining_rarity"`
	RemainingMissable  int        `json:"remaining_missable"`
	RemainingOnline    int        `json:"remaining_online"`
	Priority           int        `json:"priority"`
	LastUnlockAt       *time.Time `json:"last_unlock_at"`
	// HoursTotal/HoursLeft: HowLongToBeat (100%) e o que falta proporcional ao progresso; null = sem dado.
	HoursTotal *float64        `json:"hours_total"`
	HoursLeft  *float64        `json:"hours_left"`
	Score      float64         `json:"score"`
	Factors    ranking.Factors `json:"factors"`
}

type nextDTO struct {
	Items []nextItemDTO `json:"items"`
	// Today é a sugestão "o que fazer hoje": o 1º da fila e os troféus que faltam mais fáceis.
	Today *todayDTO `json:"today"`
}

type todayDTO struct {
	GameID   string               `json:"game_id"`
	Trophies []remainingTrophyDTO `json:"trophies"`
}

// getNext devolve a fila Próxima Platina — GET /next (ver internal/ranking).
func (h *Handlers) getNext(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rows, err := h.q.NextPlatinumCandidates(ctx, currentUser(r).ID)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	byID := make(map[string]dbgen.NextPlatinumCandidatesRow, len(rows))
	cands := make([]ranking.Candidate, 0, len(rows))
	for _, row := range rows {
		id := uuidStr(row.ID)
		byID[id] = row
		cands = append(cands, ranking.Candidate{
			ID: id, Completion: row.CompletionPct, Remaining: int(row.Remaining),
			AvgRemainingRarity: row.AvgRemainingRarity, Priority: int(row.Priority),
			RemainingMissable: int(row.RemainingMissable), RemainingOnline: int(row.RemainingOnline),
			HoursLeft: hoursLeft(row.CompletionistH, row.CompletionPct),
		})
	}
	ranked := ranking.Rank(cands)
	if len(ranked) > nextLimit {
		ranked = ranked[:nextLimit]
	}

	out := nextDTO{Items: make([]nextItemDTO, 0, len(ranked))}
	for _, rk := range ranked {
		row := byID[rk.ID]
		out.Items = append(out.Items, nextItemDTO{
			GameID: rk.ID, Title: row.Title, Platform: row.Platform, ExternalID: row.ExternalID,
			Cover: row.Cover, Hero: row.Hero, Earned: int(row.Earned), Total: int(row.Total),
			Remaining: int(row.Remaining), CompletionPct: row.CompletionPct,
			AvgRemainingRarity: row.AvgRemainingRarity, MinRemainingRarity: row.MinRemainingRarity,
			RemainingMissable: int(row.RemainingMissable), RemainingOnline: int(row.RemainingOnline),
			Priority: int(row.Priority), LastUnlockAt: tptr(row.LastUnlockAt),
			HoursTotal: positive(row.CompletionistH), HoursLeft: positive(hoursLeft(row.CompletionistH, row.CompletionPct)),
			Score: rk.Score, Factors: rk.Factors,
		})
	}
	if len(ranked) > 0 {
		top := byID[ranked[0].ID]
		easy, err := h.q.EasiestRemaining(ctx, dbgen.EasiestRemainingParams{GameID: top.ID, Lim: todayTrophies})
		if err != nil {
			writeInternal(w, r, err)
			return
		}
		today := &todayDTO{GameID: uuidStr(top.ID), Trophies: make([]remainingTrophyDTO, 0, len(easy))}
		for _, t := range easy {
			today.Trophies = append(today.Trophies, remainingTrophyDTO{
				ID: uuidStr(t.ID), Name: t.Name, Description: t.Description, Type: t.Type, Hidden: t.Hidden,
				Icon: t.Icon, IconLocked: t.IconLocked, RarityPct: t.RarityPct, Tags: nonNil(t.Tags),
			})
		}
		out.Today = today
	}
	writeJSON(w, http.StatusOK, out)
}

// hoursLeft estima as horas que faltam: horas para 100% × fração que falta (0 = sem estimativa).
func hoursLeft(total, completion float64) float64 {
	if total <= 0 {
		return 0
	}
	left := total * (1 - completion)
	if left < 0.5 {
		left = 0.5 // nunca "0 h" num jogo ainda não fechado
	}
	return math.Round(left*10) / 10
}

func positive(v float64) *float64 {
	if v <= 0 {
		return nil
	}
	return &v
}

// --- GET /timeline ------------------------------------------------------------------------------

type timelineItemDTO struct {
	TrophyID      string     `json:"trophy_id"`
	Name          string     `json:"name"`
	Description   *string    `json:"description"`
	Type          string     `json:"type"`
	Icon          string     `json:"icon"`
	RarityPct     *float64   `json:"rarity_pct"`
	EarnedAt      *time.Time `json:"earned_at"`
	GameID        string     `json:"game_id"`
	GameTitle     string     `json:"game_title"`
	Platform      string     `json:"platform"`
	ExternalID    string     `json:"external_id"`
	Cover         string     `json:"cover"`
	Hero          string     `json:"hero"`
	AccountLabel  string     `json:"account_label"`
	CompletesGame bool       `json:"completes_game"`
}

type timelinePageDTO struct {
	Items []timelineItemDTO `json:"items"`
	// NextCursor é opaco ("<RFC3339Nano>|<uuid>"); null quando não há mais.
	NextCursor *string `json:"next_cursor"`
}

// getTimeline devolve os desbloqueios (todas as contas), do mais recente — GET /timeline.
func (h *Handlers) getTimeline(w http.ResponseWriter, r *http.Request) {
	qp := r.URL.Query()
	limit := clamp(atoiDefault(qp.Get("limit"), timelineDefault), 1, timelineMax)
	params := dbgen.TimelineParams{Lim: int32(limit)} //nolint:gosec // limit clampado
	if c := qp.Get("cursor"); c != "" {
		at, id, ok := parseCursor(c)
		if !ok {
			httperr.Write(w, r, httperr.New(httperr.KindBadRequest, "Bad request", "cursor inválido"))
			return
		}
		params.BeforeAt, params.BeforeID = at, id
	}
	params.UserID = currentUser(r).ID
	rows, err := h.q.Timeline(r.Context(), params)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	out := timelinePageDTO{Items: make([]timelineItemDTO, 0, len(rows))}
	for _, t := range rows {
		out.Items = append(out.Items, timelineItemDTO{
			TrophyID: uuidStr(t.ID), Name: t.Name, Description: t.Description, Type: t.Type, Icon: t.Icon,
			RarityPct: t.RarityPct, EarnedAt: tptr(t.EarnedAt), GameID: uuidStr(t.GameID), GameTitle: t.GameTitle,
			Platform: t.Platform, ExternalID: t.ExternalID, Cover: t.Cover, Hero: t.Hero,
			AccountLabel: t.AccountLabel, CompletesGame: t.CompletesGame,
		})
	}
	if len(rows) == limit {
		last := rows[len(rows)-1]
		c := last.EarnedAt.Time.UTC().Format(time.RFC3339Nano) + "|" + uuidStr(last.ID)
		out.NextCursor = &c
	}
	writeJSON(w, http.StatusOK, out)
}

func parseCursor(c string) (pgtype.Timestamptz, pgtype.UUID, bool) {
	ts, id, found := strings.Cut(c, "|")
	if !found {
		return pgtype.Timestamptz{}, pgtype.UUID{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return pgtype.Timestamptz{}, pgtype.UUID{}, false
	}
	u, err := parseUUIDParam(id)
	if err != nil {
		return pgtype.Timestamptz{}, pgtype.UUID{}, false
	}
	return pgtype.Timestamptz{Time: t, Valid: true}, u, true
}

// --- GET /alerts --------------------------------------------------------------------------------

type alertDTO struct {
	TrophyID           string   `json:"trophy_id"`
	Name               string   `json:"name"`
	Type               string   `json:"type"`
	Hidden             bool     `json:"hidden"`
	Icon               string   `json:"icon"`
	RarityPct          *float64 `json:"rarity_pct"`
	Tags               []string `json:"tags"`
	ServerShutdownRisk bool     `json:"server_shutdown_risk"`
	GameID             string   `json:"game_id"`
	GameTitle          string   `json:"game_title"`
	Platform           string   `json:"platform"`
}

// getAlerts lista troféus que faltam marcados como perdíveis/online/dificuldade — GET /alerts.
func (h *Handlers) getAlerts(w http.ResponseWriter, r *http.Request) {
	rows, err := h.q.Alerts(r.Context(), currentUser(r).ID)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	out := make([]alertDTO, 0, len(rows))
	for _, a := range rows {
		out = append(out, alertDTO{
			TrophyID: uuidStr(a.ID), Name: a.Name, Type: a.Type, Hidden: a.Hidden, Icon: a.Icon,
			RarityPct: a.RarityPct, Tags: nonNil(a.Tags), ServerShutdownRisk: a.ServerShutdownRisk,
			GameID: uuidStr(a.GameID), GameTitle: a.GameTitle, Platform: a.Platform,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// --- PATCH /games/{id} e /trophies/{id} --------------------------------------------------------

type annotationsRequest struct {
	Abandoned          *bool     `json:"abandoned"`
	Priority           *int      `json:"priority"`
	Notes              *string   `json:"notes"`
	Tags               *[]string `json:"tags"`
	ServerShutdownRisk *bool     `json:"server_shutdown_risk"`
}

// validate confere os campos comuns; devolve os tags normalizados (nil = não enviar).
func (req annotationsRequest) validate() ([]string, *int32, []httperr.FieldError) {
	var fields []httperr.FieldError
	var prio *int32
	if req.Priority != nil {
		if *req.Priority < 0 || *req.Priority > maxPriority {
			fields = append(fields, httperr.FieldError{Field: "priority", Code: "out_of_range"})
		} else {
			v := int32(*req.Priority) //nolint:gosec // 0..3
			prio = &v
		}
	}
	if req.Notes != nil && len(*req.Notes) > maxNotesLen {
		fields = append(fields, httperr.FieldError{Field: "notes", Code: "too_long"})
	}
	var tags []string
	if req.Tags != nil {
		tags = make([]string, 0, len(*req.Tags))
		for _, t := range *req.Tags {
			if !slices.Contains(allowedTags, t) {
				fields = append(fields, httperr.FieldError{Field: "tags", Code: "invalid"})
				break
			}
			if !slices.Contains(tags, t) {
				tags = append(tags, t)
			}
		}
	}
	return tags, prio, fields
}

func decodeAnnotations(w http.ResponseWriter, r *http.Request) (annotationsRequest, pgtype.UUID, bool) {
	id, err := parseUUIDParam(chi.URLParam(r, "id"))
	if err != nil {
		httperr.Write(w, r, httperr.New(httperr.KindBadRequest, "Bad request", "id inválido"))
		return annotationsRequest{}, pgtype.UUID{}, false
	}
	var req annotationsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httperr.Write(w, r, httperr.New(httperr.KindBadRequest, "Bad request", "corpo JSON inválido"))
		return annotationsRequest{}, pgtype.UUID{}, false
	}
	return req, id, true
}

// patchGame grava as marcações do jogo (abandonado, prioridade, notas, tags) — PATCH /games/{id}.
func (h *Handlers) patchGame(w http.ResponseWriter, r *http.Request) {
	req, id, ok := decodeAnnotations(w, r)
	if !ok {
		return
	}
	tags, prio, fields := req.validate()
	if len(fields) > 0 {
		httperr.Write(w, r, httperr.Validation("marcações inválidas", fields...))
		return
	}
	g, err := h.q.UpdateGameAnnotations(r.Context(), dbgen.UpdateGameAnnotationsParams{
		ID: id, UserID: currentUser(r).ID, Abandoned: req.Abandoned, Priority: prio, Notes: req.Notes, Tags: tags,
	})
	if err != nil {
		writeNotFoundOr(w, r, err, "jogo não encontrado")
		return
	}
	writeJSON(w, http.StatusOK, toGameDTO(g))
}

// patchTrophy grava as marcações do troféu (tags, notas, prioridade, risco de servidor) — PATCH /trophies/{id}.
func (h *Handlers) patchTrophy(w http.ResponseWriter, r *http.Request) {
	req, id, ok := decodeAnnotations(w, r)
	if !ok {
		return
	}
	tags, prio, fields := req.validate()
	if req.Abandoned != nil {
		fields = append(fields, httperr.FieldError{Field: "abandoned", Code: "not_allowed"})
	}
	if len(fields) > 0 {
		httperr.Write(w, r, httperr.Validation("marcações inválidas", fields...))
		return
	}
	t, err := h.q.UpdateTrophyAnnotations(r.Context(), dbgen.UpdateTrophyAnnotationsParams{
		ID: id, UserID: currentUser(r).ID, Priority: prio, Notes: req.Notes, Tags: tags, ServerShutdownRisk: req.ServerShutdownRisk,
	})
	if err != nil {
		writeNotFoundOr(w, r, err, "troféu não encontrado")
		return
	}
	writeJSON(w, http.StatusOK, toTrophyDTO(t))
}

func writeInternal(w http.ResponseWriter, r *http.Request, err error) {
	httperr.Write(w, r, httperr.Wrap(httperr.KindInternal, "Internal error", err))
}

func writeNotFoundOr(w http.ResponseWriter, r *http.Request, err error, msg string) {
	if errors.Is(err, pgx.ErrNoRows) {
		httperr.Write(w, r, httperr.New(httperr.KindNotFound, "Not found", msg))
		return
	}
	writeInternal(w, r, err)
}

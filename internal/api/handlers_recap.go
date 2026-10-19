package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	dbgen "github.com/RodolfoBonis/trophy-hunter-api/db/gen"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/ai"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/httperr"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/recap"
)

// GET /recap?period=YYYY-MM|YYYY — retrospectiva ("Wrapped") do período, calculada na hora.

type recapGameDTO struct {
	GameID     string `json:"game_id"`
	Title      string `json:"title"`
	Platform   string `json:"platform"`
	ExternalID string `json:"external_id"`
	Cover      string `json:"cover"`
	Hero       string `json:"hero"`
	Unlocked   int    `json:"unlocked"`
	Earned     int    `json:"earned"`
	Total      int    `json:"total"`
}

type recapRareDTO struct {
	Name      string     `json:"name"`
	Type      string     `json:"type"`
	Hidden    bool       `json:"hidden"`
	Icon      string     `json:"icon"`
	RarityPct float64    `json:"rarity_pct"`
	EarnedAt  *time.Time `json:"earned_at"`
	GameTitle string     `json:"game_title"`
	Platform  string     `json:"platform"`
}

type recapDTO struct {
	Period          string          `json:"period"`
	Label           string          `json:"label"`
	Partial         bool            `json:"partial"`
	Earned          int             `json:"earned"`
	PreviousEarned  int             `json:"previous_earned"`
	Platinum        int             `json:"platinum"`
	Gold            int             `json:"gold"`
	Silver          int             `json:"silver"`
	Bronze          int             `json:"bronze"`
	Achievements    int             `json:"achievements"`
	Rare            int             `json:"rare"`
	Games           int             `json:"games"`
	ActiveDays      int             `json:"active_days"`
	LongestStreak   int             `json:"longest_streak"`
	FavoriteWeekday string          `json:"favorite_weekday"`
	BestDay         *time.Time      `json:"best_day"`
	BestDayCount    int             `json:"best_day_count"`
	TopGames        []recapGameDTO  `json:"top_games"`
	Rarest          []recapRareDTO  `json:"rarest"`
	Completions     []completionDTO `json:"completions"`
}

func ts(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

func (h *Handlers) getRecap(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p, err := recap.ParsePeriod(r.URL.Query().Get("period"), time.Now())
	if err != nil {
		httperr.Write(w, r, httperr.New(httperr.KindBadRequest, "Bad request", err.Error()))
		return
	}
	uid := currentUser(r).ID
	since, until := ts(p.Since), ts(p.Until)
	tot, err := h.q.RecapTotals(ctx, dbgen.RecapTotalsParams{UserID: uid, Since: since, Until: until})
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	prev, err := h.q.RecapTotals(ctx, dbgen.RecapTotalsParams{UserID: uid, Since: ts(p.PrevSince), Until: ts(p.PrevUntil)})
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	top, err := h.q.RecapTopGames(ctx, dbgen.RecapTopGamesParams{UserID: uid, Since: since, Until: until, Lim: 5})
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	rare, err := h.q.RecapRarest(ctx, dbgen.RecapRarestParams{UserID: uid, Since: since, Until: until, Lim: 3})
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	comps, err := h.q.RecapCompletions(ctx, dbgen.RecapCompletionsParams{UserID: uid, Since: since, Until: until})
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	days, err := h.q.RecapDays(ctx, dbgen.RecapDaysParams{UserID: uid, Since: since, Until: until})
	if err != nil {
		writeInternal(w, r, err)
		return
	}

	out := recapDTO{
		Period: p.Key, Label: p.Label, Partial: p.Partial,
		Earned: int(tot.Earned), PreviousEarned: int(prev.Earned), Platinum: int(tot.Platinum), Gold: int(tot.Gold),
		Silver: int(tot.Silver), Bronze: int(tot.Bronze), Achievements: int(tot.Achievements), Rare: int(tot.Rare),
		Games: int(tot.Games), ActiveDays: len(days),
		TopGames: make([]recapGameDTO, 0, len(top)), Rarest: make([]recapRareDTO, 0, len(rare)), Completions: make([]completionDTO, 0, len(comps)),
	}
	dts := make([]time.Time, 0, len(days))
	counts := make([]int, 0, len(days))
	for _, d := range days {
		day := time.Date(d.Day.Time.Year(), d.Day.Time.Month(), d.Day.Time.Day(), 0, 0, 0, 0, recap.Location)
		dts = append(dts, day)
		counts = append(counts, int(d.N))
		if int(d.N) > out.BestDayCount {
			bd := day
			out.BestDay, out.BestDayCount = &bd, int(d.N)
		}
	}
	out.LongestStreak = recap.LongestStreak(dts)
	out.FavoriteWeekday = recap.FavoriteWeekday(dts, counts)
	for _, g := range top {
		out.TopGames = append(out.TopGames, recapGameDTO{
			GameID: uuidStr(g.ID), Title: g.Title, Platform: g.Platform, ExternalID: g.ExternalID, Cover: g.Cover, Hero: g.Hero,
			Unlocked: int(g.Unlocked), Earned: int(g.Earned), Total: int(g.Total),
		})
	}
	for _, t := range rare {
		name := t.Name
		out.Rarest = append(out.Rarest, recapRareDTO{
			Name: name, Type: t.Type, Hidden: t.Hidden, Icon: t.Icon, RarityPct: t.RarityPct, EarnedAt: tptr(t.EarnedAt),
			GameTitle: t.GameTitle, Platform: t.Platform,
		})
	}
	for _, c := range comps {
		out.Completions = append(out.Completions, completionDTO{
			GameID: uuidStr(c.ID), Title: c.Title, Platform: c.Platform, ExternalID: c.ExternalID,
			Cover: c.Cover, Hero: c.Hero, Total: int(c.Total), CompletedAt: tptr(c.LastUnlockAt),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// --- Resumo semanal da IA -----------------------------------------------------------------------

type weeklyRecapDTO struct {
	Week      string          `json:"week"`
	Status    string          `json:"status"`
	Content   json.RawMessage `json:"content"`
	Error     *string         `json:"error"`
	UpdatedAt *time.Time      `json:"updated_at"`
}

type weeklyRecapEnvelope struct {
	Recap *weeklyRecapDTO `json:"recap"`
}

func toWeeklyRecap(r dbgen.AiRecap) *weeklyRecapDTO {
	content := json.RawMessage("null")
	if len(r.Content) > 0 {
		content = r.Content
	}
	return &weeklyRecapDTO{Week: r.Week.Time.Format("2006-01-02"), Status: r.Status, Content: content, Error: r.Error, UpdatedAt: tptr(r.UpdatedAt)}
}

// GET /recap/weekly — o resumo mais recente (null se nunca houve).
func (h *Handlers) getWeeklyRecap(w http.ResponseWriter, r *http.Request) {
	rec, err := h.q.GetLatestRecap(r.Context(), currentUser(r).ID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusOK, weeklyRecapEnvelope{})
		return
	}
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, weeklyRecapEnvelope{Recap: toWeeklyRecap(rec)})
}

// POST /recap/weekly — gera o resumo da semana corrente agora (se ainda não houver).
func (h *Handlers) generateWeeklyRecap(w http.ResponseWriter, r *http.Request) {
	if !h.aiEnabled(w, r) {
		return
	}
	ctx := r.Context()
	uid := currentUser(r).ID
	week := recap.WeekStart(time.Now())
	if _, err := ai.EnqueueWeeklyRecap(ctx, h.q, h.queue, uid, week); err != nil {
		writeInternal(w, r, err)
		return
	}
	rec, err := h.q.GetRecap(ctx, dbgen.GetRecapParams{UserID: uid, Week: pgtype.Date{Time: time.Date(week.Year(), week.Month(), week.Day(), 0, 0, 0, 0, time.UTC), Valid: true}})
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, weeklyRecapEnvelope{Recap: toWeeklyRecap(rec)})
}

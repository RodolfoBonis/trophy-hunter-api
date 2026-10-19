package api

import (
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/RodolfoBonis/trophy-hunter-api/internal/badges"
)

// Medalhas do app: GET /me/badges (vitrine + andamento + temporada) e
// GET /social/friends/{id}/badges (só as ganhas; mesma regra recíproca do social).

type badgeDTO struct {
	Key         string     `json:"key"`
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Tier        string     `json:"tier"`
	Season      string     `json:"season,omitempty"`
	EarnedAt    *time.Time `json:"earned_at,omitempty"`
	Value       int        `json:"value"`
	Goal        int        `json:"goal"`
	Earned      bool       `json:"earned"`
}

type seasonDTO struct {
	Key          string `json:"key"`
	Label        string `json:"label"`
	Platinums    int    `json:"platinums"`
	Rare         int    `json:"rare"`
	PlatinumGoal int    `json:"platinum_goal"`
	RareGoal     int    `json:"rare_goal"`
	Done         bool   `json:"done"`
}

type myBadgesDTO struct {
	Earned  []badgeDTO `json:"earned"`
	Next    []badgeDTO `json:"next"`
	Catalog []badgeDTO `json:"catalog"`
	Season  seasonDTO  `json:"season"`
}

var catalogByKey = func() map[string]badges.Def {
	m := map[string]badges.Def{}
	for _, d := range badges.Catalog {
		m[d.Key] = d
	}
	return m
}()

func progressDTO(p badges.Progress) badgeDTO {
	return badgeDTO{Key: p.Key, Name: p.Name, Description: p.Description, Tier: string(p.Tier), Value: p.Value, Goal: p.Goal, Earned: p.Earned}
}

// loadEarnedBadges: as gravadas (medalhas fixas + temporadas), mais recente primeiro.
func (h *Handlers) loadEarnedBadges(r *http.Request, userID pgtype.UUID) ([]badgeDTO, error) {
	rows, err := h.q.ListUserBadges(r.Context(), userID)
	if err != nil {
		return nil, err
	}
	out := make([]badgeDTO, 0, len(rows))
	for _, b := range rows {
		d := badgeDTO{Key: b.BadgeKey, Season: b.Season, EarnedAt: tptr(b.EarnedAt), Earned: true, Value: 1, Goal: 1}
		if b.BadgeKey == "season" {
			d.Name, d.Tier = badges.SeasonLabel(b.Season), string(badges.TierGold)
			d.Description = "Fechou a temporada do mês."
		} else if def, ok := catalogByKey[b.BadgeKey]; ok {
			d.Name, d.Description, d.Tier, d.Value, d.Goal = def.Name, def.Description, string(def.Tier), def.Goal, def.Goal
		} else {
			continue // medalha que saiu do catálogo
		}
		out = append(out, d)
	}
	return out, nil
}

func (h *Handlers) getMyBadges(w http.ResponseWriter, r *http.Request) {
	me := currentUser(r)
	// Materializa o que já foi alcançado (sem push: leitura não avisa).
	if _, err := h.badges.Sync(r.Context(), me.ID, false); err != nil {
		writeInternal(w, r, err)
		return
	}
	snap, err := h.badges.Snapshot(r.Context(), me.ID)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	earned, err := h.loadEarnedBadges(r, me.ID)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	out := myBadgesDTO{Earned: earned, Next: []badgeDTO{}, Catalog: make([]badgeDTO, 0, len(snap.Progress))}
	for _, p := range snap.Progress {
		out.Catalog = append(out.Catalog, progressDTO(p))
	}
	for _, p := range badges.Next(snap.Progress, 3) {
		out.Next = append(out.Next, progressDTO(p))
	}
	s := snap.Season
	out.Season = seasonDTO{
		Key: s.Key, Label: badges.SeasonLabel(s.Key), Platinums: s.Platinums, Rare: s.Rare,
		PlatinumGoal: badges.SeasonPlatinumGoal, RareGoal: badges.SeasonRareGoal, Done: badges.SeasonDone(s),
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handlers) getFriendBadges(w http.ResponseWriter, r *http.Request) {
	f, ok := h.loadFriend(w, r)
	if !ok {
		return
	}
	if _, err := h.badges.Sync(r.Context(), f.ID, false); err != nil {
		writeInternal(w, r, err)
		return
	}
	earned, err := h.loadEarnedBadges(r, f.ID)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string][]badgeDTO{"earned": earned})
}

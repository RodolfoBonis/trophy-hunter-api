package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	dbgen "github.com/RodolfoBonis/trophy-hunter-api/db/gen"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/ai"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/hunt"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/httperr"
)

// Modo caçada: o roadmap da IA vira checklist ligado aos troféus reais da conta aberta.
//   GET /games/{id}/hunt                    → etapas (com troféus casados), o que já foi marcado e se há sync rodando
//   PUT /games/{id}/hunt/stages/{idx} {done} → marca/desmarca uma etapa

type huntStageDTO struct {
	Index       int      `json:"index"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Missables   []string `json:"missables"`
	TrophyIDs   []string `json:"trophy_ids"`
	Unmatched   []string `json:"unmatched"`
	Earned      int      `json:"earned"`
	// Done: a pessoa marcou. Complete: todos os troféus casados da etapa já desbloqueados.
	Done     bool `json:"done"`
	Complete bool `json:"complete"`
}

type huntDTO struct {
	// Roadmap: none (ainda não gerado) | pending | running | ready | error.
	Roadmap           string         `json:"roadmap"`
	Syncing           bool           `json:"syncing"`
	MissablesOverview []string       `json:"missables_overview"`
	Stages            []huntStageDTO `json:"stages"`
}

func (h *Handlers) getHunt(w http.ResponseWriter, r *http.Request) {
	game, ok := h.loadGameParam(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	me := currentUser(r)
	syncing, err := h.q.GameSyncInFlight(ctx, uuidStr(game.ID))
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	out := huntDTO{Roadmap: "none", Syncing: syncing, MissablesOverview: []string{}, Stages: []huntStageDTO{}}

	g, err := h.q.GetRoadmap(ctx, dbgen.GetRoadmapParams{UserID: me.ID, Platform: game.Platform, GameExternalID: game.ExternalID})
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusOK, out)
		return
	}
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	out.Roadmap = g.Status
	d, err := h.guideFor(ctx, me, g)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	content := d.EditedContent // a versão corrigida pela pessoa vale sobre a gerada
	if len(content) == 0 || string(content) == "null" {
		content = d.Content
	}
	var rm ai.Roadmap
	if g.Status != "ready" || len(content) == 0 || string(content) == "null" || json.Unmarshal(content, &rm) != nil {
		writeJSON(w, http.StatusOK, out)
		return
	}

	trophies, err := h.q.ListTrophiesByGame(ctx, game.ID)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	earned := map[string]bool{}
	list := make([]hunt.Trophy, 0, len(trophies))
	for _, t := range trophies {
		id := uuidStr(t.ID)
		list = append(list, hunt.Trophy{ID: id, Name: t.Name})
		earned[id] = t.Earned
	}
	names := make([][]string, len(rm.Stages))
	for i, s := range rm.Stages {
		names[i] = s.Trophies
	}
	matches := hunt.Match(names, list)

	done, err := h.q.ListHuntStagesDone(ctx, dbgen.ListHuntStagesDoneParams{UserID: me.ID, GamePlatform: game.Platform, GameExternalID: game.ExternalID})
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	doneSet := map[int]bool{}
	for _, i := range done {
		doneSet[int(i)] = true
		if int(i) >= len(rm.Stages) { // roadmap refeito com menos etapas: marca órfã sai
			if err := h.q.ResetHuntStagesFrom(ctx, dbgen.ResetHuntStagesFromParams{
				UserID: me.ID, GamePlatform: game.Platform, GameExternalID: game.ExternalID, StageCount: int32(len(rm.Stages)), //nolint:gosec // poucas etapas
			}); err != nil {
				writeInternal(w, r, err)
				return
			}
		}
	}
	if rm.MissablesOverview != nil {
		out.MissablesOverview = rm.MissablesOverview
	}
	for i, s := range rm.Stages {
		st := huntStageDTO{
			Index: i, Title: s.Title, Description: s.Description, Missables: nonNilStrings(s.Missables),
			TrophyIDs: matches[i].TrophyIDs, Unmatched: matches[i].Unmatched, Done: doneSet[i],
		}
		for _, id := range st.TrophyIDs {
			if earned[id] {
				st.Earned++
			}
		}
		st.Complete = len(st.TrophyIDs) > 0 && st.Earned == len(st.TrophyIDs)
		out.Stages = append(out.Stages, st)
	}
	writeJSON(w, http.StatusOK, out)
}

type huntStageRequest struct {
	Done *bool `json:"done"`
}

func (h *Handlers) putHuntStage(w http.ResponseWriter, r *http.Request) {
	game, ok := h.loadGameParam(w, r)
	if !ok {
		return
	}
	idx, err := strconv.Atoi(chi.URLParam(r, "idx"))
	if err != nil || idx < 0 || idx >= 100 {
		httperr.Write(w, r, httperr.New(httperr.KindBadRequest, "Bad request", "etapa inválida"))
		return
	}
	var req huntStageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Done == nil {
		httperr.Write(w, r, httperr.New(httperr.KindBadRequest, "Bad request", "informe done (true/false)"))
		return
	}
	me := currentUser(r)
	if *req.Done {
		err = h.q.MarkHuntStage(r.Context(), dbgen.MarkHuntStageParams{UserID: me.ID, GamePlatform: game.Platform, GameExternalID: game.ExternalID, StageIdx: int32(idx)}) //nolint:gosec // idx < 100
	} else {
		err = h.q.UnmarkHuntStage(r.Context(), dbgen.UnmarkHuntStageParams{UserID: me.ID, GamePlatform: game.Platform, GameExternalID: game.ExternalID, StageIdx: int32(idx)}) //nolint:gosec // idx < 100
	}
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

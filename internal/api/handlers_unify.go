package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	dbgen "github.com/RodolfoBonis/trophy-hunter-api/db/gen"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/httperr"
	syncpkg "github.com/RodolfoBonis/trophy-hunter-api/internal/sync"
)

type linkRequest struct {
	OtherGameID string `json:"other_game_id"`
}

// linkGame vincula manualmente o jogo ao da outra plataforma — POST /games/{id}/link. Vale para
// todas as contas que têm cada jogo e trava a decisão (a unificação automática não desfaz).
func (h *Handlers) linkGame(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	a, ok := h.loadGameParam(w, r)
	if !ok {
		return
	}
	var req linkRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httperr.Write(w, r, httperr.New(httperr.KindBadRequest, "Bad request", "corpo JSON inválido"))
		return
	}
	otherID, err := parseUUIDParam(req.OtherGameID)
	if err != nil {
		httperr.Write(w, r, httperr.Validation("other_game_id inválido", httperr.FieldError{Field: "other_game_id", Code: "invalid"}))
		return
	}
	b, err := h.q.GetGameForUser(ctx, dbgen.GetGameForUserParams{ID: otherID, UserID: currentUser(r).ID})
	if err != nil {
		writeNotFoundOr(w, r, err, "jogo não encontrado")
		return
	}
	if a.Platform == b.Platform {
		httperr.Write(w, r, httperr.Validation("vincule a um jogo de OUTRA plataforma", httperr.FieldError{Field: "other_game_id", Code: "same_platform"}))
		return
	}
	target := a.UnifiedGameID
	if !target.Valid {
		target = b.UnifiedGameID
	}
	if !target.Valid {
		u, err := h.q.CreateManualUnified(ctx, dbgen.CreateManualUnifiedParams{Title: a.Title, NormalizedTitle: syncpkg.NormalizeTitle(a.Title)})
		if err != nil {
			writeInternal(w, r, err)
			return
		}
		target = u.ID
	}
	for _, g := range []dbgen.Game{a, b} {
		if err := h.q.SetUnifiedForGameKey(ctx, dbgen.SetUnifiedForGameKeyParams{UserID: currentUser(r).ID, UnifiedGameID: target, Platform: g.Platform, ExternalID: g.ExternalID}); err != nil {
			writeInternal(w, r, err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// unlinkGame desfaz o vínculo cross-platform do jogo — DELETE /games/{id}/link (decisão travada).
func (h *Handlers) unlinkGame(w http.ResponseWriter, r *http.Request) {
	g, ok := h.loadGameParam(w, r)
	if !ok {
		return
	}
	if err := h.q.SetUnifiedForGameKey(r.Context(), dbgen.SetUnifiedForGameKeyParams{UserID: currentUser(r).ID, Platform: g.Platform, ExternalID: g.ExternalID}); err != nil {
		writeInternal(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// linkCandidates busca jogos da OUTRA plataforma para vincular — GET /games/{id}/link-candidates?q=.
func (h *Handlers) linkCandidates(w http.ResponseWriter, r *http.Request) {
	g, ok := h.loadGameParam(w, r)
	if !ok {
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		q = g.Title
	}
	norm := syncpkg.EscapeLike(syncpkg.NormalizeTitle(q))
	rows, err := h.q.LinkCandidates(r.Context(), dbgen.LinkCandidatesParams{UserID: currentUser(r).ID, Platform: g.Platform, Q: norm})
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	out := make([]crossPlatformDTO, 0, len(rows))
	for _, c := range rows {
		out = append(out, crossPlatformDTO{
			GameID: uuidStr(c.ID), Platform: c.Platform, ExternalID: c.ExternalID, Title: c.Title,
			Cover: c.Cover, Hero: c.Hero, Earned: int(c.Earned), Total: int(c.Total),
			CompletionPct: c.CompletionPct, LastUnlockAt: tptr(c.LastUnlockAt),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handlers) loadGameParam(w http.ResponseWriter, r *http.Request) (dbgen.Game, bool) {
	id, err := parseUUIDParam(chi.URLParam(r, "id"))
	if err != nil {
		httperr.Write(w, r, httperr.New(httperr.KindBadRequest, "Bad request", "id inválido"))
		return dbgen.Game{}, false
	}
	// Jogo do usuário: de outra pessoa responde 404 (sem revelar que existe).
	g, err := h.q.GetGameForUser(r.Context(), dbgen.GetGameForUserParams{ID: id, UserID: currentUser(r).ID})
	if err != nil {
		writeNotFoundOr(w, r, err, "jogo não encontrado")
		return dbgen.Game{}, false
	}
	return g, true
}

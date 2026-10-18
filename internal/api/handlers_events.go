package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/httperr"
)

// unlockEventDTO ↔ components.schemas.UnlockEvent: um troféu recém-desbloqueado (aviso na UI).
type unlockEventDTO struct {
	ID            string     `json:"id"`
	DetectedAt    *time.Time `json:"detected_at"`
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
	CompletesGame bool       `json:"completes_game"`
}

// listUnseenUnlocks — GET /events/unseen: troféus desbloqueados ainda não exibidos (mais antigos primeiro).
func (h *Handlers) listUnseenUnlocks(w http.ResponseWriter, r *http.Request) {
	rows, err := h.q.ListUnseenUnlocks(r.Context())
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	out := make([]unlockEventDTO, 0, len(rows))
	for _, e := range rows {
		out = append(out, unlockEventDTO{
			ID: uuidStr(e.ID), DetectedAt: tptr(e.DetectedAt), TrophyID: uuidStr(e.TrophyID),
			Name: e.TrophyName, Description: e.TrophyDescription, Type: e.TrophyType, Icon: e.TrophyIcon,
			RarityPct: e.RarityPct, EarnedAt: tptr(e.EarnedAt), GameID: uuidStr(e.GameID), GameTitle: e.GameTitle,
			Platform: e.Platform, ExternalID: e.ExternalID, Cover: e.Cover, Hero: e.Hero, CompletesGame: e.CompletesGame,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// markUnlocksSeen — POST /events/seen {ids}: marca os avisos como exibidos.
func (h *Handlers) markUnlocksSeen(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []string `json:"ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httperr.Write(w, r, httperr.New(httperr.KindBadRequest, "Bad request", "corpo JSON inválido"))
		return
	}
	if len(req.IDs) == 0 || len(req.IDs) > 500 {
		httperr.Write(w, r, httperr.Validation("informe de 1 a 500 ids", httperr.FieldError{Field: "ids", Code: "invalid"}))
		return
	}
	ids := make([]pgtype.UUID, 0, len(req.IDs))
	for _, s := range req.IDs {
		id, err := parseUUIDParam(s)
		if err != nil {
			httperr.Write(w, r, httperr.Validation("id inválido", httperr.FieldError{Field: "ids", Code: "invalid"}))
			return
		}
		ids = append(ids, id)
	}
	if _, err := h.q.MarkUnlocksSeen(r.Context(), ids); err != nil {
		writeInternal(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	dbgen "github.com/RodolfoBonis/trophy-hunter-api/db/gen"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/httperr"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/queue"
	syncpkg "github.com/RodolfoBonis/trophy-hunter-api/internal/sync"
)

type syncRequest struct {
	Scope string `json:"scope"`
}

// syncPlatform enfileira um sync da plataforma — POST /integrations/{platform}/sync.
func (h *Handlers) syncPlatform(w http.ResponseWriter, r *http.Request) {
	platform := chi.URLParam(r, "platform")
	if !validPlatform(platform) {
		httperr.Write(w, r, httperr.New(httperr.KindBadRequest, "Bad request", "plataforma inválida"))
		return
	}
	if _, err := h.q.GetIntegration(r.Context(), platform); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httperr.Write(w, r, httperr.New(httperr.KindNotFound, "Not found", "integração não conectada"))
			return
		}
		httperr.Write(w, r, httperr.Wrap(httperr.KindInternal, "Internal error", err))
		return
	}

	scope := syncpkg.ScopeIncremental
	if r.Body != nil {
		var req syncRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err == nil && req.Scope != "" {
			scope = req.Scope
		}
	}
	if scope != syncpkg.ScopeFull && scope != syncpkg.ScopeIncremental {
		httperr.Write(w, r, httperr.Validation("scope inválido",
			httperr.FieldError{Field: "scope", Code: "invalid"}))
		return
	}

	job, enq, err := h.syncer.EnqueuePlatformSync(r.Context(), platform, scope, syncpkg.TriggerManual)
	if err != nil {
		httperr.Write(w, r, httperr.Wrap(httperr.KindInternal, "Internal error", err))
		return
	}
	writeJSON(w, http.StatusAccepted, enqueueResp(job, enq))
}

// syncGame enfileira o sync de um único jogo — POST /games/{id}/sync.
func (h *Handlers) syncGame(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	gameUUID, err := parseUUIDParam(id)
	if err != nil {
		httperr.Write(w, r, httperr.New(httperr.KindBadRequest, "Bad request", "id inválido"))
		return
	}
	if _, err := h.q.GetGameByID(r.Context(), gameUUID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httperr.Write(w, r, httperr.New(httperr.KindNotFound, "Not found", "jogo não encontrado"))
			return
		}
		httperr.Write(w, r, httperr.Wrap(httperr.KindInternal, "Internal error", err))
		return
	}
	job, enq, err := h.syncer.EnqueueGameSync(r.Context(), id, syncpkg.TriggerManual)
	if err != nil {
		httperr.Write(w, r, httperr.Wrap(httperr.KindInternal, "Internal error", err))
		return
	}
	writeJSON(w, http.StatusAccepted, enqueueResp(job, enq))
}

// listSyncRuns devolve o log de sincronização — GET /sync-runs?platform&limit.
func (h *Handlers) listSyncRuns(w http.ResponseWriter, r *http.Request) {
	var platform *string
	if p := r.URL.Query().Get("platform"); p != "" {
		if !validPlatform(p) {
			httperr.Write(w, r, httperr.New(httperr.KindBadRequest, "Bad request", "plataforma inválida"))
			return
		}
		platform = &p
	}
	limit := 50
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil {
			limit = n
		}
	}
	limit = clamp(limit, 1, 200)

	runs, err := h.q.ListSyncRuns(r.Context(), dbgen.ListSyncRunsParams{Limit: int32(limit), Platform: platform}) //nolint:gosec // limit clamped
	if err != nil {
		httperr.Write(w, r, httperr.Wrap(httperr.KindInternal, "Internal error", err))
		return
	}
	out := make([]syncRunDTO, 0, len(runs))
	for _, run := range runs {
		out = append(out, toSyncRunDTO(run))
	}
	writeJSON(w, http.StatusOK, out)
}

func enqueueResp(job *queue.Job, enq bool) enqueueResponseDTO {
	var id *string
	if job != nil {
		v := job.ID
		id = &v
	}
	return enqueueResponseDTO{Enqueued: enq, JobID: id}
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

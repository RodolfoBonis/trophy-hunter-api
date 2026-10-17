// Package api monta a borda HTTP: router, middlewares e handlers. Recebe os serviços por injeção;
// não conhece detalhes de persistência além das queries geradas (dbgen) que consome.
package api

import (
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"

	dbgen "github.com/RodolfoBonis/trophy-hunter-api/db/gen"
	syncpkg "github.com/RodolfoBonis/trophy-hunter-api/internal/sync"
)

// Handlers agrega as dependências consumidas pelas rotas.
type Handlers struct {
	q               *dbgen.Queries
	pool            *pgxpool.Pool
	syncer          *syncpkg.Syncer
	logger          *slog.Logger
	almostThreshold float64
}

// HandlerDeps injeta as dependências dos handlers.
type HandlerDeps struct {
	Queries         *dbgen.Queries
	Pool            *pgxpool.Pool
	Syncer          *syncpkg.Syncer
	Logger          *slog.Logger
	AlmostThreshold float64
}

// NewHandlers constrói os handlers.
func NewHandlers(d HandlerDeps) *Handlers {
	at := d.AlmostThreshold
	if at <= 0 {
		at = 0.8
	}
	return &Handlers{
		q:               d.Queries,
		pool:            d.Pool,
		syncer:          d.Syncer,
		logger:          d.Logger,
		almostThreshold: at,
	}
}

// health responde 200 (liveness).
func (h *Handlers) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ready responde 200 se o Postgres responde ao ping; 503 caso contrário.
func (h *Handlers) ready(w http.ResponseWriter, r *http.Request) {
	if err := h.pool.Ping(r.Context()); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

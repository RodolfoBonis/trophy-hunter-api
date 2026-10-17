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
	authReady       func() bool
}

// HandlerDeps injeta as dependências dos handlers.
type HandlerDeps struct {
	Queries         *dbgen.Queries
	Pool            *pgxpool.Pool
	Syncer          *syncpkg.Syncer
	Logger          *slog.Logger
	AlmostThreshold float64
	// AuthReady indica se o autenticador já carregou o provider OIDC (readyz). nil = sempre pronto.
	AuthReady func() bool
}

// NewHandlers constrói os handlers.
func NewHandlers(d HandlerDeps) *Handlers {
	at := d.AlmostThreshold
	if at <= 0 {
		at = 0.8
	}
	authReady := d.AuthReady
	if authReady == nil {
		authReady = func() bool { return true }
	}
	return &Handlers{
		q:               d.Queries,
		pool:            d.Pool,
		syncer:          d.Syncer,
		logger:          d.Logger,
		almostThreshold: at,
		authReady:       authReady,
	}
}

// health responde 200 (liveness).
func (h *Handlers) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ready responde 200 se o Postgres responde ao ping E o provider de auth já carregou; 503 caso
// contrário (o k8s mantém o pod fora do Service até ficar pronto).
func (h *Handlers) ready(w http.ResponseWriter, r *http.Request) {
	if err := h.pool.Ping(r.Context()); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable", "reason": "db"})
		return
	}
	if !h.authReady() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable", "reason": "auth"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

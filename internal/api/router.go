package api

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/RodolfoBonis/trophy-hunter-api/internal/api/middleware"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/auth"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/metrics"
)

// RouterDeps são as dependências para montar o router.
type RouterDeps struct {
	Handlers     *Handlers
	Auth         *auth.Authenticator
	Logger       *slog.Logger
	ServiceName  string
	WriteTimeout time.Duration
}

// requestTimeout deriva o timeout de request do chi a partir do HTTP_WRITE_TIMEOUT do servidor:
// fica ABAIXO dele para o handler cancelar e responder antes de o servidor matar a conexão.
func requestTimeout(writeTimeout time.Duration) time.Duration {
	const margin = 2 * time.Second
	if writeTimeout <= 0 {
		return 28 * time.Second
	}
	if writeTimeout <= margin {
		return writeTimeout
	}
	return writeTimeout - margin
}

// NewRouter monta o chi router. /healthz, /readyz e /metrics são abertos (probes/Prometheus);
// o resto fica sob /api/v1 exigindo autenticação (o web faz proxy de /api → api sem rewrite).
func NewRouter(d RouterDeps) http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	// RequestLogger POR FORA do Recoverer: panics viram 500 no Recoverer interno e são logados aqui
	// com o status correto.
	r.Use(middleware.RequestLogger(d.Logger))
	r.Use(middleware.Recoverer)
	r.Use(middleware.SecurityHeaders)
	r.Use(middleware.Tracing(d.ServiceName))
	r.Use(middleware.Timeout(requestTimeout(d.WriteTimeout)))

	h := d.Handlers

	// Infra (aberto).
	r.Get("/healthz", h.health)
	r.Get("/readyz", h.ready)
	r.Handle("/metrics", metrics.Handler())

	// API autenticada.
	r.Route("/api/v1", func(r chi.Router) {
		r.Use(d.Auth.Middleware)
		r.Use(middleware.EnforceJSONBody)

		r.Get("/me", h.getMe)

		r.Get("/integrations", h.listIntegrations)
		r.Put("/integrations/steam", h.connectSteam)
		r.Delete("/integrations/{platform}", h.deleteIntegration)
		r.Post("/integrations/{platform}/sync", h.syncPlatform)

		r.Get("/games", h.listGames)
		r.Get("/games/{id}", h.getGame)
		r.Post("/games/{id}/sync", h.syncGame)

		r.Get("/sync-runs", h.listSyncRuns)
	})

	return r
}

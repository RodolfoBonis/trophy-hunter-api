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
	// CORSAllowedOrigins: origens do PWA autorizadas (a API vive em api.<app>.<domínio>).
	CORSAllowedOrigins []string
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
// o resto fica sob /api/v1 exigindo autenticação. A API é servida no próprio subdomínio
// (api.<app>.<domínio>) e chamada cross-origin pelo PWA, daí o CORS antes da auth (preflight).
func NewRouter(d RouterDeps) http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	// RequestLogger POR FORA do Recoverer: panics viram 500 no Recoverer interno e são logados aqui
	// com o status correto.
	r.Use(middleware.RequestLogger(d.Logger))
	r.Use(middleware.Recoverer)
	r.Use(middleware.SecurityHeaders)
	r.Use(middleware.CORS(d.CORSAllowedOrigins))
	r.Use(middleware.Tracing(d.ServiceName))

	h := d.Handlers

	// Infra (aberto).
	r.Get("/healthz", h.health)
	r.Get("/readyz", h.ready)
	r.Handle("/metrics", metrics.Handler())

	// API autenticada.
	r.Route("/api/v1", func(r chi.Router) {
		r.Use(d.Auth.Middleware)
		r.Use(middleware.EnforceJSONBody)

		// SSE da IA: conexão longa (heartbeat + deadline de escrita renovado a cada evento), por
		// isso FORA do timeout de request.
		r.Get("/ai/streams/{id}", h.aiStream)

		r.Group(func(r chi.Router) {
			r.Use(middleware.Timeout(requestTimeout(d.WriteTimeout)))

			r.Get("/me", h.getMe)

			r.Get("/integrations", h.listIntegrations)
			r.Post("/integrations/steam", h.connectSteam)
			r.Post("/integrations/psn", h.connectPSN)
			r.Post("/integrations/{id}/reconnect", h.reconnectIntegration)
			r.Patch("/integrations/{id}", h.updateIntegration)
			r.Delete("/integrations/{id}", h.deleteIntegration)
			r.Post("/integrations/{id}/sync", h.syncIntegration)

			r.Get("/search", h.search)
			r.Get("/games", h.listGames)
			r.Get("/games/{id}", h.getGame)
			r.Post("/games/{id}/sync", h.syncGame)

			r.Get("/sync-runs", h.listSyncRuns)

			r.Get("/stats", h.getStats)
			r.Get("/next", h.getNext)
			r.Get("/timeline", h.getTimeline)
			r.Get("/alerts", h.getAlerts)
			r.Patch("/games/{id}", h.patchGame)
			r.Post("/games/{id}/link", h.linkGame)
			r.Delete("/games/{id}/link", h.unlinkGame)
			r.Get("/games/{id}/link-candidates", h.linkCandidates)
			r.Patch("/trophies/{id}", h.patchTrophy)

			// Avisos de troféu desbloqueado (toast/cerimônia).
			r.Get("/events/unseen", h.listUnseenUnlocks)
			r.Post("/events/seen", h.markUnlocksSeen)

			// Web push e preferências de notificação.
			r.Get("/push/key", h.pushStatus)
			r.Post("/push/subscriptions", h.subscribePush)
			r.Delete("/push/subscriptions", h.unsubscribePush)
			r.Post("/push/test", h.testPush)
			r.Get("/notifications/preferences", h.getNotificationPrefs)
			r.Put("/notifications/preferences", h.putNotificationPrefs)

			// IA: guias, roadmap e chat (geração assíncrona; resposta em /ai/streams/{id}).
			r.Get("/trophies/{id}/guide", h.getTrophyGuide)
			r.Post("/trophies/{id}/guide", h.generateTrophyGuide)
			r.Get("/games/{id}/roadmap", h.getRoadmap)
			r.Post("/games/{id}/roadmap", h.generateRoadmap)
			r.Patch("/ai/guides/{id}", h.editGuide)
			r.Get("/games/{id}/chat", h.getChat)
			r.Post("/games/{id}/chat", h.sendChat)
			r.Delete("/games/{id}/chat", h.resetChat)
		})
	})

	return r
}

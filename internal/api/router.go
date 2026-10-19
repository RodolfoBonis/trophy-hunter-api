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
	r.Use(middleware.Metrics)
	r.Use(middleware.Recoverer)
	r.Use(middleware.SecurityHeaders)
	r.Use(middleware.CORS(d.CORSAllowedOrigins))
	r.Use(middleware.Tracing(d.ServiceName))

	h := d.Handlers

	// Infra (aberto).
	r.Get("/healthz", h.health)
	r.Get("/readyz", h.ready)
	r.Handle("/metrics", metrics.Handler())

	// Erros do navegador: ABERTO (falha de login também precisa chegar), com limite por origem.
	r.With(middleware.Timeout(5*time.Second)).Post("/api/v1/client-errors", h.reportClientError)
	r.With(middleware.Timeout(5*time.Second)).Post("/api/v1/client-vitals", h.reportClientVitals)

	// API autenticada.
	r.Route("/api/v1", func(r chi.Router) {
		r.Use(d.Auth.Middleware)
		r.Use(h.resolveUser) // usuário do app (dono reivindicado por OWNER_EMAIL; demais nascem pending)
		r.Use(middleware.EnforceJSONBody)

		// Liberado para quem ainda não entrou: quem sou eu e aceitar um convite.
		r.Group(func(r chi.Router) {
			r.Use(middleware.Timeout(requestTimeout(d.WriteTimeout)))
			r.Get("/me", h.getMe)
			r.Post("/invites/{code}/accept", h.acceptInvite)
		})

		// SSE da IA: conexão longa (heartbeat + deadline de escrita renovado a cada evento), por
		// isso FORA do timeout de request.
		r.With(requireMember).Get("/ai/streams/{id}", h.aiStream)

		r.Group(func(r chi.Router) {
			r.Use(requireMember)
			r.Use(middleware.Timeout(requestTimeout(d.WriteTimeout)))

			// Convites e membros: só o dono.
			r.Route("/admin", func(r chi.Router) {
				r.Use(requireOwner)
				r.Post("/invites", h.createInvite)
				r.Get("/invites", h.listInvites)
				r.Delete("/invites/{id}", h.revokeInvite)
				r.Get("/members", h.listMembers)
				r.Delete("/members/{id}", h.removeMember)
				// Painel do dono (saúde).
				r.Get("/health", h.getAdminHealth)
				r.Post("/jobs/{id}/retry", h.retryJob)
				r.Post("/integrations/{id}/sync", h.adminSyncIntegration)
			})

			// Social (opt-in e recíproco — ver handlers_social.go).
			r.Put("/me/sharing", h.putSharing)
			// Seus dados (LGPD).
			r.Get("/me/export", h.exportMe)
			r.Get("/me/badges", h.getMyBadges)
			r.Delete("/me", h.deleteMe)
			r.Get("/games/{id}/friends", h.getGameFriends)
			r.Get("/games/{id}/compare/{friendId}", h.compareGame)
			r.Route("/social", func(r chi.Router) {
				r.Use(requireSharing)
				r.Get("/leaderboard", h.getLeaderboard)
				r.Get("/feed", h.getFeed)
				r.Get("/friends/{id}", h.getFriend)
				r.Get("/friends/{id}/games", h.listFriendGames)
				r.Get("/friends/{id}/badges", h.getFriendBadges)
				r.Get("/challenges", h.listChallenges)
				r.Post("/challenges", h.createChallenge)
				r.Get("/challenges/{id}", h.getChallenge)
				r.Post("/challenges/{id}/accept", h.respondChallenge(true))
				r.Post("/challenges/{id}/decline", h.respondChallenge(false))
				r.Post("/challenges/{id}/cancel", h.cancelChallenge)
			})

			r.Get("/integrations", h.listIntegrations)
			r.Post("/integrations/steam", h.connectSteam)
			r.Post("/integrations/psn", h.connectPSN)
			r.Post("/integrations/{id}/reconnect", h.reconnectIntegration)
			r.Patch("/integrations/{id}", h.updateIntegration)
			r.Delete("/integrations/{id}", h.deleteIntegration)
			r.Post("/integrations/{id}/sync", h.syncIntegration)

			r.Get("/search", h.search)

			// Backlog ("quero platinar"): lojas + lista de desejos com preço.
			r.Get("/catalog/search", h.searchCatalog)
			r.Get("/wishlist", h.listWishlist)
			r.Post("/wishlist", h.addWishlist)
			r.Patch("/wishlist/{id}", h.patchWishlist)
			r.Delete("/wishlist/{id}", h.deleteWishlist)
			r.Get("/games", h.listGames)
			r.Get("/games/{id}", h.getGame)
			r.Post("/games/{id}/sync", h.syncGame)

			r.Get("/sync-runs", h.listSyncRuns)

			r.Get("/stats", h.getStats)
			r.Get("/recap", h.getRecap)
			r.Get("/recap/weekly", h.getWeeklyRecap)
			r.Post("/recap/weekly", h.generateWeeklyRecap)
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
			// Modo caçada (checklist do roadmap ligado aos troféus reais).
			r.Get("/games/{id}/hunt", h.getHunt)
			r.Put("/games/{id}/hunt/stages/{idx}", h.putHuntStage)
			r.Post("/games/{id}/roadmap", h.generateRoadmap)
			r.Patch("/ai/guides/{id}", h.editGuide)
			r.Get("/games/{id}/chat", h.getChat)
			r.Post("/games/{id}/chat", h.sendChat)
			r.Delete("/games/{id}/chat", h.resetChat)
		})
	})

	return r
}

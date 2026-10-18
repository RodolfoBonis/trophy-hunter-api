package api

import (
	"encoding/json"
	"net/http"
	"net/url"

	dbgen "github.com/RodolfoBonis/trophy-hunter-api/db/gen"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/httperr"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/push"
)

// pushStatus — GET /push/key: se o push está ligado, a chave pública VAPID e quantos aparelhos.
func (h *Handlers) pushStatus(w http.ResponseWriter, r *http.Request) {
	n, err := h.q.CountPushSubscriptions(r.Context(), currentUser(r).ID)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled": h.push.Enabled(), "public_key": h.push.PublicKey(), "subscriptions": n,
	})
}

// pushSubscriptionRequest é o PushSubscription.toJSON() do navegador.
type pushSubscriptionRequest struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

// subscribePush — POST /push/subscriptions: grava (ou atualiza) a inscrição deste navegador.
func (h *Handlers) subscribePush(w http.ResponseWriter, r *http.Request) {
	var req pushSubscriptionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httperr.Write(w, r, httperr.New(httperr.KindBadRequest, "Bad request", "corpo JSON inválido"))
		return
	}
	u, err := url.Parse(req.Endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		httperr.Write(w, r, httperr.Validation("endpoint inválido", httperr.FieldError{Field: "endpoint", Code: "invalid"}))
		return
	}
	if req.Keys.P256dh == "" || req.Keys.Auth == "" {
		httperr.Write(w, r, httperr.Validation("chaves da inscrição ausentes", httperr.FieldError{Field: "keys", Code: "required"}))
		return
	}
	ua := r.UserAgent()
	if len(ua) > 300 {
		ua = ua[:300]
	}
	if _, err := h.q.UpsertPushSubscription(r.Context(), dbgen.UpsertPushSubscriptionParams{
		Endpoint: req.Endpoint, P256dh: req.Keys.P256dh, Auth: req.Keys.Auth, UserAgent: ua, UserID: currentUser(r).ID,
	}); err != nil {
		writeInternal(w, r, err)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

// unsubscribePush — DELETE /push/subscriptions {endpoint}: remove a inscrição deste navegador.
func (h *Handlers) unsubscribePush(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Endpoint string `json:"endpoint"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Endpoint == "" {
		httperr.Write(w, r, httperr.Validation("endpoint obrigatório", httperr.FieldError{Field: "endpoint", Code: "required"}))
		return
	}
	if _, err := h.q.DeletePushSubscriptionByEndpoint(r.Context(), dbgen.DeletePushSubscriptionByEndpointParams{Endpoint: req.Endpoint, UserID: currentUser(r).ID}); err != nil {
		writeInternal(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// testPush — POST /push/test: envia uma notificação de teste para todos os aparelhos agora.
func (h *Handlers) testPush(w http.ResponseWriter, r *http.Request) {
	if !h.push.Enabled() {
		httperr.Write(w, r, httperr.New(httperr.KindUnavailable, "Push indisponível", "as chaves VAPID não estão configuradas"))
		return
	}
	res, err := h.push.Send(r.Context(), currentUser(r).ID, push.Message{
		Kind: push.KindTest, Title: "Trophy Hunter", Body: "Notificações funcionando. Bons troféus!", URL: "/", Tag: "test",
	})
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"sent": res.Sent, "failed": res.Failed, "removed": res.Removed})
}

// getNotificationPrefs — GET /notifications/preferences.
func (h *Handlers) getNotificationPrefs(w http.ResponseWriter, r *http.Request) {
	p, err := push.LoadPreferences(r.Context(), h.q, currentUser(r).ID)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// putNotificationPrefs — PUT /notifications/preferences (objeto completo).
func (h *Handlers) putNotificationPrefs(w http.ResponseWriter, r *http.Request) {
	p := push.DefaultPreferences()
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		httperr.Write(w, r, httperr.New(httperr.KindBadRequest, "Bad request", "corpo JSON inválido"))
		return
	}
	if err := push.SavePreferences(r.Context(), h.q, currentUser(r).ID, p); err != nil {
		writeInternal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/RodolfoBonis/trophy-hunter-api/internal/integrations/keycloak"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/httperr"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/metrics"
)

// Seus dados (LGPD): exportar tudo (GET /me/export) e excluir a própria conta (DELETE /me).

// exportCooldown: um export a cada 10 min por pessoa (a consulta varre todos os troféus).
const exportCooldown = 10 * time.Minute

// userDeleter apaga a pessoa no IdP (Keycloak). nil = não remove lá (dev/teste).
type userDeleter interface {
	Issuer() string
	DeleteUser(ctx context.Context, userID string) error
}

// cooldown limita uma ação por chave (memória da réplica; a API roda com 1 réplica).
type cooldown struct {
	mu    sync.Mutex
	last  map[string]time.Time
	every time.Duration
	now   func() time.Time
}

func newCooldown(every time.Duration) *cooldown {
	return &cooldown{last: map[string]time.Time{}, every: every, now: time.Now}
}

// allow devolve 0 se pode agora (e marca), ou quanto falta.
func (c *cooldown) allow(key string) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if t, ok := c.last[key]; ok && now.Sub(t) < c.every {
		return c.every - now.Sub(t)
	}
	c.last[key] = now
	return 0
}

type exportProfileDTO struct {
	Email         string     `json:"email"`
	Name          string     `json:"name"`
	Role          string     `json:"role"`
	ShareProgress bool       `json:"share_progress"`
	CreatedAt     *time.Time `json:"created_at"`
	LastSeenAt    *time.Time `json:"last_seen_at"`
}

// exportMe — GET /me/export: tudo o que é da pessoa, em JSON para baixar. Sem credenciais.
func (h *Handlers) exportMe(w http.ResponseWriter, r *http.Request) {
	me := currentUser(r)
	if wait := h.exports.allow(uuidStr(me.ID)); wait > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		httperr.Write(w, r, httperr.New(httperr.KindRateLimited, "Too many requests",
			"você acabou de exportar; tente de novo em alguns minutos"))
		return
	}
	ctx := r.Context()
	sections := []struct {
		name string
		load func(context.Context, pgtype.UUID) ([]byte, error)
	}{
		{"accounts", h.q.ExportIntegrations},
		{"games", h.q.ExportGames},
		{"trophies", h.q.ExportTrophies},
		{"ai", h.q.ExportAIGuides},
		{"chats", h.q.ExportChats},
		{"challenges", h.q.ExportChallenges},
	}
	out := map[string]any{
		"format":      "trophy-hunter-export/1",
		"exported_at": time.Now().UTC(),
		"profile": exportProfileDTO{
			Email: me.Email, Name: me.Name, Role: me.Role, ShareProgress: me.ShareProgress,
			CreatedAt: tptr(me.CreatedAt), LastSeenAt: tptr(me.LastSeenAt),
		},
	}
	for _, s := range sections {
		raw, err := s.load(ctx, me.ID)
		if err != nil {
			writeInternal(w, r, err)
			return
		}
		out[s.name] = json.RawMessage(raw)
	}
	if prefs, err := h.q.GetSetting(ctx, "notifications:"+uuidStr(me.ID)); err == nil {
		out["notification_preferences"] = json.RawMessage(prefs)
	}
	if n, err := h.q.CountPushSubscriptions(ctx, me.ID); err == nil {
		out["push_devices"] = n // só a contagem: endpoint/chaves do navegador não são dado útil e são sensíveis
	}
	h.logger.Info("conta: export", slog.String("user_id", uuidStr(me.ID)))
	w.Header().Set("Content-Disposition", `attachment; filename="trophy-hunter-dados.json"`)
	writeJSON(w, http.StatusOK, out)
}

type deleteMeRequest struct {
	Confirm string `json:"confirm"`
}

// deleteMe — DELETE /me {confirm:"EXCLUIR"}: apaga a pessoa e todos os dados na hora, e depois o
// usuário no Keycloak. O dono não se exclui por aqui. Falha no Keycloak não desfaz nada: os dados já
// foram apagados; fica log + métrica (o login sem convite não dá acesso de volta).
func (h *Handlers) deleteMe(w http.ResponseWriter, r *http.Request) {
	me := currentUser(r)
	var req deleteMeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Confirm != "EXCLUIR" {
		httperr.Write(w, r, httperr.New(httperr.KindBadRequest, "Bad request", `confirme com {"confirm":"EXCLUIR"}`))
		return
	}
	if me.Role == "owner" {
		httperr.Write(w, r, httperr.New(httperr.KindForbidden, "Forbidden", "o dono não pode excluir a própria conta").WithCode("owner_cannot_delete"))
		return
	}
	if err := h.purgeUser(r.Context(), me.ID); err != nil {
		writeInternal(w, r, err)
		return
	}
	result := h.deleteFromIDP(r.Context(), me.Issuer, me.Subject)
	metrics.AccountDeleted(result)
	h.logger.Info("conta: excluída pelo usuário", slog.String("user_id", uuidStr(me.ID)), slog.String("keycloak", result))
	w.WriteHeader(http.StatusNoContent)
}

// purgeUser apaga a pessoa (cascade) e o que não tem FK para users, numa transação.
// Devolve errNotDeletable se não apagou nada (dono ou inexistente).
func (h *Handlers) purgeUser(ctx context.Context, id pgtype.UUID) error {
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := h.q.WithTx(tx)
	n, err := qtx.DeleteMember(ctx, id)
	if err != nil {
		return err
	}
	if n == 0 {
		return errNotDeletable
	}
	if err := qtx.DeleteUserLeftovers(ctx, uuidStr(id)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

var errNotDeletable = errors.New("usuário não encontrado ou é o dono")

// deleteFromIDP apaga a pessoa no Keycloak quando ela é do realm que a API administra.
func (h *Handlers) deleteFromIDP(ctx context.Context, issuer string, subject *string) string {
	if h.idp == nil || subject == nil || *subject == "" ||
		strings.TrimRight(issuer, "/") != strings.TrimRight(h.idp.Issuer(), "/") {
		return "skipped"
	}
	// Contexto próprio: a pessoa pode fechar a aba, mas a remoção no IdP tem que ir até o fim.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	switch err := h.idp.DeleteUser(ctx, *subject); {
	case err == nil:
		return "ok"
	case errors.Is(err, keycloak.ErrNotConfigured):
		return "skipped"
	default:
		h.logger.Error("conta: falha ao apagar no Keycloak (dados do app já apagados)", slog.String("error", err.Error()))
		return "error"
	}
}

package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	dbgen "github.com/RodolfoBonis/trophy-hunter-api/db/gen"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/auth"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/httperr"
)

// Multiusuário: a identidade vem do IdP (iss+sub); o papel (owner|member) e o estado (pending|
// active|removed) são do app. Só quem aceitou um convite (active) usa a API; o dono é reconhecido
// pelo e-mail verificado = OWNER_EMAIL e herda os dados existentes.

type appUserKey struct{}

func withAppUser(ctx context.Context, u dbgen.User) context.Context {
	return context.WithValue(ctx, appUserKey{}, u)
}

// currentUser devolve o usuário do app resolvido pelo middleware (sempre presente nas rotas /v1).
func currentUser(r *http.Request) dbgen.User {
	u, _ := r.Context().Value(appUserKey{}).(dbgen.User)
	return u
}

// touchEvery limita a escrita de last_seen/nome/e-mail a uma vez por intervalo por pessoa.
const touchEvery = time.Hour

// resolveUser (depois da autenticação) encontra ou cria o usuário do app e o põe no contexto.
func (h *Handlers) resolveUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uc, ok := auth.UserFromContext(r.Context())
		if !ok {
			httperr.Write(w, r, httperr.New(httperr.KindUnauthorized, "Unauthorized", ""))
			return
		}
		u, err := h.userFor(r.Context(), uc)
		if err != nil {
			writeInternal(w, r, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(withAppUser(r.Context(), u)))
	})
}

func (h *Handlers) userFor(ctx context.Context, uc auth.UserContext) (dbgen.User, error) {
	if h.isOwnerIdentity(uc) {
		owner, err := h.q.GetOwner(ctx)
		if err != nil {
			return owner, err
		}
		if owner.Issuer == uc.Issuer && owner.Subject != nil && *owner.Subject == uc.Subject && !h.stale(owner, uc) {
			return owner, nil
		}
		// Um login anterior (antes de OWNER_EMAIL valer) pode ter criado uma linha "pending" com esta
		// identidade: some, para a identidade passar ao dono.
		if prev, perr := h.q.GetUserByIdentity(ctx, dbgen.GetUserByIdentityParams{Issuer: uc.Issuer, Subject: &uc.Subject}); perr == nil && prev.Role != "owner" {
			if _, derr := h.q.DeleteMember(ctx, prev.ID); derr != nil {
				return owner, derr
			}
		}
		sub := uc.Subject
		return h.q.ClaimOwner(ctx, dbgen.ClaimOwnerParams{ID: owner.ID, Issuer: uc.Issuer, Subject: &sub, Email: uc.Email, Name: uc.Name})
	}

	sub := uc.Subject
	u, err := h.q.GetUserByIdentity(ctx, dbgen.GetUserByIdentityParams{Issuer: uc.Issuer, Subject: &sub})
	if errors.Is(err, pgx.ErrNoRows) {
		return h.q.CreateUser(ctx, dbgen.CreateUserParams{Issuer: uc.Issuer, Subject: &sub, Email: uc.Email, Name: uc.Name})
	}
	if err != nil {
		return u, err
	}
	if h.stale(u, uc) {
		if terr := h.q.TouchUser(ctx, dbgen.TouchUserParams{ID: u.ID, Email: uc.Email, Name: uc.Name}); terr != nil {
			h.logger.Warn("users: atualizar último acesso falhou", slog.String("err", terr.Error()))
		}
		u.Email, u.Name = uc.Email, uc.Name
	}
	return u, nil
}

func (h *Handlers) isOwnerIdentity(uc auth.UserContext) bool {
	return h.ownerEmail != "" && uc.EmailVerified && strings.EqualFold(strings.TrimSpace(uc.Email), h.ownerEmail)
}

// stale: nome/e-mail mudaram no IdP ou o último acesso registrado é antigo.
func (h *Handlers) stale(u dbgen.User, uc auth.UserContext) bool {
	return u.Email != uc.Email || u.Name != uc.Name || !u.LastSeenAt.Valid || time.Since(u.LastSeenAt.Time) > touchEvery
}

// requireMember libera só quem aceitou um convite (o dono é sempre ativo).
func requireMember(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch currentUser(r).Status {
		case "active":
			next.ServeHTTP(w, r)
		case "removed":
			httperr.Write(w, r, httperr.New(httperr.KindForbidden, "Acesso removido",
				"o dono removeu seu acesso ao Trophy Hunter").WithCode("member_removed"))
		default:
			httperr.Write(w, r, httperr.New(httperr.KindForbidden, "Convite necessário",
				"peça um link de convite a quem administra o Trophy Hunter").WithCode("not_invited"))
		}
	})
}

// requireOwner libera só o dono (convites e membros).
func requireOwner(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if currentUser(r).Role != "owner" {
			httperr.Write(w, r, httperr.New(httperr.KindNotFound, "Not found", ""))
			return
		}
		next.ServeHTTP(w, r)
	})
}

/* ── /me ───────────────────────────────────────────────────────────────────────────────── */

// meDTO ↔ components.schemas.User.
type meDTO struct {
	ID      string `json:"id"`
	Subject string `json:"subject"`
	Email   string `json:"email"`
	Name    string `json:"name"`
	Role    string `json:"role"`
	Status  string `json:"status"`
}

/* ── Convites ──────────────────────────────────────────────────────────────────────────── */

const (
	inviteTTL       = 7 * 24 * time.Hour
	inviteCodeBytes = 18 // 144 bits → 24 caracteres base64url
)

func hashInviteCode(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}

type inviteDTO struct {
	ID        string     `json:"id"`
	ExpiresAt *time.Time `json:"expires_at"`
	MaxUses   int        `json:"max_uses"`
	UsedCount int        `json:"used_count"`
	CreatedAt *time.Time `json:"created_at"`
	// URL só aparece na criação (o código não fica guardado — só o hash).
	URL string `json:"url,omitempty"`
}

func toInviteDTO(i dbgen.Invite) inviteDTO {
	return inviteDTO{ID: uuidStr(i.ID), ExpiresAt: tptr(i.ExpiresAt), MaxUses: int(i.MaxUses), UsedCount: int(i.UsedCount), CreatedAt: tptr(i.CreatedAt)}
}

// createInvite — POST /admin/invites: link de uso único, válido por 7 dias.
func (h *Handlers) createInvite(w http.ResponseWriter, r *http.Request) {
	raw := make([]byte, inviteCodeBytes)
	if _, err := rand.Read(raw); err != nil {
		writeInternal(w, r, err)
		return
	}
	code := base64.RawURLEncoding.EncodeToString(raw)
	inv, err := h.q.CreateInvite(r.Context(), dbgen.CreateInviteParams{
		CodeHash: hashInviteCode(code), CreatedBy: currentUser(r).ID,
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(inviteTTL), Valid: true}, MaxUses: 1,
	})
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	d := toInviteDTO(inv)
	d.URL = strings.TrimRight(h.appURL, "/") + "/convite/" + code
	writeJSON(w, http.StatusCreated, d)
}

// listInvites — GET /admin/invites: convites ainda válidos.
func (h *Handlers) listInvites(w http.ResponseWriter, r *http.Request) {
	rows, err := h.q.ListInvites(r.Context())
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	out := make([]inviteDTO, 0, len(rows))
	for _, i := range rows {
		out = append(out, toInviteDTO(i))
	}
	writeJSON(w, http.StatusOK, out)
}

// revokeInvite — DELETE /admin/invites/{id}.
func (h *Handlers) revokeInvite(w http.ResponseWriter, r *http.Request) {
	id, err := parseUUIDParam(chi.URLParam(r, "id"))
	if err != nil {
		httperr.Write(w, r, httperr.New(httperr.KindBadRequest, "Bad request", "id inválido"))
		return
	}
	n, err := h.q.RevokeInvite(r.Context(), id)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	if n == 0 {
		httperr.Write(w, r, httperr.New(httperr.KindNotFound, "Not found", "convite não encontrado"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// acceptInvite — POST /invites/{code}/accept: quem está "pending" vira membro.
func (h *Handlers) acceptInvite(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u.Status == "active" {
		writeJSON(w, http.StatusOK, toMeDTO(u))
		return
	}
	if u.Status == "removed" {
		httperr.Write(w, r, httperr.New(httperr.KindForbidden, "Acesso removido", "o dono removeu seu acesso").WithCode("member_removed"))
		return
	}
	code := strings.TrimSpace(chi.URLParam(r, "code"))
	by, err := h.q.ConsumeInvite(r.Context(), hashInviteCode(code))
	if errors.Is(err, pgx.ErrNoRows) {
		httperr.Write(w, r, httperr.New(httperr.KindNotFound, "Convite inválido",
			"o link expirou, já foi usado ou foi revogado — peça outro").WithCode("invite_invalid"))
		return
	}
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	act, err := h.q.ActivateUser(r.Context(), dbgen.ActivateUserParams{ID: u.ID, InvitedBy: by})
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	h.logger.Info("convite aceito", slog.String("user_id", uuidStr(act.ID)))
	writeJSON(w, http.StatusOK, toMeDTO(act))
}

func toMeDTO(u dbgen.User) meDTO {
	sub := ""
	if u.Subject != nil {
		sub = *u.Subject
	}
	return meDTO{ID: uuidStr(u.ID), Subject: sub, Email: u.Email, Name: u.Name, Role: u.Role, Status: u.Status}
}

/* ── Membros ───────────────────────────────────────────────────────────────────────────── */

type memberDTO struct {
	ID         string     `json:"id"`
	Email      string     `json:"email"`
	Name       string     `json:"name"`
	Role       string     `json:"role"`
	Status     string     `json:"status"`
	Accounts   int        `json:"accounts"`
	LastSeenAt *time.Time `json:"last_seen_at"`
	CreatedAt  *time.Time `json:"created_at"`
}

// listMembers — GET /admin/members.
func (h *Handlers) listMembers(w http.ResponseWriter, r *http.Request) {
	rows, err := h.q.ListMembers(r.Context())
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	out := make([]memberDTO, 0, len(rows))
	for _, m := range rows {
		out = append(out, memberDTO{
			ID: uuidStr(m.ID), Email: m.Email, Name: m.Name, Role: m.Role, Status: m.Status,
			Accounts: int(m.Accounts), LastSeenAt: tptr(m.LastSeenAt), CreatedAt: tptr(m.CreatedAt),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// removeMember — DELETE /admin/members/{id}[?purge=true]: revoga o acesso; com purge, apaga a
// pessoa e todos os dados dela. O dono não pode ser removido.
func (h *Handlers) removeMember(w http.ResponseWriter, r *http.Request) {
	id, err := parseUUIDParam(chi.URLParam(r, "id"))
	if err != nil {
		httperr.Write(w, r, httperr.New(httperr.KindBadRequest, "Bad request", "id inválido"))
		return
	}
	var n int64
	if r.URL.Query().Get("purge") == "true" {
		n, err = h.q.DeleteMember(r.Context(), id)
	} else {
		n, err = h.q.RemoveMember(r.Context(), id)
	}
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	if n == 0 {
		httperr.Write(w, r, httperr.New(httperr.KindNotFound, "Not found", "membro não encontrado (o dono não pode ser removido)"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

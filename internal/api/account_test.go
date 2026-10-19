package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	syncpkg "github.com/RodolfoBonis/trophy-hunter-api/internal/sync"
)

// fakeIDP registra quem foi apagado no "Keycloak" (issuer "dev" = o do auth de dev dos testes).
type fakeIDP struct {
	mu      sync.Mutex
	deleted []string
	err     error
}

func (f *fakeIDP) Issuer() string { return "dev" }

func (f *fakeIDP) DeleteUser(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.deleted = append(f.deleted, id)
	return nil
}

func TestExportAndDeleteOwnAccount(t *testing.T) {
	var mu sync.Mutex
	var keys []string
	idp := &fakeIDP{}
	e := build(t, func(key string) syncpkg.SteamAPI { return accountFake{mu: &mu, keys: &keys, key: key} }, nil, nil,
		func(o *buildOpts) { o.steamServerKey = "SERVER-KEY"; o.idp = idp })

	call[me](t, e, http.MethodGet, "/api/v1/me", "", http.StatusOK) // dono existe
	inviteFriend(t, e)
	callAs[struct{ ID string }](t, e, friendSub, friendEmail, http.MethodPost, "/api/v1/integrations/steam", `{"steam_id_or_vanity":"`+steamAlt+`"}`, http.StatusCreated)
	e.drain(t)
	friendGames := callAs[struct{ Items []gameItem }](t, e, friendSub, friendEmail, http.MethodGet, "/api/v1/games?has_achievements=all", "", http.StatusOK)
	require.NotEmpty(t, friendGames.Items)
	callAs[any](t, e, friendSub, friendEmail, http.MethodPatch, "/api/v1/games/"+friendGames.Items[0].ID, `{"notes":"minha nota"}`, http.StatusOK)
	callAs[any](t, e, friendSub, friendEmail, http.MethodPut, "/api/v1/notifications/preferences", `{}`, http.StatusOK)

	// Export: tudo da pessoa, nada de credencial (a key do servidor nem a cifrada aparecem).
	resp := e.doAs(t, friendSub, friendEmail, http.MethodGet, "/api/v1/me/export", "")
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	require.Contains(t, resp.Header.Get("Content-Disposition"), "attachment")
	var exp struct {
		Format   string `json:"format"`
		Profile  struct{ Email string }
		Accounts []map[string]any
		Games    []struct{ Title, Notes string }
		Trophies []map[string]any
	}
	require.NoError(t, json.Unmarshal(body, &exp))
	require.Equal(t, "trophy-hunter-export/1", exp.Format)
	require.Equal(t, friendEmail, exp.Profile.Email)
	require.Len(t, exp.Accounts, 1)
	require.NotEmpty(t, exp.Trophies)
	require.True(t, func() bool {
		for _, g := range exp.Games {
			if g.Notes == "minha nota" {
				return true
			}
		}
		return false
	}(), "nota do jogo no export")
	for _, secret := range []string{"SERVER-KEY", "credentials", "p256dh", "claude_session"} {
		require.NotContains(t, string(body), secret)
	}
	// Só os dados dele: nada do dono.
	require.NotContains(t, string(body), ownerEmail)
	// Cooldown.
	callAs[problem](t, e, friendSub, friendEmail, http.MethodGet, "/api/v1/me/export", "", http.StatusTooManyRequests)

	// Excluir: confirmação obrigatória; dono não se exclui.
	callAs[problem](t, e, friendSub, friendEmail, http.MethodDelete, "/api/v1/me", `{"confirm":"sim"}`, http.StatusBadRequest)
	p := call[problem](t, e, http.MethodDelete, "/api/v1/me", `{"confirm":"EXCLUIR"}`, http.StatusForbidden)
	require.Equal(t, "owner_cannot_delete", p.Code)

	friend := callAs[me](t, e, friendSub, friendEmail, http.MethodGet, "/api/v1/me", "", http.StatusOK)
	res := e.doAs(t, friendSub, friendEmail, http.MethodDelete, "/api/v1/me", `{"confirm":"EXCLUIR"}`)
	_ = res.Body.Close()
	require.Equal(t, http.StatusNoContent, res.StatusCode)
	require.Equal(t, []string{friendSub}, idp.deleted, "apagado no Keycloak")

	// Nada dele sobra no banco (inclusive o que não tem FK).
	ctx := context.Background()
	var n int
	for _, q := range []string{
		"SELECT count(*) FROM users WHERE id = $1",
		"SELECT count(*) FROM integrations WHERE user_id = $1",
		"SELECT count(*) FROM games WHERE user_id = $1",
		"SELECT count(*) FROM settings WHERE key = 'notifications:' || $1::text",
	} {
		require.NoError(t, e.pool.QueryRow(ctx, q, friend.ID).Scan(&n))
		require.Zero(t, n, q)
	}
	require.NoError(t, e.pool.QueryRow(ctx, "SELECT count(*) FROM sync_runs WHERE integration_id IS NULL").Scan(&n))
	require.Zero(t, n, "histórico de sync órfão apagado")
	// O dono segue intacto.
	require.Equal(t, "owner", call[me](t, e, http.MethodGet, "/api/v1/me", "", http.StatusOK).Role)

	// Voltar a logar com a mesma identidade = pessoa nova, sem acesso (precisa de convite).
	again := callAs[me](t, e, friendSub, friendEmail, http.MethodGet, "/api/v1/me", "", http.StatusOK)
	require.Equal(t, "pending", again.Status)
	require.NotEqual(t, friend.ID, again.ID)
}

func TestDeleteOwnAccountKeycloakFailureStillDeletes(t *testing.T) {
	idp := &fakeIDP{err: errors.New("keycloak fora")}
	e := build(t, func(string) syncpkg.SteamAPI { return fakeSteam{} }, nil, nil, func(o *buildOpts) { o.idp = idp })
	call[me](t, e, http.MethodGet, "/api/v1/me", "", http.StatusOK)
	inviteFriend(t, e)
	res := e.doAs(t, friendSub, friendEmail, http.MethodDelete, "/api/v1/me", `{"confirm":"EXCLUIR"}`)
	_ = res.Body.Close()
	require.Equal(t, http.StatusNoContent, res.StatusCode)
	var n int
	require.NoError(t, e.pool.QueryRow(context.Background(), "SELECT count(*) FROM users WHERE email = $1", friendEmail).Scan(&n))
	require.Zero(t, n)
}

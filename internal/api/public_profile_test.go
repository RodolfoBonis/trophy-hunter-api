package api_test

import (
	"bytes"
	"image/png"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	syncpkg "github.com/RodolfoBonis/trophy-hunter-api/internal/sync"
)

func get(t *testing.T, e *testEnv, path string) (int, string, http.Header) {
	t.Helper()
	resp, err := http.Get(e.srv.URL + path)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

func TestPublicProfile(t *testing.T) {
	var mu sync.Mutex
	var keys []string
	e := build(t, func(key string) syncpkg.SteamAPI { return accountFake{mu: &mu, keys: &keys, key: key} }, nil, nil,
		func(o *buildOpts) { o.steamServerKey = "SERVER-KEY" })
	call[struct{ ID string }](t, e, http.MethodPost, "/api/v1/integrations/steam", `{"api_key":"OWNER-KEY","steam_id_or_vanity":"`+steamMain+`"}`, http.StatusCreated)
	e.drain(t)
	call[any](t, e, http.MethodPatch, "/api/v1/games/"+ownerGames(t, e)[0], `{"notes":"nota secreta"}`, http.StatusOK)

	// Desligado por padrão: não existe.
	code, _, _ := get(t, e, "/api/v1/public/profiles/rodolfo")
	require.Equal(t, http.StatusNotFound, code)

	// Validação do link.
	p := call[problem](t, e, http.MethodPut, "/api/v1/me/public-profile", `{"enabled":true,"slug":"Admin"}`, http.StatusUnprocessableEntity)
	require.Equal(t, "slug_invalid", p.Code)
	call[problem](t, e, http.MethodPut, "/api/v1/me/public-profile", `{"enabled":true,"slug":"a"}`, http.StatusUnprocessableEntity)
	call[problem](t, e, http.MethodPut, "/api/v1/me/public-profile", `{"enabled":true}`, http.StatusUnprocessableEntity)

	m := call[struct {
		PublicEnabled bool   `json:"public_enabled"`
		PublicSlug    string `json:"public_slug"`
		PublicName    string `json:"public_name"`
	}](t, e, http.MethodPut, "/api/v1/me/public-profile", `{"enabled":true,"slug":"rodolfo","name":"Rodolfo"}`, http.StatusOK)
	require.True(t, m.PublicEnabled)
	require.Equal(t, "rodolfo", m.PublicSlug)

	// Outra pessoa não pega o mesmo link.
	inviteFriend(t, e)
	p = callAs[problem](t, e, friendSub, friendEmail, http.MethodPut, "/api/v1/me/public-profile", `{"enabled":true,"slug":"rodolfo"}`, http.StatusConflict)
	require.Equal(t, "slug_taken", p.Code)

	// Vitrine pública (sem login): só o que pode.
	code, body, h := get(t, e, "/api/v1/public/profiles/RODOLFO")
	require.Equal(t, http.StatusOK, code, body)
	require.Contains(t, h.Get("Cache-Control"), "public")
	require.Contains(t, body, `"name":"Rodolfo"`)
	for _, leak := range []string{ownerEmail, "OWNER-KEY", steamMain, "nota secreta", "wishlist", "share_progress"} {
		require.NotContains(t, body, leak)
	}

	// Cartão PNG 1200×630 com ETag.
	resp, err := http.Get(e.srv.URL + "/api/v1/public/profiles/rodolfo/card.png")
	require.NoError(t, err)
	img, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.Equal(t, "image/png", resp.Header.Get("Content-Type"))
	cfg, err := png.DecodeConfig(bytes.NewReader(img))
	require.NoError(t, err)
	require.Equal(t, 1200, cfg.Width)
	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/api/v1/public/profiles/rodolfo/card.png", nil)
	req.Header.Set("If-None-Match", resp.Header.Get("ETag"))
	resp2, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp2.Body.Close()
	require.Equal(t, http.StatusNotModified, resp2.StatusCode)

	// HTML para os robôs do WhatsApp/Discord: og:* e redirect para a página do app.
	code, html, _ := get(t, e, "/api/v1/public/profiles/rodolfo/share")
	require.Equal(t, http.StatusOK, code)
	require.Contains(t, html, `property="og:image" content="https://trophy.test/u/rodolfo/card.png"`)
	require.Contains(t, html, `url=https://trophy.test/perfil/rodolfo`)
	require.Contains(t, html, `<title>Rodolfo no Trophy Hunter</title>`)

	// Apelido com HTML não vira HTML.
	call[any](t, e, http.MethodPut, "/api/v1/me/public-profile", `{"enabled":true,"slug":"rodolfo","name":"<script>x</script>"}`, http.StatusOK)
	_, html, _ = get(t, e, "/api/v1/public/profiles/rodolfo/share")
	require.False(t, strings.Contains(html, "<script>x"), "nome escapado")

	// Desligar some na hora; trocar o link invalida o antigo.
	call[any](t, e, http.MethodPut, "/api/v1/me/public-profile", `{"enabled":true,"slug":"rb-hunter","name":"Rodolfo"}`, http.StatusOK)
	code, _, _ = get(t, e, "/api/v1/public/profiles/rodolfo")
	require.Equal(t, http.StatusNotFound, code)
	call[any](t, e, http.MethodPut, "/api/v1/me/public-profile", `{"enabled":false,"slug":"rb-hunter"}`, http.StatusOK)
	code, _, _ = get(t, e, "/api/v1/public/profiles/rb-hunter")
	require.Equal(t, http.StatusNotFound, code)
	code, _, _ = get(t, e, "/api/v1/public/profiles/rb-hunter/share")
	require.Equal(t, http.StatusNotFound, code)
}

func ownerGames(t *testing.T, e *testEnv) []string {
	t.Helper()
	g := call[struct{ Items []gameItem }](t, e, http.MethodGet, "/api/v1/games?has_achievements=all", "", http.StatusOK)
	ids := make([]string, 0, len(g.Items))
	for _, it := range g.Items {
		ids = append(ids, it.ID)
	}
	require.NotEmpty(t, ids)
	return ids
}

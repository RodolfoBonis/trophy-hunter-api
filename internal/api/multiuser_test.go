package api_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	syncpkg "github.com/RodolfoBonis/trophy-hunter-api/internal/sync"
)

const (
	friendSub   = "friend"
	friendEmail = "amigo@test"
)

// callAs é o call[T] como outra pessoa.
func callAs[T any](t *testing.T, e *testEnv, sub, email, method, path, body string, want int) T {
	t.Helper()
	resp := e.doAs(t, sub, email, method, path, body)
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	require.Equal(t, want, resp.StatusCode, "%s %s: %s", method, path, string(b))
	var v T
	if len(b) > 0 {
		require.NoError(t, json.Unmarshal(b, &v), string(b))
	}
	return v
}

type gameItem struct {
	ID         string `json:"id"`
	ExternalID string `json:"external_id"`
}

type problem struct {
	Code string `json:"code"`
}

type me struct {
	ID, Role, Status string
}

// inviteFriend: o dono cria um convite e o amigo aceita (devolve o código usado).
func inviteFriend(t *testing.T, e *testEnv) string {
	t.Helper()
	inv := call[struct {
		ID  string `json:"id"`
		URL string `json:"url"`
	}](t, e, http.MethodPost, "/api/v1/admin/invites", "", http.StatusCreated)
	require.True(t, strings.HasPrefix(inv.URL, "https://trophy.test/convite/"), inv.URL)
	code := strings.TrimPrefix(inv.URL, "https://trophy.test/convite/")
	got := callAs[me](t, e, friendSub, friendEmail, http.MethodPost, "/api/v1/invites/"+code+"/accept", "", http.StatusOK)
	require.Equal(t, "active", got.Status)
	require.Equal(t, "member", got.Role)
	return code
}

func TestMultiUserIsolationAndInvites(t *testing.T) {
	var mu sync.Mutex
	var keys []string
	r := &fakeRunner{}
	e := build(t, func(key string) syncpkg.SteamAPI { return accountFake{mu: &mu, keys: &keys, key: key} }, nil, r,
		func(o *buildOpts) { o.steamServerKey = "SERVER-KEY"; o.aiDailyLimit = 2 })

	// Dono: e-mail verificado = OWNER_EMAIL → é o dono (reivindica a linha da migration).
	owner := call[me](t, e, http.MethodGet, "/api/v1/me", "", http.StatusOK)
	require.Equal(t, "owner", owner.Role)
	require.Equal(t, "00000000-0000-0000-0000-000000000001", owner.ID)
	call[struct{ ID string }](t, e, http.MethodPost, "/api/v1/integrations/steam", `{"api_key":"OWNER-KEY","steam_id_or_vanity":"`+steamMain+`"}`, http.StatusCreated)
	e.drain(t)
	ownerGames := call[struct {
		Items []gameItem
	}](t, e, http.MethodGet, "/api/v1/games?has_achievements=all", "", http.StatusOK)
	require.Len(t, ownerGames.Items, 2)

	// Amigo sem convite: loga (nasce pending), mas só vê /me.
	pending := callAs[me](t, e, friendSub, friendEmail, http.MethodGet, "/api/v1/me", "", http.StatusOK)
	require.Equal(t, "pending", pending.Status)
	p := callAs[problem](t, e, friendSub, friendEmail, http.MethodGet, "/api/v1/games", "", http.StatusForbidden)
	require.Equal(t, "not_invited", p.Code)
	callAs[problem](t, e, friendSub, friendEmail, http.MethodPost, "/api/v1/invites/codigo-falso/accept", "", http.StatusNotFound)

	// Convite: uso único.
	code := inviteFriend(t, e)
	other := callAs[problem](t, e, "third", "terceiro@test", http.MethodPost, "/api/v1/invites/"+code+"/accept", "", http.StatusNotFound)
	require.Equal(t, "invite_invalid", other.Code)
	// Convite revogado não funciona.
	inv := call[struct{ ID, URL string }](t, e, http.MethodPost, "/api/v1/admin/invites", "", http.StatusCreated)
	res := e.do(t, http.MethodDelete, "/api/v1/admin/invites/"+inv.ID, "", true)
	_ = res.Body.Close()
	require.Equal(t, http.StatusNoContent, res.StatusCode)
	revoked := strings.TrimPrefix(inv.URL, "https://trophy.test/convite/")
	callAs[problem](t, e, "third", "terceiro@test", http.MethodPost, "/api/v1/invites/"+revoked+"/accept", "", http.StatusNotFound)

	// Membro não administra.
	callAs[problem](t, e, friendSub, friendEmail, http.MethodGet, "/api/v1/admin/members", "", http.StatusNotFound)
	callAs[problem](t, e, friendSub, friendEmail, http.MethodPost, "/api/v1/admin/invites", "", http.StatusNotFound)

	// Isolamento: o amigo não vê nem mexe em nada do dono.
	empty := callAs[struct {
		Items []any
		Total int
	}](t, e, friendSub, friendEmail, http.MethodGet, "/api/v1/games?has_achievements=all", "", http.StatusOK)
	require.Empty(t, empty.Items)
	require.Empty(t, callAs[[]any](t, e, friendSub, friendEmail, http.MethodGet, "/api/v1/integrations", "", http.StatusOK))
	ownerGame := ownerGames.Items[0].ID
	callAs[problem](t, e, friendSub, friendEmail, http.MethodGet, "/api/v1/games/"+ownerGame, "", http.StatusNotFound)
	callAs[problem](t, e, friendSub, friendEmail, http.MethodPatch, "/api/v1/games/"+ownerGame, `{"notes":"invasão"}`, http.StatusNotFound)
	callAs[problem](t, e, friendSub, friendEmail, http.MethodPost, "/api/v1/games/"+ownerGame+"/sync", "", http.StatusNotFound)
	callAs[problem](t, e, friendSub, friendEmail, http.MethodGet, "/api/v1/games/"+ownerGame+"/chat", "", http.StatusNotFound)
	search := callAs[struct{ Games, Trophies []any }](t, e, friendSub, friendEmail, http.MethodGet, "/api/v1/search?q=portal", "", http.StatusOK)
	require.Empty(t, search.Games)
	stats := callAs[struct {
		Games struct{ Total int } `json:"games"`
	}](t, e, friendSub, friendEmail, http.MethodGet, "/api/v1/stats", "", http.StatusOK)
	require.Zero(t, stats.Games.Total)
	ownerIntegs := call[[]struct{ ID string }](t, e, http.MethodGet, "/api/v1/integrations", "", http.StatusOK)
	callAs[problem](t, e, friendSub, friendEmail, http.MethodPost, "/api/v1/integrations/"+ownerIntegs[0].ID+"/sync", "", http.StatusNotFound)
	callAs[problem](t, e, friendSub, friendEmail, http.MethodDelete, "/api/v1/integrations/"+ownerIntegs[0].ID, "", http.StatusNotFound)

	// Amigo conecta a Steam SÓ com o perfil: usa a key do servidor.
	callAs[struct{ ID string }](t, e, friendSub, friendEmail, http.MethodPost, "/api/v1/integrations/steam", `{"steam_id_or_vanity":"`+steamAlt+`"}`, http.StatusCreated)
	mu.Lock()
	require.Equal(t, "SERVER-KEY", keys[len(keys)-1], "conta sem key própria usa a do servidor")
	mu.Unlock()
	e.drain(t)
	friendGames := callAs[struct {
		Items []gameItem
	}](t, e, friendSub, friendEmail, http.MethodGet, "/api/v1/games?has_achievements=all", "", http.StatusOK)
	require.Len(t, friendGames.Items, 1, "só o Portal 2 da conta dele")
	again := call[struct {
		Items []any
	}](t, e, http.MethodGet, "/api/v1/games?has_achievements=all", "", http.StatusOK)
	require.Len(t, again.Items, 2, "o mesmo jogo do amigo não mistura com o do dono")

	// IA: guia de troféu COMPARTILHADO (mesmo jogo/troféu), edição PRIVADA, roadmap por pessoa.
	ownerTrophy := firstTrophy(t, e, "me", ownerEmail, ownerGameByExt(t, ownerGames.Items, "620"))
	friendTrophy := firstTrophy(t, e, friendSub, friendEmail, friendGames.Items[0].ID)
	g := call[guideResp](t, e, http.MethodPost, "/api/v1/trophies/"+ownerTrophy+"/guide", "", http.StatusAccepted)
	require.Empty(t, e.drainAI(t))
	shared := callAs[struct{ Guide *guideResp }](t, e, friendSub, friendEmail, http.MethodGet, "/api/v1/trophies/"+friendTrophy+"/guide", "", http.StatusOK)
	require.NotNil(t, shared.Guide)
	require.Equal(t, g.ID, shared.Guide.ID, "o amigo reaproveita o guia já gerado")
	require.Equal(t, "ready", shared.Guide.Status)
	edited := strings.Replace(validTrophyGuide, "Vença o jogo.", "Do amigo.", 1)
	callAs[guideResp](t, e, friendSub, friendEmail, http.MethodPatch, "/api/v1/ai/guides/"+g.ID, `{"edited_content":`+edited+`}`, http.StatusOK)
	mine := call[struct{ Guide *guideResp }](t, e, http.MethodGet, "/api/v1/trophies/"+ownerTrophy+"/guide", "", http.StatusOK)
	require.Equal(t, "null", string(mine.Guide.EditedContent), "a edição do amigo é só dele")

	rm := call[guideResp](t, e, http.MethodPost, "/api/v1/games/"+ownerGameByExt(t, ownerGames.Items, "620")+"/roadmap", "", http.StatusAccepted)
	require.Empty(t, e.drainAI(t))
	fr := callAs[struct{ Guide *guideResp }](t, e, friendSub, friendEmail, http.MethodGet, "/api/v1/games/"+friendGames.Items[0].ID+"/roadmap", "", http.StatusOK)
	require.Nil(t, fr.Guide, "roadmap é de cada pessoa")
	callAs[problem](t, e, friendSub, friendEmail, http.MethodPatch, "/api/v1/ai/guides/"+rm.ID, `{"edited_content":null}`, http.StatusNotFound)
	resp := e.doAs(t, friendSub, friendEmail, http.MethodGet, "/api/v1/ai/streams/"+rm.ID, "")
	_ = resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode, "stream do roadmap alheio não abre")

	// Teto diário de IA (2 nos testes): o amigo já usou 0 → 2 pedidos ok, o 3º barra. O dono não tem teto.
	callAs[guideResp](t, e, friendSub, friendEmail, http.MethodPost, "/api/v1/games/"+friendGames.Items[0].ID+"/roadmap", "", http.StatusAccepted)
	callAs[guideResp](t, e, friendSub, friendEmail, http.MethodPost, "/api/v1/trophies/"+friendTrophy+"/guide", "", http.StatusAccepted)
	lim := callAs[problem](t, e, friendSub, friendEmail, http.MethodPost, "/api/v1/games/"+friendGames.Items[0].ID+"/chat", `{"message":"oi"}`, http.StatusTooManyRequests)
	require.Equal(t, "ai_daily_limit", lim.Code)
	for i := 0; i < 3; i++ {
		call[guideResp](t, e, http.MethodPost, "/api/v1/games/"+ownerGameByExt(t, ownerGames.Items, "620")+"/roadmap", "", http.StatusAccepted)
	}

	// Membros: o dono vê e remove; removido perde o acesso; purge apaga os dados.
	members := call[[]struct {
		ID, Role, Status string
		Accounts         int
	}](t, e, http.MethodGet, "/api/v1/admin/members", "", http.StatusOK)
	require.Len(t, members, 2)
	friendID := members[1].ID
	require.Equal(t, 1, members[1].Accounts)
	res = e.do(t, http.MethodDelete, "/api/v1/admin/members/"+owner.ID, "", true)
	_ = res.Body.Close()
	require.Equal(t, http.StatusNotFound, res.StatusCode, "o dono não se remove")
	res = e.do(t, http.MethodDelete, "/api/v1/admin/members/"+friendID, "", true)
	_ = res.Body.Close()
	require.Equal(t, http.StatusNoContent, res.StatusCode)
	gone := callAs[problem](t, e, friendSub, friendEmail, http.MethodGet, "/api/v1/games", "", http.StatusForbidden)
	require.Equal(t, "member_removed", gone.Code)
	res = e.do(t, http.MethodDelete, "/api/v1/admin/members/"+friendID+"?purge=true", "", true)
	_ = res.Body.Close()
	require.Equal(t, http.StatusNoContent, res.StatusCode)
	var n int
	require.NoError(t, e.pool.QueryRow(t.Context(), "SELECT count(*) FROM integrations WHERE account_id = $1", steamAlt).Scan(&n))
	require.Zero(t, n, "purge apaga as contas do amigo")
}

// TestOwnerClaimSurvivesIdPSwitch: trocar de realm muda o sub; o e-mail verificado do dono mantém
// a mesma conta (e os mesmos dados). E-mail igual mas NÃO verificado não reivindica nada.
func TestOwnerClaimSurvivesIdPSwitch(t *testing.T) {
	e := setup(t)
	first := call[me](t, e, http.MethodGet, "/api/v1/me", "", http.StatusOK)
	switched := callAs[me](t, e, "outro-sub-no-realm-novo", ownerEmail, http.MethodGet, "/api/v1/me", "", http.StatusOK)
	require.Equal(t, first.ID, switched.ID)
	require.Equal(t, "owner", switched.Role)
	stranger := callAs[me](t, e, "intruso", "", http.MethodGet, "/api/v1/me", "", http.StatusOK)
	require.Equal(t, "pending", stranger.Status, "sem e-mail verificado não vira dono")
}

func ownerGameByExt(t *testing.T, items []gameItem, ext string) string {
	t.Helper()
	for _, it := range items {
		if it.ExternalID == ext {
			return it.ID
		}
	}
	t.Fatalf("jogo %s não encontrado", ext)
	return ""
}

func firstTrophy(t *testing.T, e *testEnv, sub, email, gameID string) string {
	t.Helper()
	d := callAs[struct {
		Groups []struct {
			Trophies []struct{ ID string }
		}
	}](t, e, sub, email, http.MethodGet, "/api/v1/games/"+gameID, "", http.StatusOK)
	require.NotEmpty(t, d.Groups)
	require.NotEmpty(t, d.Groups[0].Trophies)
	return d.Groups[0].Trophies[0].ID
}

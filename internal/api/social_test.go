package api_test

import (
	"net/http"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	syncpkg "github.com/RodolfoBonis/trophy-hunter-api/internal/sync"
)

type socialUser struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	IsMe bool   `json:"is_me"`
}

// TestSocialOptInAndReciprocity: ninguém aparece até ligar o compartilhamento; só vê quem compartilha;
// perfil/biblioteca/ranking/feed/"amigos neste jogo" só com os dois lados ligados.
func TestSocialOptInAndReciprocity(t *testing.T) {
	var mu sync.Mutex
	var keys []string
	e := build(t, func(key string) syncpkg.SteamAPI { return accountFake{mu: &mu, keys: &keys, key: key} }, nil, &fakeRunner{},
		func(o *buildOpts) { o.steamServerKey = "SERVER-KEY" })

	owner := call[me](t, e, http.MethodGet, "/api/v1/me", "", http.StatusOK)
	call[struct{ ID string }](t, e, http.MethodPost, "/api/v1/integrations/steam", `{"api_key":"OWNER-KEY","steam_id_or_vanity":"`+steamMain+`"}`, http.StatusCreated)
	e.drain(t)
	ownerGames := call[struct{ Items []gameItem }](t, e, http.MethodGet, "/api/v1/games?has_achievements=all", "", http.StatusOK)
	portal := ownerGameByExt(t, ownerGames.Items, "620")

	inviteFriend(t, e)
	callAs[struct{ ID string }](t, e, friendSub, friendEmail, http.MethodPost, "/api/v1/integrations/steam", `{"steam_id_or_vanity":"`+steamAlt+`"}`, http.StatusCreated)
	e.drain(t)
	friend := callAs[struct {
		ID            string `json:"id"`
		ShareProgress bool   `json:"share_progress"`
	}](t, e, friendSub, friendEmail, http.MethodGet, "/api/v1/me", "", http.StatusOK)
	require.False(t, friend.ShareProgress, "opt-in: começa desligado")

	// Ninguém compartilha: social barra quem pede; "amigos neste jogo" responde vazio com sharing=false.
	p := call[problem](t, e, http.MethodGet, "/api/v1/social/leaderboard", "", http.StatusForbidden)
	require.Equal(t, "sharing_disabled", p.Code)
	gf := call[struct {
		Sharing bool  `json:"sharing"`
		Items   []any `json:"items"`
	}](t, e, http.MethodGet, "/api/v1/games/"+portal+"/friends", "", http.StatusOK)
	require.False(t, gf.Sharing)
	require.Empty(t, gf.Items)

	// Só o dono liga: ele vê o placar, mas o amigo (que não compartilha) não aparece nem abre.
	on := call[struct {
		ShareProgress bool `json:"share_progress"`
	}](t, e, http.MethodPut, "/api/v1/me/sharing", `{"share_progress":true}`, http.StatusOK)
	require.True(t, on.ShareProgress)
	board := call[struct {
		Items []struct {
			User socialUser `json:"user"`
		} `json:"items"`
	}](t, e, http.MethodGet, "/api/v1/social/leaderboard?period=all", "", http.StatusOK)
	require.Len(t, board.Items, 1)
	require.True(t, board.Items[0].User.IsMe)
	call[problem](t, e, http.MethodGet, "/api/v1/social/friends/"+friend.ID, "", http.StatusNotFound)
	callAs[problem](t, e, friendSub, friendEmail, http.MethodGet, "/api/v1/social/friends/"+owner.ID, "", http.StatusForbidden)

	// Os dois ligam: placar com os dois, perfil e biblioteca (só leitura), e o Portal 2 em comum.
	callAs[struct{}](t, e, friendSub, friendEmail, http.MethodPut, "/api/v1/me/sharing", `{"share_progress":true}`, http.StatusOK)
	board = call[struct {
		Items []struct {
			User socialUser `json:"user"`
		} `json:"items"`
	}](t, e, http.MethodGet, "/api/v1/social/leaderboard?period=year", "", http.StatusOK)
	require.Len(t, board.Items, 2)
	prof := call[struct {
		User  socialUser `json:"user"`
		Stats struct {
			Games struct{ Total int } `json:"games"`
		} `json:"stats"`
	}](t, e, http.MethodGet, "/api/v1/social/friends/"+friend.ID, "", http.StatusOK)
	require.False(t, prof.User.IsMe)
	require.Equal(t, 1, prof.Stats.Games.Total)
	lib := call[struct{ Items []gameItem }](t, e, http.MethodGet, "/api/v1/social/friends/"+friend.ID+"/games?has_achievements=all", "", http.StatusOK)
	require.Len(t, lib.Items, 1)
	require.Equal(t, "620", lib.Items[0].ExternalID)
	// Ver a biblioteca não dá acesso ao jogo em si (detalhe, notas, sync continuam só do dono).
	call[problem](t, e, http.MethodGet, "/api/v1/games/"+lib.Items[0].ID, "", http.StatusNotFound)

	gf2 := call[struct {
		Sharing bool `json:"sharing"`
		Items   []struct {
			User socialUser `json:"user"`
		} `json:"items"`
	}](t, e, http.MethodGet, "/api/v1/games/"+portal+"/friends", "", http.StatusOK)
	require.True(t, gf2.Sharing)
	require.Len(t, gf2.Items, 1)
	require.Equal(t, friend.ID, gf2.Items[0].User.ID)

	feed := call[struct {
		Items []struct {
			Kind string     `json:"kind"`
			User socialUser `json:"user"`
		} `json:"items"`
	}](t, e, http.MethodGet, "/api/v1/social/feed", "", http.StatusOK)
	for _, it := range feed.Items {
		require.Contains(t, []string{"platinum", "completion", "rare"}, it.Kind)
	}
	call[problem](t, e, http.MethodGet, "/api/v1/social/leaderboard?period=decada", "", http.StatusBadRequest)

	// Desligar volta a esconder: o dono não vê mais o perfil do amigo.
	callAs[struct{}](t, e, friendSub, friendEmail, http.MethodPut, "/api/v1/me/sharing", `{"share_progress":false}`, http.StatusOK)
	call[problem](t, e, http.MethodGet, "/api/v1/social/friends/"+friend.ID, "", http.StatusNotFound)
	gf3 := call[struct {
		Items []any `json:"items"`
	}](t, e, http.MethodGet, "/api/v1/games/"+portal+"/friends", "", http.StatusOK)
	require.Empty(t, gf3.Items)
}

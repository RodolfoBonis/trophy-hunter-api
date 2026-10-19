package api_test

import (
	"io"
	"log/slog"
	"net/http"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	dbgen "github.com/RodolfoBonis/trophy-hunter-api/db/gen"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/challenges"
	syncpkg "github.com/RodolfoBonis/trophy-hunter-api/internal/sync"
)

type challengeResp struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	MyState   string `json:"my_state"`
	IsCreator bool   `json:"is_creator"`
	Winner    *struct {
		ID string `json:"id"`
	} `json:"winner"`
	Standings []struct {
		User struct {
			ID string `json:"id"`
		} `json:"user"`
		State   string `json:"state"`
		HasGame bool   `json:"has_game"`
		Score   int    `json:"score"`
	} `json:"standings"`
}

func TestChallengesAndCompare(t *testing.T) {
	var mu sync.Mutex
	var keys []string
	e := build(t, func(key string) syncpkg.SteamAPI { return accountFake{mu: &mu, keys: &keys, key: key} }, nil, &fakeRunner{},
		func(o *buildOpts) { o.steamServerKey = "SERVER-KEY" })
	owner := call[me](t, e, http.MethodGet, "/api/v1/me", "", http.StatusOK)
	call[struct{ ID string }](t, e, http.MethodPost, "/api/v1/integrations/steam", `{"api_key":"K","steam_id_or_vanity":"`+steamMain+`"}`, http.StatusCreated)
	e.drain(t)
	ownerGames := call[struct{ Items []gameItem }](t, e, http.MethodGet, "/api/v1/games?has_achievements=all", "", http.StatusOK)
	portal := ownerGameByExt(t, ownerGames.Items, "620")

	inviteFriend(t, e)
	callAs[struct{ ID string }](t, e, friendSub, friendEmail, http.MethodPost, "/api/v1/integrations/steam", `{"steam_id_or_vanity":"`+steamAlt+`"}`, http.StatusCreated)
	e.drain(t)
	friend := callAs[me](t, e, friendSub, friendEmail, http.MethodGet, "/api/v1/me", "", http.StatusOK)

	// Sem compartilhar: barrado (recíproco).
	p := call[problem](t, e, http.MethodGet, "/api/v1/social/challenges", "", http.StatusForbidden)
	require.Equal(t, "sharing_disabled", p.Code)
	call[struct{}](t, e, http.MethodPut, "/api/v1/me/sharing", `{"share_progress":true}`, http.StatusOK)
	// Amigo ainda não compartilha → não pode ser desafiado.
	call[problem](t, e, http.MethodPost, "/api/v1/social/challenges", `{"kind":"first_to_complete","game_id":"`+portal+`","friend_ids":["`+friend.ID+`"]}`, http.StatusNotFound)
	callAs[struct{}](t, e, friendSub, friendEmail, http.MethodPut, "/api/v1/me/sharing", `{"share_progress":true}`, http.StatusOK)

	// Validações.
	call[problem](t, e, http.MethodPost, "/api/v1/social/challenges", `{"kind":"first_to_complete","game_id":"`+portal+`","friend_ids":[]}`, http.StatusBadRequest)
	call[problem](t, e, http.MethodPost, "/api/v1/social/challenges", `{"kind":"most_trophies","days":3,"friend_ids":["`+friend.ID+`"]}`, http.StatusBadRequest)
	call[problem](t, e, http.MethodPost, "/api/v1/social/challenges", `{"kind":"first_to_complete","game_id":"`+portal+`","friend_ids":["`+owner.ID+`"]}`, http.StatusBadRequest)

	// Corrida no Portal 2: criador aceito, amigo convidado.
	c := call[challengeResp](t, e, http.MethodPost, "/api/v1/social/challenges", `{"kind":"first_to_complete","game_id":"`+portal+`","friend_ids":["`+friend.ID+`"]}`, http.StatusCreated)
	require.Equal(t, "open", c.Status)
	require.True(t, c.IsCreator)
	list := callAs[[]challengeResp](t, e, friendSub, friendEmail, http.MethodGet, "/api/v1/social/challenges", "", http.StatusOK)
	require.Len(t, list, 1)
	require.Equal(t, "invited", list[0].MyState)
	var invites int
	require.NoError(t, e.pool.QueryRow(t.Context(), "SELECT count(*) FROM jobs WHERE kind = 'notify' AND payload->>'kind' = 'challenges'").Scan(&invites))
	require.Equal(t, 1, invites, "convite vira push para o amigo")

	// Terceiro (não participante) não enxerga.
	callAs[problem](t, e, "third", "terceiro@test", http.MethodGet, "/api/v1/social/challenges/"+c.ID, "", http.StatusForbidden) // pending: nem é membro
	// No fake, o amigo já tem o Portal 2 a 100% → aceitar é recusado (já tinha fechado antes).
	pre := callAs[problem](t, e, friendSub, friendEmail, http.MethodPost, "/api/v1/social/challenges/"+c.ID+"/accept", "", http.StatusConflict)
	require.Equal(t, "already_completed", pre.Code)
	_, err := e.pool.Exec(t.Context(), `UPDATE trophies SET earned = false, earned_at = NULL
		WHERE game_id IN (SELECT id FROM games WHERE user_id = $1 AND external_id = '620')`, friend.ID)
	require.NoError(t, err)
	acc := callAs[challengeResp](t, e, friendSub, friendEmail, http.MethodPost, "/api/v1/social/challenges/"+c.ID+"/accept", "", http.StatusOK)
	require.Equal(t, "accepted", acc.MyState)
	callAs[problem](t, e, friendSub, friendEmail, http.MethodPost, "/api/v1/social/challenges/"+c.ID+"/accept", "", http.StatusConflict)
	callAs[problem](t, e, friendSub, friendEmail, http.MethodPost, "/api/v1/social/challenges/"+c.ID+"/cancel", "", http.StatusConflict)
	for _, s := range acc.Standings {
		require.True(t, s.HasGame, "os dois têm o Portal 2")
	}

	// Amigo fecha o Portal 2 → avaliação encerra com ele vencendo.
	_, err = e.pool.Exec(t.Context(), `UPDATE trophies SET earned = true, earned_at = now()
		WHERE game_id IN (SELECT id FROM games WHERE user_id = $1 AND external_id = '620')`, friend.ID)
	require.NoError(t, err)
	svc := challenges.New(dbgen.New(e.pool), e.queue, slog.New(slog.NewTextHandler(io.Discard, nil)))
	svc.EvaluateAll(t.Context())
	done := call[challengeResp](t, e, http.MethodGet, "/api/v1/social/challenges/"+c.ID, "", http.StatusOK)
	require.Equal(t, "finished", done.Status)
	require.NotNil(t, done.Winner)
	require.Equal(t, friend.ID, done.Winner.ID)

	// Comparar troféu a troféu: mesma plataforma, casando pelo id; o amigo tem tudo.
	cmp := call[struct {
		SamePlatform bool `json:"same_platform"`
		Trophies     []struct {
			Matched bool `json:"matched"`
			Friend  bool `json:"friend"`
		} `json:"trophies"`
	}](t, e, http.MethodGet, "/api/v1/games/"+portal+"/compare/"+friend.ID, "", http.StatusOK)
	require.True(t, cmp.SamePlatform)
	require.NotEmpty(t, cmp.Trophies)
	for _, tr := range cmp.Trophies {
		require.True(t, tr.Matched)
		require.True(t, tr.Friend)
	}

	// Mais troféus em 7 dias: só termina no prazo; ninguém pontuou → sem vencedor. Recusar e cancelar.
	m := call[challengeResp](t, e, http.MethodPost, "/api/v1/social/challenges", `{"kind":"most_trophies","days":7,"friend_ids":["`+friend.ID+`"]}`, http.StatusCreated)
	callAs[challengeResp](t, e, friendSub, friendEmail, http.MethodPost, "/api/v1/social/challenges/"+m.ID+"/decline", "", http.StatusOK)
	_, err = e.pool.Exec(t.Context(), `UPDATE challenges SET ends_at = now() - interval '1 minute' WHERE id = $1`, m.ID)
	require.NoError(t, err)
	svc.EvaluateAll(t.Context())
	md := call[challengeResp](t, e, http.MethodGet, "/api/v1/social/challenges/"+m.ID, "", http.StatusOK)
	require.Equal(t, "finished", md.Status)
	require.Nil(t, md.Winner)

	k := call[challengeResp](t, e, http.MethodPost, "/api/v1/social/challenges", `{"kind":"most_rare","days":14,"friend_ids":["`+friend.ID+`"]}`, http.StatusCreated)
	kd := call[challengeResp](t, e, http.MethodPost, "/api/v1/social/challenges/"+k.ID+"/cancel", "", http.StatusOK)
	require.Equal(t, "canceled", kd.Status)

	// Jogo já fechado por quem cria → 409.
	_, err = e.pool.Exec(t.Context(), `UPDATE trophies SET earned = true, earned_at = now()
		WHERE game_id IN (SELECT id FROM games WHERE user_id = $1 AND external_id = '620')`, owner.ID)
	require.NoError(t, err)
	dup := call[problem](t, e, http.MethodPost, "/api/v1/social/challenges", `{"kind":"first_to_complete","game_id":"`+portal+`","friend_ids":["`+friend.ID+`"]}`, http.StatusConflict)
	require.Equal(t, "already_completed", dup.Code)
}

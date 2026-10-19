package api_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/require"

	"github.com/RodolfoBonis/trophy-hunter-api/internal/badges"
	syncpkg "github.com/RodolfoBonis/trophy-hunter-api/internal/sync"
)

type badgesResp struct {
	Earned []struct {
		Key, Name, Season string
	} `json:"earned"`
	Next    []struct{ Key string } `json:"next"`
	Catalog []struct {
		Key         string
		Value, Goal int
		Earned      bool
	} `json:"catalog"`
	Season struct {
		Key, Label      string
		Platinums, Rare int
		Done            bool
	} `json:"season"`
}

func earnedKeys(b badgesResp) map[string]bool {
	m := map[string]bool{}
	for _, e := range b.Earned {
		m[e.Key] = true
	}
	return m
}

func TestBadges(t *testing.T) {
	e := build(t, func(string) syncpkg.SteamAPI { return fakeSteam{} }, nil, nil)
	ctx := context.Background()
	owner := call[me](t, e, http.MethodGet, "/api/v1/me", "", http.StatusOK)
	call[struct{ ID string }](t, e, http.MethodPost, "/api/v1/integrations/steam", `{"api_key":"K","steam_id_or_vanity":"76561197960000000"}`, http.StatusCreated)
	e.drain(t)

	// Portal 2 com 1 de 2 conquistas: nada ganho ainda; catálogo com andamento.
	b := call[badgesResp](t, e, http.MethodGet, "/api/v1/me/badges", "", http.StatusOK)
	require.Empty(t, b.Earned)
	require.Len(t, b.Catalog, len(badges.Catalog))
	require.Len(t, b.Next, 3)
	require.Regexp(t, `^Temporada [a-z]{3}/\d{4}$`, b.Season.Label)

	// Fecha o jogo (100% na Steam) com conquistas ultra raras.
	_, err := e.pool.Exec(ctx, "UPDATE trophies SET earned = true, earned_at = now(), rarity_pct = 0.5")
	require.NoError(t, err)
	b = call[badgesResp](t, e, http.MethodGet, "/api/v1/me/badges", "", http.StatusOK)
	got := earnedKeys(b)
	require.True(t, got["platinum_1"], "100%% na Steam conta como platina")
	require.True(t, got["ultra_1"])
	require.False(t, got["platinum_10"])
	require.Equal(t, 1, b.Season.Platinums)
	require.Equal(t, 2, b.Season.Rare)
	require.False(t, b.Season.Done)

	countBadgePush := func() int {
		var n int
		require.NoError(t, e.pool.QueryRow(ctx, "SELECT count(*) FROM jobs WHERE kind = 'notify' AND payload->>'kind' = 'badges'").Scan(&n))
		return n
	}
	// Leitura (GET) nunca avisa; backfill também não.
	require.Zero(t, countBadgePush())

	// Worker: medalha nova depois do backfill → push.
	_, err = e.pool.Exec(ctx, "DELETE FROM user_badges WHERE badge_key = 'ultra_1'")
	require.NoError(t, err)
	var uid pgtype.UUID
	require.NoError(t, uid.Scan(owner.ID))
	svc := badges.New(e.q, e.queue, slog.New(slog.NewTextHandler(io.Discard, nil)))
	awards, err := svc.Sync(ctx, uid, true)
	require.NoError(t, err)
	require.Len(t, awards, 1)
	require.Equal(t, "ultra_1", awards[0].Key)
	require.Equal(t, 1, countBadgePush())
	// Idempotente: de novo não ganha nem avisa.
	awards, err = svc.Sync(ctx, uid, true)
	require.NoError(t, err)
	require.Empty(t, awards)
	require.Equal(t, 1, countBadgePush())

	// Amigos: recíproco como o resto do social.
	inviteFriend(t, e)
	p := callAs[problem](t, e, friendSub, friendEmail, http.MethodGet, "/api/v1/social/friends/"+owner.ID+"/badges", "", http.StatusForbidden)
	require.Equal(t, "sharing_disabled", p.Code)
	callAs[any](t, e, friendSub, friendEmail, http.MethodPut, "/api/v1/me/sharing", `{"share_progress":true}`, http.StatusOK)
	callAs[problem](t, e, friendSub, friendEmail, http.MethodGet, "/api/v1/social/friends/"+owner.ID+"/badges", "", http.StatusNotFound)
	call[any](t, e, http.MethodPut, "/api/v1/me/sharing", `{"share_progress":true}`, http.StatusOK)
	fb := callAs[badgesResp](t, e, friendSub, friendEmail, http.MethodGet, "/api/v1/social/friends/"+owner.ID+"/badges", "", http.StatusOK)
	require.True(t, earnedKeys(fb)["platinum_1"])
	lb := callAs[struct {
		Items []struct {
			User   struct{ ID string } `json:"user"`
			Badges int                 `json:"badges"`
		} `json:"items"`
	}](t, e, friendSub, friendEmail, http.MethodGet, "/api/v1/social/leaderboard", "", http.StatusOK)
	for _, it := range lb.Items {
		if it.User.ID == owner.ID {
			require.GreaterOrEqual(t, it.Badges, 2)
		}
	}

	// Export leva as medalhas.
	body := e.do(t, http.MethodGet, "/api/v1/me/export", "", true)
	raw, _ := io.ReadAll(body.Body)
	_ = body.Body.Close()
	require.Contains(t, string(raw), `"platinum_1"`)
}

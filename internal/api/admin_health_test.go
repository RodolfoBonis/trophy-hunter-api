package api_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/queue"
	syncpkg "github.com/RodolfoBonis/trophy-hunter-api/internal/sync"
)

type healthResp struct {
	Accounts []struct {
		ID, User, Platform, Status string
	} `json:"accounts"`
	Queue []struct {
		Kind, Status string
		Count        int
	} `json:"queue"`
	RecentDead []struct{ ID, Kind, Error string } `json:"recent_dead"`
	AIUsage    []struct {
		User     string `json:"user"`
		Owner    bool   `json:"owner"`
		DayLimit int    `json:"day_limit"`
	} `json:"ai_usage"`
	Totals     map[string]int `json:"totals"`
	Prometheus struct {
		Available bool                              `json:"available"`
		Alerts    []struct{ Name, Severity string } `json:"alerts"`
		Providers []struct {
			Provider   string  `json:"provider"`
			ErrorRatio float64 `json:"error_ratio_1h"`
		} `json:"providers"`
	} `json:"prometheus"`
}

func TestAdminHealth(t *testing.T) {
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		switch {
		case strings.HasPrefix(q, "ALERTS"):
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"alertname":"TrophyHunterExternalAPIFailing","severity":"warning"},"value":[1,"1"]}]}}`))
		case strings.Contains(q, "status!~"):
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"provider":"psstore"},"value":[1,"0.8"]}]}}`))
		case strings.Contains(q, "external_api_requests_total"):
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{"provider":"psstore"},"value":[1,"40"]},{"metric":{"provider":"steam"},"value":[1,"900"]}]}}`))
		default:
			_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
		}
	}))
	defer prom.Close()

	var mu sync.Mutex
	var keys []string
	e := build(t, func(key string) syncpkg.SteamAPI { return accountFake{mu: &mu, keys: &keys, key: key} }, nil, nil,
		func(o *buildOpts) { o.steamServerKey = "SERVER-KEY"; o.promURL = prom.URL })
	call[me](t, e, http.MethodGet, "/api/v1/me", "", http.StatusOK)
	inviteFriend(t, e)
	friendInteg := callAs[struct{ ID string }](t, e, friendSub, friendEmail, http.MethodPost, "/api/v1/integrations/steam", `{"steam_id_or_vanity":"`+steamAlt+`"}`, http.StatusCreated)
	e.drain(t)

	// Um job morto para reprocessar.
	ctx := context.Background()
	j, _, err := e.queue.Enqueue(ctx, queue.EnqueueParams{Kind: "sync_game", Payload: map[string]string{"game_id": "x"}, MaxAttempts: 1})
	require.NoError(t, err)
	claimed, err := e.queue.Claim(ctx, time.Minute, "sync_game")
	require.NoError(t, err)
	require.Equal(t, j.ID, claimed.ID)
	require.NoError(t, e.queue.Fail(ctx, claimed.ID, claimed.LeaseToken, queue.Permanent(errors.New("steam: profile is not public"))))

	h := call[healthResp](t, e, http.MethodGet, "/api/v1/admin/health", "", http.StatusOK)
	require.Len(t, h.Accounts, 1)
	require.Equal(t, friendSub, h.Accounts[0].User)
	require.Equal(t, 2, h.Totals["members"])
	require.NotEmpty(t, h.RecentDead)
	require.Contains(t, h.RecentDead[0].Error, "not public")
	require.True(t, h.Prometheus.Available)
	require.Equal(t, "TrophyHunterExternalAPIFailing", h.Prometheus.Alerts[0].Name)
	require.Len(t, h.Prometheus.Providers, 2)
	require.Equal(t, "psstore", h.Prometheus.Providers[0].Provider)
	require.InDelta(t, 0.8, h.Prometheus.Providers[0].ErrorRatio, 0.001)
	for _, u := range h.AIUsage {
		if u.Owner {
			require.Zero(t, u.DayLimit, "dono não tem teto")
		}
	}

	// Ações do dono.
	resp := e.do(t, http.MethodPost, "/api/v1/admin/jobs/"+j.ID+"/retry", "", true)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusAccepted, resp.StatusCode)
	call[problem](t, e, http.MethodPost, "/api/v1/admin/jobs/"+j.ID+"/retry", "", http.StatusConflict)
	call[struct{ Enqueued bool }](t, e, http.MethodPost, "/api/v1/admin/integrations/"+friendInteg.ID+"/sync", "", http.StatusAccepted)

	// Membro não vê nem mexe (404, como o resto do /admin).
	callAs[problem](t, e, friendSub, friendEmail, http.MethodGet, "/api/v1/admin/health", "", http.StatusNotFound)
	callAs[problem](t, e, friendSub, friendEmail, http.MethodPost, "/api/v1/admin/jobs/"+j.ID+"/retry", "", http.StatusNotFound)
}

func TestAdminHealthWithoutPrometheus(t *testing.T) {
	e := setup(t)
	h := call[healthResp](t, e, http.MethodGet, "/api/v1/admin/health", "", http.StatusOK)
	require.False(t, h.Prometheus.Available)
	require.Empty(t, h.Prometheus.Alerts)
}

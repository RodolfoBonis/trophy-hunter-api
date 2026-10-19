package api_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	dbgen "github.com/RodolfoBonis/trophy-hunter-api/db/gen"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/integrations/hltb"
	syncpkg "github.com/RodolfoBonis/trophy-hunter-api/internal/sync"
)

// fakeHLTBSource: o HLTB conhece só o Portal 2 (o outro jogo do fake fica "não achado").
type fakeHLTBSource struct{ calls int }

func (f *fakeHLTBSource) Search(_ context.Context, _ string) ([]hltb.Result, error) {
	f.calls++
	return []hltb.Result{
		{ID: 2, Name: "Portal 2: Peer Review", CompletionistH: 3},
		{ID: 1, Name: "Portal 2", MainH: 8.6, ExtraH: 13.5, CompletionistH: 22},
	}, nil
}

func TestEstimatesFlowIntoGameAndNext(t *testing.T) {
	var mu sync.Mutex
	var keys []string
	e := build(t, func(key string) syncpkg.SteamAPI { return accountFake{mu: &mu, keys: &keys, key: key} }, nil, &fakeRunner{})
	call[struct{ ID string }](t, e, http.MethodPost, "/api/v1/integrations/steam", `{"api_key":"K","steam_id_or_vanity":"`+steamMain+`"}`, http.StatusCreated)
	e.drain(t)
	games := call[struct{ Items []gameItem }](t, e, http.MethodGet, "/api/v1/games?has_achievements=all", "", http.StatusOK)
	portal := ownerGameByExt(t, games.Items, "620")

	// Sem estimativa ainda: estimate = null.
	before := call[struct {
		Estimate *struct{} `json:"estimate"`
	}](t, e, http.MethodGet, "/api/v1/games/"+portal, "", http.StatusOK)
	require.Nil(t, before.Estimate)

	src := &fakeHLTBSource{}
	est := syncpkg.NewEstimator(dbgen.New(e.pool), src, time.Minute, 10, slog.New(slog.NewTextHandler(io.Discard, nil)))
	n, err := est.RefreshBatch(t.Context())
	require.NoError(t, err)
	require.Positive(t, n)
	// Já resolvidos (achado ou não): a rodada seguinte não busca de novo.
	calls := src.calls
	_, err = est.RefreshBatch(t.Context())
	require.NoError(t, err)
	require.Equal(t, calls, src.calls)

	got := call[struct {
		Estimate *struct {
			MainH          *float64 `json:"main_h"`
			CompletionistH *float64 `json:"completionist_h"`
			HoursLeft      *float64 `json:"hours_left"`
			SourceURL      string   `json:"source_url"`
		} `json:"estimate"`
	}](t, e, http.MethodGet, "/api/v1/games/"+portal, "", http.StatusOK)
	require.NotNil(t, got.Estimate)
	require.InDelta(t, 22, *got.Estimate.CompletionistH, 0.01, "o título exato, não o DLC")
	require.InDelta(t, 8.6, *got.Estimate.MainH, 0.01)
	require.Equal(t, "https://howlongtobeat.com/game/1", got.Estimate.SourceURL)
	require.NotNil(t, got.Estimate.HoursLeft)
	require.Less(t, *got.Estimate.HoursLeft, 22.0)

	next := call[struct {
		Items []struct {
			GameID    string   `json:"game_id"`
			HoursLeft *float64 `json:"hours_left"`
			Factors   struct {
				Time float64 `json:"time"`
			} `json:"factors"`
		} `json:"items"`
	}](t, e, http.MethodGet, "/api/v1/next", "", http.StatusOK)
	for _, it := range next.Items {
		if it.GameID == portal {
			require.NotNil(t, it.HoursLeft)
			require.Positive(t, it.Factors.Time)
		}
	}
}

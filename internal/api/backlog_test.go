package api_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/RodolfoBonis/trophy-hunter-api/internal/backlog"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/integrations/itad"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/integrations/psstore"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/integrations/steamstore"
	syncpkg "github.com/RodolfoBonis/trophy-hunter-api/internal/sync"
)

// fakeStores faz as três lojas (Steam, PS Store e ITAD).
type fakeStores struct {
	mu         sync.Mutex
	psDown     bool
	steamFinal int
	psFinal    int
}

func (f *fakeStores) Search(context.Context, string) ([]steamstore.App, error) {
	return []steamstore.App{
		{ID: 1245620, Name: "ELDEN RING", Image: "https://cdn/elden.jpg", Price: &steamstore.Price{Currency: "BRL", FinalCents: 27450, InitialCents: 27450}},
		{ID: 620, Name: "Portal 2", Image: "https://cdn/p2.jpg"},
	}, nil
}

func (f *fakeStores) Price(_ context.Context, appID int) (steamstore.Price, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if appID != 1245620 {
		return steamstore.Price{}, false, nil
	}
	return steamstore.Price{Currency: "BRL", FinalCents: f.steamFinal, InitialCents: 27450}, true, nil
}

type fakePS struct{ f *fakeStores }

func (f *fakeStores) ps() backlog.PSStore { return fakePS{f} }

func (p fakePS) Search(context.Context, string) ([]psstore.Product, error) {
	p.f.mu.Lock()
	defer p.f.mu.Unlock()
	if p.f.psDown {
		return nil, psstore.ErrQueryRejected
	}
	return []psstore.Product{
		{ID: "UP0700-ELDEN", Name: "ELDEN RING", Classification: "FULL_GAME", Image: "https://img/elden.png", BaseCents: 29990, FinalCents: p.f.psFinal},
		{ID: "UP0700-DLC", Name: "ELDEN RING Tarnished Pack", Classification: "ADD_ON_PACK", BaseCents: 2850, FinalCents: 2850},
	}, nil
}

type fakeITAD struct{}

func (f *fakeStores) itad() backlog.ITAD { return fakeITAD{} }

func (fakeITAD) Enabled() bool { return true }
func (fakeITAD) LookupSteam(_ context.Context, appID int) (string, bool, error) {
	return "g-elden", appID == 1245620, nil
}
func (fakeITAD) Prices(context.Context, []string) (map[string]itad.Prices, error) {
	low := 9990
	return map[string]itad.Prices{"g-elden": {LowCents: &low, Best: &itad.Deal{Shop: "Nuuvem", Cents: 19990, RegularCents: 27450, Cut: 27, URL: "https://itad.link/x", Currency: "BRL"}}}, nil
}

type catalogResp struct {
	Items []struct {
		PlatformJ  string `json:"platform"`
		StoreIDJ   string `json:"store_id"`
		TitleJ     string `json:"title"`
		PriceCents *int   `json:"price_cents"`
		Owned      bool   `json:"owned"`
	} `json:"items"`
	Unavailable []string `json:"unavailable"`
}

type wishResp struct {
	Items []struct {
		ID               string   `json:"id"`
		Platform         string   `json:"platform"`
		Title            string   `json:"title"`
		PriceStatus      string   `json:"price_status"`
		PriceCents       *int     `json:"price_cents"`
		LowCents         *int     `json:"low_cents"`
		Shop             string   `json:"shop"`
		TargetPriceCents *int     `json:"target_price_cents"`
		CompletionistH   *float64 `json:"completionist_h"`
	} `json:"items"`
}

func TestBacklog(t *testing.T) {
	stores := &fakeStores{steamFinal: 27450, psFinal: 29990}
	e := build(t, func(string) syncpkg.SteamAPI { return fakeSteam{} }, nil, nil, func(o *buildOpts) { o.stores = stores })
	ctx := context.Background()
	call[struct{ ID string }](t, e, http.MethodPost, "/api/v1/integrations/steam", `{"api_key":"K","steam_id_or_vanity":"76561197960000000"}`, http.StatusCreated)
	e.drain(t)

	// Busca: Steam + PS Store (sem DLC); Portal 2 já está na biblioteca.
	c := call[catalogResp](t, e, http.MethodGet, "/api/v1/catalog/search?q=elden", "", http.StatusOK)
	require.Len(t, c.Items, 3)
	require.Empty(t, c.Unavailable)
	for _, it := range c.Items {
		require.Equal(t, it.TitleJ == "Portal 2", it.Owned, it.TitleJ)
	}
	call[problem](t, e, http.MethodGet, "/api/v1/catalog/search?q=e", "", http.StatusBadRequest)
	// PS Store fora: a Steam segue e a UI sabe.
	stores.mu.Lock()
	stores.psDown = true
	stores.mu.Unlock()
	c = call[catalogResp](t, e, http.MethodGet, "/api/v1/catalog/search?q=elden", "", http.StatusOK)
	require.Equal(t, []string{"psn"}, c.Unavailable)
	require.Len(t, c.Items, 2)
	stores.mu.Lock()
	stores.psDown = false
	stores.mu.Unlock()

	// Adicionar: preço já vem. Steam: o ITAD achou mais barato na Nuuvem + menor histórico.
	_, err := e.pool.Exec(ctx, "INSERT INTO game_estimates (normalized_title, status, completionist_h, fetched_at) VALUES ('elden ring', 'found', 136.2, now())")
	require.NoError(t, err)
	call[struct{ ID string }](t, e, http.MethodPost, "/api/v1/wishlist", `{"platform":"steam","store_id":"1245620","title":"ELDEN RING","cover":"https://cdn/elden.jpg"}`, http.StatusCreated)
	call[struct{ ID string }](t, e, http.MethodPost, "/api/v1/wishlist", `{"platform":"psn","store_id":"UP0700-ELDEN","title":"ELDEN RING"}`, http.StatusCreated)
	call[problem](t, e, http.MethodPost, "/api/v1/wishlist", `{"platform":"xbox","store_id":"1","title":"x"}`, http.StatusUnprocessableEntity)
	call[problem](t, e, http.MethodPost, "/api/v1/wishlist", `{"platform":"steam","store_id":"abc","title":"x"}`, http.StatusUnprocessableEntity)

	w := call[wishResp](t, e, http.MethodGet, "/api/v1/wishlist", "", http.StatusOK)
	require.Len(t, w.Items, 2)
	byPlat := map[string]int{}
	for i, it := range w.Items {
		byPlat[it.Platform] = i
	}
	st := w.Items[byPlat["steam"]]
	require.Equal(t, "ok", st.PriceStatus)
	require.Equal(t, 19990, *st.PriceCents)
	require.Equal(t, "Nuuvem", st.Shop)
	require.Equal(t, 9990, *st.LowCents)
	require.InDelta(t, 136.2, *st.CompletionistH, 0.01)
	ps := w.Items[byPlat["psn"]]
	require.Equal(t, 29990, *ps.PriceCents)
	require.Equal(t, "PlayStation Store", ps.Shop)

	// Preço-alvo + promoção na PS Store → um aviso só (não repete no mesmo preço).
	noContent(t, e, http.MethodPatch, "/api/v1/wishlist/"+ps.ID, `{"target_price_cents":20000}`)
	stores.mu.Lock()
	stores.psFinal = 14995
	stores.mu.Unlock()
	_, err = e.pool.Exec(ctx, "UPDATE game_prices SET fetched_at = now() - interval '13 hours'")
	require.NoError(t, err)
	svc := backlog.New(e.q, stores, stores.ps(), stores.itad(), e.queue, testLogger())
	require.NoError(t, svc.RefreshPrices(ctx, 10))
	countDeals := func() int {
		var n int
		require.NoError(t, e.pool.QueryRow(ctx, "SELECT count(*) FROM jobs WHERE kind = 'notify' AND payload->>'kind' = 'deals'").Scan(&n))
		return n
	}
	require.Equal(t, 1, countDeals())
	require.NoError(t, svc.RefreshPrices(ctx, 10))
	require.Equal(t, 1, countDeals())
	var body string
	require.NoError(t, e.pool.QueryRow(ctx, "SELECT payload->>'body' FROM jobs WHERE kind = 'notify' AND payload->>'kind' = 'deals'").Scan(&body))
	require.Contains(t, body, "R$ 149,95")
	require.Contains(t, body, "−50%")

	// Outra pessoa não vê nem mexe.
	inviteFriend(t, e)
	fw := callAs[wishResp](t, e, friendSub, friendEmail, http.MethodGet, "/api/v1/wishlist", "", http.StatusOK)
	require.Empty(t, fw.Items)
	callAs[problem](t, e, friendSub, friendEmail, http.MethodDelete, "/api/v1/wishlist/"+ps.ID, "", http.StatusNotFound)
	callAs[problem](t, e, friendSub, friendEmail, http.MethodPatch, "/api/v1/wishlist/"+ps.ID, `{"target_price_cents":1}`, http.StatusNotFound)

	noContent(t, e, http.MethodDelete, "/api/v1/wishlist/"+ps.ID, "")
	require.Len(t, call[wishResp](t, e, http.MethodGet, "/api/v1/wishlist", "", http.StatusOK).Items, 1)
}

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

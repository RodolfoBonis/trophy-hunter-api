package steamstore

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSearchAndPrice(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/storesearch/":
			if r.URL.Query().Get("cc") != "BR" {
				t.Errorf("cc = %q", r.URL.Query().Get("cc"))
			}
			_, _ = w.Write([]byte(`{"total":2,"items":[{"id":1245620,"name":"ELDEN RING","tiny_image":"x","price":{"currency":"BRL","initial":27450,"final":13725}},{"id":7,"name":"Grátis"}]}`))
		case "/api/appdetails":
			switch r.URL.Query().Get("appids") {
			case "1245620":
				_, _ = w.Write([]byte(`{"1245620":{"success":true,"data":{"price_overview":{"currency":"BRL","initial":27450,"final":27450,"discount_percent":0}}}}`))
			case "429":
				w.WriteHeader(http.StatusTooManyRequests)
			default:
				_, _ = w.Write([]byte(`{"7":{"success":true,"data":[]}}`))
			}
		}
	}))
	defer srv.Close()
	c := New(srv.URL, srv.Client())
	c.interval = 0
	ctx := context.Background()
	apps, err := c.Search(ctx, "elden")
	if err != nil || len(apps) != 2 {
		t.Fatalf("search: %v %+v", err, apps)
	}
	if apps[0].Price == nil || apps[0].Price.FinalCents != 13725 || apps[0].Price.Discount != 50 || apps[1].Price != nil {
		t.Fatalf("preços: %+v %+v", apps[0].Price, apps[1].Price)
	}
	p, ok, err := c.Price(ctx, 1245620)
	if err != nil || !ok || p.FinalCents != 27450 {
		t.Fatalf("price: %+v %v %v", p, ok, err)
	}
	if _, ok, err := c.Price(ctx, 7); err == nil && ok {
		t.Fatal("sem price_overview deveria ser found=false")
	}
	if _, _, err := c.Price(ctx, 429); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("429: %v", err)
	}
}

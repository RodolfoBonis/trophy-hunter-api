package itad

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLookupAndPrices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("ITAD-API-Key") != "k" || strings.Contains(r.URL.RawQuery, "key=") {
			t.Errorf("chave tem que ir só no header")
		}
		switch r.URL.Path {
		case "/games/lookup/v1":
			if r.URL.Query().Get("appid") == "1" {
				_, _ = w.Write([]byte(`{"found":false}`))
				return
			}
			_, _ = w.Write([]byte(`{"found":true,"game":{"id":"g-elden"}}`))
		case "/games/prices/v3":
			var ids []string
			_ = json.NewDecoder(r.Body).Decode(&ids)
			if r.URL.Query().Get("country") != "BR" || len(ids) != 1 {
				t.Errorf("prices: %v %v", r.URL.Query(), ids)
			}
			_, _ = w.Write([]byte(`[{"id":"g-elden","historyLow":{"all":{"amountInt":9990,"currency":"BRL"}},"deals":[
				{"shop":{"name":"Steam"},"price":{"amountInt":13725,"currency":"BRL"},"regular":{"amountInt":27450,"currency":"BRL"},"cut":50,"url":"https://itad.link/a"},
				{"shop":{"name":"Nuuvem"},"price":{"amountInt":12990,"currency":"BRL"},"regular":{"amountInt":27450,"currency":"BRL"},"cut":53,"url":"https://itad.link/b"}]}]`))
		default:
			w.WriteHeader(http.StatusTooManyRequests)
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "k", srv.Client())
	c.interval = 0
	ctx := context.Background()
	id, ok, err := c.LookupSteam(ctx, 1245620)
	if err != nil || !ok || id != "g-elden" {
		t.Fatalf("lookup: %q %v %v", id, ok, err)
	}
	if _, ok, _ := c.LookupSteam(ctx, 1); ok {
		t.Fatal("não encontrado")
	}
	ps, err := c.Prices(ctx, []string{"g-elden"})
	if err != nil {
		t.Fatal(err)
	}
	p := ps["g-elden"]
	if p.Best == nil || p.Best.Shop != "Nuuvem" || p.Best.Cents != 12990 || p.LowCents == nil || *p.LowCents != 9990 {
		t.Fatalf("prices: %+v", p)
	}
}

func TestNotConfigured(t *testing.T) {
	c := New("http://invalido", "", nil)
	if c.Enabled() {
		t.Fatal("sem chave")
	}
	if _, _, err := c.LookupSteam(context.Background(), 1); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("err = %v", err)
	}
}

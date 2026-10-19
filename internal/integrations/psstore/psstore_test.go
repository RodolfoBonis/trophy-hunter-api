package psstore

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseBRL(t *testing.T) {
	cases := map[string]int{"R$299,90": 29990, "R$ 1.299,90": 129990, "R$28,50": 2850, "R$0,00": 0}
	for in, want := range cases {
		if got, ok := ParseBRL(in); !ok || got != want {
			t.Errorf("ParseBRL(%q) = %d %v", in, got, ok)
		}
	}
	for _, bad := range []string{"", "Grátis", "Incluído"} {
		if _, ok := ParseBRL(bad); ok {
			t.Errorf("ParseBRL(%q) deveria falhar", bad)
		}
	}
}

const sample = `{"data":{"universalSearch":{"results":[
{"__typename":"Product","id":"UP0700-PPSA04610_00-ELDENRING0000000","name":"ELDEN RING","storeDisplayClassification":"FULL_GAME","platforms":["PS4","PS5"],
 "media":[{"role":"PREVIEW","type":"VIDEO","url":"v.mp4"},{"role":"MASTER","type":"IMAGE","url":"https://img/master.png"}],
 "price":{"basePrice":"R$299,90","discountedPrice":"R$149,95","isFree":false}},
{"__typename":"Concept","id":"123","name":"x"}]}}}`

func TestSearch(t *testing.T) {
	var gotHash string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHash = r.URL.Query().Get("extensions")
		if r.Header.Get("x-psn-store-locale-override") != "pt-BR" {
			t.Errorf("locale header ausente")
		}
		switch {
		case strings.Contains(r.URL.Query().Get("variables"), `"searchTerm":"limite"`):
			w.WriteHeader(http.StatusTooManyRequests)
		case strings.Contains(gotHash, "velho"):
			_, _ = w.Write([]byte(`{"errors":[{"message":"PersistedQueryNotFound"}]}`))
		default:
			_, _ = w.Write([]byte(sample))
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "", srv.Client())
	c.interval = 0
	ctx := context.Background()
	ps, err := c.Search(ctx, "elden ring")
	if err != nil || len(ps) != 1 {
		t.Fatalf("search: %v %+v", err, ps)
	}
	p := ps[0]
	if p.BaseCents != 29990 || p.FinalCents != 14995 || p.Image != "https://img/master.png" || p.Classification != "FULL_GAME" {
		t.Fatalf("produto: %+v", p)
	}
	if !strings.Contains(gotHash, DefaultSearchHash) {
		t.Fatalf("hash padrão não enviado: %s", gotHash)
	}
	if _, err := c.Search(ctx, "limite"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("429: %v", err)
	}
	old := New(srv.URL, "hash-velho", srv.Client())
	old.interval = 0
	if _, err := old.Search(ctx, "x"); !errors.Is(err, ErrQueryRejected) {
		t.Fatalf("hash velho: %v", err)
	}
}

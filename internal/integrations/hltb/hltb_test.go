package hltb

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeHLTB imita o site: rota de busca configurável, token obrigatório, home + bundle JS.
type fakeHLTB struct {
	route      string // rota atual da busca (ex.: /api/search/site)
	token      atomic.Value
	expireNext atomic.Bool
	limited    atomic.Bool
	searches   atomic.Int32
	inits      atomic.Int32
}

func (f *fakeHLTB) handler(t *testing.T) http.Handler {
	t.Helper()
	f.token.Store("tok-1")
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/":
			_, _ = fmt.Fprint(w, `<html><script src="/_next/static/chunks/abc-1.js"></script><script src="/_next/static/chunks/main.js"></script></html>`)
		case r.URL.Path == "/_next/static/chunks/main.js":
			_, _ = fmt.Fprint(w, "let er=async()=>{let e=await fetch(`"+f.route+"/init?t=${Date.now()}`)}")
		case strings.HasPrefix(r.URL.Path, "/_next/"):
			_, _ = fmt.Fprint(w, "console.log(1)")
		case r.URL.Path == f.route+"/init":
			f.inits.Add(1)
			require.Equal(t, userAgent, r.Header.Get("User-Agent"))
			tok := fmt.Sprintf("tok-%d", f.inits.Load())
			f.token.Store(tok)
			_ = json.NewEncoder(w).Encode(map[string]string{"token": tok})
		case r.URL.Path == f.route && r.Method == http.MethodPost:
			f.searches.Add(1)
			if f.limited.Load() {
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			if f.expireNext.CompareAndSwap(true, false) || r.Header.Get("x-auth-token") != f.token.Load() {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			var req searchRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			require.Equal(t, []string{"Elden", "Ring"}, req.SearchTerms)
			_, _ = fmt.Fprint(w, `{"count":2,"data":[
				{"game_id":68151,"game_name":"Elden Ring","release_world":2022,"comp_main":216479,"comp_plus":364894,"comp_100":490288,"comp_all_count":5000},
				{"game_id":139385,"game_name":"Elden Ring: Shadow of the Erdtree","comp_100":184519}]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	return mux
}

func newTestClient(t *testing.T, f *fakeHLTB) *Client {
	t.Helper()
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	c := New(Options{BaseURL: srv.URL})
	c.sleep = func(context.Context, time.Duration) error { return nil }
	return c
}

func TestSearchConvertsSecondsToHours(t *testing.T) {
	f := &fakeHLTB{route: defaultSearchPath}
	c := newTestClient(t, f)
	res, err := c.Search(t.Context(), "Elden Ring")
	require.NoError(t, err)
	require.Len(t, res, 2)
	require.InDelta(t, 136.2, res[0].CompletionistH, 0.1)
	require.InDelta(t, 60.1, res[0].MainH, 0.1)
	// Token reaproveitado na 2ª busca.
	_, err = c.Search(t.Context(), "Elden Ring")
	require.NoError(t, err)
	require.EqualValues(t, 1, f.inits.Load())
}

func TestSearchRefreshesExpiredToken(t *testing.T) {
	f := &fakeHLTB{route: defaultSearchPath}
	c := newTestClient(t, f)
	f.expireNext.Store(true)
	res, err := c.Search(t.Context(), "Elden Ring")
	require.NoError(t, err)
	require.NotEmpty(t, res)
	require.EqualValues(t, 2, f.inits.Load(), "403 → token novo e 1 nova tentativa")
}

func TestSearchRediscoversMovedRoute(t *testing.T) {
	f := &fakeHLTB{route: "/api/find/v2"} // o site mudou a rota; o cliente ainda usa a antiga
	c := newTestClient(t, f)
	res, err := c.Search(t.Context(), "Elden Ring")
	require.NoError(t, err)
	require.NotEmpty(t, res)
	require.Equal(t, "/api/find/v2", c.searchPath)
}

func TestSearchRateLimited(t *testing.T) {
	f := &fakeHLTB{route: defaultSearchPath}
	c := newTestClient(t, f)
	f.limited.Store(true)
	_, err := c.Search(t.Context(), "Elden Ring")
	require.ErrorIs(t, err, ErrRateLimited)
	require.EqualValues(t, 1, f.searches.Load(), "429 não insiste")
}

func TestBestMatchOnlyExactNormalizedTitle(t *testing.T) {
	norm := func(s string) string { return strings.ToLower(strings.ReplaceAll(s, ":", "")) }
	res := []Result{{ID: 2, Name: "Elden Ring: Nightreign", CompletionistH: 60}, {ID: 1, Name: "Elden Ring", CompletionistH: 136}}
	m, ok := BestMatch(norm, "ELDEN RING", res)
	require.True(t, ok)
	require.EqualValues(t, 1, m.ID)
	_, ok = BestMatch(norm, "Hades II", res)
	require.False(t, ok, "sem título igual, sem estimativa")
}

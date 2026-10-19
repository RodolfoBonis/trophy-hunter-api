// Package itad fala com a API oficial do IsThereAnyDeal (v2): acha o jogo pelo appid da Steam e traz
// o melhor preço atual nas lojas de PC (BR) e o menor preço histórico. A chave (ITAD_API_KEY) vai no
// header ITAD-API-Key — nunca em log/URL. Sem chave, o cliente responde ErrNotConfigured e o backlog
// usa só o preço da própria Steam.
package itad

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/metrics"
)

var (
	// ErrNotConfigured indica que não há ITAD_API_KEY.
	ErrNotConfigured = errors.New("itad: sem chave")
	// ErrRateLimited indica 429 (limite de 1000 req / 5 min).
	ErrRateLimited = errors.New("itad: rate limited")
)

const minInterval = 400 * time.Millisecond

// Deal é o melhor preço atual do jogo (centavos).
type Deal struct {
	Shop         string
	Cents        int
	RegularCents int
	Cut          int
	URL          string
	Currency     string
}

// Prices de um jogo: melhor oferta agora e o menor preço já visto.
type Prices struct {
	Best     *Deal
	LowCents *int
}

// Client é seguro para uso concorrente.
type Client struct {
	base     string
	key      string
	hc       *http.Client
	mu       sync.Mutex
	last     time.Time
	interval time.Duration
}

// New monta o cliente; base vazia = https://api.isthereanydeal.com.
func New(base, key string, hc *http.Client) *Client {
	if base == "" {
		base = "https://api.isthereanydeal.com"
	}
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	return &Client{base: base, key: key, hc: hc, interval: minInterval}
}

// Enabled indica se há chave.
func (c *Client) Enabled() bool { return c.key != "" }

func (c *Client) do(ctx context.Context, endpoint, method, path string, q url.Values, body any, out any) error {
	if c.key == "" {
		return ErrNotConfigured
	}
	c.mu.Lock()
	if wait := c.interval - time.Since(c.last); wait > 0 {
		select {
		case <-ctx.Done():
			c.mu.Unlock()
			return ctx.Err()
		case <-time.After(wait):
		}
	}
	c.last = time.Now()
	c.mu.Unlock()

	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path+"?"+q.Encode(), rdr)
	if err != nil {
		return err
	}
	req.Header.Set("ITAD-API-Key", c.key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		metrics.ExternalAPIRequest("itad", endpoint, "error")
		return fmt.Errorf("itad %s: %w", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		metrics.ExternalAPIRequest("itad", endpoint, "error")
		return ErrRateLimited
	case resp.StatusCode != http.StatusOK:
		metrics.ExternalAPIRequest("itad", endpoint, "error")
		return fmt.Errorf("itad %s: status %d", endpoint, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		metrics.ExternalAPIRequest("itad", endpoint, "error")
		return fmt.Errorf("itad %s: resposta inválida", endpoint)
	}
	metrics.ExternalAPIRequest("itad", endpoint, "2xx")
	return nil
}

// LookupSteam acha o id do jogo no ITAD pelo appid da Steam.
func (c *Client) LookupSteam(ctx context.Context, appID int) (string, bool, error) {
	var body struct {
		Found bool `json:"found"`
		Game  *struct {
			ID string `json:"id"`
		} `json:"game"`
	}
	if err := c.do(ctx, "lookup", http.MethodGet, "/games/lookup/v1", url.Values{"appid": {strconv.Itoa(appID)}}, nil, &body); err != nil {
		return "", false, err
	}
	if !body.Found || body.Game == nil {
		return "", false, nil
	}
	return body.Game.ID, true, nil
}

type money struct {
	AmountInt int    `json:"amountInt"`
	Currency  string `json:"currency"`
}

// Prices traz preço atual (lojas BR) e menor histórico de vários jogos de uma vez.
func (c *Client) Prices(ctx context.Context, ids []string) (map[string]Prices, error) {
	var body []struct {
		ID         string `json:"id"`
		HistoryLow *struct {
			All *money `json:"all"`
		} `json:"historyLow"`
		Deals []struct {
			Shop struct {
				Name string `json:"name"`
			} `json:"shop"`
			Price   money  `json:"price"`
			Regular money  `json:"regular"`
			Cut     int    `json:"cut"`
			URL     string `json:"url"`
		} `json:"deals"`
	}
	q := url.Values{"country": {"BR"}, "vouchers": {"false"}}
	if err := c.do(ctx, "prices", http.MethodPost, "/games/prices/v3", q, ids, &body); err != nil {
		return nil, err
	}
	out := make(map[string]Prices, len(body))
	for _, g := range body {
		p := Prices{}
		if g.HistoryLow != nil && g.HistoryLow.All != nil {
			v := g.HistoryLow.All.AmountInt
			p.LowCents = &v
		}
		for _, d := range g.Deals {
			if p.Best == nil || d.Price.AmountInt < p.Best.Cents {
				p.Best = &Deal{Shop: d.Shop.Name, Cents: d.Price.AmountInt, RegularCents: d.Regular.AmountInt, Cut: d.Cut, URL: d.URL, Currency: d.Price.Currency}
			}
		}
		out[g.ID] = p
	}
	return out, nil
}

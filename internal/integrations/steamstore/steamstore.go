// Package steamstore fala com a loja pública da Steam (sem chave): busca de jogos e preço no Brasil.
// Usado pelo backlog ("quero platinar"): achar jogos que a pessoa ainda não tem e o preço em BRL.
package steamstore

import (
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

// ErrRateLimited indica que a Steam pediu para esperar (429).
var ErrRateLimited = errors.New("steamstore: rate limited")

const minInterval = 1500 * time.Millisecond

// App é um resultado da busca da loja.
type App struct {
	ID    int
	Name  string
	Image string
	Price *Price
}

// Price em centavos (BRL).
type Price struct {
	Currency     string
	FinalCents   int
	InitialCents int
	Discount     int
}

// Client é seguro para uso concorrente (chamadas serializadas com intervalo mínimo).
type Client struct {
	base string
	hc   *http.Client
	mu   sync.Mutex
	last time.Time
	// interval entre chamadas (testes zeram).
	interval time.Duration
}

// New monta o cliente; base vazia = https://store.steampowered.com.
func New(base string, hc *http.Client) *Client {
	if base == "" {
		base = "https://store.steampowered.com"
	}
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	return &Client{base: base, hc: hc, interval: minInterval}
}

func (c *Client) get(ctx context.Context, endpoint, path string, q url.Values, out any) error {
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		metrics.ExternalAPIRequest("steamstore", endpoint, "error")
		return fmt.Errorf("steamstore %s: %w", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		metrics.ExternalAPIRequest("steamstore", endpoint, "error")
		return ErrRateLimited
	case resp.StatusCode != http.StatusOK:
		metrics.ExternalAPIRequest("steamstore", endpoint, "error")
		return fmt.Errorf("steamstore %s: status %d", endpoint, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		metrics.ExternalAPIRequest("steamstore", endpoint, "error")
		return fmt.Errorf("steamstore %s: resposta inválida: %w", endpoint, err)
	}
	metrics.ExternalAPIRequest("steamstore", endpoint, "2xx")
	return nil
}

// Search busca na loja do Brasil (até 10 resultados).
func (c *Client) Search(ctx context.Context, term string) ([]App, error) {
	var body struct {
		Items []struct {
			ID        int    `json:"id"`
			Name      string `json:"name"`
			TinyImage string `json:"tiny_image"`
			Price     *struct {
				Currency string `json:"currency"`
				Initial  int    `json:"initial"`
				Final    int    `json:"final"`
			} `json:"price"`
		} `json:"items"`
	}
	q := url.Values{"term": {term}, "l": {"portuguese"}, "cc": {"BR"}}
	if err := c.get(ctx, "search", "/api/storesearch/", q, &body); err != nil {
		return nil, err
	}
	out := make([]App, 0, len(body.Items))
	for _, it := range body.Items {
		a := App{ID: it.ID, Name: it.Name, Image: CapsuleURL(it.ID)}
		if it.Price != nil {
			a.Price = &Price{Currency: it.Price.Currency, FinalCents: it.Price.Final, InitialCents: it.Price.Initial, Discount: discount(it.Price.Initial, it.Price.Final)}
		}
		out = append(out, a)
	}
	return out, nil
}

// Price devolve o preço no Brasil; found=false para jogo grátis/sem preço/indisponível.
func (c *Client) Price(ctx context.Context, appID int) (Price, bool, error) {
	id := strconv.Itoa(appID)
	var body map[string]struct {
		Success bool `json:"success"`
		Data    *struct {
			PriceOverview *struct {
				Currency        string `json:"currency"`
				Initial         int    `json:"initial"`
				Final           int    `json:"final"`
				DiscountPercent int    `json:"discount_percent"`
			} `json:"price_overview"`
		} `json:"data"`
	}
	q := url.Values{"appids": {id}, "cc": {"br"}, "filters": {"price_overview"}}
	if err := c.get(ctx, "appdetails", "/api/appdetails", q, &body); err != nil {
		return Price{}, false, err
	}
	e, ok := body[id]
	if !ok || !e.Success || e.Data == nil || e.Data.PriceOverview == nil {
		return Price{}, false, nil
	}
	p := e.Data.PriceOverview
	return Price{Currency: p.Currency, FinalCents: p.Final, InitialCents: p.Initial, Discount: p.DiscountPercent}, true, nil
}

// CapsuleURL é a capa vertical do app (a mesma da biblioteca).
func CapsuleURL(appID int) string {
	return fmt.Sprintf("https://cdn.cloudflare.steamstatic.com/steam/apps/%d/library_600x900.jpg", appID)
}

// StoreURL é a página do app na loja.
func StoreURL(appID int) string { return fmt.Sprintf("https://store.steampowered.com/app/%d/", appID) }

func discount(initial, final int) int {
	if initial <= 0 || final >= initial {
		return 0
	}
	return (initial - final) * 100 / initial
}

// Package psstore busca jogos e preços na PlayStation Store do Brasil.
//
// NÃO É API OFICIAL: é o mesmo GraphQL que a loja usa no navegador (persisted query
// `getSearchResults`). O hash da query muda quando a Sony publica uma versão nova da loja; aí a
// busca passa a responder PersistedQueryNotFound → ErrQueryRejected, métrica com erro e o alerta
// TrophyHunterPriceProviderFailing. Para atualizar: abrir a busca da loja com o DevTools (ou o
// script do runbook), copiar o sha256Hash da chamada getSearchResults e trocar PSSTORE_SEARCH_HASH.
package psstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/metrics"
)

// DefaultSearchHash é o hash de getSearchResults visto em out/2026.
const DefaultSearchHash = "4df6284f982e57bec70f23c77e2c219dc792eb19af7fb3d3a81767aa3f1958aa"

var (
	// ErrQueryRejected indica que a loja não reconhece mais o hash (versão nova) — atualizar PSSTORE_SEARCH_HASH.
	ErrQueryRejected = errors.New("psstore: persisted query rejeitada (hash desatualizado)")
	// ErrRateLimited indica 429.
	ErrRateLimited = errors.New("psstore: rate limited")
)

const minInterval = 2 * time.Second

// Product é um item da busca.
type Product struct {
	ID             string // ex.: UP0700-PPSA04610_00-ELDENRING0000000
	Name           string
	Classification string // FULL_GAME, GAME_BUNDLE, ADD_ON_PACK…
	Platforms      []string
	Image          string
	// Centavos (BRL). 0/0 = sem preço (grátis, só assinatura ou indisponível).
	BaseCents  int
	FinalCents int
	Free       bool
}

// Client é seguro para uso concorrente.
type Client struct {
	base     string
	hash     string
	hc       *http.Client
	mu       sync.Mutex
	last     time.Time
	interval time.Duration
}

// New monta o cliente; base vazia = https://web.np.playstation.com; hash vazio = DefaultSearchHash.
func New(base, hash string, hc *http.Client) *Client {
	if base == "" {
		base = "https://web.np.playstation.com"
	}
	if hash == "" {
		hash = DefaultSearchHash
	}
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	return &Client{base: base, hash: hash, hc: hc, interval: minInterval}
}

type searchResp struct {
	Data *struct {
		UniversalSearch *struct {
			Results []struct {
				Typename                   string   `json:"__typename"`
				ID                         string   `json:"id"`
				Name                       string   `json:"name"`
				StoreDisplayClassification string   `json:"storeDisplayClassification"`
				Platforms                  []string `json:"platforms"`
				Media                      []struct {
					Role string `json:"role"`
					Type string `json:"type"`
					URL  string `json:"url"`
				} `json:"media"`
				Price *struct {
					BasePrice       string `json:"basePrice"`
					DiscountedPrice string `json:"discountedPrice"`
					IsFree          bool   `json:"isFree"`
				} `json:"price"`
			} `json:"results"`
		} `json:"universalSearch"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// Search busca na loja do Brasil (até 24 itens: jogos, edições e DLCs).
func (c *Client) Search(ctx context.Context, term string) ([]Product, error) {
	c.mu.Lock()
	if wait := c.interval - time.Since(c.last); wait > 0 {
		select {
		case <-ctx.Done():
			c.mu.Unlock()
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
	c.last = time.Now()
	c.mu.Unlock()

	vars, _ := json.Marshal(map[string]any{
		"countryCode": "BR", "languageCode": "pt", "nextCursor": "", "pageOffset": 0, "pageSize": 24, "searchTerm": term,
	})
	ext, _ := json.Marshal(map[string]any{"persistedQuery": map[string]any{"version": 1, "sha256Hash": c.hash}})
	q := url.Values{"operationName": {"getSearchResults"}, "variables": {string(vars)}, "extensions": {string(ext)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/graphql/v1/op?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-psn-store-locale-override", "pt-BR")
	req.Header.Set("apollographql-client-name", "@sie-ppr-web-store/app")
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130 Safari/537.36")
	resp, err := c.hc.Do(req)
	if err != nil {
		metrics.ExternalAPIRequest("psstore", "search", "error")
		return nil, fmt.Errorf("psstore: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusTooManyRequests {
		metrics.ExternalAPIRequest("psstore", "search", "error")
		return nil, ErrRateLimited
	}
	var body searchResp
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		metrics.ExternalAPIRequest("psstore", "search", "error")
		return nil, fmt.Errorf("psstore: status %d, resposta inválida", resp.StatusCode)
	}
	for _, e := range body.Errors {
		if strings.Contains(strings.ToLower(e.Message), "persistedquery") {
			metrics.ExternalAPIRequest("psstore", "search", "error")
			return nil, ErrQueryRejected
		}
	}
	if resp.StatusCode != http.StatusOK || body.Data == nil || body.Data.UniversalSearch == nil {
		metrics.ExternalAPIRequest("psstore", "search", "error")
		return nil, fmt.Errorf("psstore: status %d", resp.StatusCode)
	}
	metrics.ExternalAPIRequest("psstore", "search", "2xx")
	out := []Product{}
	for _, r := range body.Data.UniversalSearch.Results {
		if r.Typename != "Product" || r.ID == "" {
			continue
		}
		p := Product{ID: r.ID, Name: r.Name, Classification: r.StoreDisplayClassification, Platforms: r.Platforms, Image: pickImage(r.Media)}
		if r.Price != nil {
			p.Free = r.Price.IsFree
			p.BaseCents, _ = ParseBRL(r.Price.BasePrice)
			p.FinalCents, _ = ParseBRL(r.Price.DiscountedPrice)
		}
		out = append(out, p)
	}
	return out, nil
}

func pickImage(media []struct {
	Role string `json:"role"`
	Type string `json:"type"`
	URL  string `json:"url"`
}) string {
	for _, want := range []string{"MASTER", "GAMEHUB_COVER_ART", "PORTRAIT_BANNER", "EDITION_KEY_ART"} {
		for _, m := range media {
			if m.Type == "IMAGE" && m.Role == want {
				return m.URL
			}
		}
	}
	return ""
}

// ParseBRL converte "R$299,90" / "R$ 1.299,90" em centavos. "Grátis"/vazio → false.
func ParseBRL(s string) (int, bool) {
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "R$"))
	s = strings.ReplaceAll(strings.ReplaceAll(s, ".", ""), " ", "")
	reais, cents, ok := strings.Cut(s, ",")
	if !ok {
		cents = "00"
	}
	r, err1 := strconv.Atoi(strings.TrimSpace(reais))
	c, err2 := strconv.Atoi(cents)
	if err1 != nil || err2 != nil || len(cents) != 2 {
		return 0, false
	}
	return r*100 + c, true
}

// StoreURL é a página do produto na loja.
func StoreURL(productID string) string {
	return "https://store.playstation.com/pt-br/product/" + url.PathEscape(productID)
}

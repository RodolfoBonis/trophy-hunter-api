// Package hltb busca estimativas de tempo no HowLongToBeat (quanto leva para zerar / fazer 100%).
//
// O HLTB NÃO tem API oficial e muda a rota da busca de tempos em tempos (out/2026: /api/bleed →
// /api/search/site). O cliente segue o próprio site:
//   - GET {search}/init devolve um token (amarrado a IP + User-Agent — por isso o UA é fixo);
//   - POST {search} com o header x-auth-token;
//   - 403 → renova o token e tenta 1×; 404 → redescobre a rota no bundle JS do site (no máx. 1× a
//     cada 10 min) e tenta 1×; 429 → ErrRateLimited (o chamador para e volta depois);
//   - no máximo 1 requisição a cada 2 s, para não ser bloqueado.
//
// Tudo é "melhor esforço": se o HLTB mudar de novo, as estimativas param de chegar (métrica
// external_api_requests_total{provider="hltb"} e alerta), mas o resto do app segue igual.
package hltb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/metrics"
)

const (
	provider          = "hltb"
	defaultBase       = "https://howlongtobeat.com"
	defaultSearchPath = "/api/search/site"
	// UA fixo: o token do HLTB é amarrado ao User-Agent de quem pediu.
	userAgent       = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Safari/537.36"
	tokenTTL        = 50 * time.Minute
	minInterval     = 2 * time.Second
	rediscoverEvery = 10 * time.Minute
	maxBody         = 2 << 20
)

// ErrRateLimited indica que o HLTB pediu para esperar (429).
var ErrRateLimited = errors.New("hltb: rate limited")

// Result é um jogo encontrado, com os tempos em horas (0 = sem dados).
type Result struct {
	ID              int64
	Name            string
	Year            int
	MainH           float64
	ExtraH          float64
	CompletionistH  float64
	SubmissionCount int
}

// Client é seguro para uso concorrente (as chamadas são serializadas pelo throttle).
type Client struct {
	hc     *http.Client
	base   string
	logger *slog.Logger
	now    func() time.Time
	sleep  func(context.Context, time.Duration) error

	mu             sync.Mutex
	searchPath     string
	token          string
	tokenAt        time.Time
	lastReq        time.Time
	lastDiscoverAt time.Time
}

// Options configura o cliente (BaseURL/HTTPClient para testes).
type Options struct {
	BaseURL    string
	HTTPClient *http.Client
	Logger     *slog.Logger
}

// New constrói o cliente.
func New(o Options) *Client {
	hc := o.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second}
	}
	base := strings.TrimRight(o.BaseURL, "/")
	if base == "" {
		base = defaultBase
	}
	lg := o.Logger
	if lg == nil {
		lg = slog.Default()
	}
	return &Client{hc: hc, base: base, logger: lg, now: time.Now, sleep: sleepCtx, searchPath: defaultSearchPath}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// throttle garante o intervalo mínimo entre requisições (chamar com c.mu travado).
func (c *Client) throttle(ctx context.Context) error {
	if wait := minInterval - c.now().Sub(c.lastReq); wait > 0 {
		if err := c.sleep(ctx, wait); err != nil {
			return err
		}
	}
	c.lastReq = c.now()
	return nil
}

func (c *Client) do(ctx context.Context, endpoint, method, path string, body []byte, token string) (int, []byte, error) {
	if err := c.throttle(ctx); err != nil {
		return 0, nil, err
	}
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Referer", c.base+"/")
	req.Header.Set("Origin", c.base)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("x-auth-token", token)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		metrics.ExternalAPIRequest(provider, endpoint, "error")
		return 0, nil, fmt.Errorf("hltb %s: %w", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	metrics.ExternalAPIRequest(provider, endpoint, strconv.Itoa(resp.StatusCode/100)+"xx")
	return resp.StatusCode, b, err
}

// ensureToken pede um token novo se não houver ou se venceu (chamar com c.mu travado).
func (c *Client) ensureToken(ctx context.Context, force bool) error {
	if !force && c.token != "" && c.now().Sub(c.tokenAt) < tokenTTL {
		return nil
	}
	path := fmt.Sprintf("%s/init?t=%d", c.searchPath, c.now().UnixMilli())
	st, b, err := c.do(ctx, "init", http.MethodGet, path, nil, "")
	if err != nil {
		return err
	}
	if st == http.StatusNotFound {
		return errRouteGone
	}
	if st != http.StatusOK {
		return fmt.Errorf("hltb init: status %d", st)
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(b, &out); err != nil || out.Token == "" {
		return fmt.Errorf("hltb init: resposta sem token")
	}
	c.token, c.tokenAt = out.Token, c.now()
	return nil
}

var errRouteGone = errors.New("hltb: rota de busca mudou")

// Os bundles do Next.js do site; a rota é o que vem antes de "/init" num fetch(`...`).
var (
	chunkRe = regexp.MustCompile(`/_next/static/chunks/[A-Za-z0-9_\-./]+\.js`)
	routeRe = regexp.MustCompile("fetch\\(`(/api/[A-Za-z0-9_/\\-]+)/init")
)

// discover redescobre a rota de busca lendo o JS do site (chamar com c.mu travado).
func (c *Client) discover(ctx context.Context) error {
	if !c.lastDiscoverAt.IsZero() && c.now().Sub(c.lastDiscoverAt) < rediscoverEvery {
		return errRouteGone
	}
	c.lastDiscoverAt = c.now()
	st, page, err := c.do(ctx, "home", http.MethodGet, "/", nil, "")
	if err != nil {
		return err
	}
	if st != http.StatusOK {
		return fmt.Errorf("hltb home: status %d", st)
	}
	seen := map[string]bool{}
	for _, chunk := range chunkRe.FindAllString(string(page), -1) {
		if seen[chunk] {
			continue
		}
		seen[chunk] = true
		st, js, err := c.do(ctx, "chunk", http.MethodGet, chunk, nil, "")
		if err != nil || st != http.StatusOK {
			continue
		}
		if m := routeRe.FindSubmatch(js); m != nil {
			path := string(m[1])
			if path != c.searchPath {
				c.logger.Warn("hltb: rota de busca mudou", slog.String("de", c.searchPath), slog.String("para", path))
			}
			c.searchPath, c.token = path, ""
			return nil
		}
	}
	return fmt.Errorf("hltb: rota de busca não encontrada no site")
}

type searchRequest struct {
	SearchType    string        `json:"searchType"`
	SearchTerms   []string      `json:"searchTerms"`
	SearchPage    int           `json:"searchPage"`
	Size          int           `json:"size"`
	SearchOptions searchOptions `json:"searchOptions"`
	UseCache      bool          `json:"useCache"`
}

type searchOptions struct {
	Games struct {
		UserID        int    `json:"userId"`
		Platform      string `json:"platform"`
		SortCategory  string `json:"sortCategory"`
		RangeCategory string `json:"rangeCategory"`
		RangeTime     struct {
			Min int `json:"min"`
			Max int `json:"max"`
		} `json:"rangeTime"`
		Gameplay struct {
			Perspective string `json:"perspective"`
			Flow        string `json:"flow"`
			Genre       string `json:"genre"`
		} `json:"gameplay"`
		Modifier string `json:"modifier"`
	} `json:"games"`
	Filter     string `json:"filter"`
	Sort       int    `json:"sort"`
	Randomizer int    `json:"randomizer"`
}

type searchResponse struct {
	Data []struct {
		GameID       int64  `json:"game_id"`
		GameName     string `json:"game_name"`
		ReleaseWorld int    `json:"release_world"`
		CompMain     int64  `json:"comp_main"`
		CompPlus     int64  `json:"comp_plus"`
		Comp100      int64  `json:"comp_100"`
		CompAllCount int    `json:"comp_all_count"`
	} `json:"data"`
}

func hours(sec int64) float64 {
	if sec <= 0 {
		return 0
	}
	return float64(sec) / 3600
}

// Search busca jogos pelo título (até 10 resultados, mais populares primeiro).
func (c *Client) Search(ctx context.Context, title string) ([]Result, error) {
	terms := strings.Fields(title)
	if len(terms) == 0 {
		return nil, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	req := searchRequest{SearchType: "games", SearchTerms: terms, SearchPage: 1, Size: 10, UseCache: true}
	req.SearchOptions.Games.SortCategory = "popular"
	req.SearchOptions.Games.RangeCategory = "main"
	body, _ := json.Marshal(req)

	retriedToken, rediscovered := false, false
	for {
		err := c.ensureToken(ctx, false)
		if errors.Is(err, errRouteGone) && !rediscovered {
			rediscovered = true
			if derr := c.discover(ctx); derr != nil {
				return nil, derr
			}
			continue
		}
		if err != nil {
			return nil, err
		}
		st, b, err := c.do(ctx, "search", http.MethodPost, c.searchPath, body, c.token)
		if err != nil {
			return nil, err
		}
		switch {
		case st == http.StatusOK:
			var out searchResponse
			if err := json.Unmarshal(b, &out); err != nil {
				return nil, fmt.Errorf("hltb search: resposta inválida: %w", err)
			}
			res := make([]Result, 0, len(out.Data))
			for _, d := range out.Data {
				res = append(res, Result{
					ID: d.GameID, Name: d.GameName, Year: d.ReleaseWorld, MainH: hours(d.CompMain),
					ExtraH: hours(d.CompPlus), CompletionistH: hours(d.Comp100), SubmissionCount: d.CompAllCount,
				})
			}
			return res, nil
		case st == http.StatusTooManyRequests:
			return nil, ErrRateLimited
		case st == http.StatusForbidden && !retriedToken:
			retriedToken = true
			if err := c.ensureToken(ctx, true); err != nil && !errors.Is(err, errRouteGone) {
				return nil, err
			}
		case st == http.StatusNotFound && !rediscovered:
			rediscovered = true
			if err := c.discover(ctx); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("hltb search: status %d", st)
		}
	}
}

// BestMatch escolhe o resultado do mesmo jogo: título normalizado igual (preferindo o mais popular,
// que vem primeiro); sem igual exato, nenhum — melhor ficar sem estimativa que mostrar a de outro jogo.
func BestMatch(normalize func(string) string, title string, results []Result) (Result, bool) {
	want := normalize(title)
	if want == "" {
		return Result{}, false
	}
	for _, r := range results {
		if normalize(r.Name) == want && r.CompletionistH > 0 {
			return r, true
		}
	}
	for _, r := range results {
		if normalize(r.Name) == want {
			return r, true
		}
	}
	return Result{}, false
}

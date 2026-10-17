// Package steam é um cliente da Steam Web API focado em biblioteca e conquistas.
//
// Robustez:
//   - Rate limit por token bucket (STEAM_RPS) — Do() espera o bucket antes de cada request.
//   - Retry em 429/5xx com backoff exponencial + jitter, respeitando o context.
//   - A API key vai só na query string; NUNCA é logada e é removida de qualquer mensagem de erro
//     (ver scrubKey). URLs completas nunca são logadas.
//   - Erros de domínio são tipados (perfil privado, sem stats, chave inválida, vanity não achada).
package steam

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/metrics"
)

// Erros de domínio tipados.
var (
	// ErrProfilePrivate indica perfil/visibilidade de conquistas não-pública.
	ErrProfilePrivate = errors.New("steam: profile is not public")
	// ErrNoStats indica que o app não possui estatísticas/conquistas para o jogador.
	ErrNoStats = errors.New("steam: requested app has no stats")
	// ErrInvalidKey indica API key inválida/sem permissão (401/403).
	ErrInvalidKey = errors.New("steam: invalid api key")
	// ErrVanityNotFound indica que a vanity URL não resolveu para um SteamID.
	ErrVanityNotFound = errors.New("steam: vanity url not found")
)

const (
	defaultTimeout = 20 * time.Second
	maxRetries     = 4
	provider       = "steam"
)

// Client é o cliente HTTP da Steam Web API. Seguro para uso concorrente.
type Client struct {
	httpClient *http.Client
	baseURL    string
	apiKey     string
	lang       string
	limiter    *rate.Limiter
	logger     *slog.Logger
}

// Options configura o cliente.
type Options struct {
	BaseURL    string
	APIKey     string
	Lang       string
	RPS        float64
	HTTPClient *http.Client
	Logger     *slog.Logger
}

// New constrói o cliente.
func New(opts Options) *Client {
	hc := opts.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: defaultTimeout}
	}
	rps := opts.RPS
	if rps <= 0 {
		rps = 1
	}
	lang := opts.Lang
	if lang == "" {
		lang = "brazilian"
	}
	base := strings.TrimRight(opts.BaseURL, "/")
	if base == "" {
		base = "https://api.steampowered.com"
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Client{
		httpClient: hc,
		baseURL:    base,
		apiKey:     opts.APIKey,
		lang:       lang,
		// burst = teto de 1 para serializar; rps define a taxa sustentada.
		limiter: rate.NewLimiter(rate.Limit(rps), 1),
		logger:  logger,
	}
}

// doJSON executa um GET com rate limit + retry e decodifica o corpo em out.
// endpoint é o rótulo de métrica/log (nunca a URL completa, que carrega a key).
func (c *Client) doJSON(ctx context.Context, endpoint, path string, q url.Values, out any) error {
	if err := c.limiter.Wait(ctx); err != nil {
		return fmt.Errorf("rate limit wait: %w", err)
	}
	full := c.baseURL + path + "?" + q.Encode()

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			if err := sleepBackoff(ctx, attempt); err != nil {
				return err
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
		if err != nil {
			return fmt.Errorf("montar request %s: %w", endpoint, scrubKeyErr(err, c.apiKey))
		}
		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("%s: %w", endpoint, scrubKeyErr(err, c.apiKey))
			metrics.ExternalAPIRequest(provider, endpoint, "error")
			continue // erro de transporte é retentável
		}

		statusLabel := statusClass(resp.StatusCode)
		metrics.ExternalAPIRequest(provider, endpoint, statusLabel)

		switch {
		case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
			_ = resp.Body.Close()
			return ErrInvalidKey
		case resp.StatusCode == http.StatusTooManyRequests, resp.StatusCode >= 500:
			_ = resp.Body.Close()
			lastErr = fmt.Errorf("%s: status %d", endpoint, resp.StatusCode)
			continue // retentável
		case resp.StatusCode != http.StatusOK:
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			_ = resp.Body.Close()
			return fmt.Errorf("%s: status %d: %s", endpoint, resp.StatusCode, scrubKey(string(body), c.apiKey))
		}

		dec := json.NewDecoder(resp.Body)
		err = dec.Decode(out)
		_ = resp.Body.Close()
		if err != nil {
			return fmt.Errorf("%s: decodificar resposta: %w", endpoint, err)
		}
		return nil
	}
	return fmt.Errorf("%s: esgotadas %d tentativas: %w", endpoint, maxRetries, lastErr)
}

func statusClass(code int) string {
	switch {
	case code == http.StatusTooManyRequests:
		return "429"
	case code >= 500:
		return "5xx"
	case code >= 400:
		return "4xx"
	case code >= 200 && code < 300:
		return "2xx"
	default:
		return fmt.Sprintf("%dxx", code/100)
	}
}

// scrubKey remove a API key de uma string (defesa contra vazamento em logs/erros).
func scrubKey(s, key string) string {
	if key == "" {
		return s
	}
	return strings.ReplaceAll(s, key, "[REDACTED]")
}

func scrubKeyErr(err error, key string) error {
	if err == nil {
		return nil
	}
	return errors.New(scrubKey(err.Error(), key))
}

// Package steam é um cliente da Steam Web API focado em biblioteca e conquistas.
//
// Robustez:
//   - Rate limit por token bucket (STEAM_RPS) — Do() espera o bucket DENTRO do loop de retry, para
//     cada tentativa respeitar a taxa (inclusive as re-tentativas).
//   - Retry em 429/5xx com backoff exponencial + jitter, respeitando o context e o header
//     Retry-After (429).
//   - 400/401/403 têm o corpo lido (limitado) e classificado em erros de domínio (perfil privado,
//     sem stats) ANTES de cair em ErrInvalidKey — que só vale p/ 401/403 sem essas mensagens.
//   - A API key vai só na query string; NUNCA é logada e é removida de qualquer mensagem de erro
//     (ver scrubKey). URLs completas nunca são logadas. Corpos são lidos com io.LimitReader.
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
	"strconv"
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
	// ErrInvalidKey indica API key inválida/sem permissão (401/403 sem mensagem de domínio).
	ErrInvalidKey = errors.New("steam: invalid api key")
	// ErrVanityNotFound indica que a vanity URL não resolveu para um SteamID.
	ErrVanityNotFound = errors.New("steam: vanity url not found")
)

const (
	defaultTimeout = 20 * time.Second
	maxRetries     = 4
	provider       = "steam"
	// maxBodyPeek limita a leitura do corpo em respostas de erro (classificação).
	maxBodyPeek = 4 << 10 // 4 KiB
	// maxRetryAfter limita quanto respeitamos o Retry-After (evita bloquear o worker por minutos).
	maxRetryAfter = 30 * time.Second
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
	full := c.baseURL + path + "?" + q.Encode()

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		// Rate limit DENTRO do loop: cada tentativa (inclusive retries) consome um token.
		if err := c.limiter.Wait(ctx); err != nil {
			return fmt.Errorf("%s: rate limit wait: %w", endpoint, err)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
		if err != nil {
			return fmt.Errorf("montar request %s: %w", endpoint, scrubKeyErr(err, c.apiKey))
		}
		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("%s: %w", endpoint, scrubKeyErr(err, c.apiKey))
			metrics.ExternalAPIRequest(provider, endpoint, "error")
			if werr := c.backoffOrStop(ctx, attempt, 0); werr != nil {
				return werr
			}
			continue // erro de transporte é retentável
		}

		statusLabel := statusClass(resp.StatusCode)
		metrics.ExternalAPIRequest(provider, endpoint, statusLabel)

		switch {
		case resp.StatusCode == http.StatusOK:
			dec := json.NewDecoder(resp.Body)
			derr := dec.Decode(out)
			_ = resp.Body.Close()
			if derr != nil {
				return fmt.Errorf("%s: decodificar resposta: %w", endpoint, derr)
			}
			return nil

		case resp.StatusCode == http.StatusTooManyRequests:
			wait := retryAfter(resp.Header)
			_ = resp.Body.Close()
			lastErr = fmt.Errorf("%s: status 429", endpoint)
			if werr := c.backoffOrStop(ctx, attempt, wait); werr != nil {
				return werr
			}
			continue

		case resp.StatusCode >= 500:
			_ = resp.Body.Close()
			lastErr = fmt.Errorf("%s: status %d", endpoint, resp.StatusCode)
			if werr := c.backoffOrStop(ctx, attempt, 0); werr != nil {
				return werr
			}
			continue

		case resp.StatusCode == http.StatusBadRequest,
			resp.StatusCode == http.StatusUnauthorized,
			resp.StatusCode == http.StatusForbidden:
			// Classifica pelo corpo ANTES de assumir chave inválida.
			body := peekBody(resp.Body)
			_ = resp.Body.Close()
			if cls := classifyErrorBody(body); cls != nil {
				return cls
			}
			if resp.StatusCode == http.StatusBadRequest {
				return fmt.Errorf("%s: status 400: %s", endpoint, scrubKey(body, c.apiKey))
			}
			return ErrInvalidKey // 401/403 sem mensagem de domínio reconhecida

		default:
			body := peekBody(resp.Body)
			_ = resp.Body.Close()
			return fmt.Errorf("%s: status %d: %s", endpoint, resp.StatusCode, scrubKey(body, c.apiKey))
		}
	}
	return fmt.Errorf("%s: esgotadas %d tentativas: %w", endpoint, maxRetries, lastErr)
}

// backoffOrStop dorme antes da próxima tentativa (respeitando Retry-After quando >0), ou devolve
// nil sem dormir se esta já foi a última tentativa (o loop sairá e retornará lastErr).
func (c *Client) backoffOrStop(ctx context.Context, attempt int, retryAfterDur time.Duration) error {
	if attempt >= maxRetries {
		return nil // última tentativa: não adianta dormir
	}
	if retryAfterDur > 0 {
		if retryAfterDur > maxRetryAfter {
			retryAfterDur = maxRetryAfter
		}
		t := time.NewTimer(retryAfterDur)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			return nil
		}
	}
	return sleepBackoff(ctx, attempt+1)
}

// retryAfter extrai a duração do header Retry-After (segundos ou data HTTP). 0 se ausente/inválido.
func retryAfter(h http.Header) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// peekBody lê no máximo maxBodyPeek bytes do corpo (para classificação de erro).
func peekBody(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, maxBodyPeek))
	return string(b)
}

// classifyErrorBody devolve o erro de domínio quando o corpo traz uma mensagem conhecida.
func classifyErrorBody(body string) error {
	m := strings.ToLower(body)
	switch {
	case strings.Contains(m, "profile is not public"), strings.Contains(m, "not public"):
		return ErrProfilePrivate
	case strings.Contains(m, "requested app has no stats"), strings.Contains(m, "no stats"):
		return ErrNoStats
	default:
		return nil
	}
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

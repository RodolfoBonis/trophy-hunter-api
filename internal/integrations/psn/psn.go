// Package psn é um cliente da API (não oficial) de troféus da PlayStation Network — a mesma
// superfície da lib comunitária psn-api (JS), portada para Go.
//
// Autenticação (sem reusar o NPSSO a cada chamada):
//  1. NPSSO (cookie do login em playstation.com) → código de autorização (authorize, redirect 302);
//  2. código → access + refresh tokens (token, grant authorization_code);
//  3. renovação só pelo refresh token (grant refresh_token). Refresh inválido/expirado →
//     ErrInvalidGrant: o usuário precisa colar um NPSSO novo (status "reconectar").
//
// Robustez: rate limit por token bucket (a PSN limita ~300 req/15 min), retry em 429/5xx com
// backoff exponencial + jitter respeitando Retry-After, corpos lidos com io.LimitReader. Tokens e
// NPSSO nunca são logados nem aparecem em mensagens de erro.
package psn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/time/rate"

	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/metrics"
)

// Erros de domínio.
var (
	// ErrInvalidNPSSO indica NPSSO inválido/expirado (o authorize não devolveu código).
	ErrInvalidNPSSO = errors.New("psn: npsso inválido ou expirado")
	// ErrInvalidGrant indica refresh token inválido/expirado — é preciso um NPSSO novo.
	ErrInvalidGrant = errors.New("psn: refresh token inválido ou expirado")
	// ErrUnauthorized indica access token recusado pela API (401).
	ErrUnauthorized = errors.New("psn: access token recusado")
	// ErrNotFound indica recurso inexistente (ex.: título sem troféus para a conta).
	ErrNotFound = errors.New("psn: não encontrado")
)

// Constantes do app oficial (as mesmas da psn-api).
const (
	clientID    = "09515159-7237-4370-9b40-3806e67c0891"
	basicAuth   = "Basic MDk1MTUxNTktNzIzNy00MzcwLTliNDAtMzgwNmU2N2MwODkxOnVjUGprYTV0bnRCMktxc1A="
	redirectURI = "com.scee.psxandroid.scecompcall://redirect"
	scope       = "psn:mobile.v2.core psn:clientapp"

	defaultAuthBase = "https://ca.account.sony.com/api/authz/v3/oauth"
	defaultAPIBase  = "https://m.np.playstation.com/api"

	provider       = "psn"
	defaultTimeout = 20 * time.Second
	maxRetries     = 4
	maxBodyPeek    = 4 << 10
	maxBody        = 8 << 20
	maxRetryAfter  = 60 * time.Second
	backoffBase    = time.Second
	backoffMax     = 20 * time.Second
	pageSize       = 800
)

// Tokens é o resultado da troca/renovação.
type Tokens struct {
	AccessToken      string
	AccessExpiresAt  time.Time
	RefreshToken     string
	RefreshExpiresAt time.Time
}

// LogValue nunca expõe os tokens em logs.
func (t Tokens) LogValue() slog.Value { return slog.StringValue("[REDACTED]") }

// Options configura o cliente.
type Options struct {
	AuthBaseURL string
	APIBaseURL  string
	// Lang é o Accept-Language (nomes/descrições localizados), ex.: "pt-BR".
	Lang string
	// RPS é a taxa sustentada; Burst o pico.
	RPS        float64
	Burst      int
	HTTPClient *http.Client
	Logger     *slog.Logger
}

// Client é seguro para uso concorrente; use UMA instância por processo (o rate limit é dela).
type Client struct {
	http     *http.Client
	noFollow *http.Client
	authBase string
	apiBase  string
	lang     string
	limiter  *rate.Limiter
	logger   *slog.Logger
}

// New constrói o cliente.
func New(o Options) *Client {
	hc := o.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: defaultTimeout}
	}
	nf := *hc
	nf.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	rps := o.RPS
	if rps <= 0 {
		rps = 0.5
	}
	burst := o.Burst
	if burst <= 0 {
		burst = 5
	}
	lang := o.Lang
	if lang == "" {
		lang = "pt-BR"
	}
	logger := o.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Client{
		http: hc, noFollow: &nf,
		authBase: strings.TrimRight(orDefault(o.AuthBaseURL, defaultAuthBase), "/"),
		apiBase:  strings.TrimRight(orDefault(o.APIBaseURL, defaultAPIBase), "/"),
		lang:     lang,
		limiter:  rate.NewLimiter(rate.Limit(rps), burst),
		logger:   logger,
	}
}

// --- autenticação ---------------------------------------------------------------------------

// ExchangeNPSSO troca o NPSSO por tokens (authorize → code → token). O NPSSO não é guardado.
func (c *Client) ExchangeNPSSO(ctx context.Context, npsso string) (Tokens, error) {
	npsso = strings.TrimSpace(npsso)
	if npsso == "" {
		return Tokens{}, ErrInvalidNPSSO
	}
	q := url.Values{}
	q.Set("access_type", "offline")
	q.Set("client_id", clientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("response_type", "code")
	q.Set("scope", scope)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.authBase+"/authorize?"+q.Encode(), nil)
	if err != nil {
		return Tokens{}, err
	}
	req.Header.Set("Cookie", "npsso="+npsso)
	if err := c.limiter.Wait(ctx); err != nil {
		return Tokens{}, err
	}
	resp, err := c.noFollow.Do(req)
	if err != nil {
		metrics.ExternalAPIRequest(provider, "authorize", "error")
		return Tokens{}, fmt.Errorf("psn authorize: %w", errors.New(redact(err.Error(), npsso)))
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyPeek))
	metrics.ExternalAPIRequest(provider, "authorize", strconv.Itoa(resp.StatusCode))

	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || loc.Query().Get("code") == "" {
		return Tokens{}, ErrInvalidNPSSO
	}
	form := url.Values{}
	form.Set("code", loc.Query().Get("code"))
	form.Set("redirect_uri", redirectURI)
	form.Set("grant_type", "authorization_code")
	form.Set("token_format", "jwt")
	return c.token(ctx, "token_code", form, ErrInvalidNPSSO)
}

// Refresh renova os tokens pelo refresh token.
func (c *Client) Refresh(ctx context.Context, refreshToken string) (Tokens, error) {
	form := url.Values{}
	form.Set("refresh_token", refreshToken)
	form.Set("grant_type", "refresh_token")
	form.Set("token_format", "jwt")
	form.Set("scope", scope)
	return c.token(ctx, "token_refresh", form, ErrInvalidGrant)
}

type tokenResp struct {
	AccessToken           string `json:"access_token"`
	ExpiresIn             int    `json:"expires_in"`
	RefreshToken          string `json:"refresh_token"`
	RefreshTokenExpiresIn int    `json:"refresh_token_expires_in"`
	Error                 string `json:"error"`
}

func (c *Client) token(ctx context.Context, endpoint string, form url.Values, badGrant error) (Tokens, error) {
	var out tokenResp
	status, err := c.do(ctx, endpoint, http.MethodPost, c.authBase+"/token", strings.NewReader(form.Encode()), map[string]string{
		"Content-Type":  "application/x-www-form-urlencoded",
		"Authorization": basicAuth,
	}, &out)
	if err != nil {
		if status == http.StatusBadRequest || status == http.StatusUnauthorized {
			return Tokens{}, badGrant
		}
		return Tokens{}, err
	}
	if out.AccessToken == "" || out.Error != "" {
		return Tokens{}, badGrant
	}
	now := time.Now()
	return Tokens{
		AccessToken:      out.AccessToken,
		AccessExpiresAt:  now.Add(time.Duration(out.ExpiresIn) * time.Second),
		RefreshToken:     out.RefreshToken,
		RefreshExpiresAt: now.Add(time.Duration(out.RefreshTokenExpiresIn) * time.Second),
	}, nil
}

// --- troféus --------------------------------------------------------------------------------

// TrophyCounts são contagens por tipo.
type TrophyCounts struct {
	Bronze   int `json:"bronze"`
	Silver   int `json:"silver"`
	Gold     int `json:"gold"`
	Platinum int `json:"platinum"`
}

// TrophyTitle é um jogo (lista de títulos com resumo de troféus).
type TrophyTitle struct {
	NpServiceName       string       `json:"npServiceName"`
	NpCommunicationID   string       `json:"npCommunicationId"`
	TrophyTitleName     string       `json:"trophyTitleName"`
	TrophyTitleIconURL  string       `json:"trophyTitleIconUrl"`
	TrophyTitlePlatform string       `json:"trophyTitlePlatform"`
	HasTrophyGroups     bool         `json:"hasTrophyGroups"`
	DefinedTrophies     TrophyCounts `json:"definedTrophies"`
	EarnedTrophies      TrophyCounts `json:"earnedTrophies"`
	Progress            int          `json:"progress"`
	HiddenFlag          bool         `json:"hiddenFlag"`
	LastUpdatedDateTime time.Time    `json:"lastUpdatedDateTime"`
}

// TrophyDef é a definição de um troféu do título.
type TrophyDef struct {
	TrophyID                  int    `json:"trophyId"`
	TrophyHidden              bool   `json:"trophyHidden"`
	TrophyType                string `json:"trophyType"`
	TrophyName                string `json:"trophyName"`
	TrophyDetail              string `json:"trophyDetail"`
	TrophyIconURL             string `json:"trophyIconUrl"`
	TrophyGroupID             string `json:"trophyGroupId"`
	TrophyProgressTargetValue string `json:"trophyProgressTargetValue"`
}

// TrophyGroup é um grupo (jogo base "default" ou DLC "001", "002"…).
type TrophyGroup struct {
	TrophyGroupID      string `json:"trophyGroupId"`
	TrophyGroupName    string `json:"trophyGroupName"`
	TrophyGroupIconURL string `json:"trophyGroupIconUrl"`
}

// UserTrophy é o estado do troféu para o usuário (+ raridade).
type UserTrophy struct {
	TrophyID           int       `json:"trophyId"`
	TrophyHidden       bool      `json:"trophyHidden"`
	Earned             bool      `json:"earned"`
	EarnedDateTime     time.Time `json:"earnedDateTime"`
	TrophyType         string    `json:"trophyType"`
	TrophyEarnedRate   string    `json:"trophyEarnedRate"`
	Progress           string    `json:"progress"`
	ProgressRate       int       `json:"progressRate"`
	ProgressedDateTime string    `json:"progressedDateTime"`
}

// Summary é o resumo de troféus da conta (nível etc.).
type Summary struct {
	AccountID      string       `json:"accountId"`
	TrophyLevel    int          `json:"trophyLevel"`
	Progress       int          `json:"progress"`
	Tier           int          `json:"tier"`
	EarnedTrophies TrophyCounts `json:"earnedTrophies"`
}

// Profile é o perfil público (online ID e avatar).
type Profile struct {
	OnlineID string
	Avatar   string
}

// TrophySummary devolve o resumo da conta do token ("me"); inclui o accountId.
func (c *Client) TrophySummary(ctx context.Context, token string) (Summary, error) {
	var out Summary
	err := c.api(ctx, token, "trophySummary", "/trophy/v1/users/me/trophySummary", nil, &out)
	return out, err
}

// Profile devolve online ID e avatar de uma conta (falha aqui não é fatal para o sync).
func (c *Client) Profile(ctx context.Context, token, accountID string) (Profile, error) {
	var out struct {
		OnlineID string `json:"onlineId"`
		Avatars  []struct {
			Size string `json:"size"`
			URL  string `json:"url"`
		} `json:"avatars"`
	}
	if err := c.api(ctx, token, "profile", "/userProfile/v1/internal/users/"+url.PathEscape(accountID)+"/profiles", nil, &out); err != nil {
		return Profile{}, err
	}
	p := Profile{OnlineID: out.OnlineID}
	for _, a := range out.Avatars {
		if p.Avatar == "" || a.Size == "xl" || a.Size == "l" {
			p.Avatar = a.URL
		}
	}
	return p, nil
}

// TrophyTitles lista todos os títulos com troféus da conta (paginado), do mais recente.
func (c *Client) TrophyTitles(ctx context.Context, token string) ([]TrophyTitle, error) {
	var all []TrophyTitle
	offset := 0
	for {
		q := url.Values{}
		q.Set("limit", strconv.Itoa(pageSize))
		q.Set("offset", strconv.Itoa(offset))
		var page struct {
			TrophyTitles   []TrophyTitle `json:"trophyTitles"`
			TotalItemCount int           `json:"totalItemCount"`
			NextOffset     *int          `json:"nextOffset"`
		}
		if err := c.api(ctx, token, "trophyTitles", "/trophy/v1/users/me/trophyTitles", q, &page); err != nil {
			return nil, err
		}
		all = append(all, page.TrophyTitles...)
		if page.NextOffset == nil || *page.NextOffset <= offset || len(page.TrophyTitles) == 0 {
			return all, nil
		}
		offset = *page.NextOffset
	}
}

// TitleTrophies devolve as definições de todos os troféus (todos os grupos) do título.
func (c *Client) TitleTrophies(ctx context.Context, token, npCommID, service string) ([]TrophyDef, error) {
	var out struct {
		Trophies []TrophyDef `json:"trophies"`
	}
	err := c.api(ctx, token, "titleTrophies", "/trophy/v1/npCommunicationIds/"+url.PathEscape(npCommID)+"/trophyGroups/all/trophies", serviceQuery(service), &out)
	return out.Trophies, err
}

// TitleGroups devolve os grupos (jogo base + DLCs) do título.
func (c *Client) TitleGroups(ctx context.Context, token, npCommID, service string) ([]TrophyGroup, error) {
	var out struct {
		TrophyGroups []TrophyGroup `json:"trophyGroups"`
	}
	err := c.api(ctx, token, "titleGroups", "/trophy/v1/npCommunicationIds/"+url.PathEscape(npCommID)+"/trophyGroups", serviceQuery(service), &out)
	return out.TrophyGroups, err
}

// UserTrophies devolve o estado (obtido, data, raridade, progresso) de todos os troféus do título.
func (c *Client) UserTrophies(ctx context.Context, token, npCommID, service string) ([]UserTrophy, error) {
	var out struct {
		Trophies []UserTrophy `json:"trophies"`
	}
	err := c.api(ctx, token, "userTrophies", "/trophy/v1/users/me/npCommunicationIds/"+url.PathEscape(npCommID)+"/trophyGroups/all/trophies", serviceQuery(service), &out)
	return out.Trophies, err
}

func serviceQuery(service string) url.Values {
	q := url.Values{}
	if service == "" {
		service = "trophy"
	}
	q.Set("npServiceName", service)
	return q
}

// --- transporte -----------------------------------------------------------------------------

func (c *Client) api(ctx context.Context, token, endpoint, path string, q url.Values, out any) error {
	u := c.apiBase + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	status, err := c.do(ctx, endpoint, http.MethodGet, u, nil, map[string]string{
		"Authorization":   "Bearer " + token,
		"Accept-Language": c.lang,
	}, out)
	switch {
	case err == nil:
		return nil
	case status == http.StatusUnauthorized:
		return ErrUnauthorized
	case status == http.StatusNotFound:
		return ErrNotFound
	default:
		return errors.New(redact(err.Error(), token))
	}
}

// do executa a requisição com rate limit + retry (429/5xx/rede) e decodifica JSON em out.
// Devolve o status final (0 em erro de rede) para o chamador classificar 400/401/404.
func (c *Client) do(ctx context.Context, endpoint, method, u string, body io.Reader, headers map[string]string, out any) (int, error) {
	var payload []byte
	if body != nil {
		b, err := io.ReadAll(io.LimitReader(body, maxBody))
		if err != nil {
			return 0, err
		}
		payload = b
	}
	for attempt := 1; ; attempt++ {
		if err := c.limiter.Wait(ctx); err != nil {
			return 0, err
		}
		var rd io.Reader
		if payload != nil {
			rd = strings.NewReader(string(payload))
		}
		req, err := http.NewRequestWithContext(ctx, method, u, rd)
		if err != nil {
			return 0, err
		}
		req.Header.Set("Accept", "application/json")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			metrics.ExternalAPIRequest(provider, endpoint, "error")
			if attempt >= maxRetries || ctx.Err() != nil {
				return 0, fmt.Errorf("psn %s: requisição falhou", endpoint)
			}
			if werr := sleep(ctx, backoff(attempt)); werr != nil {
				return 0, werr
			}
			continue
		}
		metrics.ExternalAPIRequest(provider, endpoint, strconv.Itoa(resp.StatusCode))

		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			ra := retryAfter(resp.Header)
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyPeek))
			_ = resp.Body.Close()
			if attempt >= maxRetries {
				return resp.StatusCode, fmt.Errorf("psn %s: status %d após %d tentativas", endpoint, resp.StatusCode, attempt)
			}
			d := backoff(attempt)
			if ra > 0 {
				d = min(ra, maxRetryAfter)
			}
			c.logger.Warn("psn: retentando", slog.String("endpoint", endpoint), slog.Int("status", resp.StatusCode), slog.Duration("em", d))
			if werr := sleep(ctx, d); werr != nil {
				return resp.StatusCode, werr
			}
			continue
		}
		if resp.StatusCode >= 400 {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyPeek))
			_ = resp.Body.Close()
			return resp.StatusCode, fmt.Errorf("psn %s: status %d", endpoint, resp.StatusCode)
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(out)
		_ = resp.Body.Close()
		if err != nil {
			return resp.StatusCode, fmt.Errorf("psn %s: resposta inválida: %w", endpoint, err)
		}
		return resp.StatusCode, nil
	}
}

func backoff(attempt int) time.Duration {
	d := float64(backoffBase) * math.Pow(2, float64(attempt-1))
	d = math.Min(d, float64(backoffMax))
	d *= 1 + 0.2*(2*rand.Float64()-1) //nolint:gosec // jitter não-criptográfico
	return time.Duration(d)
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func retryAfter(h http.Header) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if s, err := strconv.Atoi(v); err == nil && s > 0 {
		return time.Duration(s) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		return time.Until(t)
	}
	return 0
}

// redact remove um segredo (token/NPSSO) de uma mensagem.
func redact(s, secret string) string {
	if secret == "" {
		return s
	}
	return strings.ReplaceAll(s, secret, "[REDACTED]")
}

func orDefault(v, d string) string {
	if strings.TrimSpace(v) == "" {
		return d
	}
	return v
}

// ParseRate converte o trophyEarnedRate ("12.3") em %; ok=false se vazio/inválido.
func ParseRate(s string) (float64, bool) {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || math.IsNaN(v) {
		return 0, false
	}
	return v, true
}

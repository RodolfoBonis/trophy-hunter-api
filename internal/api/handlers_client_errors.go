package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/httperr"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/metrics"
)

// Erros do navegador (POST /client-errors): sem Sentry, o app manda os erros acionáveis para cá —
// viram log estruturado `client_error` e a métrica client_errors_total (alerta no Telegram). Aberto
// (erro de login também conta), por isso: corpo pequeno, campos truncados, segredos mascarados e
// limite por origem.

const (
	clientErrorMaxBody    = 8 << 10
	clientErrorMaxMessage = 500
	clientErrorMaxStack   = 4 << 10
	clientErrorPerMinute  = 30
)

var clientErrorKinds = map[string]bool{"render": true, "unhandled": true, "api_contract": true, "network": true, "other": true}

// Nada de token/código OIDC nos logs, mesmo que venha numa mensagem ou URL.
var clientErrorSecrets = regexp.MustCompile(`(?i)(bearer\s+[a-z0-9._~+/=-]+|(access_token|refresh_token|id_token|code|state|session_state|npsso)=[^&\s"']+|eyJ[a-zA-Z0-9_-]{10,}\.[a-zA-Z0-9_-]+\.[a-zA-Z0-9_-]+)`)

type clientErrorRequest struct {
	Kind      string `json:"kind"`
	Message   string `json:"message"`
	Stack     string `json:"stack"`
	Route     string `json:"route"`
	Release   string `json:"release"`
	UserAgent string `json:"user_agent"`
}

// clientErrorLimiter: janela fixa de 1 min por origem (IP) — suficiente para um punhado de usuários.
type clientErrorLimiter struct {
	mu      sync.Mutex
	window  time.Time
	counts  map[string]int
	perMin  int
	nowFunc func() time.Time
}

func newClientErrorLimiter(perMin int) *clientErrorLimiter {
	return &clientErrorLimiter{counts: map[string]int{}, perMin: perMin, nowFunc: time.Now}
}

func (l *clientErrorLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.nowFunc().Truncate(time.Minute)
	if !now.Equal(l.window) {
		l.window, l.counts = now, map[string]int{}
	}
	l.counts[key]++
	return l.counts[key] <= l.perMin
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return strings.TrimSpace(strings.Split(xff, ",")[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func clip(s string, n int) string {
	s = clientErrorSecrets.ReplaceAllString(s, "[redacted]")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func (h *Handlers) reportClientError(w http.ResponseWriter, r *http.Request) {
	if !h.clientErrors.allow(clientIP(r)) {
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, clientErrorMaxBody+1))
	if err != nil || len(body) > clientErrorMaxBody {
		httperr.Write(w, r, httperr.New(httperr.KindBadRequest, "Bad request", "corpo inválido ou grande demais"))
		return
	}
	var req clientErrorRequest
	if err := json.Unmarshal(body, &req); err != nil || strings.TrimSpace(req.Message) == "" {
		httperr.Write(w, r, httperr.New(httperr.KindBadRequest, "Bad request", "informe ao menos message"))
		return
	}
	if !clientErrorKinds[req.Kind] {
		req.Kind = "other"
	}
	release := clip(strings.TrimSpace(req.Release), 32)
	if release == "" {
		release = "unknown"
	}
	route, _, _ := strings.Cut(req.Route, "?")
	metrics.ClientError(req.Kind, release)
	h.logger.Warn("client_error",
		slog.String("kind", req.Kind),
		slog.String("message", clip(req.Message, clientErrorMaxMessage)),
		slog.String("route", clip(route, 200)),
		slog.String("release", release),
		slog.String("user_agent", clip(req.UserAgent, 200)),
		slog.String("stack", clip(req.Stack, clientErrorMaxStack)),
	)
	w.WriteHeader(http.StatusNoContent)
}

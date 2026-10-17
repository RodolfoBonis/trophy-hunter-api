// Package middleware reúne os middlewares HTTP transversais da borda da API: request id, recover,
// cabeçalhos de segurança, tracing OTel, log estruturado e timeout de request.
package middleware

import (
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/httperr"
)

// RequestID injeta um id único por request (header X-Request-Id + contexto).
var RequestID = middleware.RequestID

// Recoverer converte panics em 500 (problem+json) sem derrubar o processo e loga o stack.
func Recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rec := recover()
			if rec == nil {
				return
			}
			if rec == http.ErrAbortHandler { //nolint:errorlint // sentinel de panic, não é error wrap
				panic(rec)
			}
			slog.ErrorContext(r.Context(), "panic recuperado",
				slog.Any("panic", rec),
				slog.String("request_id", middleware.GetReqID(r.Context())),
				slog.String("stack", string(debug.Stack())),
			)
			httperr.Write(w, r, httperr.New(httperr.KindInternal, "Internal error", ""))
		}()
		next.ServeHTTP(w, r)
	})
}

// Timeout limita a duração de um request (cancela o contexto).
func Timeout(d time.Duration) func(http.Handler) http.Handler {
	return middleware.Timeout(d)
}

// Tracing instrumenta cada request com um span OTel (no-op se tracing desligado).
func Tracing(service string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return otelhttp.NewHandler(next, service)
	}
}

// SecurityHeaders aplica cabeçalhos de segurança básicos (OWASP).
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		next.ServeHTTP(w, r)
	})
}

// RequestLogger registra cada request de forma estruturada (sem segredos/PII).
func RequestLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			start := time.Now()
			defer func() {
				logger.LogAttrs(r.Context(), slog.LevelInfo, "http_request",
					slog.String("method", r.Method),
					slog.String("path", r.URL.Path),
					slog.Int("status", ww.Status()),
					slog.Int("bytes", ww.BytesWritten()),
					slog.Duration("duration", time.Since(start)),
					slog.String("request_id", middleware.GetReqID(r.Context())),
				)
			}()
			next.ServeHTTP(ww, r)
		})
	}
}

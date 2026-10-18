package metrics

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func scrape(t *testing.T) string {
	t.Helper()
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	b, _ := io.ReadAll(rec.Body)
	return string(b)
}

func TestSetStateReplacesSnapshot(t *testing.T) {
	SetState(
		[]JobQueueCount{{Kind: "sync_game", Status: "queued", N: 7}},
		[]AccountState{{Platform: "psn", Account: "AlnWolf", Status: "needs_reconnect", LastSynced: time.Unix(1700000000, 0)}},
	)
	out := scrape(t)
	require.Contains(t, out, `jobs_queue{kind="sync_game",status="queued"} 7`)
	require.Contains(t, out, `integration_account_status{account="AlnWolf",platform="psn",status="needs_reconnect"} 1`)
	require.Contains(t, out, `integration_account_status{account="AlnWolf",platform="psn",status="connected"} 0`)
	require.Contains(t, out, `integration_last_synced_timestamp_seconds{account="AlnWolf",platform="psn"} 1.7e+09`)

	// Novo retrato: fila vazia e conta removida somem.
	SetState(nil, []AccountState{{Platform: "steam", Account: "Fenrir.DD", Status: "connected"}})
	out = scrape(t)
	require.NotContains(t, out, `kind="sync_game"`)
	require.NotContains(t, out, `AlnWolf`)
	require.Contains(t, out, `integration_last_synced_timestamp_seconds{account="Fenrir.DD",platform="steam"} 0`)
}

func TestObserveHTTP(t *testing.T) {
	ObserveHTTP("GET", "/api/v1/games/{id}", 503, 30*time.Millisecond, true)
	ObserveHTTP("GET", "/api/v1/ai/streams/{id}", 200, 2*time.Minute, false)
	out := scrape(t)
	require.Contains(t, out, `http_requests_total{code="5xx",method="GET",route="/api/v1/games/{id}"} 1`)
	require.True(t, strings.Contains(out, `http_request_duration_seconds_count{method="GET",route="/api/v1/games/{id}"} 1`))
	require.NotContains(t, out, `http_request_duration_seconds_count{method="GET",route="/api/v1/ai/streams/{id}"}`, "SSE fora da latência")
}

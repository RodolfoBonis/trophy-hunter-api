package api

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestClipRedactsSecrets(t *testing.T) {
	got := clip("Bearer eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiIxIn0.sig ?code=abc&state=xyz npsso=123 ok", 500)
	require.NotContains(t, got, "abc")
	require.NotContains(t, got, "xyz")
	require.NotContains(t, got, "123")
	require.NotContains(t, got, "eyJ")
	require.Contains(t, got, "ok")
	require.Equal(t, "abc…", clip("abcdef", 3))
}

func TestClientErrorLimiterWindow(t *testing.T) {
	now := time.Date(2026, 10, 19, 1, 0, 10, 0, time.UTC)
	l := newClientErrorLimiter(2)
	l.nowFunc = func() time.Time { return now }
	require.True(t, l.allow("a"))
	require.True(t, l.allow("a"))
	require.False(t, l.allow("a"))
	require.True(t, l.allow("b"), "cada origem tem a própria cota")
	now = now.Add(time.Minute)
	require.True(t, l.allow("a"), "nova janela")
}

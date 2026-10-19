package api_test

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestClientErrorsOpenRedactedAndLimited(t *testing.T) {
	e := build(t, nil, nil, nil)

	// Aberto (sem token) e mascara segredos.
	res := e.do(t, http.MethodPost, "/api/v1/client-errors",
		`{"kind":"render","message":"falhou com Bearer abc.def.ghi e ?code=SEGREDO","route":"/games/1?code=x","release":"0.13.2"}`, false)
	_ = res.Body.Close()
	require.Equal(t, http.StatusNoContent, res.StatusCode)

	// Sem message → 400; tipo desconhecido vira "other".
	res = e.do(t, http.MethodPost, "/api/v1/client-errors", `{"kind":"render"}`, false)
	_ = res.Body.Close()
	require.Equal(t, http.StatusBadRequest, res.StatusCode)

	// Corpo grande demais → 400.
	res = e.do(t, http.MethodPost, "/api/v1/client-errors", `{"message":"`+strings.Repeat("x", 9000)+`"}`, false)
	_ = res.Body.Close()
	require.Equal(t, http.StatusBadRequest, res.StatusCode)

	// Métrica exposta.
	m := e.do(t, http.MethodGet, "/metrics", "", false)
	body, _ := io.ReadAll(m.Body)
	_ = m.Body.Close()
	require.Contains(t, string(body), `client_errors_total{kind="render",release="0.13.2"} 1`)

	// Limite por origem: 30/min (2 já usados acima contam só os aceitos e recusados pelo limiter).
	got429 := false
	for i := 0; i < 40; i++ {
		r := e.do(t, http.MethodPost, "/api/v1/client-errors", `{"kind":"other","message":"x"}`, false)
		_ = r.Body.Close()
		if r.StatusCode == http.StatusTooManyRequests {
			got429 = true
			break
		}
	}
	require.True(t, got429)
}

package api_test

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestClientVitals(t *testing.T) {
	e := setup(t)
	post := func(body string) int {
		req, err := http.NewRequest(http.MethodPost, e.srv.URL+"/api/v1/client-vitals", strings.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "text/plain;charset=UTF-8") // como o sendBeacon manda
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	require.Equal(t, http.StatusNoContent, post(`{"device":"mobile","items":[{"name":"LCP","value":1830,"route":"home"},{"name":"CLS","value":0.02,"route":"home"},{"name":"INP","value":120,"route":"rota-inventada"}]}`))
	require.Equal(t, http.StatusBadRequest, post(`{"items":[]}`))
	require.Equal(t, http.StatusBadRequest, post(`nada`))

	resp, err := http.Get(e.srv.URL + "/metrics")
	require.NoError(t, err)
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	m := string(b)
	require.Contains(t, m, `web_vitals_seconds_count{device="mobile",name="LCP",route="home"} 1`)
	require.Contains(t, m, `web_vitals_seconds_bucket{device="mobile",name="LCP",route="home",le="2"} 1`)
	require.Contains(t, m, `web_vitals_cls_count{device="mobile",route="home"} 1`)
	require.Contains(t, m, `web_vitals_seconds_count{device="mobile",name="INP",route="other"} 1`, "rota fora da lista vira other")
}

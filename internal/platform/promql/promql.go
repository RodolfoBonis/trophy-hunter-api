// Package promql faz consultas instantâneas no Prometheus (HTTP API) — o painel do dono mostra
// alertas, erros do front e saúde das fontes externas sem precisar abrir o Grafana.
package promql

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ErrNotConfigured indica que não há PROMETHEUS_URL.
var ErrNotConfigured = errors.New("promql: sem PROMETHEUS_URL")

// Sample é um ponto de um vetor instantâneo.
type Sample struct {
	Labels map[string]string
	Value  float64
}

// Client consulta um Prometheus.
type Client struct {
	base string
	hc   *http.Client
}

// New monta o cliente; base vazia = desligado (ErrNotConfigured).
func New(base string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 4 * time.Second}
	}
	return &Client{base: strings.TrimRight(base, "/"), hc: hc}
}

// Query roda uma consulta instantânea e devolve o vetor (NaN/Inf viram 0 e são descartados).
func (c *Client) Query(ctx context.Context, q string) ([]Sample, error) {
	if c.base == "" {
		return nil, ErrNotConfigured
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/v1/query?"+url.Values{"query": {q}}.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("promql: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var body struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string `json:"metric"`
				Value  [2]any            `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("promql: status %d, resposta inválida", resp.StatusCode)
	}
	if body.Status != "success" {
		return nil, fmt.Errorf("promql: %s", body.Error)
	}
	out := make([]Sample, 0, len(body.Data.Result))
	for _, r := range body.Data.Result {
		s, _ := r.Value[1].(string)
		v, err := strconv.ParseFloat(s, 64)
		if err != nil || v != v || v > 1e300 || v < -1e300 { // NaN/Inf: sem dado
			continue
		}
		out = append(out, Sample{Labels: r.Metric, Value: v})
	}
	return out, nil
}

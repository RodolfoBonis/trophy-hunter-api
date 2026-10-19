package api

import (
	"encoding/json"
	"io"
	"net/http"
	"slices"

	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/metrics"
)

// Web Vitals dos usuários de verdade (POST /client-vitals): o app manda um lote quando a aba é
// escondida (sendBeacon, corpo text/plain — sem preflight de CORS). Aberto como o /client-errors e
// com o mesmo limite por origem. Só vira métrica (histograma por tela e aparelho); nada é logado.

const (
	clientVitalsMaxBody  = 4 << 10
	clientVitalsMaxItems = 10
)

type clientVitalsRequest struct {
	Device string `json:"device"`
	Items  []struct {
		Name  string  `json:"name"`
		Value float64 `json:"value"`
		Route string  `json:"route"`
	} `json:"items"`
}

func (h *Handlers) reportClientVitals(w http.ResponseWriter, r *http.Request) {
	if !h.clientErrors.allow("vitals:" + clientIP(r)) {
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, clientVitalsMaxBody+1))
	if err != nil || len(body) > clientVitalsMaxBody {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	var req clientVitalsRequest
	if err := json.Unmarshal(body, &req); err != nil || len(req.Items) == 0 || len(req.Items) > clientVitalsMaxItems {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	device := req.Device
	if device != "mobile" {
		device = "desktop"
	}
	for _, it := range req.Items {
		route := it.Route
		if !slices.Contains(metrics.VitalRoutes, route) {
			route = "other"
		}
		metrics.ObserveWebVital(it.Name, route, device, it.Value)
	}
	w.WriteHeader(http.StatusNoContent)
}

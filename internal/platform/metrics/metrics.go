// Package metrics define os coletores Prometheus do trophy-hunter e o handler /metrics.
//
// Os coletores vivem num registry dedicado (não o default global), exposto por Handler(). As
// funções de registro são livres de alocação no caminho quente e seguras para uso concorrente.
// O kube-prometheus-stack do rblab descobre o alvo via ServiceMonitor (API) e PodMonitor/Service
// (worker).
//
// Semântica por processo (finding 23): o gauge integration_status é um estado pontual e é emitido
// SÓ pelo worker (quem roda o sync e conhece o status). A API nunca o registra — evita duas séries
// com os mesmos labels e valores conflitantes vindos de processos diferentes. Os counters
// (external_api_requests_total, sync_runs_total, jobs_processed_total) podem ser emitidos por
// ambos os processos: agregam por instância sem conflito.
package metrics

import (
	"net/http"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	registry = prometheus.NewRegistry()

	// externalAPIRequests conta chamadas a APIs externas, por provider, endpoint e status.
	// status é "2xx"/"4xx"/"5xx"/"429"/"error" (nunca o corpo nem segredos).
	externalAPIRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "external_api_requests_total",
		Help: "Chamadas a APIs externas (Steam/PSN), por provider, endpoint e status.",
	}, []string{"provider", "endpoint", "status"})

	// syncRuns conta execuções de sync, por escopo (full|incremental|game) e status (ok|error).
	syncRuns = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "sync_runs_total",
		Help: "Execuções de sincronização concluídas, por escopo e status.",
	}, []string{"scope", "status"})

	// jobsProcessed conta jobs processados pela fila, por kind e resultado (ok|retry|dead).
	jobsProcessed = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "jobs_processed_total",
		Help: "Jobs processados pela fila, por kind e resultado.",
	}, []string{"kind", "result"})

	// aiJobs conta execuções da IA por tipo (trophy|roadmap|chat) e resultado (ok|error|rate_limited).
	aiJobs = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "ai_jobs_total",
		Help: "Execuções da IA (guias e chat), por tipo e resultado.",
	}, []string{"kind", "result"})

	// pushSent conta envios de web push por tipo e resultado (ok|gone|error).
	pushSent = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "push_sent_total",
		Help: "Notificações Web Push enviadas, por tipo e resultado.",
	}, []string{"kind", "result"})

	// integrationStatus expõe, por plataforma e status, 1 para o status atual e 0 para os demais.
	// Registrado SÓ no worker (ver EnableIntegrationStatus).
	integrationStatus = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "integration_status",
		Help: "Status atual de cada integração (1 = ativo) por plataforma e status.",
	}, []string{"platform", "status"})

	integrationStatusOnce    sync.Once
	integrationStatusEnabled bool
)

func init() {
	// Coletores de runtime do Go e do processo (goroutines, GC, heap, CPU, FDs, etc.).
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	registry.MustRegister(externalAPIRequests, syncRuns, jobsProcessed, aiJobs, pushSent)
}

// EnableIntegrationStatus registra e habilita o gauge integration_status. Chamado SÓ pelo worker.
func EnableIntegrationStatus() {
	integrationStatusOnce.Do(func() {
		registry.MustRegister(integrationStatus)
		integrationStatusEnabled = true
	})
}

// ExternalAPIRequest registra uma chamada a uma API externa.
func ExternalAPIRequest(provider, endpoint, status string) {
	externalAPIRequests.WithLabelValues(provider, endpoint, status).Inc()
}

// SyncRun registra o término de uma execução de sync.
func SyncRun(scope, status string) {
	syncRuns.WithLabelValues(scope, status).Inc()
}

// JobProcessed registra o resultado do processamento de um job.
func JobProcessed(kind, result string) {
	jobsProcessed.WithLabelValues(kind, result).Inc()
}

// PushSent registra um envio de web push.
func PushSent(kind, result string) {
	pushSent.WithLabelValues(kind, result).Inc()
}

// AIJob registra uma execução da IA.
func AIJob(kind, result string) {
	aiJobs.WithLabelValues(kind, result).Inc()
}

// SetIntegrationStatus marca o status atual de uma integração (zera os demais status da plataforma).
// No-op quando o gauge não está habilitado (processo API) — ver EnableIntegrationStatus.
func SetIntegrationStatus(platform, current string) {
	if !integrationStatusEnabled {
		return
	}
	for _, s := range []string{"connected", "needs_reconnect", "error_private", "error", "disabled"} {
		v := 0.0
		if s == current {
			v = 1.0
		}
		integrationStatus.WithLabelValues(platform, s).Set(v)
	}
}

// Handler retorna o http.Handler que expõe /metrics no formato Prometheus.
func Handler() http.Handler {
	return promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
}

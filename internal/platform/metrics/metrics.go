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
	"time"

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

	// httpRequests / httpDuration: tráfego da API por rota (padrão do chi, não o path cru — sem
	// explosão de cardinalidade) e classe de status. A stream SSE da IA fica fora da latência.
	httpRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "http_requests_total",
		Help: "Requisições HTTP da API, por método, rota e classe de status (2xx..5xx).",
	}, []string{"method", "route", "code"})
	httpDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "http_request_duration_seconds",
		Help:    "Latência das requisições HTTP da API, por método e rota.",
		Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
	}, []string{"method", "route"})

	// Estado (só no worker, lido do banco a cada minuto): fila de jobs e situação de cada conta.
	jobsQueue = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "jobs_queue",
		Help: "Jobs na fila por tipo e status (queued|running|dead).",
	}, []string{"kind", "status"})
	accountStatus = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "integration_account_status",
		Help: "Status atual de cada conta (1 = ativo), por plataforma, conta e status.",
	}, []string{"platform", "account", "status"})
	accountLastSync = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "integration_last_synced_timestamp_seconds",
		Help: "Fim do último sync de cada conta (unix seconds; 0 = nunca).",
	}, []string{"platform", "account"})
	stateOnce sync.Once

	// integrationStatus expõe, por plataforma e status, 1 para o status atual e 0 para os demais.
	// Registrado SÓ no worker (ver EnableIntegrationStatus).
	integrationStatus = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "integration_status",
		Help: "Status atual de cada integração (1 = ativo) por plataforma e status.",
	}, []string{"platform", "status"})

	integrationStatusOnce    sync.Once
	integrationStatusEnabled bool

	clientErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "client_errors_total",
		Help: "Erros reportados pelo app no navegador, por tipo.",
	}, []string{"kind"})
)

func init() {
	// Coletores de runtime do Go e do processo (goroutines, GC, heap, CPU, FDs, etc.).
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	registry.MustRegister(externalAPIRequests, syncRuns, jobsProcessed, aiJobs, pushSent, httpRequests, httpDuration, clientErrors)
	for _, k := range ClientErrorKinds {
		clientErrors.WithLabelValues(k)
	}
}

// ClientErrorKinds são os tipos aceitos; todos nascem em 0 para o increase() do primeiro erro contar
// (série que já surge com valor > 0 dá increase 0 no Prometheus). A versão do web vai só no log.
var ClientErrorKinds = []string{"render", "unhandled", "api_contract", "network", "other"}

// ClientError registra um erro reportado pelo navegador (POST /client-errors).
func ClientError(kind string) {
	clientErrors.WithLabelValues(kind).Inc()
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

// ObserveHTTP registra uma requisição da API. withLatency=false para conexões longas (SSE).
func ObserveHTTP(method, route string, status int, d time.Duration, withLatency bool) {
	httpRequests.WithLabelValues(method, route, statusClass(status)).Inc()
	if withLatency {
		httpDuration.WithLabelValues(method, route).Observe(d.Seconds())
	}
}

func statusClass(status int) string {
	switch {
	case status >= 500:
		return "5xx"
	case status >= 400:
		return "4xx"
	case status >= 300:
		return "3xx"
	default:
		return "2xx"
	}
}

// JobQueueCount é uma linha da contagem da fila.
type JobQueueCount struct {
	Kind, Status string
	N            int64
}

// AccountState é a situação de uma conta.
type AccountState struct {
	Platform, Account, Status string
	LastSynced                time.Time // zero = nunca
}

// SetState publica o retrato atual da fila e das contas (substitui o anterior: contas removidas e
// filas esvaziadas somem). Só o worker chama.
func SetState(queue []JobQueueCount, accounts []AccountState) {
	stateOnce.Do(func() { registry.MustRegister(jobsQueue, accountStatus, accountLastSync) })
	jobsQueue.Reset()
	for _, q := range queue {
		jobsQueue.WithLabelValues(q.Kind, q.Status).Set(float64(q.N))
	}
	accountStatus.Reset()
	accountLastSync.Reset()
	for _, a := range accounts {
		for _, st := range []string{"connected", "needs_reconnect", "error_private", "error", "disabled"} {
			v := 0.0
			if st == a.Status {
				v = 1
			}
			accountStatus.WithLabelValues(a.Platform, a.Account, st).Set(v)
		}
		ts := 0.0
		if !a.LastSynced.IsZero() {
			ts = float64(a.LastSynced.Unix())
		}
		accountLastSync.WithLabelValues(a.Platform, a.Account).Set(ts)
	}
}

// Handler retorna o http.Handler que expõe /metrics no formato Prometheus.
func Handler() http.Handler {
	return promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
}

package sync

import (
	"strings"

	"github.com/RodolfoBonis/trophy-hunter-api/internal/push"

	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	dbgen "github.com/RodolfoBonis/trophy-hunter-api/db/gen"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/metrics"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/queue"
)

const (
	defaultLease   = 10 * time.Minute
	emptyPollDelay = 2 * time.Second
	// bookkeepingTimeout limita Complete/Fail (rodam desacoplados do ctx do job).
	bookkeepingTimeout = 10 * time.Second

	// maintenanceInterval é a cadência do reaper + limpeza de jobs antigos.
	maintenanceInterval = 10 * time.Minute
	// reapOlderThan: runs presos em running por mais que isto sem jobs vivos são interrompidos.
	reapOlderThanMinutes = 15
	// jobRetention: jobs done mais antigos que isto são apagados; os dead ficam mais (deadRetention),
	// para dar tempo de investigar/reprocessar pelo painel do dono.
	jobRetention  = 7 * 24 * time.Hour
	deadRetention = 30 * 24 * time.Hour
)

// Worker consome a fila: reivindica jobs, executa e marca done/fail. Roda N goroutines + uma de
// manutenção (reaper de sync_runs presos + limpeza de jobs antigos).
type Worker struct {
	queue       queue.Queue
	queries     *dbgen.Queries
	syncer      *Syncer
	concurrency int
	jobTimeout  time.Duration
	logger      *slog.Logger
}

// NewWorker constrói o worker. concurrency <= 0 vira 1; jobTimeout <= 0 vira defaultLease/2.
func NewWorker(q queue.Queue, queries *dbgen.Queries, syncer *Syncer, concurrency int, jobTimeout time.Duration, logger *slog.Logger) *Worker {
	if concurrency <= 0 {
		concurrency = 1
	}
	if jobTimeout <= 0 {
		jobTimeout = defaultLease / 2
	}
	return &Worker{
		queue: q, queries: queries, syncer: syncer,
		concurrency: concurrency, jobTimeout: jobTimeout, logger: logger,
	}
}

// Run executa o loop de consumo + a manutenção até ctx cancelar.
func (w *Worker) Run(ctx context.Context) {
	w.reapOnce(ctx) // reaper no boot (worker reiniciou com runs presos)

	var wg sync.WaitGroup
	for i := 0; i < w.concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.loop(ctx)
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		w.maintenance(ctx)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		w.reportState(ctx)
	}()

	w.logger.Info("worker iniciado", slog.Int("concurrency", w.concurrency), slog.Duration("job_timeout", w.jobTimeout))
	wg.Wait()
}

func (w *Worker) loop(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		job, err := w.queue.Claim(ctx, defaultLease, KindSyncPlatform, KindSyncGame, push.JobKind)
		if err != nil {
			w.logger.Error("claim falhou", slog.String("err", err.Error()))
			if !sleepCtx(ctx, emptyPollDelay) {
				return
			}
			continue
		}
		if job == nil {
			if !sleepCtx(ctx, emptyPollDelay) {
				return
			}
			continue
		}
		w.process(ctx, *job)
	}
}

func (w *Worker) process(ctx context.Context, job queue.Job) {
	jobCtx, cancel := context.WithTimeout(ctx, w.jobTimeout)
	defer cancel()

	err := w.syncer.HandleJob(jobCtx, job)

	// Complete/Fail rodam desacoplados do ctx do job (que pode ter expirado), com timeout curto.
	bgCtx, bgCancel := context.WithTimeout(context.WithoutCancel(ctx), bookkeepingTimeout)
	defer bgCancel()

	if err != nil {
		result := "retry"
		if queue.IsPermanent(err) || job.Attempts >= job.MaxAttempts {
			result = "dead"
		}
		w.logger.Error("job falhou",
			slog.String("kind", job.Kind), slog.String("job_id", job.ID),
			slog.String("result", result), slog.String("err", err.Error()))
		ferr := w.queue.Fail(bgCtx, job.ID, job.LeaseToken, err)
		switch {
		case errors.Is(ferr, queue.ErrLeaseLost):
			w.logger.Info("lease perdido no fail; outro worker assumiu o job", slog.String("job_id", job.ID))
		case ferr != nil:
			w.logger.Error("marcar job como falho falhou", slog.String("err", ferr.Error()))
		}
		metrics.JobProcessed(job.Kind, result)
		return
	}

	cerr := w.queue.Complete(bgCtx, job.ID, job.LeaseToken)
	switch {
	case errors.Is(cerr, queue.ErrLeaseLost):
		w.logger.Info("lease perdido no complete; outro worker assumiu o job", slog.String("job_id", job.ID))
	case cerr != nil:
		w.logger.Error("marcar job concluído falhou", slog.String("err", cerr.Error()))
	}
	metrics.JobProcessed(job.Kind, "ok")
}

// maintenance roda o reaper + a limpeza periodicamente.
func (w *Worker) maintenance(ctx context.Context) {
	ticker := time.NewTicker(maintenanceInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.reapOnce(ctx)
			w.cleanupOnce(ctx)
		}
	}
}

func (w *Worker) reapOnce(ctx context.Context) {
	n, err := w.queries.ReapStuckSyncRuns(ctx, reapOlderThanMinutes)
	if err != nil {
		w.logger.Warn("reaper de sync_runs falhou", slog.String("err", err.Error()))
		return
	}
	if n > 0 {
		w.logger.Info("sync_runs presos marcados como interrompidos", slog.Int64("count", n))
	}
}

func (w *Worker) cleanupOnce(ctx context.Context) {
	n, err := w.queue.Cleanup(ctx, jobRetention, deadRetention)
	if err != nil {
		w.logger.Warn("limpeza de jobs antigos falhou", slog.String("err", err.Error()))
		return
	}
	if n > 0 {
		w.logger.Info("jobs terminais antigos removidos", slog.Int64("count", n))
	}
}

// sleepCtx dorme d ou retorna false se o ctx cancelar antes.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// stateInterval é a frequência do retrato de estado (fila e contas) publicado em /metrics.
const stateInterval = time.Minute

// reportState publica a cada minuto o retrato da fila de jobs e da situação de cada conta, lido do
// banco (fonte da verdade): base dos alertas de operação (fila travada, sync parado, conta em erro).
func (w *Worker) reportState(ctx context.Context) {
	ticker := time.NewTicker(stateInterval)
	defer ticker.Stop()
	for {
		w.stateOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (w *Worker) stateOnce(ctx context.Context) {
	rows, err := w.queries.CountJobsByKindStatus(ctx)
	if err != nil {
		w.logger.Warn("métricas: contar fila falhou", slog.String("err", err.Error()))
		return
	}
	integs, err := w.queries.ListAccountsForMetrics(ctx)
	if err != nil {
		w.logger.Warn("métricas: listar contas falhou", slog.String("err", err.Error()))
		return
	}
	queue := make([]metrics.JobQueueCount, 0, len(rows))
	for _, r := range rows {
		queue = append(queue, metrics.JobQueueCount{Kind: r.Kind, Status: r.Status, N: r.N})
	}
	accounts := make([]metrics.AccountState, 0, len(integs))
	for _, i := range integs {
		a := metrics.AccountState{Platform: i.Platform, Account: accountLabel(i.OwnerName, i.OwnerEmail, i.Label), Status: i.Status}
		if i.LastSyncedAt.Valid {
			a.LastSynced = i.LastSyncedAt.Time
		}
		accounts = append(accounts, a)
	}
	metrics.SetState(queue, accounts)
}

// accountLabel identifica a conta nas métricas/alertas: "pessoa/conta" (amigos podem ter contas com
// o mesmo apelido).
func accountLabel(name, email, label string) string {
	who := name
	if who == "" {
		who, _, _ = strings.Cut(email, "@")
	}
	if who == "" {
		return label
	}
	return who + "/" + label
}

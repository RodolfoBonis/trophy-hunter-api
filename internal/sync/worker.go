package sync

import (
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
	// jobRetention: jobs terminais (done/dead) mais antigos que isto são apagados.
	jobRetention = 7 * 24 * time.Hour
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
	n, err := w.queue.Cleanup(ctx, jobRetention)
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

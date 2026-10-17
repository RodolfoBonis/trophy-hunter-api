package sync

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/metrics"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/queue"
)

const (
	defaultLease    = 10 * time.Minute
	emptyPollDelay  = 2 * time.Second
	jobHandleBudget = 9 * time.Minute // < lease, para o lease não expirar durante o job
)

// Worker consome a fila: reivindica jobs, executa e marca done/fail. Roda N goroutines.
type Worker struct {
	queue       queue.Queue
	syncer      *Syncer
	concurrency int
	logger      *slog.Logger
}

// NewWorker constrói o worker. concurrency <= 0 vira 1.
func NewWorker(q queue.Queue, syncer *Syncer, concurrency int, logger *slog.Logger) *Worker {
	if concurrency <= 0 {
		concurrency = 1
	}
	return &Worker{queue: q, syncer: syncer, concurrency: concurrency, logger: logger}
}

// Run executa o loop de consumo até ctx cancelar.
func (w *Worker) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for i := 0; i < w.concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.loop(ctx)
		}()
	}
	w.logger.Info("worker iniciado", slog.Int("concurrency", w.concurrency))
	wg.Wait()
}

func (w *Worker) loop(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		job, err := w.queue.Claim(ctx, defaultLease)
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
	jobCtx, cancel := context.WithTimeout(ctx, jobHandleBudget)
	defer cancel()

	err := w.syncer.HandleJob(jobCtx, job)
	if err != nil {
		w.logger.Error("job falhou",
			slog.String("kind", job.Kind), slog.String("job_id", job.ID), slog.String("err", err.Error()))
		result := "retry"
		if job.Attempts >= job.MaxAttempts {
			result = "dead"
		}
		if ferr := w.queue.Fail(context.WithoutCancel(ctx), job.ID, err); ferr != nil {
			w.logger.Error("marcar job como falho falhou", slog.String("err", ferr.Error()))
		}
		metrics.JobProcessed(job.Kind, result)
		return
	}
	if cerr := w.queue.Complete(context.WithoutCancel(ctx), job.ID); cerr != nil {
		w.logger.Error("marcar job concluído falhou", slog.String("err", cerr.Error()))
	}
	metrics.JobProcessed(job.Kind, "ok")
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

package ai

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	dbgen "github.com/RodolfoBonis/trophy-hunter-api/db/gen"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/ai/stream"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/metrics"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/queue"
)

const (
	emptyPollDelay = 2 * time.Second
	bookkeeping    = 15 * time.Second
)

// Worker consome só os jobs de IA (ai_guide, ai_chat), um por vez: a CLI usa a cota da assinatura
// e cada execução já é pesada (Node + modelo + pesquisa na web).
type Worker struct {
	svc        *Service
	queue      queue.Queue
	jobTimeout time.Duration
	logger     *slog.Logger
}

// NewWorker constrói o worker de IA.
func NewWorker(svc *Service, q queue.Queue, jobTimeout time.Duration, logger *slog.Logger) *Worker {
	return &Worker{svc: svc, queue: q, jobTimeout: jobTimeout, logger: logger}
}

// Run consome a fila até ctx cancelar.
func (w *Worker) Run(ctx context.Context) {
	w.logger.Info("ai-worker iniciado", slog.Duration("job_timeout", w.jobTimeout))
	for ctx.Err() == nil {
		// O lease cobre o timeout da execução com folga: outro worker só assume se este morrer.
		job, err := w.queue.Claim(ctx, w.jobTimeout+2*time.Minute, KindGuide, KindChat, KindRecap)
		if err != nil {
			w.logger.Error("ai: claim falhou", slog.String("err", err.Error()))
		}
		if job == nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(emptyPollDelay):
			}
			continue
		}
		w.process(ctx, *job)
	}
}

func (w *Worker) process(ctx context.Context, job queue.Job) {
	jobCtx, cancel := context.WithTimeout(ctx, w.jobTimeout)
	err := w.svc.Handle(jobCtx, job)
	cancel()

	bg, bgCancel := context.WithTimeout(context.WithoutCancel(ctx), bookkeeping)
	defer bgCancel()
	if err == nil {
		if cerr := w.queue.Complete(bg, job.ID, job.LeaseToken); cerr != nil && !errors.Is(cerr, queue.ErrLeaseLost) {
			w.logger.Error("ai: complete falhou", slog.String("err", cerr.Error()))
		}
		metrics.JobProcessed(job.Kind, "ok")
		return
	}
	result := "retry"
	if queue.IsPermanent(err) || job.Attempts >= job.MaxAttempts {
		result = "dead"
	}
	w.logger.Warn("ai: job falhou", slog.String("kind", job.Kind), slog.String("job_id", job.ID),
		slog.String("result", result), slog.String("err", err.Error()))
	if ferr := w.queue.Fail(bg, job.ID, job.LeaseToken, err); ferr != nil && !errors.Is(ferr, queue.ErrLeaseLost) {
		w.logger.Error("ai: fail falhou", slog.String("err", ferr.Error()))
	}
	if result == "dead" && !queue.IsPermanent(err) {
		w.svc.giveUp(bg, job)
	}
	metrics.JobProcessed(job.Kind, result)
}

// giveUp encerra o pedido cujas tentativas acabaram por erro transitório (ex.: limite de uso
// persistente), para a UI não ficar esperando para sempre.
func (s *Service) giveUp(ctx context.Context, job queue.Job) {
	const msg = "A IA está indisponível agora (limite de uso). Tente de novo mais tarde."
	// Falta de cota é assunto de operação: alerta TrophyHunterAIRateLimited (Telegram), não push.
	switch job.Kind {
	case KindGuide:
		var p guidePayload
		if json.Unmarshal(job.Payload, &p) != nil {
			return
		}
		if id, err := parseUUID(p.GuideID); err == nil {
			_ = s.q.FailGuide(ctx, dbFailGuide(id, msg))
			_ = stream.NewPublisher(ctx, s.rdb, p.GuideID).Close(ctx, stream.TypeError, msg)
		}
	case KindChat:
		var p chatPayload
		if json.Unmarshal(job.Payload, &p) != nil {
			return
		}
		if id, err := parseUUID(p.MessageID); err == nil {
			_ = s.q.FailChatMessage(ctx, dbFailChat(id, msg))
			_ = stream.NewPublisher(ctx, s.rdb, p.MessageID).Close(ctx, stream.TypeError, msg)
		}
	case KindRecap:
		var p recapPayload
		if json.Unmarshal(job.Payload, &p) != nil {
			return
		}
		uid, err := parseUUID(p.UserID)
		week, werr := time.Parse("2006-01-02", p.Week)
		if err == nil && werr == nil {
			_ = s.q.FailRecap(ctx, dbgen.FailRecapParams{UserID: uid, Week: pgtype.Date{Time: week, Valid: true}, Error: ptr(msg)})
		}
	}
}

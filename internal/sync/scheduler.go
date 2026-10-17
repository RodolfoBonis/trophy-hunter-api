package sync

import (
	"context"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	dbgen "github.com/RodolfoBonis/trophy-hunter-api/db/gen"
)

// schedulerLockKey é a chave do lock de agendamento (uma réplica agenda por tick).
const schedulerLockKey = "trophy:scheduler:lock"

// Scheduler dispara, periodicamente, syncs incrementais para as integrações conectadas.
//
// Com múltiplas réplicas do worker, um lock SET NX no Redis garante que só uma agenda por tick
// (o dedupe da fila é uma segunda defesa). redis nil desliga o lock (dev single-replica).
type Scheduler struct {
	syncer   *Syncer
	redis    *redis.Client
	q        *dbgen.Queries
	interval time.Duration
	logger   *slog.Logger
}

// NewScheduler constrói o scheduler.
func NewScheduler(syncer *Syncer, rdb *redis.Client, q *dbgen.Queries, interval time.Duration, logger *slog.Logger) *Scheduler {
	return &Scheduler{syncer: syncer, redis: rdb, q: q, interval: interval, logger: logger}
}

// Run roda até ctx cancelar, agendando a cada interval.
func (s *Scheduler) Run(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	s.logger.Info("scheduler iniciado", slog.Duration("interval", s.interval))
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.tick(ctx)
		}
	}
}

func (s *Scheduler) tick(ctx context.Context) {
	if !s.acquire(ctx) {
		return // outra réplica agendou neste tick
	}
	integs, err := s.q.ListIntegrations(ctx)
	if err != nil {
		s.logger.Error("scheduler: listar integrações falhou", slog.String("err", err.Error()))
		return
	}
	for _, i := range integs {
		if i.Status == "disabled" {
			continue
		}
		if _, enq, err := s.syncer.EnqueuePlatformSync(ctx, i.Platform, ScopeIncremental, TriggerSchedule); err != nil {
			s.logger.Error("scheduler: enfileirar sync falhou",
				slog.String("platform", i.Platform), slog.String("err", err.Error()))
		} else if enq {
			s.logger.Info("sync incremental agendado", slog.String("platform", i.Platform))
		}
	}
}

// acquire tenta o lock de agendamento. Sem Redis, sempre agenda (single-replica).
func (s *Scheduler) acquire(ctx context.Context) bool {
	if s.redis == nil {
		return true
	}
	// TTL < interval para o lock expirar antes do próximo tick.
	ttl := s.interval - s.interval/10
	ok, err := s.redis.SetNX(ctx, schedulerLockKey, "1", ttl).Result()
	if err != nil {
		s.logger.Warn("scheduler: lock redis indisponível, agendando assim mesmo", slog.String("err", err.Error()))
		return true
	}
	return ok
}

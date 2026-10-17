package sync

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	dbgen "github.com/RodolfoBonis/trophy-hunter-api/db/gen"
)

// schedulerLockKey é a chave do lock de agendamento (uma réplica agenda por tick).
const schedulerLockKey = "trophy:scheduler:lock"

// schedulerLockTTL é a janela do lock de agendamento entre réplicas.
const schedulerLockTTL = 2 * time.Minute

// initialPassDelay é o atraso do primeiro agendamento após o boot (não espera um intervalo inteiro).
const initialPassDelay = time.Minute

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

// Run roda até ctx cancelar. Faz uma passada inicial ~1min após o boot e depois a cada interval
// (finding 21): o sync não espera um intervalo inteiro para rodar pela primeira vez.
func (s *Scheduler) Run(ctx context.Context) {
	s.logger.Info("scheduler iniciado", slog.Duration("interval", s.interval))

	initial := time.NewTimer(initialPassDelay)
	defer initial.Stop()
	select {
	case <-ctx.Done():
		return
	case <-initial.C:
		s.tick(ctx)
	}

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
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
		_, enq, err := s.syncer.EnqueuePlatformSync(ctx, i.ID, ScopeIncremental, TriggerSchedule)
		switch {
		case errors.Is(err, ErrNotConnected):
			// conta removida entre a listagem e o enfileiramento; ignora.
		case err != nil:
			s.logger.Error("scheduler: enfileirar sync falhou",
				slog.String("platform", i.Platform), slog.String("account", i.Label), slog.String("err", err.Error()))
		case enq:
			s.logger.Info("sync incremental agendado",
				slog.String("platform", i.Platform), slog.String("account", i.Label))
		}
	}
}

// acquire tenta o lock de agendamento. Sem Redis, sempre agenda (single-replica).
func (s *Scheduler) acquire(ctx context.Context) bool {
	if s.redis == nil {
		return true
	}
	// O lock só serializa réplicas que disparam no MESMO momento (o dedupe da fila já evita jobs
	// duplicados). Um TTL longo (ex.: ~interval) fazia todo restart do worker pular a passada inicial.
	ok, err := s.redis.SetNX(ctx, schedulerLockKey, "1", schedulerLockTTL).Result()
	if err != nil {
		s.logger.Warn("scheduler: lock redis indisponível, agendando assim mesmo", slog.String("err", err.Error()))
		return true
	}
	return ok
}

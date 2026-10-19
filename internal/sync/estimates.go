package sync

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	dbgen "github.com/RodolfoBonis/trophy-hunter-api/db/gen"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/integrations/hltb"
)

// EstimateSource busca tempos de jogo (HowLongToBeat; fake nos testes).
type EstimateSource interface {
	Search(ctx context.Context, title string) ([]hltb.Result, error)
}

// Estimator preenche game_estimates em segundo plano, aos poucos (o HLTB é lento e sem API
// oficial): a cada intervalo, um lote de títulos sem estimativa. Falha num título não para o lote;
// 429 para o lote até a próxima rodada.
type Estimator struct {
	q        *dbgen.Queries
	src      EstimateSource
	interval time.Duration
	batch    int
	logger   *slog.Logger
}

// NewEstimator constrói o Estimator (intervalo/lote padrão: 10 min / 15 títulos).
func NewEstimator(q *dbgen.Queries, src EstimateSource, interval time.Duration, batch int, logger *slog.Logger) *Estimator {
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	if batch <= 0 {
		batch = 15
	}
	return &Estimator{q: q, src: src, interval: interval, batch: batch, logger: logger}
}

// Run roda até o ctx cancelar (primeira rodada logo após 1 min, para não competir com o boot).
func (e *Estimator) Run(ctx context.Context) {
	if !sleepCtx(ctx, time.Minute) {
		return
	}
	for {
		if _, err := e.RefreshBatch(ctx); err != nil && ctx.Err() == nil {
			e.logger.Warn("estimativas: rodada falhou", slog.String("err", err.Error()))
		}
		if !sleepCtx(ctx, e.interval) {
			return
		}
	}
}

var symbolsRe = strings.NewReplacer("™", "", "®", "", "©", "")

// RefreshBatch busca um lote de títulos; devolve quantos foram resolvidos (achados ou não).
func (e *Estimator) RefreshBatch(ctx context.Context) (int, error) {
	titles, err := e.q.TitlesNeedingEstimate(ctx, int32(e.batch)) //nolint:gosec // batch pequeno
	if err != nil {
		return 0, err
	}
	done := 0
	for _, t := range titles {
		res, err := e.src.Search(ctx, symbolsRe.Replace(t.Title))
		if errors.Is(err, hltb.ErrRateLimited) {
			e.logger.Info("estimativas: HLTB pediu para esperar; continua na próxima rodada")
			return done, nil
		}
		if err != nil {
			if ctx.Err() != nil {
				return done, ctx.Err()
			}
			e.logger.Warn("estimativas: busca falhou", slog.String("title", t.Title), slog.String("err", err.Error()))
			continue
		}
		p := dbgen.UpsertEstimateParams{NormalizedTitle: t.NormalizedTitle, Status: "not_found"}
		if m, ok := hltb.BestMatch(NormalizeTitle, t.Title, res); ok {
			id, name := m.ID, m.Name
			mainH, extraH, compH := f32(m.MainH), f32(m.ExtraH), f32(m.CompletionistH)
			p = dbgen.UpsertEstimateParams{
				NormalizedTitle: t.NormalizedTitle, Status: "found", HltbID: &id, HltbName: &name,
				MainH: mainH, ExtraH: extraH, CompletionistH: compH,
			}
		}
		if err := e.q.UpsertEstimate(ctx, p); err != nil {
			return done, err
		}
		done++
	}
	if done > 0 {
		e.logger.Info("estimativas atualizadas", slog.Int("count", done))
	}
	return done, nil
}

func f32(h float64) *float32 {
	if h <= 0 {
		return nil
	}
	v := float32(h)
	return &v
}

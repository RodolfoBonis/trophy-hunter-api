// Command ai-worker gera guias por troféu, roadmaps e respostas do chat com a CLI do Claude Code
// (headless), consumindo só os jobs ai_* da fila e transmitindo a resposta por Redis Streams.
// Roda em imagem própria (node:22-slim + @anthropic-ai/claude-code; ver Dockerfile.ai-worker).
// Expõe /metrics e /healthz em METRICS_ADDR.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"

	trophyhunter "github.com/RodolfoBonis/trophy-hunter-api"
	dbgen "github.com/RodolfoBonis/trophy-hunter-api/db/gen"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/ai"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/ai/claude"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/config"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/db"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/httpserver"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/metrics"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/queue"
	redisplat "github.com/RodolfoBonis/trophy-hunter-api/internal/platform/redis"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/telemetry"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", slog.String("err", err.Error()))
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(config.WithoutCrypto())
	if err != nil {
		return err
	}
	logger := telemetry.NewLogger(cfg.Telemetry)
	slog.SetDefault(logger)
	logger.Info("iniciando ai-worker", slog.String("version", trophyhunter.Version()), slog.String("model", cfg.AI.Model), slog.String("guide_model", cfg.AI.GuideModel))
	if cfg.AI.OAuthToken == "" {
		return errors.New("CLAUDE_CODE_OAUTH_TOKEN ausente (gere com `claude setup-token`)")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// As migrations são do worker/API; o ai-worker só usa o schema.
	pool, err := db.NewPool(ctx, cfg.DB)
	if err != nil {
		return err
	}
	defer pool.Close()

	// Redis é obrigatório aqui: é o canal da resposta em tempo real.
	rdb, err := redisplat.New(ctx, cfg.Redis)
	if err != nil {
		return err
	}
	defer func() { _ = rdb.Close() }()

	jobs := queue.NewPostgres(pool, queue.DefaultBackoff)
	svc := ai.New(ai.Deps{
		Queries: dbgen.New(pool), Queue: jobs, Redis: rdb,
		Runner: claude.New(cfg.AI.ClaudeBin, cfg.AI.OAuthToken, cfg.AI.Home, logger),
		Model:  cfg.AI.Model, Workdir: cfg.AI.Workdir, Logger: logger,
	})
	worker := ai.NewWorker(svc, jobs, cfg.AI.JobTimeout, logger)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		worker.Run(ctx)
	}()

	mux := http.NewServeMux()
	mux.Handle("/metrics", metrics.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	srv := httpserver.NewAddr(cfg.Metrics.Addr, mux, logger)
	wg.Add(1)
	go func() {
		defer wg.Done()
		if merr := srv.Run(ctx); merr != nil {
			logger.Error("metrics server parou com erro", slog.String("err", merr.Error()))
		}
	}()

	<-ctx.Done()
	wg.Wait()
	return nil
}

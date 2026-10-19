// Command worker processa a fila de jobs (sync_platform, sync_game) e agenda syncs incrementais.
// Expõe /metrics e /healthz em METRICS_ADDR. Mesma imagem do api; o deployment sobrescreve o command.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	trophyhunter "github.com/RodolfoBonis/trophy-hunter-api"
	dbgen "github.com/RodolfoBonis/trophy-hunter-api/db/gen"
	migrations "github.com/RodolfoBonis/trophy-hunter-api/db/migrations"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/challenges"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/integrations/hltb"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/integrations/psn"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/integrations/steam"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/config"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/crypto"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/db"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/httpserver"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/metrics"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/queue"
	redisplat "github.com/RodolfoBonis/trophy-hunter-api/internal/platform/redis"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/telemetry"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/push"
	syncpkg "github.com/RodolfoBonis/trophy-hunter-api/internal/sync"

	"github.com/redis/go-redis/v9"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", slog.String("err", err.Error()))
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger := telemetry.NewLogger(cfg.Telemetry)
	slog.SetDefault(logger)
	logger.Info("iniciando worker", slog.String("version", trophyhunter.Version()))

	// O worker é o dono do gauge integration_status (um único emissor — ver metrics / finding 23).
	metrics.EnableIntegrationStatus()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	shutdownTracing, err := telemetry.SetupTracing(ctx, cfg.Telemetry)
	if err != nil {
		return err
	}
	defer func() { _ = shutdownTracing(context.WithoutCancel(ctx)) }()

	if cfg.DB.MigrateOnStart {
		if err := db.Migrate(ctx, cfg.DB.URL, migrations.FS); err != nil {
			return err
		}
	}

	pool, err := db.NewPool(ctx, cfg.DB)
	if err != nil {
		return err
	}
	defer pool.Close()

	cipher, err := newCipher(cfg)
	if err != nil {
		return err
	}

	// Redis é opcional: o scheduler degrada para single-replica se indisponível.
	var rdb *redis.Client
	if rc, rerr := redisplat.New(ctx, cfg.Redis); rerr != nil {
		logger.Warn("redis indisponível; scheduler sem lock entre réplicas", slog.String("err", rerr.Error()))
	} else {
		rdb = rc
		defer func() { _ = rdb.Close() }()
	}

	queries := dbgen.New(pool)
	jobs := queue.NewPostgres(pool, queue.DefaultBackoff)
	steamFactory := func(apiKey string) syncpkg.SteamAPI {
		return steam.New(steam.Options{BaseURL: cfg.Steam.BaseURL, APIKey: apiKey, Lang: cfg.Steam.Lang, RPS: cfg.Steam.RPS, Logger: logger})
	}
	notifier := push.New(queries, push.Config{PublicKey: cfg.Push.VAPIDPublicKey, PrivateKey: cfg.Push.VAPIDPrivateKey, Subject: cfg.Push.VAPIDSubject}, nil, logger)
	if !notifier.Enabled() {
		logger.Warn("web push desligado: VAPID_PUBLIC_KEY/VAPID_PRIVATE_KEY ausentes")
	}
	challengeSvc := challenges.New(queries, jobs, logger)
	syncer := syncpkg.New(syncpkg.Deps{
		Challenges: challengeSvc,
		Queries:    queries, Pool: pool, Queue: jobs, Cipher: cipher,
		SteamFactory: steamFactory, Logger: logger, Notifier: notifier, SteamServerKey: cfg.Steam.ServerAPIKey,
		PSN: psn.New(psn.Options{AuthBaseURL: cfg.PSN.AuthBaseURL, APIBaseURL: cfg.PSN.APIBaseURL, Lang: cfg.PSN.Lang, RPS: cfg.PSN.RPS, Burst: cfg.PSN.Burst, Logger: logger}),
	})

	syncer.AutoLink(ctx) // vínculos Steam × PSN existentes (ex.: após a migration de unificação)
	worker := syncpkg.NewWorker(jobs, queries, syncer, cfg.WorkerConcurrency, cfg.Sync.JobTimeout, logger)

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		worker.Run(ctx)
	}()

	// Desafios: prazos vencem sem sync nenhum — reavalia a cada 5 min.
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(5 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				challengeSvc.EvaluateAll(ctx)
			}
		}
	}()

	// Estimativas de tempo (HowLongToBeat) em segundo plano — melhor esforço, sem API oficial.
	if cfg.HLTB.Enabled {
		estimator := syncpkg.NewEstimator(queries, hltb.New(hltb.Options{BaseURL: cfg.HLTB.BaseURL, Logger: logger}), cfg.HLTB.Interval, cfg.HLTB.Batch, logger)
		wg.Add(1)
		go func() {
			defer wg.Done()
			estimator.Run(ctx)
		}()
	}

	if cfg.Sync.Enabled {
		scheduler := syncpkg.NewScheduler(syncer, rdb, queries, cfg.Sync.Interval, logger)
		wg.Add(1)
		go func() {
			defer wg.Done()
			scheduler.Run(ctx)
		}()
	}

	// Servidor de métricas/health do worker.
	metricsSrv := httpserver.NewAddr(cfg.Metrics.Addr, metricsMux(), logger)
	wg.Add(1)
	go func() {
		defer wg.Done()
		if merr := metricsSrv.Run(ctx); merr != nil {
			logger.Error("metrics server parou com erro", slog.String("err", merr.Error()))
		}
	}()

	<-ctx.Done()
	wg.Wait()
	return nil
}

// newCipher constrói o cifrador com a chave primária e registra as chaves antigas (rotação).
func newCipher(cfg config.Config) (*crypto.Cipher, error) {
	cipher, err := crypto.New(cfg.Crypto.KeyVersion, cfg.Crypto.Key)
	if err != nil {
		return nil, err
	}
	for version, key := range cfg.Crypto.OldKeys {
		if err := cipher.AddKey(version, key); err != nil {
			return nil, err
		}
	}
	return cipher, nil
}

func metricsMux() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", metrics.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	return mux
}

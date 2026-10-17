// Command api é o servidor HTTP do Trophy Hunter: expõe a API REST (/api/v1), valida o JWT do
// Keycloak (realm platform) e enfileira jobs de sync. Não processa jobs — isso é do cmd/worker.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	trophyhunter "github.com/RodolfoBonis/trophy-hunter-api"
	dbgen "github.com/RodolfoBonis/trophy-hunter-api/db/gen"
	migrations "github.com/RodolfoBonis/trophy-hunter-api/db/migrations"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/api"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/integrations/steam"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/auth"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/config"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/crypto"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/db"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/httpserver"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/queue"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/telemetry"
	syncpkg "github.com/RodolfoBonis/trophy-hunter-api/internal/sync"
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
	logger.Info("iniciando api", slog.String("version", trophyhunter.Version()))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	shutdownTracing, err := telemetry.SetupTracing(ctx, cfg.Telemetry)
	if err != nil {
		return err
	}
	defer func() { _ = shutdownTracing(context.WithoutCancel(ctx)) }()
	shutdownMetrics, err := telemetry.SetupMetrics(ctx, cfg.Telemetry)
	if err != nil {
		return err
	}
	defer func() { _ = shutdownMetrics(context.WithoutCancel(ctx)) }()

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

	// Autenticador: no modo oidc descobre o provider em background (API sobe mesmo com o Keycloak
	// fora; readyz só fica pronto após carregar — ver auth.New).
	authenticator := auth.New(ctx, cfg.Auth, logger)

	cipher, err := newCipher(cfg)
	if err != nil {
		return err
	}

	queries := dbgen.New(pool)
	jobs := queue.NewPostgres(pool, queue.DefaultBackoff)
	steamFactory := newSteamFactory(cfg, logger)
	syncer := syncpkg.New(syncpkg.Deps{
		Queries: queries, Pool: pool, Queue: jobs, Cipher: cipher,
		SteamFactory: steamFactory, Logger: logger,
	})

	handlers := api.NewHandlers(api.HandlerDeps{
		Queries: queries, Pool: pool, Syncer: syncer, Logger: logger,
		AuthReady: authenticator.Ready,
	})
	router := api.NewRouter(api.RouterDeps{
		Handlers: handlers, Auth: authenticator, Logger: logger,
		ServiceName: cfg.Telemetry.ServiceName, WriteTimeout: cfg.HTTP.WriteTimeout,
		CORSAllowedOrigins: cfg.HTTP.CORSAllowedOrigins,
	})

	srv := httpserver.New(cfg.HTTP, router, logger)
	return srv.Run(ctx)
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

// newSteamFactory devolve uma fábrica de clientes Steam (um por API key de integração).
func newSteamFactory(cfg config.Config, logger *slog.Logger) syncpkg.SteamFactory {
	return func(apiKey string) syncpkg.SteamAPI {
		return steam.New(steam.Options{
			BaseURL: cfg.Steam.BaseURL,
			APIKey:  apiKey,
			Lang:    cfg.Steam.Lang,
			RPS:     cfg.Steam.RPS,
			Logger:  logger,
		})
	}
}

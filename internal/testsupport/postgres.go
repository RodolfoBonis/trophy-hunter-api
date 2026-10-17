// Package testsupport fornece utilitários compartilhados de teste (ex.: Postgres efêmero via
// testcontainers já migrado). Só é importado por arquivos _test.go, então não entra nos binários.
package testsupport

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	migrations "github.com/RodolfoBonis/trophy-hunter-api/db/migrations"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/db"
)

// NewPostgres sobe um Postgres efêmero, aplica as migrations e devolve um pool pronto.
// O container e o pool são encerrados automaticamente via t.Cleanup. Pula o teste em -short.
func NewPostgres(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if testing.Short() {
		t.Skip("pula teste de integração (-short)")
	}
	ctx := context.Background()

	container, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("trophy"),
		postgres.WithUsername("trophy"),
		postgres.WithPassword("trophy"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		t.Fatalf("subir postgres: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}

	if err := db.Migrate(ctx, dsn, migrations.FS); err != nil {
		t.Fatalf("migrar: %v", err)
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

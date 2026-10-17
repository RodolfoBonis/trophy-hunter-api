package migrations_test

import (
	"context"
	"database/sql"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/require"

	migrations "github.com/RodolfoBonis/trophy-hunter-api/db/migrations"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/testsupport"
)

// TestMultiAccountBackfill reproduz o estado de produção antes da 00013 (uma integração Steam com
// jogos e troféus) e garante que a migration preserva tudo, vinculando os jogos à conta existente.
func TestMultiAccountBackfill(t *testing.T) {
	ctx := context.Background()
	sqlDB, err := sql.Open("pgx", testsupport.NewPostgresDSN(t))
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	goose.SetBaseFS(migrations.FS)
	t.Cleanup(func() { goose.SetBaseFS(nil) })
	require.NoError(t, goose.SetDialect("postgres"))
	require.NoError(t, goose.UpToContext(ctx, sqlDB, ".", 12))

	var integID string
	require.NoError(t, sqlDB.QueryRowContext(ctx, `
		INSERT INTO integrations (platform, status, account_id, display_name)
		VALUES ('steam', 'connected', '76561197960000000', 'Wolf.DD') RETURNING id`).Scan(&integID))
	var gameID string
	require.NoError(t, sqlDB.QueryRowContext(ctx, `
		INSERT INTO games (platform, external_id, title) VALUES ('steam', '620', 'Portal 2') RETURNING id`).Scan(&gameID))
	_, err = sqlDB.ExecContext(ctx, `INSERT INTO games (platform, external_id, title) VALUES ('psn', 'NPWR1', 'Órfão')`)
	require.NoError(t, err)

	require.NoError(t, goose.UpToContext(ctx, sqlDB, ".", 13))

	var label string
	require.NoError(t, sqlDB.QueryRowContext(ctx, `SELECT label FROM integrations WHERE id = $1`, integID).Scan(&label))
	require.Equal(t, "Wolf.DD", label, "label = display_name")

	var owner string
	require.NoError(t, sqlDB.QueryRowContext(ctx, `SELECT integration_id FROM games WHERE id = $1`, gameID).Scan(&owner))
	require.Equal(t, integID, owner, "jogo existente vinculado à conta da plataforma")

	var orphans int
	require.NoError(t, sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM games WHERE platform = 'psn'`).Scan(&orphans))
	require.Zero(t, orphans, "jogo sem conta da plataforma é removido")

	// Segunda conta Steam passa a ser permitida; a mesma conta não.
	_, err = sqlDB.ExecContext(ctx, `INSERT INTO integrations (platform, account_id, label) VALUES ('steam', '76561197960000001', 'Alt')`)
	require.NoError(t, err)
	_, err = sqlDB.ExecContext(ctx, `INSERT INTO integrations (platform, account_id, label) VALUES ('steam', '76561197960000001', 'Dup')`)
	require.Error(t, err)

	// Down volta ao modelo antigo sem erro.
	require.NoError(t, goose.DownToContext(ctx, sqlDB, ".", 12))
}

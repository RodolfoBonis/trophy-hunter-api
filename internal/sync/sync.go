// Package sync coordena a sincronização de bibliotecas e conquistas com as plataformas.
//
// Modelo de fan-out (cabe no orçamento por job mesmo com rate limit baixo):
//   - sync_platform: lista a biblioteca (1 chamada), upserta a IDENTIDADE dos jogos e emite UM
//     job sync_game por jogo que precisa de trabalho. Cria o sync_run PAI e, ao terminar o
//     fan-out, o sela (fixa games_total). Não toca em conquistas.
//   - sync_game: sincroniza schema/conquistas/raridade de UM jogo; ao terminar (terminal) atualiza
//     os contadores do run pai. O último filho a terminar fecha o run (barreira fanout_sealed +
//     games_done>=games_total).
//   - O "sync de jogo" manual reaproveita o kind sync_game (com um run próprio scope=game).
//
// O diff de conquistas só gera unlock_events DEPOIS que o full sync inicial da integração
// terminou (integrations.initial_sync_done). initial_sync_done só liga quando o full inicial fecha
// com ZERO jogos falhos. A marca d'água do incremental (remote_updated_at) só é gravada APÓS o
// sync de conquistas do jogo ter sucesso — um jogo que falhou é re-tentado no próximo incremental.
package sync

import (
	"github.com/RodolfoBonis/trophy-hunter-api/internal/push"

	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	dbgen "github.com/RodolfoBonis/trophy-hunter-api/db/gen"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/integrations/steam"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/crypto"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/metrics"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/queue"
)

// Kinds de job.
const (
	KindSyncPlatform = "sync_platform"
	KindSyncGame     = "sync_game"
)

// Scopes e triggers.
const (
	ScopeFull        = "full"
	ScopeIncremental = "incremental"
	ScopeGame        = "game"

	TriggerSchedule = "schedule"
	TriggerManual   = "manual"
)

// Janela de re-fetch do sync incremental.
const (
	schemaMaxAge      = 7 * 24 * time.Hour
	percentagesMaxAge = 24 * time.Hour
)

// bgTimeout é o teto para o bookkeeping (fechar run / status da integração) rodar mesmo que o ctx
// do job esteja cancelado (context.WithoutCancel). Ver finding 8.
const bgTimeout = 10 * time.Second

// ErrNotConnected indica que a integração (conta) não existe — ex.: removida.
var ErrNotConnected = errors.New("integração não conectada")

// SteamAPI é a superfície do cliente Steam usada pelo syncer (mockável em testes).
type SteamAPI interface {
	GetOwnedGames(ctx context.Context, steamID string) ([]steam.OwnedGame, error)
	GetPlayerAchievements(ctx context.Context, steamID string, appID int) ([]steam.PlayerAchievement, error)
	GetSchemaForGame(ctx context.Context, appID int) (string, []steam.SchemaAchievement, error)
	GetGlobalAchievementPercentages(ctx context.Context, appID int) (map[string]float64, error)
	GetPlayerSummary(ctx context.Context, steamID string) (steam.PlayerSummary, error)
	ResolveVanityURL(ctx context.Context, vanity string) (string, error)
	GetAppAssets(ctx context.Context, appIDs []int) (map[int]steam.AppAssets, error)
}

// SteamFactory constrói um SteamAPI para uma dada API key.
type SteamFactory func(apiKey string) SteamAPI

// PlatformPayload é o payload do job sync_platform.
type PlatformPayload struct {
	IntegrationID string `json:"integration_id"`
	Platform      string `json:"platform"`
	Scope         string `json:"scope"`
	Trigger       string `json:"trigger"`
	SyncRunID     string `json:"sync_run_id"`
}

// GamePayload é o payload do job sync_game.
//
// SyncRunID é o run pai (fan-out) ou o run próprio (manual). CreateEvents é calculado por quem
// enfileira (= integration.initial_sync_done naquele instante). Quando HasSnapshot, o pai já
// mandou o estado remoto do owned-games (evita uma chamada por filho); senão o filho busca.
type GamePayload struct {
	GameID       string `json:"game_id"`
	Trigger      string `json:"trigger"`
	Scope        string `json:"scope"`
	SyncRunID    string `json:"sync_run_id"`
	CreateEvents bool   `json:"create_events"`
	HasSnapshot  bool   `json:"has_snapshot"`
	AppID        int    `json:"appid"`
	Playtime     int    `json:"playtime"`
	Rtime        int64  `json:"rtime"`
	HasStats     bool   `json:"has_stats"`
}

// steamCredentials é o segredo persistido (cifrado) de uma integração Steam.
type steamCredentials struct {
	APIKey  string `json:"api_key"`
	SteamID string `json:"steam_id"`
}

// Syncer é o serviço de sincronização.
type Syncer struct {
	q            *dbgen.Queries
	pool         *pgxpool.Pool
	queue        queue.Queue
	cipher       *crypto.Cipher
	steamFactory SteamFactory
	psn          PSNAPI
	notifier     *push.Service
	logger       *slog.Logger
}

// Deps injeta as dependências do Syncer.
type Deps struct {
	Queries      *dbgen.Queries
	Pool         *pgxpool.Pool
	Queue        queue.Queue
	Cipher       *crypto.Cipher
	SteamFactory SteamFactory
	// PSN é o cliente da PlayStation (uma instância por processo; nil desliga a PSN).
	PSN PSNAPI
	// Notifier entrega os jobs `notify` (só no worker; nil = descarta).
	Notifier *push.Service
	Logger   *slog.Logger
}

// New constrói o Syncer.
func New(d Deps) *Syncer {
	return &Syncer{
		q:            d.Queries,
		pool:         d.Pool,
		queue:        d.Queue,
		cipher:       d.Cipher,
		steamFactory: d.SteamFactory,
		psn:          d.PSN,
		notifier:     d.Notifier,
		logger:       d.Logger,
	}
}

// EnqueuePlatformSync cria o run PAI e enfileira o job sync_platform de UMA conta (dedupe por
// conta+scope). Se o enfileiramento for suprimido por dedupe, o run recém-criado é removido.
func (s *Syncer) EnqueuePlatformSync(ctx context.Context, integrationID pgtype.UUID, scope, trigger string) (*queue.Job, bool, error) {
	integ, err := s.q.GetIntegration(ctx, integrationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, ErrNotConnected
	}
	if err != nil {
		return nil, false, fmt.Errorf("carregar integração: %w", err)
	}

	run, err := s.q.StartSyncRun(ctx, dbgen.StartSyncRunParams{
		IntegrationID: integ.ID, Platform: integ.Platform, Scope: scope, Trigger: trigger,
		GamesTotal: 0, Sealed: false,
	})
	if err != nil {
		return nil, false, fmt.Errorf("iniciar sync_run: %w", err)
	}

	job, enq, err := s.queue.Enqueue(ctx, queue.EnqueueParams{
		Kind: KindSyncPlatform,
		Payload: PlatformPayload{
			IntegrationID: uuidStr(integ.ID), Platform: integ.Platform, Scope: scope, Trigger: trigger,
			SyncRunID: uuidStr(run.ID),
		},
		DedupeKey: fmt.Sprintf("%s:%s:%s", KindSyncPlatform, uuidStr(integ.ID), scope),
	})
	if err != nil || !enq {
		s.deleteRun(ctx, run.ID)
		return job, enq, err
	}
	return job, enq, nil
}

// EnqueueGameSync cria um run próprio (scope=game, total=1) e enfileira o sync_game (dedupe por jogo).
func (s *Syncer) EnqueueGameSync(ctx context.Context, gameID, trigger string) (*queue.Job, bool, error) {
	gameUUID, err := parseUUID(gameID)
	if err != nil {
		return nil, false, fmt.Errorf("game id inválido: %w", err)
	}
	game, err := s.q.GetGameByID(ctx, gameUUID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, fmt.Errorf("jogo não encontrado")
	}
	if err != nil {
		return nil, false, fmt.Errorf("carregar jogo: %w", err)
	}
	integ, err := s.q.GetIntegration(ctx, game.IntegrationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, ErrNotConnected
	}
	if err != nil {
		return nil, false, fmt.Errorf("carregar integração: %w", err)
	}

	run, err := s.q.StartSyncRun(ctx, dbgen.StartSyncRunParams{
		IntegrationID: integ.ID, Platform: game.Platform, Scope: ScopeGame, Trigger: trigger,
		GamesTotal: 1, Sealed: true,
	})
	if err != nil {
		return nil, false, fmt.Errorf("iniciar sync_run: %w", err)
	}

	job, enq, err := s.queue.Enqueue(ctx, queue.EnqueueParams{
		Kind: KindSyncGame,
		Payload: GamePayload{
			GameID: gameID, Trigger: trigger, Scope: ScopeGame,
			SyncRunID: uuidStr(run.ID), CreateEvents: integ.InitialSyncDone, HasSnapshot: false,
		},
		DedupeKey: fmt.Sprintf("%s:%s", KindSyncGame, gameID),
	})
	if err != nil || !enq {
		s.deleteRun(ctx, run.ID)
		return job, enq, err
	}
	return job, enq, nil
}

// HandleJob despacha um job reivindicado da fila para o handler adequado.
func (s *Syncer) HandleJob(ctx context.Context, job queue.Job) error {
	switch job.Kind {
	case KindSyncPlatform:
		var p PlatformPayload
		if err := json.Unmarshal(job.Payload, &p); err != nil {
			return fmt.Errorf("payload sync_platform inválido: %w", err)
		}
		return s.handlePlatform(ctx, p)
	case KindSyncGame:
		var p GamePayload
		if err := json.Unmarshal(job.Payload, &p); err != nil {
			return fmt.Errorf("payload sync_game inválido: %w", err)
		}
		return s.handleGame(ctx, job, p)
	case push.JobKind:
		if s.notifier == nil {
			return nil
		}
		return s.notifier.HandleJob(ctx, job)
	default:
		return fmt.Errorf("kind de job desconhecido: %s", job.Kind)
	}
}

// loadSteam carrega a integração (conta) Steam, decifra as credenciais e constrói o cliente.
func (s *Syncer) loadSteam(ctx context.Context, integrationID pgtype.UUID) (dbgen.Integration, SteamAPI, steamCredentials, error) {
	integ, err := s.q.GetIntegration(ctx, integrationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return dbgen.Integration{}, nil, steamCredentials{}, ErrNotConnected
	}
	if err != nil {
		return dbgen.Integration{}, nil, steamCredentials{}, fmt.Errorf("carregar integração steam: %w", err)
	}
	plain, err := s.cipher.Decrypt(integ.CredentialsEnc, "steam")
	if err != nil {
		return dbgen.Integration{}, nil, steamCredentials{}, fmt.Errorf("decifrar credenciais steam: %w", err)
	}
	var creds steamCredentials
	if err := json.Unmarshal(plain, &creds); err != nil {
		return dbgen.Integration{}, nil, steamCredentials{}, fmt.Errorf("parse credenciais steam: %w", err)
	}
	return integ, s.steamFactory(creds.APIKey), creds, nil
}

// deleteRun remove um run (bookkeeping) num contexto que sobrevive ao cancelamento do job.
func (s *Syncer) deleteRun(ctx context.Context, id pgtype.UUID) {
	bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), bgTimeout)
	defer cancel()
	if err := s.q.DeleteSyncRun(bg, id); err != nil {
		s.logger.Warn("falha ao remover sync_run órfão", slog.String("err", err.Error()))
	}
}

// bg devolve um contexto desacoplado do cancelamento do job, com timeout curto, para o bookkeeping.
func bg(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), bgTimeout)
}

// --- helpers de conversão pgtype ---

func ts(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

func tsFromUnix(sec int64) pgtype.Timestamptz {
	if sec <= 0 {
		return pgtype.Timestamptz{}
	}
	return ts(time.Unix(sec, 0).UTC())
}

func parseUUID(s string) (pgtype.UUID, error) {
	var u pgtype.UUID
	if err := u.Scan(s); err != nil {
		return pgtype.UUID{}, err
	}
	return u, nil
}

// uuidStr formata um pgtype.UUID na forma canônica (vazio quando inválido).
func uuidStr(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	b := u.Bytes
	const hex = "0123456789abcdef"
	buf := make([]byte, 36)
	j := 0
	for i := 0; i < 16; i++ {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			buf[j] = '-'
			j++
		}
		buf[j] = hex[b[i]>>4]
		buf[j+1] = hex[b[i]&0x0f]
		j += 2
	}
	return string(buf)
}

// setStatus reflete o status da integração no gauge de métrica.
func (s *Syncer) setStatus(platform, status string) {
	metrics.SetIntegrationStatus(platform, status)
}

// isUniqueViolation indica violação de UNIQUE no Postgres (SQLSTATE 23505).
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

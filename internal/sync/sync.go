// Package sync coordena a sincronização de bibliotecas e conquistas com as plataformas.
//
// Papéis:
//   - Connect*: valida credenciais, grava cifrado e enfileira o full sync inicial.
//   - Enqueue*: enfileira jobs de sync (plataforma/jogo) com dedupe.
//   - HandleJob: executado pelo worker; despacha o job para o syncer da plataforma.
//
// O diff de conquistas só gera unlock_events DEPOIS que o full sync inicial da integração
// terminou (integrations.initial_sync_done). O sync inicial popula o estado sem gerar eventos.
package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
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

// ErrNotConnected indica que não há integração conectada para a plataforma.
var ErrNotConnected = errors.New("integração não conectada")

// SteamAPI é a superfície do cliente Steam usada pelo syncer (mockável em testes).
type SteamAPI interface {
	GetOwnedGames(ctx context.Context, steamID string) ([]steam.OwnedGame, error)
	GetPlayerAchievements(ctx context.Context, steamID string, appID int) ([]steam.PlayerAchievement, error)
	GetSchemaForGame(ctx context.Context, appID int) (string, []steam.SchemaAchievement, error)
	GetGlobalAchievementPercentages(ctx context.Context, appID int) (map[string]float64, error)
	GetPlayerSummary(ctx context.Context, steamID string) (steam.PlayerSummary, error)
	ResolveVanityURL(ctx context.Context, vanity string) (string, error)
}

// SteamFactory constrói um SteamAPI para uma dada API key.
type SteamFactory func(apiKey string) SteamAPI

// PlatformPayload é o payload do job sync_platform.
type PlatformPayload struct {
	Platform string `json:"platform"`
	Scope    string `json:"scope"`
	Trigger  string `json:"trigger"`
}

// GamePayload é o payload do job sync_game.
type GamePayload struct {
	GameID  string `json:"game_id"`
	Trigger string `json:"trigger"`
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
	logger       *slog.Logger
}

// Deps injeta as dependências do Syncer.
type Deps struct {
	Queries      *dbgen.Queries
	Pool         *pgxpool.Pool
	Queue        queue.Queue
	Cipher       *crypto.Cipher
	SteamFactory SteamFactory
	Logger       *slog.Logger
}

// New constrói o Syncer.
func New(d Deps) *Syncer {
	return &Syncer{
		q:            d.Queries,
		pool:         d.Pool,
		queue:        d.Queue,
		cipher:       d.Cipher,
		steamFactory: d.SteamFactory,
		logger:       d.Logger,
	}
}

// EnqueuePlatformSync enfileira um sync de plataforma (dedupe por plataforma+scope).
func (s *Syncer) EnqueuePlatformSync(ctx context.Context, platform, scope, trigger string) (*queue.Job, bool, error) {
	return s.queue.Enqueue(ctx, queue.EnqueueParams{
		Kind:      KindSyncPlatform,
		Payload:   PlatformPayload{Platform: platform, Scope: scope, Trigger: trigger},
		DedupeKey: fmt.Sprintf("%s:%s:%s", KindSyncPlatform, platform, scope),
	})
}

// EnqueueGameSync enfileira um sync de um jogo (dedupe por jogo).
func (s *Syncer) EnqueueGameSync(ctx context.Context, gameID, trigger string) (*queue.Job, bool, error) {
	return s.queue.Enqueue(ctx, queue.EnqueueParams{
		Kind:      KindSyncGame,
		Payload:   GamePayload{GameID: gameID, Trigger: trigger},
		DedupeKey: fmt.Sprintf("%s:%s", KindSyncGame, gameID),
	})
}

// HandleJob despacha um job reivindicado da fila para o syncer da plataforma.
func (s *Syncer) HandleJob(ctx context.Context, job queue.Job) error {
	switch job.Kind {
	case KindSyncPlatform:
		var p PlatformPayload
		if err := json.Unmarshal(job.Payload, &p); err != nil {
			return fmt.Errorf("payload sync_platform inválido: %w", err)
		}
		if p.Platform != "steam" {
			s.logger.Warn("plataforma ainda não suportada no sync", slog.String("platform", p.Platform))
			return nil
		}
		return s.syncSteam(ctx, p.Scope, p.Trigger, "")
	case KindSyncGame:
		var p GamePayload
		if err := json.Unmarshal(job.Payload, &p); err != nil {
			return fmt.Errorf("payload sync_game inválido: %w", err)
		}
		return s.syncSteam(ctx, ScopeGame, p.Trigger, p.GameID)
	default:
		return fmt.Errorf("kind de job desconhecido: %s", job.Kind)
	}
}

// loadSteamClient carrega a integração Steam, decifra as credenciais e constrói o cliente.
func (s *Syncer) loadSteam(ctx context.Context) (dbgen.Integration, SteamAPI, steamCredentials, error) {
	integ, err := s.q.GetIntegration(ctx, "steam")
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

// metricStatus reflete o status da integração no gauge de métrica.
func (s *Syncer) setStatus(platform, status string) {
	metrics.SetIntegrationStatus(platform, status)
}

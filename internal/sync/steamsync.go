package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	dbgen "github.com/RodolfoBonis/trophy-hunter-api/db/gen"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/integrations/steam"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/db"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/metrics"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/queue"
)

const (
	iconURLFmt  = "https://media.steampowered.com/steamcommunity/public/images/apps/%d/%s.jpg"
	coverURLFmt = "https://cdn.akamai.steamstatic.com/steam/apps/%d/library_600x900.jpg"
)

// handlePlatform executa a etapa de fan-out: lista a biblioteca, upserta as identidades e emite um
// sync_game por jogo que precisa de trabalho, selando o run pai ao final.
func (s *Syncer) handlePlatform(ctx context.Context, p PlatformPayload) error {
	runID, err := parseUUID(p.SyncRunID)
	if err != nil {
		return fmt.Errorf("sync_run_id inválido: %w", err)
	}

	if p.Platform != "steam" {
		s.logger.Warn("plataforma ainda não suportada no sync", slog.String("platform", p.Platform))
		s.closeEmptyRun(ctx, runID)
		return nil
	}

	integID, err := s.platformIntegrationID(ctx, p)
	if err != nil {
		s.failRun(ctx, runID, "integração não conectada")
		return nil //nolint:nilerr // conta removida: nada a fazer, o run fecha como erro
	}
	integ, client, creds, err := s.loadSteam(ctx, integID)
	if errors.Is(err, ErrNotConnected) {
		s.logger.Info("sync steam ignorado: integração não conectada")
		s.failRun(ctx, runID, "integração não conectada")
		return nil
	}
	if err != nil {
		s.failRun(ctx, runID, "erro interno ao carregar integração")
		return queue.Permanent(err)
	}

	owned, err := client.GetOwnedGames(ctx, creds.SteamID)
	if err != nil {
		s.failPlatform(ctx, runID, integID, err)
		return queue.Permanent(err) // o cliente já retentou 429/5xx; não rodar retry de job em cima
	}

	if len(owned) == 0 {
		// Perfil privado devolve {"response":{}} → biblioteca "vazia". Confirme a visibilidade antes
		// de concluir, para não marcar initial_sync_done nem voltar a status connected (finding 6).
		summary, serr := client.GetPlayerSummary(ctx, creds.SteamID)
		if serr != nil {
			s.failPlatform(ctx, runID, integID, serr)
			return queue.Permanent(serr)
		}
		if !summary.IsPublic() {
			s.setPermanentStatus(ctx, integID, steam.ErrProfilePrivate)
			s.failRun(ctx, runID, "perfil Steam não é público")
			return queue.Permanent(steam.ErrProfilePrivate)
		}
		s.sealAndMaybeClose(ctx, runID, 0, p.Scope) // biblioteca pública genuinamente vazia
		return nil
	}

	createEvents := integ.InitialSyncDone
	total := 0
	for _, og := range owned {
		prior, priorErr := s.q.GetGameByExternal(ctx, dbgen.GetGameByExternalParams{
			IntegrationID: integID, ExternalID: strconv.Itoa(og.AppID),
		})
		isNew := errors.Is(priorErr, pgx.ErrNoRows)
		if priorErr != nil && !isNew {
			s.failPlatform(ctx, runID, integID, priorErr)
			return queue.Permanent(fmt.Errorf("carregar jogo: %w", priorErr))
		}

		gameRow, uerr := s.upsertGameIdentity(ctx, integID, og)
		if uerr != nil {
			s.failPlatform(ctx, runID, integID, uerr)
			return queue.Permanent(fmt.Errorf("upsert jogo: %w", uerr))
		}

		if !og.HasCommunityVisibleStats {
			// Sem conquistas visíveis: não há filho; grava a marca d'água para não re-detectar sempre.
			if werr := s.q.SetGameWatermark(ctx, dbgen.SetGameWatermarkParams{
				ID: gameRow.ID, RemoteUpdatedAt: tsFromUnix(og.RtimeLastPlayed),
			}); werr != nil {
				s.logger.Warn("falha ao gravar marca d'água (sem stats)", slog.String("err", werr.Error()))
			}
			continue
		}

		need := p.Scope == ScopeFull || isNew || remoteChanged(prior, og)
		if !need {
			continue
		}

		_, enq, eerr := s.queue.Enqueue(ctx, queue.EnqueueParams{
			Kind: KindSyncGame,
			Payload: GamePayload{
				GameID: uuidStr(gameRow.ID), Trigger: p.Trigger, Scope: p.Scope,
				SyncRunID: p.SyncRunID, CreateEvents: createEvents,
				HasSnapshot: true, AppID: og.AppID, Playtime: og.PlaytimeForever,
				Rtime: og.RtimeLastPlayed, HasStats: true,
			},
			DedupeKey: fmt.Sprintf("%s:%s", KindSyncGame, uuidStr(gameRow.ID)),
		})
		if eerr != nil {
			s.failPlatform(ctx, runID, integID, eerr)
			return queue.Permanent(fmt.Errorf("enfileirar sync_game: %w", eerr))
		}
		if enq {
			total++
		}
	}

	s.sealAndMaybeClose(ctx, runID, total, p.Scope)
	return nil
}

// handleGame sincroniza UM jogo (filho do fan-out ou run manual) e contabiliza o término no run.
func (s *Syncer) handleGame(ctx context.Context, job queue.Job, p GamePayload) error {
	runID, err := parseUUID(p.SyncRunID)
	if err != nil {
		return fmt.Errorf("sync_run_id inválido: %w", err)
	}

	gameUUID, err := parseUUID(p.GameID)
	if err != nil {
		s.finalizeChild(ctx, runID, true, 0)
		return nil //nolint:nilerr // payload inválido: contabiliza como falha e descarta
	}
	game, err := s.q.GetGameByID(ctx, gameUUID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			s.finalizeChild(ctx, runID, true, 0) // jogo/conta removidos no meio do sync
			return nil
		}
		return s.gameTransient(ctx, job, runID, pgtype.UUID{}, fmt.Errorf("carregar jogo: %w", err))
	}
	integID := game.IntegrationID

	_, client, creds, err := s.loadSteam(ctx, integID)
	if errors.Is(err, ErrNotConnected) {
		s.finalizeChild(ctx, runID, true, 0)
		return nil
	}
	if err != nil {
		return s.gameTransient(ctx, job, runID, integID, err)
	}

	appID := p.AppID
	if appID == 0 {
		appID, _ = strconv.Atoi(game.ExternalID)
	}

	rtime := p.Rtime
	hasStats := p.HasStats
	if !p.HasSnapshot {
		// Sync manual: busca o owned-games para obter o estado remoto (marca d'água) e has_stats.
		owned, oerr := client.GetOwnedGames(ctx, creds.SteamID)
		if oerr != nil {
			return s.gameTransient(ctx, job, runID, integID, oerr)
		}
		found := false
		for _, og := range owned {
			if og.AppID == appID {
				rtime, hasStats, found = og.RtimeLastPlayed, og.HasCommunityVisibleStats, true
				break
			}
		}
		if !found {
			s.finalizeChild(ctx, runID, false, 0) // não está mais na biblioteca
			return nil
		}
	}

	if !hasStats {
		if herr := s.q.SetGameHasAchievements(ctx, dbgen.SetGameHasAchievementsParams{ID: game.ID, HasAchievements: false}); herr != nil {
			s.logger.Warn("falha ao marcar sem conquistas", slog.String("err", herr.Error()))
		}
		s.setWatermark(ctx, game.ID, rtime)
		s.finalizeChild(ctx, runID, false, 0)
		return nil
	}

	fetchSchema, fetchAch, fetchPct := decideFetchesChild(p.Scope, game)
	changed, gerr := s.syncGameTrophies(ctx, client, creds.SteamID, game, fetchSchema, fetchAch, fetchPct, p.CreateEvents)
	switch {
	case gerr == nil:
		s.setWatermark(ctx, game.ID, rtime) // marca d'água SÓ após sucesso (finding 5)
		s.finalizeChild(ctx, runID, false, changed)
		return nil
	case errors.Is(gerr, steam.ErrNoStats):
		if herr := s.q.SetGameHasAchievements(ctx, dbgen.SetGameHasAchievementsParams{ID: game.ID, HasAchievements: false}); herr != nil {
			s.logger.Warn("falha ao marcar sem conquistas", slog.String("err", herr.Error()))
		}
		s.setWatermark(ctx, game.ID, rtime)
		s.finalizeChild(ctx, runID, false, changed)
		return nil
	case errors.Is(gerr, steam.ErrProfilePrivate), errors.Is(gerr, steam.ErrInvalidKey):
		s.setPermanentStatus(ctx, integID, gerr) // permanente: status já, job morre imediatamente
		s.finalizeChild(ctx, runID, true, changed)
		return queue.Permanent(gerr)
	default:
		return s.gameTransient(ctx, job, runID, integID, gerr)
	}
}

// gameTransient trata um erro transitório por jogo: re-tenta enquanto houver tentativas; na última
// (job já virará dead) contabiliza a falha e só então marca a integração em erro (finding 9).
func (s *Syncer) gameTransient(ctx context.Context, job queue.Job, runID, integID pgtype.UUID, cause error) error {
	if job.Attempts >= job.MaxAttempts {
		s.finalizeChild(ctx, runID, true, 0)
		if integID.Valid {
			s.setTransientErrorStatus(ctx, integID, cause)
		}
		return cause
	}
	s.logger.Warn("falha transitória ao sincronizar jogo; vai re-tentar",
		slog.String("job_id", job.ID), slog.String("err", cause.Error()))
	return cause
}

// decideFetchesChild resolve o que buscar para o jogo conforme o escopo e a idade dos dados.
func decideFetchesChild(scope string, game dbgen.Game) (schema, ach, pct bool) {
	ach = true // o filho existe justamente para sincronizar conquistas
	schema = scope == ScopeFull || olderThan(game.SchemaSyncedAt, schemaMaxAge)
	pct = scope == ScopeFull || olderThan(game.PercentagesSyncedAt, percentagesMaxAge)
	return schema, ach, pct
}

// syncGameTrophies busca schema/conquistas/raridade de um jogo e aplica o diff.
func (s *Syncer) syncGameTrophies(ctx context.Context, client SteamAPI, steamID string, game dbgen.Game, fetchSchema, fetchAch, fetchPct, createEvents bool) (changed int, err error) {
	appID, _ := strconv.Atoi(game.ExternalID)

	if fetchSchema {
		_, defs, serr := client.GetSchemaForGame(ctx, appID)
		if serr != nil {
			return 0, serr
		}
		if len(defs) == 0 {
			// Schema vazio: o jogo não tem conquistas. Preserva has_achievements=false (finding 18).
			if herr := s.q.SetGameHasAchievements(ctx, dbgen.SetGameHasAchievementsParams{ID: game.ID, HasAchievements: false}); herr != nil {
				return 0, herr
			}
			return 0, nil
		}
		grp, gerr := s.q.UpsertTrophyGroup(ctx, dbgen.UpsertTrophyGroupParams{
			GameID: game.ID, ExternalID: "default", Name: "Base Game", IsBase: true,
		})
		if gerr != nil {
			return 0, fmt.Errorf("upsert grupo: %w", gerr)
		}
		for _, d := range defs {
			if _, derr := s.q.UpsertTrophyDefinition(ctx, dbgen.UpsertTrophyDefinitionParams{
				GameID: game.ID, GroupID: grp.ID, ExternalID: d.Name, Name: d.DisplayName,
				Description: d.Description, Type: "achievement", Hidden: d.Hidden,
				Icon: d.Icon, IconLocked: d.IconGray,
			}); derr != nil {
				return 0, fmt.Errorf("upsert definição: %w", derr)
			}
		}
		// Schema tem conquistas → has_achievements=true. Steam não tem platina.
		if serr := s.q.SetGameSchemaSynced(ctx, dbgen.SetGameSchemaSyncedParams{ID: game.ID, HasPlatinum: false}); serr != nil {
			return 0, serr
		}
		if herr := s.q.SetGameHasAchievements(ctx, dbgen.SetGameHasAchievementsParams{ID: game.ID, HasAchievements: true}); herr != nil {
			return 0, herr
		}
	}

	// Carrega os troféus APÓS o upsert do schema (inclui definições novas) para o diff/raridade.
	trophies, err := s.q.ListTrophiesByGame(ctx, game.ID)
	if err != nil {
		return 0, fmt.Errorf("listar troféus: %w", err)
	}
	byExt := make(map[string]dbgen.Trophy, len(trophies))
	for _, t := range trophies {
		byExt[t.ExternalID] = t
	}

	if fetchAch {
		achs, aerr := client.GetPlayerAchievements(ctx, steamID, appID)
		if aerr != nil {
			return changed, aerr
		}
		for _, a := range achs {
			t, ok := byExt[a.APIName]
			if !ok || a.Achieved == t.Earned {
				continue
			}
			var earnedAt pgtype.Timestamptz
			if a.Achieved {
				earnedAt = tsFromUnix(a.UnlockTime)
			}
			// SetTrophyEarned + InsertUnlockEvent na MESMA transação (finding 18): ou os dois
			// efeitos acontecem, ou nenhum — nunca um troféu "earned" sem o evento (ou vice-versa).
			emitEvent := a.Achieved && !t.Earned && createEvents
			if terr := s.applyEarned(ctx, t.ID, a.Achieved, earnedAt, emitEvent); terr != nil {
				return changed, fmt.Errorf("aplicar earned: %w", terr)
			}
			changed++
		}
	}

	if fetchPct {
		pct, perr := client.GetGlobalAchievementPercentages(ctx, appID)
		if perr != nil {
			s.logger.Warn("falha ao buscar raridade global", slog.Int("appid", appID), slog.String("err", perr.Error()))
		} else {
			for ext, t := range byExt {
				if pv, ok := pct[ext]; ok {
					val := pv
					if serr := s.q.SetTrophyRarity(ctx, dbgen.SetTrophyRarityParams{ID: t.ID, RarityPct: &val}); serr != nil {
						return changed, fmt.Errorf("set rarity: %w", serr)
					}
				}
			}
			if serr := s.q.SetGamePercentagesSynced(ctx, game.ID); serr != nil {
				return changed, serr
			}
		}
	}

	return changed, nil
}

// applyEarned grava o earned e, quando emitEvent, insere o unlock_event na mesma transação.
func (s *Syncer) applyEarned(ctx context.Context, trophyID pgtype.UUID, achieved bool, earnedAt pgtype.Timestamptz, emitEvent bool) error {
	return db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		if err := q.SetTrophyEarned(ctx, dbgen.SetTrophyEarnedParams{ID: trophyID, Earned: achieved, EarnedAt: earnedAt}); err != nil {
			return err
		}
		if emitEvent {
			if err := q.InsertUnlockEvent(ctx, trophyID); err != nil {
				return err
			}
		}
		return nil
	})
}

// upsertGameIdentity insere/atualiza identidade + exibição de um jogo Steam (sem tocar na marca
// d'água nem reverter has_achievements — ver UpsertGameIdentity).
func (s *Syncer) upsertGameIdentity(ctx context.Context, integID pgtype.UUID, og steam.OwnedGame) (dbgen.Game, error) {
	icon := ""
	if og.ImgIconURL != "" {
		icon = fmt.Sprintf(iconURLFmt, og.AppID, og.ImgIconURL)
	}
	return s.q.UpsertGameIdentity(ctx, dbgen.UpsertGameIdentityParams{
		IntegrationID:   integID,
		Platform:        "steam",
		ExternalID:      strconv.Itoa(og.AppID),
		NpServiceName:   nil,
		Title:           og.Name,
		NormalizedTitle: NormalizeTitle(og.Name),
		Icon:            icon,
		Cover:           fmt.Sprintf(coverURLFmt, og.AppID),
		Platforms:       []string{},
		HasAchievements: og.HasCommunityVisibleStats, // só vale p/ INSERT; no conflito é preservado
		LastPlayedAt:    tsFromUnix(og.RtimeLastPlayed),
		PlaytimeMinutes: int32(og.PlaytimeForever), //nolint:gosec // playtime em minutos cabe em int32
	})
}

// --- bookkeeping do run / integração (roda em contexto desacoplado do cancelamento do job) ---

func (s *Syncer) setWatermark(ctx context.Context, gameID pgtype.UUID, rtime int64) {
	if err := s.q.SetGameWatermark(ctx, dbgen.SetGameWatermarkParams{ID: gameID, RemoteUpdatedAt: tsFromUnix(rtime)}); err != nil {
		s.logger.Warn("falha ao gravar marca d'água", slog.String("err", err.Error()))
	}
}

func (s *Syncer) sealAndMaybeClose(ctx context.Context, runID pgtype.UUID, total int, scope string) {
	bgctx, cancel := bg(ctx)
	defer cancel()
	row, err := s.q.SealSyncRun(bgctx, dbgen.SealSyncRunParams{ID: runID, GamesTotal: int32(total)}) //nolint:gosec // total pequeno
	if err != nil {
		s.logger.Error("falha ao selar sync_run", slog.String("err", err.Error()))
		return
	}
	s.logger.Info("fan-out selado", slog.String("scope", scope), slog.Int("games_total", total))
	if row.Closed {
		s.onRunClose(bgctx, row.IntegrationID, row.Scope, row.GamesFailed)
	}
}

func (s *Syncer) finalizeChild(ctx context.Context, runID pgtype.UUID, failed bool, changed int) {
	bgctx, cancel := bg(ctx)
	defer cancel()
	failedN := int32(0)
	if failed {
		failedN = 1
	}
	row, err := s.q.FinalizeSyncRunChild(bgctx, dbgen.FinalizeSyncRunChildParams{
		ID: runID, Failed: failedN, Changed: int32(changed), //nolint:gosec // contagens pequenas
	})
	if err != nil {
		s.logger.Error("falha ao finalizar filho do sync_run", slog.String("err", err.Error()))
		return
	}
	if row.Closed {
		s.onRunClose(bgctx, row.IntegrationID, row.Scope, row.GamesFailed)
	}
}

// onRunClose roda uma única vez por run (quem observa closed==true) e reflete o resultado na
// integração e na métrica.
func (s *Syncer) onRunClose(ctx context.Context, integID pgtype.UUID, scope string, gamesFailed int32) {
	if !integID.Valid {
		return // conta removida (sync_runs.integration_id ON DELETE SET NULL)
	}
	if gamesFailed == 0 {
		metrics.SyncRun(scope, "ok")
		if err := s.q.MarkIntegrationSynced(ctx, dbgen.MarkIntegrationSyncedParams{
			ID: integID, MarkInitial: scope == ScopeFull,
		}); err != nil {
			s.logger.Error("falha ao marcar integração sincronizada", slog.String("err", err.Error()))
			return
		}
		s.setStatus("steam", "connected")
		return
	}
	metrics.SyncRun(scope, "error")
	// Falhas: só atualiza last_synced_at; o status de erro foi definido pelos filhos mortos.
	if err := s.q.TouchIntegrationSynced(ctx, integID); err != nil {
		s.logger.Error("falha ao tocar last_synced_at", slog.String("err", err.Error()))
	}
}

func (s *Syncer) closeEmptyRun(ctx context.Context, runID pgtype.UUID) {
	bgctx, cancel := bg(ctx)
	defer cancel()
	if _, err := s.q.SealSyncRun(bgctx, dbgen.SealSyncRunParams{ID: runID, GamesTotal: 0}); err != nil {
		s.logger.Warn("falha ao fechar run vazio", slog.String("err", err.Error()))
	}
}

func (s *Syncer) failRun(ctx context.Context, runID pgtype.UUID, msg string) {
	bgctx, cancel := bg(ctx)
	defer cancel()
	if err := s.q.FailSyncRun(bgctx, dbgen.FailSyncRunParams{ID: runID, Error: truncateErr(msg)}); err != nil {
		s.logger.Warn("falha ao marcar sync_run com erro", slog.String("err", err.Error()))
	}
}

// failPlatform fecha o run com erro e reflete o erro na integração (permanente ou transitório).
func (s *Syncer) failPlatform(ctx context.Context, runID, integID pgtype.UUID, cause error) {
	msg := "falha ao listar biblioteca Steam"
	switch {
	case errors.Is(cause, steam.ErrProfilePrivate):
		s.setPermanentStatus(ctx, integID, cause)
		msg = "perfil Steam não é público"
	case errors.Is(cause, steam.ErrInvalidKey):
		s.setPermanentStatus(ctx, integID, cause)
		msg = "API key Steam inválida ou sem permissão"
	default:
		s.setTransientErrorStatus(ctx, integID, cause)
	}
	s.failRun(ctx, runID, msg)
}

// setPermanentStatus define o status de erro definitivo da integração com mensagem sanitizada.
func (s *Syncer) setPermanentStatus(ctx context.Context, integID pgtype.UUID, cause error) {
	status, msg := "error", "erro ao sincronizar"
	switch {
	case errors.Is(cause, steam.ErrProfilePrivate):
		status, msg = "error_private", "perfil Steam não é público"
	case errors.Is(cause, steam.ErrInvalidKey):
		status, msg = "error", "API key Steam inválida ou sem permissão"
	}
	s.applyIntegrationStatus(ctx, integID, status, msg)
}

// setTransientErrorStatus marca a integração em erro (chamado só quando o job já morreu).
func (s *Syncer) setTransientErrorStatus(ctx context.Context, integID pgtype.UUID, _ error) {
	s.applyIntegrationStatus(ctx, integID, "error", "falha ao sincronizar; tente novamente")
}

func (s *Syncer) applyIntegrationStatus(ctx context.Context, integID pgtype.UUID, status, msg string) {
	bgctx, cancel := bg(ctx)
	defer cancel()
	if err := s.q.UpdateIntegrationStatus(ctx, dbgen.UpdateIntegrationStatusParams{
		ID: integID, Status: status, LastError: truncateErr(msg),
	}); err != nil {
		// tenta de novo no contexto desacoplado
		if err2 := s.q.UpdateIntegrationStatus(bgctx, dbgen.UpdateIntegrationStatusParams{
			ID: integID, Status: status, LastError: truncateErr(msg),
		}); err2 != nil {
			s.logger.Error("falha ao atualizar status da integração", slog.String("err", err2.Error()))
		}
	}
	s.setStatus("steam", status)
}

// remoteChanged indica se a marca d'água (remote_updated_at) difere do rtime remoto atual.
func remoteChanged(prior dbgen.Game, og steam.OwnedGame) bool {
	newRt := tsFromUnix(og.RtimeLastPlayed)
	if prior.RemoteUpdatedAt.Valid != newRt.Valid {
		return true
	}
	if newRt.Valid && !prior.RemoteUpdatedAt.Time.Equal(newRt.Time) {
		return true
	}
	return false
}

// olderThan indica se um timestamp é mais antigo que d (ou nunca definido).
func olderThan(t pgtype.Timestamptz, d time.Duration) bool {
	if !t.Valid {
		return true
	}
	return time.Since(t.Time) > d
}

var (
	trademarkRe   = regexp.MustCompile(`[™®©]`)
	nonAlnumRe    = regexp.MustCompile(`[^a-z0-9]+`)
	multiSpaceRe  = regexp.MustCompile(`\s+`)
	editionWordRe = regexp.MustCompile(`\b(remastered|remaster|goty|game of the year|definitive|deluxe|complete|enhanced|ps5|ps4|edition)\b`)
)

// NormalizeTitle normaliza um título para busca/unificação (lower, sem símbolos/edições/pontuação).
// Exportada para a busca da biblioteca usar a MESMA normalização de normalized_title (finding 19).
func NormalizeTitle(s string) string {
	s = strings.ToLower(s)
	s = trademarkRe.ReplaceAllString(s, "")
	s = editionWordRe.ReplaceAllString(s, " ")
	s = nonAlnumRe.ReplaceAllString(s, " ")
	s = multiSpaceRe.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// EscapeLike escapa %, _ e \ para uso seguro em padrões ILIKE (ESCAPE '\'). Defensivo: após
// NormalizeTitle não restam esses caracteres, mas mantém a query robusta a qualquer entrada.
func EscapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

func truncateErr(s string) string {
	const n = 1000
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut]
}

// platformIntegrationID devolve a conta do job. Jobs enfileirados antes da migration 00013 (multi-
// conta) só carregam a plataforma: nesse caso usa a conta mais antiga da plataforma.
func (s *Syncer) platformIntegrationID(ctx context.Context, p PlatformPayload) (pgtype.UUID, error) {
	if p.IntegrationID != "" {
		return parseUUID(p.IntegrationID)
	}
	integs, err := s.q.ListIntegrationsByPlatform(ctx, p.Platform)
	if err != nil {
		return pgtype.UUID{}, err
	}
	if len(integs) == 0 {
		return pgtype.UUID{}, ErrNotConnected
	}
	return integs[0].ID, nil
}

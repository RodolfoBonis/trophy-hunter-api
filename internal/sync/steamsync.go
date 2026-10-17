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
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/metrics"
)

const (
	iconURLFmt  = "https://media.steampowered.com/steamcommunity/public/images/apps/%d/%s.jpg"
	coverURLFmt = "https://cdn.akamai.steamstatic.com/steam/apps/%d/library_600x900.jpg"
)

// syncSteam executa uma sincronização Steam, registrando o sync_run e ajustando o status da
// integração. singleGameID != "" restringe a um único jogo (scope game).
func (s *Syncer) syncSteam(ctx context.Context, scope, trigger, singleGameID string) error {
	integ, client, creds, err := s.loadSteam(ctx)
	if errors.Is(err, ErrNotConnected) {
		s.logger.Info("sync steam ignorado: integração não conectada")
		return nil
	}
	if err != nil {
		return err
	}

	run, err := s.q.StartSyncRun(ctx, dbgen.StartSyncRunParams{
		IntegrationID: integ.ID, Platform: "steam", Scope: scope, Trigger: trigger,
	})
	if err != nil {
		return fmt.Errorf("iniciar sync_run: %w", err)
	}

	changed, touched, syncErr := s.runSteam(ctx, integ, client, creds, scope, singleGameID)

	status, errMsg := "ok", ""
	if syncErr != nil {
		status, errMsg = "error", syncErr.Error()
	}
	if ferr := s.q.FinishSyncRun(ctx, dbgen.FinishSyncRunParams{
		ID: run.ID, Status: status,
		ItemsChanged: int32(changed), GamesTouched: int32(touched), //nolint:gosec // contagens pequenas
		Error: truncateErr(errMsg),
	}); ferr != nil {
		s.logger.Error("falha ao finalizar sync_run", slog.String("err", ferr.Error()))
	}
	metrics.SyncRun(scope, status)

	if syncErr != nil {
		s.applyErrorStatus(ctx, syncErr)
		return syncErr
	}

	if err := s.q.MarkIntegrationSynced(ctx, dbgen.MarkIntegrationSyncedParams{
		Platform: "steam", MarkInitial: scope == ScopeFull,
	}); err != nil {
		return fmt.Errorf("marcar sincronizado: %w", err)
	}
	s.setStatus("steam", "connected")
	return nil
}

// applyErrorStatus traduz o erro do sync num status de integração + métrica.
func (s *Syncer) applyErrorStatus(ctx context.Context, syncErr error) {
	status, msg := "error", syncErr.Error()
	switch {
	case errors.Is(syncErr, steam.ErrProfilePrivate):
		status, msg = "error_private", "perfil Steam não é público"
	case errors.Is(syncErr, steam.ErrInvalidKey):
		status, msg = "error", "API key Steam inválida"
	}
	if err := s.q.UpdateIntegrationStatus(ctx, dbgen.UpdateIntegrationStatusParams{
		Platform: "steam", Status: status, LastError: truncateErr(msg),
	}); err != nil {
		s.logger.Error("falha ao atualizar status da integração", slog.String("err", err.Error()))
	}
	s.setStatus("steam", status)
}

// runSteam percorre a biblioteca e sincroniza cada jogo conforme o escopo. Devolve o total de
// troféus alterados e de jogos tocados.
func (s *Syncer) runSteam(ctx context.Context, integ dbgen.Integration, client SteamAPI, creds steamCredentials, scope, singleGameID string) (changed, touched int, err error) {
	owned, err := client.GetOwnedGames(ctx, creds.SteamID)
	if err != nil {
		return 0, 0, fmt.Errorf("owned games: %w", err)
	}
	createEvents := integ.InitialSyncDone // eventos só após o full sync inicial

	onlyAppID := -1
	if singleGameID != "" {
		id, perr := parseUUID(singleGameID)
		if perr != nil {
			return 0, 0, fmt.Errorf("game id inválido: %w", perr)
		}
		g, gerr := s.q.GetGameByID(ctx, id)
		if gerr != nil {
			return 0, 0, fmt.Errorf("jogo não encontrado: %w", gerr)
		}
		onlyAppID, _ = strconv.Atoi(g.ExternalID)
	}

	for _, og := range owned {
		if onlyAppID >= 0 && og.AppID != onlyAppID {
			continue
		}

		prior, priorErr := s.q.GetGameByExternal(ctx, dbgen.GetGameByExternalParams{
			Platform: "steam", ExternalID: strconv.Itoa(og.AppID),
		})
		isNew := errors.Is(priorErr, pgx.ErrNoRows)
		if priorErr != nil && !isNew {
			return changed, touched, fmt.Errorf("carregar jogo: %w", priorErr)
		}

		gameChanged := isNew || remoteChanged(prior, og)
		hasStats := og.HasCommunityVisibleStats

		gameRow, uerr := s.upsertGame(ctx, og, hasStats)
		if uerr != nil {
			return changed, touched, fmt.Errorf("upsert jogo: %w", uerr)
		}
		touched++

		if !hasStats {
			continue // sem estatísticas visíveis: sem troféus a buscar
		}

		fetchSchema, fetchAch, fetchPct := s.decideFetches(scope, isNew, gameChanged, prior)
		if !fetchSchema && !fetchAch && !fetchPct {
			continue
		}

		n, gerr := s.syncGameTrophies(ctx, client, creds.SteamID, gameRow, fetchSchema, fetchAch, fetchPct, createEvents)
		switch {
		case gerr == nil:
			changed += n
		case errors.Is(gerr, steam.ErrProfilePrivate), errors.Is(gerr, steam.ErrInvalidKey):
			return changed, touched, gerr // fatais: abortam o sync inteiro
		case errors.Is(gerr, steam.ErrNoStats):
			if herr := s.q.SetGameHasAchievements(ctx, dbgen.SetGameHasAchievementsParams{ID: gameRow.ID, HasAchievements: false}); herr != nil {
				s.logger.Warn("falha ao marcar sem conquistas", slog.String("err", herr.Error()))
			}
		default:
			// Erro por jogo (ex.: schema indisponível): loga e segue.
			s.logger.Warn("falha ao sincronizar jogo",
				slog.Int("appid", og.AppID), slog.String("err", gerr.Error()))
		}
	}
	return changed, touched, nil
}

// decideFetches resolve o que buscar para um jogo conforme o escopo e a idade dos dados.
func (s *Syncer) decideFetches(scope string, isNew, gameChanged bool, prior dbgen.Game) (schema, ach, pct bool) {
	switch scope {
	case ScopeFull:
		return true, true, true
	case ScopeGame:
		return isNew || olderThan(prior.SchemaSyncedAt, schemaMaxAge), true, true
	default: // incremental
		ach = gameChanged
		schema = gameChanged && (isNew || olderThan(prior.SchemaSyncedAt, schemaMaxAge))
		pct = gameChanged && olderThan(prior.PercentagesSyncedAt, percentagesMaxAge)
		return schema, ach, pct
	}
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
		// Steam não tem platina.
		if serr := s.q.SetGameSchemaSynced(ctx, dbgen.SetGameSchemaSyncedParams{ID: game.ID, HasPlatinum: false}); serr != nil {
			return 0, serr
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
			if !ok {
				continue
			}
			if a.Achieved == t.Earned {
				continue
			}
			var earnedAt pgtype.Timestamptz
			if a.Achieved {
				earnedAt = tsFromUnix(a.UnlockTime)
			}
			if serr := s.q.SetTrophyEarned(ctx, dbgen.SetTrophyEarnedParams{ID: t.ID, Earned: a.Achieved, EarnedAt: earnedAt}); serr != nil {
				return changed, fmt.Errorf("set earned: %w", serr)
			}
			changed++
			if a.Achieved && !t.Earned && createEvents {
				if ierr := s.q.InsertUnlockEvent(ctx, t.ID); ierr != nil {
					s.logger.Warn("falha ao inserir unlock_event", slog.String("err", ierr.Error()))
				}
			}
		}
	}

	if fetchPct {
		pct, perr := client.GetGlobalAchievementPercentages(ctx, appID)
		if perr != nil {
			s.logger.Warn("falha ao buscar raridade global", slog.Int("appid", appID), slog.String("err", perr.Error()))
		} else {
			for ext, t := range byExt {
				if p, ok := pct[ext]; ok {
					val := p
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

// upsertGame insere/atualiza um jogo Steam a partir do item de GetOwnedGames.
func (s *Syncer) upsertGame(ctx context.Context, og steam.OwnedGame, hasStats bool) (dbgen.Game, error) {
	icon := ""
	if og.ImgIconURL != "" {
		icon = fmt.Sprintf(iconURLFmt, og.AppID, og.ImgIconURL)
	}
	return s.q.UpsertGame(ctx, dbgen.UpsertGameParams{
		Platform:        "steam",
		ExternalID:      strconv.Itoa(og.AppID),
		NpServiceName:   nil,
		Title:           og.Name,
		NormalizedTitle: normalizeTitle(og.Name),
		Icon:            icon,
		Cover:           fmt.Sprintf(coverURLFmt, og.AppID),
		Platforms:       []string{},
		HasAchievements: hasStats,
		LastPlayedAt:    tsFromUnix(og.RtimeLastPlayed),
		PlaytimeMinutes: int32(og.PlaytimeForever), //nolint:gosec // playtime em minutos cabe em int32
		RemoteUpdatedAt: tsFromUnix(og.RtimeLastPlayed),
	})
}

// remoteChanged indica se o estado remoto (playtime/último jogado) mudou desde o último sync.
func remoteChanged(prior dbgen.Game, og steam.OwnedGame) bool {
	if int(prior.PlaytimeMinutes) != og.PlaytimeForever {
		return true
	}
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

// normalizeTitle normaliza um título para busca/unificação (lower, sem símbolos/edições/pontuação).
func normalizeTitle(s string) string {
	s = strings.ToLower(s)
	s = trademarkRe.ReplaceAllString(s, "")
	s = editionWordRe.ReplaceAllString(s, " ")
	s = nonAlnumRe.ReplaceAllString(s, " ")
	s = multiSpaceRe.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

func truncateErr(s string) string {
	const n = 1000
	if len(s) <= n {
		return s
	}
	return s[:n]
}

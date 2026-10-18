package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	dbgen "github.com/RodolfoBonis/trophy-hunter-api/db/gen"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/integrations/psn"
	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/queue"
)

// PSNAPI é a superfície do cliente PSN usada pelo syncer (mockável). Uma instância por processo:
// o rate limit é dela.
type PSNAPI interface {
	ExchangeNPSSO(ctx context.Context, npsso string) (psn.Tokens, error)
	Refresh(ctx context.Context, refreshToken string) (psn.Tokens, error)
	TrophySummary(ctx context.Context, token string) (psn.Summary, error)
	Profile(ctx context.Context, token, accountID string) (psn.Profile, error)
	TrophyTitles(ctx context.Context, token string) ([]psn.TrophyTitle, error)
	TitleTrophies(ctx context.Context, token, npCommID, service string) ([]psn.TrophyDef, error)
	TitleGroups(ctx context.Context, token, npCommID, service string) ([]psn.TrophyGroup, error)
	UserTrophies(ctx context.Context, token, npCommID, service string) ([]psn.UserTrophy, error)
}

// ErrAccountMismatch indica que o NPSSO colado na reconexão é de OUTRA conta PSN.
var ErrAccountMismatch = errors.New("o NPSSO é de outra conta PSN")

// psnCredentials é o segredo persistido (cifrado com AES-GCM, como todas as credenciais).
//
// O NPSSO também é guardado (cifrado), por decisão do usuário: o refresh token da PSN vale só ~10
// dias e a renovação não estende o prazo, enquanto o NPSSO dura ~2 meses. O NPSSO NÃO é usado a
// cada chamada — só para obter tokens novos quando o refresh está para vencer ou é recusado.
type psnCredentials struct {
	AccessToken      string    `json:"access_token"`
	AccessExpiresAt  time.Time `json:"access_expires_at"`
	RefreshToken     string    `json:"refresh_token"`
	RefreshExpiresAt time.Time `json:"refresh_expires_at"`
	AccountID        string    `json:"account_id"`
	NPSSO            string    `json:"npsso,omitempty"`
}

// LogValue nunca expõe os tokens.
func (psnCredentials) LogValue() slog.Value { return slog.StringValue("[REDACTED]") }

// accessSkew renova o access token se ele vencer nesse intervalo.
const accessSkew = 2 * time.Minute

// refreshSkew: com o NPSSO guardado, obtém tokens novos se o refresh vencer nesse intervalo.
const refreshSkew = 24 * time.Hour

// ConnectPSNParams descreve uma nova conta PSN.
type ConnectPSNParams struct {
	Label string
	NPSSO string
}

// ConnectPSN troca o NPSSO por tokens, identifica a conta, grava cifrado e enfileira o full sync.
func (s *Syncer) ConnectPSN(ctx context.Context, p ConnectPSNParams) (dbgen.Integration, error) {
	if s.psn == nil {
		return dbgen.Integration{}, errors.New("cliente PSN não configurado")
	}
	tok, err := s.psn.ExchangeNPSSO(ctx, p.NPSSO)
	if err != nil {
		return dbgen.Integration{}, err
	}
	summary, err := s.psn.TrophySummary(ctx, tok.AccessToken)
	if err != nil {
		return dbgen.Integration{}, err
	}
	if _, err := s.q.GetIntegrationByAccount(ctx, dbgen.GetIntegrationByAccountParams{Platform: "psn", AccountID: summary.AccountID}); err == nil {
		return dbgen.Integration{}, ErrAccountExists
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return dbgen.Integration{}, fmt.Errorf("checar conta existente: %w", err)
	}
	profile, perr := s.psn.Profile(ctx, tok.AccessToken, summary.AccountID)
	if perr != nil {
		s.logger.Warn("psn: perfil indisponível no connect", slog.String("err", perr.Error()))
	}
	display := profile.OnlineID
	if display == "" {
		display = "PSN " + summary.AccountID
	}
	creds := psnCredentials{
		AccessToken: tok.AccessToken, AccessExpiresAt: tok.AccessExpiresAt,
		RefreshToken: tok.RefreshToken, RefreshExpiresAt: tok.RefreshExpiresAt, AccountID: summary.AccountID,
		NPSSO: strings.TrimSpace(p.NPSSO),
	}
	blob, err := s.encryptPSN(creds)
	if err != nil {
		return dbgen.Integration{}, err
	}
	label := strings.TrimSpace(p.Label)
	if label == "" {
		label = display
	}
	integ, err := s.q.CreateIntegration(ctx, dbgen.CreateIntegrationParams{
		Platform: "psn", Label: label, AccountID: summary.AccountID, DisplayName: display, Avatar: profile.Avatar,
		CredentialsEnc: blob, KeyVersion: int32(s.cipher.Version()), //nolint:gosec // versão pequena
	})
	if err != nil {
		if isUniqueViolation(err) {
			return dbgen.Integration{}, ErrAccountExists
		}
		return dbgen.Integration{}, fmt.Errorf("gravar integração: %w", err)
	}
	if err := s.saveTokens(ctx, integ.ID, creds); err != nil {
		return dbgen.Integration{}, err
	}
	s.setStatus("psn", "connected")
	if _, _, err := s.EnqueuePlatformSync(ctx, integ.ID, ScopeFull, TriggerManual); err != nil {
		return dbgen.Integration{}, fmt.Errorf("enfileirar full sync: %w", err)
	}
	return s.q.GetIntegration(ctx, integ.ID)
}

// ReconnectPSN troca um NPSSO novo por tokens para uma conta existente (NPSSO expirado), confere que
// é a mesma conta, volta a "connected" e enfileira um incremental.
func (s *Syncer) ReconnectPSN(ctx context.Context, integrationID pgtype.UUID, npsso string) (dbgen.Integration, error) {
	integ, err := s.q.GetIntegration(ctx, integrationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return dbgen.Integration{}, ErrNotConnected
	}
	if err != nil {
		return dbgen.Integration{}, err
	}
	if integ.Platform != "psn" {
		return dbgen.Integration{}, errors.New("reconexão por NPSSO só vale para PSN")
	}
	tok, err := s.psn.ExchangeNPSSO(ctx, npsso)
	if err != nil {
		return dbgen.Integration{}, err
	}
	summary, err := s.psn.TrophySummary(ctx, tok.AccessToken)
	if err != nil {
		return dbgen.Integration{}, err
	}
	if summary.AccountID != integ.AccountID {
		return dbgen.Integration{}, ErrAccountMismatch
	}
	creds := psnCredentials{
		AccessToken: tok.AccessToken, AccessExpiresAt: tok.AccessExpiresAt,
		RefreshToken: tok.RefreshToken, RefreshExpiresAt: tok.RefreshExpiresAt, AccountID: summary.AccountID,
		NPSSO: strings.TrimSpace(npsso),
	}
	if err := s.saveTokens(ctx, integ.ID, creds); err != nil {
		return dbgen.Integration{}, err
	}
	if err := s.q.UpdateIntegrationStatus(ctx, dbgen.UpdateIntegrationStatusParams{ID: integ.ID, Status: "connected", LastError: ""}); err != nil {
		return dbgen.Integration{}, err
	}
	s.setStatus("psn", "connected")
	s.notifyStatus(ctx, integ.ID, "connected")
	if _, _, err := s.EnqueuePlatformSync(ctx, integ.ID, ScopeIncremental, TriggerManual); err != nil {
		return dbgen.Integration{}, fmt.Errorf("enfileirar sync: %w", err)
	}
	return s.q.GetIntegration(ctx, integ.ID)
}

// psnSession carrega as credenciais e garante um access token válido (renova pelo refresh e grava).
// Refresh inválido → ErrInvalidGrant (permanente: status needs_reconnect).
func (s *Syncer) psnSession(ctx context.Context, integID pgtype.UUID) (dbgen.Integration, psnCredentials, error) {
	integ, err := s.q.GetIntegration(ctx, integID)
	if errors.Is(err, pgx.ErrNoRows) {
		return dbgen.Integration{}, psnCredentials{}, ErrNotConnected
	}
	if err != nil {
		return dbgen.Integration{}, psnCredentials{}, err
	}
	plain, err := s.cipher.Decrypt(integ.CredentialsEnc, "psn")
	if err != nil {
		return dbgen.Integration{}, psnCredentials{}, fmt.Errorf("decifrar credenciais psn: %w", err)
	}
	var creds psnCredentials
	if err := json.Unmarshal(plain, &creds); err != nil {
		return dbgen.Integration{}, psnCredentials{}, fmt.Errorf("parse credenciais psn: %w", err)
	}
	// Refresh perto de vencer e NPSSO guardado: tokens novos antes de dar erro.
	if creds.NPSSO != "" && time.Until(creds.RefreshExpiresAt) < refreshSkew {
		renewed, rerr := s.reauthPSN(ctx, integ.ID, creds)
		if rerr == nil {
			return integ, renewed, nil
		}
		s.logger.Warn("psn: reautenticação pelo NPSSO falhou; tentando o refresh", slog.String("err", rerr.Error()))
	}
	if time.Until(creds.AccessExpiresAt) > accessSkew {
		return integ, creds, nil
	}
	creds, err = s.refreshPSN(ctx, integ.ID, creds)
	return integ, creds, err
}

// reauthPSN troca o NPSSO guardado por tokens novos. NPSSO expirado → ErrInvalidNPSSO (permanente).
func (s *Syncer) reauthPSN(ctx context.Context, integID pgtype.UUID, creds psnCredentials) (psnCredentials, error) {
	tok, err := s.psn.ExchangeNPSSO(ctx, creds.NPSSO)
	if err != nil {
		return creds, err
	}
	creds.AccessToken, creds.AccessExpiresAt = tok.AccessToken, tok.AccessExpiresAt
	creds.RefreshToken, creds.RefreshExpiresAt = tok.RefreshToken, tok.RefreshExpiresAt
	bgctx, cancel := bg(ctx)
	defer cancel()
	s.logger.Info("psn: tokens renovados pelo NPSSO guardado")
	return creds, s.saveTokens(bgctx, integID, creds)
}

func (s *Syncer) refreshPSN(ctx context.Context, integID pgtype.UUID, creds psnCredentials) (psnCredentials, error) {
	tok, err := s.psn.Refresh(ctx, creds.RefreshToken)
	if errors.Is(err, psn.ErrInvalidGrant) && creds.NPSSO != "" {
		return s.reauthPSN(ctx, integID, creds) // refresh recusado: tenta pelo NPSSO guardado
	}
	if err != nil {
		return creds, err
	}
	creds.AccessToken, creds.AccessExpiresAt = tok.AccessToken, tok.AccessExpiresAt
	if tok.RefreshToken != "" {
		creds.RefreshToken, creds.RefreshExpiresAt = tok.RefreshToken, tok.RefreshExpiresAt
	}
	bgctx, cancel := bg(ctx)
	defer cancel()
	return creds, s.saveTokens(bgctx, integID, creds)
}

func (s *Syncer) saveTokens(ctx context.Context, integID pgtype.UUID, creds psnCredentials) error {
	blob, err := s.encryptPSN(creds)
	if err != nil {
		return err
	}
	return s.q.UpdateIntegrationTokens(ctx, dbgen.UpdateIntegrationTokensParams{
		ID: integID, CredentialsEnc: blob, KeyVersion: int32(s.cipher.Version()), //nolint:gosec // versão pequena
		AccessTokenExpiresAt: ts(creds.AccessExpiresAt), RefreshTokenExpiresAt: ts(creds.RefreshExpiresAt),
	})
}

func (s *Syncer) encryptPSN(creds psnCredentials) ([]byte, error) {
	plain, err := json.Marshal(creds) //nolint:gosec // serializado só para cifragem imediata (AES-GCM)
	if err != nil {
		return nil, fmt.Errorf("serializar credenciais: %w", err)
	}
	return s.cipher.Encrypt(plain, "psn")
}

// withPSNAuth executa fn com o access token; se a API recusar (401), renova uma vez e tenta de novo.
func (s *Syncer) withPSNAuth(ctx context.Context, integID pgtype.UUID, creds *psnCredentials, fn func(token string) error) error {
	err := fn(creds.AccessToken)
	if !errors.Is(err, psn.ErrUnauthorized) {
		return err
	}
	renewed, rerr := s.refreshPSN(ctx, integID, *creds)
	if rerr != nil {
		return rerr
	}
	*creds = renewed
	return fn(creds.AccessToken)
}

// handlePSNPlatform: lista os títulos, upserta as identidades e emite um sync_game por título novo
// ou alterado (lastUpdatedDateTime ≠ marca d'água) — ou por todos no full.
func (s *Syncer) handlePSNPlatform(ctx context.Context, p PlatformPayload, runID, integID pgtype.UUID) error {
	integ, creds, err := s.psnSession(ctx, integID)
	if err != nil {
		s.failPSN(ctx, runID, integID, err)
		return queue.Permanent(err)
	}
	var titles []psn.TrophyTitle
	if err := s.withPSNAuth(ctx, integID, &creds, func(tok string) error {
		var e error
		titles, e = s.psn.TrophyTitles(ctx, tok)
		return e
	}); err != nil {
		s.failPSN(ctx, runID, integID, err)
		if isPSNPermanent(err) {
			return queue.Permanent(err)
		}
		return err
	}
	s.refreshPSNProfile(ctx, integID, &creds)

	createEvents := integ.InitialSyncDone
	total := 0
	for _, t := range titles {
		prior, priorErr := s.q.GetGameByExternal(ctx, dbgen.GetGameByExternalParams{IntegrationID: integID, ExternalID: t.NpCommunicationID})
		isNew := errors.Is(priorErr, pgx.ErrNoRows)
		if priorErr != nil && !isNew {
			s.failRun(ctx, runID, "erro ao carregar jogo")
			return queue.Permanent(fmt.Errorf("carregar jogo: %w", priorErr))
		}
		row, uerr := s.q.UpsertGameIdentity(ctx, dbgen.UpsertGameIdentityParams{
			IntegrationID: integID, Platform: "psn", ExternalID: t.NpCommunicationID, NpServiceName: strPtr(t.NpServiceName),
			Title: t.TrophyTitleName, NormalizedTitle: NormalizeTitle(t.TrophyTitleName),
			Icon: t.TrophyTitleIconURL, Cover: t.TrophyTitleIconURL, Hero: t.TrophyTitleIconURL,
			Platforms: splitPlatforms(t.TrophyTitlePlatform), HasAchievements: true,
			LastPlayedAt: ts(t.LastUpdatedDateTime), PlaytimeMinutes: 0,
		})
		if uerr != nil {
			s.failRun(ctx, runID, "erro ao gravar jogo")
			return queue.Permanent(fmt.Errorf("upsert jogo: %w", uerr))
		}
		changed := isNew || !prior.RemoteUpdatedAt.Valid || !prior.RemoteUpdatedAt.Time.Equal(t.LastUpdatedDateTime)
		if p.Scope != ScopeFull && !changed {
			continue
		}
		_, enq, eerr := s.queue.Enqueue(ctx, queue.EnqueueParams{
			Kind: KindSyncGame,
			Payload: GamePayload{
				GameID: uuidStr(row.ID), Trigger: p.Trigger, Scope: p.Scope, SyncRunID: uuidStr(runID),
				CreateEvents: createEvents, HasSnapshot: true, Rtime: t.LastUpdatedDateTime.Unix(), HasStats: true,
			},
			DedupeKey: fmt.Sprintf("%s:%s", KindSyncGame, uuidStr(row.ID)),
		})
		if eerr != nil {
			s.failRun(ctx, runID, "erro ao enfileirar")
			return queue.Permanent(fmt.Errorf("enfileirar sync_game: %w", eerr))
		}
		if enq {
			total++
		}
	}
	s.sealAndMaybeClose(ctx, runID, total, p.Scope)
	return nil
}

// refreshPSNProfile atualiza online ID e avatar (best-effort).
func (s *Syncer) refreshPSNProfile(ctx context.Context, integID pgtype.UUID, creds *psnCredentials) {
	var prof psn.Profile
	if err := s.withPSNAuth(ctx, integID, creds, func(tok string) error {
		var e error
		prof, e = s.psn.Profile(ctx, tok, creds.AccountID)
		return e
	}); err != nil || prof.OnlineID == "" {
		return
	}
	if err := s.q.UpdateIntegrationProfile(ctx, dbgen.UpdateIntegrationProfileParams{ID: integID, DisplayName: prof.OnlineID, Avatar: prof.Avatar}); err != nil {
		s.logger.Warn("psn: falha ao gravar perfil", slog.String("err", err.Error()))
	}
}

// handlePSNGame sincroniza grupos, definições, estado do usuário, raridade e progresso de um título.
func (s *Syncer) handlePSNGame(ctx context.Context, job queue.Job, p GamePayload, runID pgtype.UUID, game dbgen.Game) error {
	integID := game.IntegrationID
	_, creds, err := s.psnSession(ctx, integID)
	if err != nil {
		if errors.Is(err, ErrNotConnected) {
			s.finalizeChild(ctx, runID, true, 0)
			return nil
		}
		if isPSNPermanent(err) {
			s.setPSNStatus(ctx, integID, err)
			s.finalizeChild(ctx, runID, true, 0)
			return queue.Permanent(err)
		}
		return s.gameTransient(ctx, job, runID, integID, err)
	}
	service := "trophy"
	if game.NpServiceName != nil && *game.NpServiceName != "" {
		service = *game.NpServiceName
	}
	var (
		defs   []psn.TrophyDef
		groups []psn.TrophyGroup
		user   []psn.UserTrophy
	)
	fetchDefs := p.Scope == ScopeFull || olderThan(game.SchemaSyncedAt, schemaMaxAge)
	ferr := s.withPSNAuth(ctx, integID, &creds, func(tok string) error {
		if fetchDefs {
			var e error
			if groups, e = s.psn.TitleGroups(ctx, tok, game.ExternalID, service); e != nil {
				return e
			}
			if defs, e = s.psn.TitleTrophies(ctx, tok, game.ExternalID, service); e != nil {
				return e
			}
		}
		var e error
		user, e = s.psn.UserTrophies(ctx, tok, game.ExternalID, service)
		return e
	})
	switch {
	case ferr == nil:
	case errors.Is(ferr, psn.ErrNotFound):
		s.finalizeChild(ctx, runID, false, 0)
		return nil
	case isPSNPermanent(ferr):
		s.setPSNStatus(ctx, integID, ferr)
		s.finalizeChild(ctx, runID, true, 0)
		return queue.Permanent(ferr)
	default:
		return s.gameTransient(ctx, job, runID, integID, ferr)
	}

	changed, aerr := s.applyPSNTrophies(ctx, game, groups, defs, user, fetchDefs, p.CreateEvents)
	if aerr != nil {
		return s.gameTransient(ctx, job, runID, integID, aerr)
	}
	s.setWatermark(ctx, game.ID, p.Rtime)
	s.finalizeChild(ctx, runID, false, changed)
	return nil
}

// applyPSNTrophies grava grupos/definições (quando buscados) e aplica o estado do usuário.
func (s *Syncer) applyPSNTrophies(ctx context.Context, game dbgen.Game, groups []psn.TrophyGroup, defs []psn.TrophyDef, user []psn.UserTrophy, withDefs, createEvents bool) (int, error) {
	if withDefs && len(defs) > 0 {
		groupIDs := make(map[string]pgtype.UUID, len(groups)+1)
		names := make(map[string]string, len(groups))
		icons := make(map[string]string, len(groups))
		for _, g := range groups {
			names[g.TrophyGroupID], icons[g.TrophyGroupID] = g.TrophyGroupName, g.TrophyGroupIconURL
		}
		hasPlatinum := false
		for _, d := range defs {
			gid := d.TrophyGroupID
			if gid == "" {
				gid = "default"
			}
			if _, ok := groupIDs[gid]; !ok {
				name := names[gid]
				if name == "" {
					name = map[bool]string{true: "Jogo base", false: "DLC " + gid}[gid == "default"]
				}
				grp, err := s.q.UpsertTrophyGroup(ctx, dbgen.UpsertTrophyGroupParams{
					GameID: game.ID, ExternalID: gid, Name: name, IsBase: gid == "default", Icon: icons[gid],
				})
				if err != nil {
					return 0, fmt.Errorf("upsert grupo: %w", err)
				}
				groupIDs[gid] = grp.ID
			}
			typ := strings.ToLower(d.TrophyType)
			if typ == "platinum" {
				hasPlatinum = true
			}
			var desc *string
			if d.TrophyDetail != "" {
				desc = &d.TrophyDetail
			}
			if _, err := s.q.UpsertTrophyDefinition(ctx, dbgen.UpsertTrophyDefinitionParams{
				GameID: game.ID, GroupID: groupIDs[gid], ExternalID: strconv.Itoa(d.TrophyID), Name: d.TrophyName,
				Description: desc, Type: typ, Hidden: d.TrophyHidden, Icon: d.TrophyIconURL, IconLocked: d.TrophyIconURL,
				Target: atoiPtr(d.TrophyProgressTargetValue),
			}); err != nil {
				return 0, fmt.Errorf("upsert definição: %w", err)
			}
		}
		if err := s.q.SetGameSchemaSynced(ctx, dbgen.SetGameSchemaSyncedParams{ID: game.ID, HasPlatinum: hasPlatinum}); err != nil {
			return 0, err
		}
	}

	trophies, err := s.q.ListTrophiesByGame(ctx, game.ID)
	if err != nil {
		return 0, fmt.Errorf("listar troféus: %w", err)
	}
	byExt := make(map[string]dbgen.Trophy, len(trophies))
	for _, t := range trophies {
		byExt[t.ExternalID] = t
	}
	changed := 0
	for _, u := range user {
		t, ok := byExt[strconv.Itoa(u.TrophyID)]
		if !ok {
			continue
		}
		if u.Earned != t.Earned {
			var at pgtype.Timestamptz
			if u.Earned && !u.EarnedDateTime.IsZero() {
				at = ts(u.EarnedDateTime)
			}
			if err := s.applyEarned(ctx, t.ID, u.Earned, at, u.Earned && !t.Earned && createEvents); err != nil {
				return changed, fmt.Errorf("aplicar earned: %w", err)
			}
			changed++
		}
		if rate, ok := psn.ParseRate(u.TrophyEarnedRate); ok && (t.RarityPct == nil || *t.RarityPct != rate) {
			r := rate
			if err := s.q.SetTrophyRarity(ctx, dbgen.SetTrophyRarityParams{ID: t.ID, RarityPct: &r}); err != nil {
				return changed, fmt.Errorf("set rarity: %w", err)
			}
		}
		if prog := atoiPtr(u.Progress); prog != nil {
			if err := s.q.SetTrophyProgress(ctx, dbgen.SetTrophyProgressParams{ID: t.ID, Progress: prog, Target: t.Target}); err != nil {
				return changed, fmt.Errorf("set progress: %w", err)
			}
		}
	}
	if err := s.q.SetGamePercentagesSynced(ctx, game.ID); err != nil {
		return changed, err
	}
	return changed, nil
}

// failPSN fecha o run com erro e reflete na integração (reconectar quando o refresh expirou).
func (s *Syncer) failPSN(ctx context.Context, runID, integID pgtype.UUID, cause error) {
	msg := "falha ao listar troféus da PSN"
	if isPSNPermanent(cause) {
		s.setPSNStatus(ctx, integID, cause)
		msg = "sessão PSN expirou — cole um NPSSO novo"
	} else if !errors.Is(cause, ErrNotConnected) {
		s.setTransientErrorStatus(ctx, integID, cause)
	}
	s.failRun(ctx, runID, msg)
}

func (s *Syncer) setPSNStatus(ctx context.Context, integID pgtype.UUID, _ error) {
	s.applyIntegrationStatusFor(ctx, integID, "psn", "needs_reconnect", "A sessão da PSN expirou. Cole um NPSSO novo para reconectar.")
}

// isPSNPermanent: só um NPSSO novo resolve (refresh expirado/recusado ou access recusado após renovar).
func isPSNPermanent(err error) bool {
	return errors.Is(err, psn.ErrInvalidGrant) || errors.Is(err, psn.ErrUnauthorized) || errors.Is(err, psn.ErrInvalidNPSSO)
}

func splitPlatforms(s string) []string {
	out := []string{}
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func atoiPtr(s string) *int32 {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return nil
	}
	v := int32(n) //nolint:gosec // contadores pequenos
	return &v
}

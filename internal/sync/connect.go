package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	dbgen "github.com/RodolfoBonis/trophy-hunter-api/db/gen"
)

// ErrAccountExists indica que a conta (mesmo SteamID64) já está conectada.
var ErrAccountExists = errors.New("conta já conectada")

// ErrAPIKeyRequired indica que não há API key informada nem outra conta Steam para reaproveitar.
var ErrAPIKeyRequired = errors.New("api key obrigatória")

// ConnectSteamParams descreve uma nova conta Steam.
type ConnectSteamParams struct {
	// Label é o apelido da conta no app (ex.: "Principal", "Alt"). Vazio → persona da Steam.
	Label string
	// APIKey é opcional quando já existe outra conta Steam: a Steam Web API key lê QUALQUER perfil
	// público, então a key da conta existente é reaproveitada.
	APIKey     string
	IDOrVanity string
}

// ConnectSteam valida a conta com a Steam, grava as credenciais cifradas e enfileira o full sync.
//
// idOrVanity pode ser um SteamID64 (17 dígitos) ou uma vanity URL (resolvida via API). A API key
// é validada implicitamente (GetPlayerSummary falha com ErrInvalidKey se inválida). Se o perfil
// não for público, a integração é gravada com status error_private (banner na UI) — ainda assim
// conectada, para o usuário corrigir a privacidade sem reconfigurar a chave.
func (s *Syncer) ConnectSteam(ctx context.Context, p ConnectSteamParams) (dbgen.Integration, error) {
	apiKey := strings.TrimSpace(p.APIKey)
	if apiKey == "" {
		reused, err := s.reusableSteamKey(ctx)
		if err != nil {
			return dbgen.Integration{}, err
		}
		apiKey = reused
	}
	client := s.steamFactory(apiKey)

	steamID, err := s.resolveSteamID(ctx, client, p.IDOrVanity)
	if err != nil {
		return dbgen.Integration{}, err
	}

	if _, err := s.q.GetIntegrationByAccount(ctx, dbgen.GetIntegrationByAccountParams{
		Platform: "steam", AccountID: steamID,
	}); err == nil {
		return dbgen.Integration{}, ErrAccountExists
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return dbgen.Integration{}, fmt.Errorf("checar conta existente: %w", err)
	}

	summary, err := client.GetPlayerSummary(ctx, steamID)
	if err != nil {
		return dbgen.Integration{}, err // ErrInvalidKey/ErrProfilePrivate tratados pelo handler
	}

	blob, err := s.encryptSteam(steamCredentials{APIKey: apiKey, SteamID: steamID})
	if err != nil {
		return dbgen.Integration{}, err
	}

	label := strings.TrimSpace(p.Label)
	if label == "" {
		label = summary.PersonaName
	}
	integ, err := s.q.CreateIntegration(ctx, dbgen.CreateIntegrationParams{
		Platform:       "steam",
		Label:          label,
		AccountID:      summary.SteamID,
		DisplayName:    summary.PersonaName,
		Avatar:         summary.AvatarFull,
		CredentialsEnc: blob,
		KeyVersion:     int32(s.cipher.Version()), //nolint:gosec // versão pequena e não-negativa
	})
	if err != nil {
		if isUniqueViolation(err) { // corrida entre dois connects da mesma conta
			return dbgen.Integration{}, ErrAccountExists
		}
		return dbgen.Integration{}, fmt.Errorf("gravar integração: %w", err)
	}

	// Se o perfil não é público, marca error_private (o sync não conseguiria ler conquistas).
	if !summary.IsPublic() {
		if err := s.q.UpdateIntegrationStatus(ctx, dbgen.UpdateIntegrationStatusParams{
			ID: integ.ID, Status: "error_private", LastError: "perfil Steam não é público",
		}); err != nil {
			return dbgen.Integration{}, fmt.Errorf("marcar error_private: %w", err)
		}
		integ.Status = "error_private"
		integ.LastError = "perfil Steam não é público"
		s.setStatus("steam", "error_private")
	} else {
		s.setStatus("steam", "connected")
	}

	// Enfileira o full sync inicial (manual).
	if _, _, err := s.EnqueuePlatformSync(ctx, integ.ID, ScopeFull, TriggerManual); err != nil {
		return dbgen.Integration{}, fmt.Errorf("enfileirar full sync: %w", err)
	}
	return integ, nil
}

// UpdateSteamKey troca a API key de uma conta Steam, validando-a contra a própria conta.
func (s *Syncer) UpdateSteamKey(ctx context.Context, integrationID pgtype.UUID, apiKey string) error {
	integ, _, creds, err := s.loadSteam(ctx, integrationID)
	if err != nil {
		return err
	}
	apiKey = strings.TrimSpace(apiKey)
	if _, err := s.steamFactory(apiKey).GetPlayerSummary(ctx, creds.SteamID); err != nil {
		return err
	}
	blob, err := s.encryptSteam(steamCredentials{APIKey: apiKey, SteamID: creds.SteamID})
	if err != nil {
		return err
	}
	return s.q.UpdateIntegrationCredentials(ctx, dbgen.UpdateIntegrationCredentialsParams{
		ID: integ.ID, CredentialsEnc: blob, KeyVersion: int32(s.cipher.Version()), //nolint:gosec // versão pequena
	})
}

// reusableSteamKey devolve a API key da conta Steam mais antiga (ErrAPIKeyRequired se não houver).
func (s *Syncer) reusableSteamKey(ctx context.Context) (string, error) {
	integs, err := s.q.ListIntegrationsByPlatform(ctx, "steam")
	if err != nil {
		return "", fmt.Errorf("listar contas steam: %w", err)
	}
	if len(integs) == 0 {
		return "", ErrAPIKeyRequired
	}
	_, _, creds, err := s.loadSteam(ctx, integs[0].ID)
	if err != nil {
		return "", err
	}
	return creds.APIKey, nil
}

func (s *Syncer) encryptSteam(creds steamCredentials) ([]byte, error) {
	plain, err := json.Marshal(creds) //nolint:gosec // serializado só para cifragem imediata (AES-GCM)
	if err != nil {
		return nil, fmt.Errorf("serializar credenciais: %w", err)
	}
	blob, err := s.cipher.Encrypt(plain, "steam")
	if err != nil {
		return nil, fmt.Errorf("cifrar credenciais: %w", err)
	}
	return blob, nil
}

// resolveSteamID devolve o SteamID64: usa idOrVanity direto se parecer um ID (17 dígitos), senão
// resolve a vanity URL.
func (s *Syncer) resolveSteamID(ctx context.Context, client SteamAPI, idOrVanity string) (string, error) {
	v := strings.TrimSpace(idOrVanity)
	if isSteamID64(v) {
		return v, nil
	}
	id, err := client.ResolveVanityURL(ctx, v)
	if err != nil {
		return "", err
	}
	return id, nil
}

func isSteamID64(s string) bool {
	if len(s) != 17 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

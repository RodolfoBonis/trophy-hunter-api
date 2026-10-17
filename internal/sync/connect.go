package sync

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	dbgen "github.com/RodolfoBonis/trophy-hunter-api/db/gen"
)

// ConnectSteam valida as credenciais com a Steam, grava-as cifradas e enfileira o full sync.
//
// idOrVanity pode ser um SteamID64 (17 dígitos) ou uma vanity URL (resolvida via API). A API key
// é validada implicitamente (GetPlayerSummary falha com ErrInvalidKey se inválida). Se o perfil
// não for público, a integração é gravada com status error_private (banner na UI) — ainda assim
// conectada, para o usuário corrigir a privacidade sem reconfigurar a chave.
func (s *Syncer) ConnectSteam(ctx context.Context, apiKey, idOrVanity string) (dbgen.Integration, error) {
	client := s.steamFactory(apiKey)

	steamID, err := s.resolveSteamID(ctx, client, idOrVanity)
	if err != nil {
		return dbgen.Integration{}, err
	}

	summary, err := client.GetPlayerSummary(ctx, steamID)
	if err != nil {
		return dbgen.Integration{}, err // ErrInvalidKey/ErrProfilePrivate tratados pelo handler
	}

	creds := steamCredentials{APIKey: apiKey, SteamID: steamID}
	plain, err := json.Marshal(creds) //nolint:gosec // serializado só para cifragem imediata (AES-GCM)
	if err != nil {
		return dbgen.Integration{}, fmt.Errorf("serializar credenciais: %w", err)
	}
	blob, err := s.cipher.Encrypt(plain, "steam")
	if err != nil {
		return dbgen.Integration{}, fmt.Errorf("cifrar credenciais: %w", err)
	}

	integ, err := s.q.UpsertIntegrationCredentials(ctx, dbgen.UpsertIntegrationCredentialsParams{
		Platform:       "steam",
		AccountID:      summary.SteamID,
		DisplayName:    summary.PersonaName,
		Avatar:         summary.AvatarFull,
		CredentialsEnc: blob,
		KeyVersion:     int32(s.cipher.Version()), //nolint:gosec // versão pequena e não-negativa
	})
	if err != nil {
		return dbgen.Integration{}, fmt.Errorf("gravar integração: %w", err)
	}

	// Se o perfil não é público, marca error_private (o sync não conseguiria ler conquistas).
	if !summary.IsPublic() {
		if err := s.q.UpdateIntegrationStatus(ctx, dbgen.UpdateIntegrationStatusParams{
			Platform:  "steam",
			Status:    "error_private",
			LastError: "perfil Steam não é público",
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
	if _, _, err := s.EnqueuePlatformSync(ctx, "steam", ScopeFull, TriggerManual); err != nil {
		return dbgen.Integration{}, fmt.Errorf("enfileirar full sync: %w", err)
	}
	return integ, nil
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

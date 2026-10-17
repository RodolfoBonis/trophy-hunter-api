package config_test

import (
	"crypto/rand"
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/RodolfoBonis/trophy-hunter-api/internal/platform/config"
)

func key32(t *testing.T) string {
	t.Helper()
	b := make([]byte, 32)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return base64.StdEncoding.EncodeToString(b)
}

// setBase define o ambiente mínimo para Load() ter sucesso (DB + chave de criptografia).
func setBase(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://u:p@localhost:5432/db?sslmode=disable")
	t.Setenv("TOKEN_ENC_KEY", key32(t))
}

func TestLoadDefaultsToOIDCAndRequiresIssuer(t *testing.T) {
	setBase(t)
	t.Setenv("APP_ENV", "prod")
	// AUTH_MODE não definido → default oidc; sem AUTH_ISSUER → erro.
	_, err := config.Load()
	require.Error(t, err)
	require.Contains(t, err.Error(), "AUTH_ISSUER")
}

func TestLoadOIDCSucceedsWithDefaultAudience(t *testing.T) {
	setBase(t)
	t.Setenv("APP_ENV", "prod")
	t.Setenv("AUTH_MODE", "oidc")
	t.Setenv("AUTH_ISSUER", "https://auth.example/realms/platform")
	cfg, err := config.Load()
	require.NoError(t, err)
	require.Equal(t, config.AuthModeOIDC, cfg.Auth.Mode)
	// AUTH_AUDIENCE tem default não-vazio → a checagem de aud no verifier é sempre aplicada.
	require.Equal(t, "trophy-hunter-api", cfg.Auth.Audience)
}

func TestLoadDevModeRefusedOutsideDev(t *testing.T) {
	setBase(t)
	t.Setenv("AUTH_MODE", "dev")
	t.Setenv("APP_ENV", "prod")
	t.Setenv("AUTH_ISSUER", "")
	_, err := config.Load()
	require.Error(t, err)
	require.Contains(t, err.Error(), "APP_ENV=dev")
}

func TestLoadDevModeRefusedWithIssuer(t *testing.T) {
	setBase(t)
	t.Setenv("AUTH_MODE", "dev")
	t.Setenv("APP_ENV", "dev")
	t.Setenv("AUTH_ISSUER", "https://auth.example/realms/platform")
	_, err := config.Load()
	require.Error(t, err)
	require.Contains(t, err.Error(), "AUTH_ISSUER vazio")
}

func TestLoadDevModeOK(t *testing.T) {
	setBase(t)
	t.Setenv("AUTH_MODE", "dev")
	t.Setenv("APP_ENV", "dev")
	t.Setenv("AUTH_ISSUER", "")
	cfg, err := config.Load()
	require.NoError(t, err)
	require.Equal(t, config.AuthModeDev, cfg.Auth.Mode)
	require.EqualValues(t, 4, cfg.Steam.RPS, "STEAM_RPS default agora é 4")
}

func TestLoadUnknownAuthMode(t *testing.T) {
	setBase(t)
	t.Setenv("APP_ENV", "dev")
	t.Setenv("AUTH_MODE", "banana")
	_, err := config.Load()
	require.Error(t, err)
	require.Contains(t, err.Error(), "AUTH_MODE inválido")
}

func TestLoadParsesOldEncKeys(t *testing.T) {
	setBase(t)
	t.Setenv("APP_ENV", "dev")
	t.Setenv("AUTH_MODE", "dev")
	t.Setenv("AUTH_ISSUER", "")
	old2 := key32(t)
	old3 := key32(t)
	t.Setenv("TOKEN_ENC_KEYS_OLD", "2:"+old2+", 3:"+old3)

	cfg, err := config.Load()
	require.NoError(t, err)
	require.Len(t, cfg.Crypto.OldKeys, 2)
	require.Contains(t, cfg.Crypto.OldKeys, 2)
	require.Contains(t, cfg.Crypto.OldKeys, 3)

	want2, _ := base64.StdEncoding.DecodeString(old2)
	require.Equal(t, want2, cfg.Crypto.OldKeys[2])
}

func TestLoadRejectsBadOldEncKeys(t *testing.T) {
	setBase(t)
	t.Setenv("APP_ENV", "dev")
	t.Setenv("AUTH_MODE", "dev")
	t.Setenv("AUTH_ISSUER", "")
	t.Setenv("TOKEN_ENC_KEYS_OLD", "2:not-base64!!")
	_, err := config.Load()
	require.Error(t, err)
	require.Contains(t, err.Error(), "TOKEN_ENC_KEYS_OLD")
}

func TestLoadWithoutCrypto(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("AUTH_ISSUER", "https://kc/realms/platform")
	t.Setenv("TOKEN_ENC_KEY", "")
	_, err := config.Load()
	require.Error(t, err, "API/worker exigem a chave")
	cfg, err := config.Load(config.WithoutCrypto())
	require.NoError(t, err)
	require.Empty(t, cfg.Crypto.Key)
	require.Equal(t, "sonnet", cfg.AI.Model)
}

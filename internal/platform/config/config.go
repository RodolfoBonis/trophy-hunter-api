// Package config carrega a configuração do serviço a partir do ambiente.
//
// O carregamento fica atrás de Load() para que, no futuro, a fonte possa passar
// a ser o Vault/SSM sem alterar os consumidores. Campos obrigatórios sem default
// fazem Load() falhar com erro claro. Segredos NUNCA são embutidos em mensagens de erro.
package config

import (
	"encoding/base64"
	"fmt"
	"time"

	"github.com/caarlos0/env/v11"
)

// AuthMode define como os tokens são validados.
type AuthMode string

const (
	// AuthModeDev confia no header de debug e NÃO valida assinatura. Apenas dev local.
	AuthModeDev AuthMode = "dev"
	// AuthModeOIDC valida o JWT (Bearer) contra o JWKS do issuer (realm Keycloak).
	AuthModeOIDC AuthMode = "oidc"
)

// Config agrega toda a configuração do serviço (API + worker).
type Config struct {
	Env               string `env:"APP_ENV" envDefault:"dev"`
	HTTP              HTTP
	Metrics           Metrics
	DB                DB
	Redis             Redis
	Auth              Auth
	Crypto            Crypto
	Telemetry         Telemetry
	Sync              Sync
	Steam             Steam
	WorkerConcurrency int `env:"WORKER_CONCURRENCY" envDefault:"4"`
}

// HTTP configura o servidor HTTP da API.
type HTTP struct {
	Addr            string        `env:"HTTP_ADDR" envDefault:":8080"`
	ReadTimeout     time.Duration `env:"HTTP_READ_TIMEOUT" envDefault:"15s"`
	WriteTimeout    time.Duration `env:"HTTP_WRITE_TIMEOUT" envDefault:"30s"`
	ShutdownTimeout time.Duration `env:"HTTP_SHUTDOWN_TIMEOUT" envDefault:"15s"`
}

// Metrics configura o endpoint de métricas do worker (a API expõe /metrics no próprio HTTP_ADDR).
type Metrics struct {
	Addr string `env:"METRICS_ADDR" envDefault:":9090"`
}

// DB configura a conexão com o Postgres.
type DB struct {
	URL            string `env:"DATABASE_URL,required"`
	MaxConns       int32  `env:"DB_MAX_CONNS" envDefault:"20"`
	MinConns       int32  `env:"DB_MIN_CONNS" envDefault:"2"`
	MigrateOnStart bool   `env:"DB_MIGRATE_ON_START" envDefault:"true"`
}

// Redis configura o cliente Redis (lock do scheduler; nomes seguem o subchart redis do rb-stack).
type Redis struct {
	Addr     string `env:"REDIS_ADDR" envDefault:"localhost:6379"`
	Password string `env:"REDIS_PASSWORD" envDefault:""`
	DB       int    `env:"REDIS_DB" envDefault:"0"`
}

// Auth configura a validação dos tokens (Keycloak, OIDC/JWT) do realm platform.
//
//   - Mode "dev" confia no header X-Debug-User (dev local, sem Keycloak); "oidc" valida o JWT
//     contra o JWKS do Issuer, conferindo iss, aud (Audience) e a role exigida em realm_access.roles.
type Auth struct {
	Mode         AuthMode `env:"AUTH_MODE" envDefault:"dev"`
	Issuer       string   `env:"AUTH_ISSUER" envDefault:""`
	Audience     string   `env:"AUTH_AUDIENCE" envDefault:"trophy-hunter-api"`
	RequiredRole string   `env:"AUTH_REQUIRED_ROLE" envDefault:"trophy-hunter-user"`
}

// Crypto configura a chave mestra de criptografia das credenciais (AES-256-GCM).
//
// Key é a chave em claro (32 bytes) já decodificada de TOKEN_ENC_KEY (base64). KeyVersion
// prefixa o ciphertext para permitir rotação. A chave NUNCA é logada.
type Crypto struct {
	KeyB64     string `env:"TOKEN_ENC_KEY,required"`
	KeyVersion int    `env:"TOKEN_ENC_KEY_VERSION" envDefault:"1"`
	Key        []byte `env:"-"`
}

// Telemetry configura logs e OpenTelemetry (exporter desligado por default).
type Telemetry struct {
	OTelEnabled  bool   `env:"OTEL_ENABLED" envDefault:"false"`
	ServiceName  string `env:"OTEL_SERVICE_NAME" envDefault:"trophy-hunter-api"`
	OTLPEndpoint string `env:"OTEL_EXPORTER_OTLP_ENDPOINT" envDefault:"localhost:4317"`
	LogLevel     string `env:"LOG_LEVEL" envDefault:"info"`
}

// Sync configura o scheduler de sincronização (no worker).
type Sync struct {
	Enabled  bool          `env:"SYNC_ENABLED" envDefault:"true"`
	Interval time.Duration `env:"SYNC_INTERVAL" envDefault:"6h"`
}

// Steam configura o cliente da Steam Web API.
//
//   - BaseURL é configurável para apontar a um servidor fake em testes/smoke (STEAM_API_BASE_URL).
//   - Lang é o idioma dos textos (l=; default "brazilian").
//   - RPS é o teto de requests por segundo do token bucket por processo.
type Steam struct {
	BaseURL string  `env:"STEAM_API_BASE_URL" envDefault:"https://api.steampowered.com"`
	Lang    string  `env:"STEAM_LANG" envDefault:"brazilian"`
	RPS     float64 `env:"STEAM_RPS" envDefault:"1"`
}

// Load lê a configuração do ambiente, valida campos obrigatórios e decodifica a chave mestra.
func Load() (Config, error) {
	var cfg Config
	if err := env.Parse(&cfg); err != nil {
		return Config{}, fmt.Errorf("carregar config: %w", err)
	}
	if cfg.Auth.Mode == AuthModeOIDC && cfg.Auth.Issuer == "" {
		return Config{}, fmt.Errorf("carregar config: AUTH_ISSUER é obrigatório quando AUTH_MODE=oidc")
	}
	key, err := base64.StdEncoding.DecodeString(cfg.Crypto.KeyB64)
	if err != nil {
		return Config{}, fmt.Errorf("carregar config: TOKEN_ENC_KEY não é base64 válido")
	}
	if len(key) != 32 {
		return Config{}, fmt.Errorf("carregar config: TOKEN_ENC_KEY deve ter 32 bytes (AES-256), tem %d", len(key))
	}
	cfg.Crypto.Key = key
	cfg.Crypto.KeyB64 = "" // não manter o segredo em memória após decodificar
	if cfg.Steam.RPS <= 0 {
		return Config{}, fmt.Errorf("carregar config: STEAM_RPS deve ser > 0")
	}
	if cfg.WorkerConcurrency <= 0 {
		return Config{}, fmt.Errorf("carregar config: WORKER_CONCURRENCY deve ser > 0")
	}
	return cfg, nil
}

// IsProd indica se o serviço roda em produção.
func (c Config) IsProd() bool { return c.Env == "prod" }

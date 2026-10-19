// Package config carrega a configuração do serviço a partir do ambiente.
//
// O carregamento fica atrás de Load() para que, no futuro, a fonte possa passar
// a ser o Vault/SSM sem alterar os consumidores. Campos obrigatórios sem default
// fazem Load() falhar com erro claro. Segredos NUNCA são embutidos em mensagens de erro.
package config

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
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
	Env       string `env:"APP_ENV" envDefault:"dev"`
	HTTP      HTTP
	Metrics   Metrics
	DB        DB
	Redis     Redis
	Auth      Auth
	Crypto    Crypto
	Telemetry Telemetry
	Sync      Sync
	Steam     Steam
	PSN       PSN
	AI        AI
	Push      Push
	HLTB      HLTB
	Prices    Prices
	// PrometheusURL: HTTP API do Prometheus no cluster (painel do dono). Vazio = seção indisponível.
	PrometheusURL     string `env:"PROMETHEUS_URL"`
	WorkerConcurrency int    `env:"WORKER_CONCURRENCY" envDefault:"4"`
}

// HTTP configura o servidor HTTP da API.
type HTTP struct {
	Addr            string        `env:"HTTP_ADDR" envDefault:":8080"`
	ReadTimeout     time.Duration `env:"HTTP_READ_TIMEOUT" envDefault:"15s"`
	WriteTimeout    time.Duration `env:"HTTP_WRITE_TIMEOUT" envDefault:"30s"`
	ShutdownTimeout time.Duration `env:"HTTP_SHUTDOWN_TIMEOUT" envDefault:"15s"`
	// CORSAllowedOrigins são as origens (scheme://host[:port]) autorizadas a chamar a API pelo
	// browser. A API fica em subdomínio próprio (api.<app>.<domínio>), então o PWA é cross-origin.
	CORSAllowedOrigins []string `env:"CORS_ALLOWED_ORIGINS" envSeparator:","`
	// AppURL: base pública do web (links de convite).
	AppURL string `env:"APP_URL" envDefault:"https://trophy.rodolfodebonis.com.br"`
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
//   - Mode "oidc" (default) valida o JWT contra o JWKS do Issuer, conferindo iss, aud (Audience)
//     e a role exigida em realm_access.roles.
//   - Mode "dev" confia no header X-Debug-User (dev local, sem Keycloak). É aceito SOMENTE quando
//     APP_ENV=dev e AUTH_ISSUER está vazio — nunca escorrega para produção por engano.
type Auth struct {
	Mode     AuthMode `env:"AUTH_MODE" envDefault:"oidc"`
	Issuer   string   `env:"AUTH_ISSUER" envDefault:""`
	Audience string   `env:"AUTH_AUDIENCE" envDefault:"trophy-hunter-api"`
	// RequiredRole: realm role exigida no token. Vazio = não exige (multiusuário: o acesso é o
	// convite aceito no app). No realm platform (antes da virada) era trophy-hunter-user.
	RequiredRole string `env:"AUTH_REQUIRED_ROLE"`
	// OwnerEmail: e-mail (verificado) do dono — reivindica a conta do dono no login, inclusive ao
	// trocar de IdP. Vazio = ninguém reivindica (o dono não loga).
	OwnerEmail string `env:"OWNER_EMAIL"`
	// AdminClientID/Secret: service account do realm (só manage-users) usado para apagar o usuário no
	// Keycloak quando ele exclui a própria conta. Vazio = não remove no Keycloak (dev/teste).
	AdminClientID     string `env:"KEYCLOAK_ADMIN_CLIENT_ID"`
	AdminClientSecret string `env:"KEYCLOAK_ADMIN_CLIENT_SECRET"`
}

// Crypto configura a chave mestra de criptografia das credenciais (AES-256-GCM).
//
// Key é a chave em claro (32 bytes) já decodificada de TOKEN_ENC_KEY (base64). KeyVersion
// prefixa o ciphertext para permitir rotação. A chave NUNCA é logada.
//
// OldKeysB64 traz chaves antigas (rotação) no formato "version:base64,version:base64" — usadas
// SÓ para decifrar blobs ainda cifrados com elas; a cifragem nova usa sempre a chave primária.
type Crypto struct {
	KeyB64     string `env:"TOKEN_ENC_KEY"`
	KeyVersion int    `env:"TOKEN_ENC_KEY_VERSION" envDefault:"1"`
	OldKeysB64 string `env:"TOKEN_ENC_KEYS_OLD" envDefault:""`
	Key        []byte `env:"-"`
	// OldKeys mapeia version → chave (32 bytes) decodificada de OldKeysB64.
	OldKeys map[int][]byte `env:"-"`
}

// Telemetry configura logs e OpenTelemetry (exporter desligado por default).
type Telemetry struct {
	OTelEnabled  bool   `env:"OTEL_ENABLED" envDefault:"false"`
	ServiceName  string `env:"OTEL_SERVICE_NAME" envDefault:"trophy-hunter-api"`
	OTLPEndpoint string `env:"OTEL_EXPORTER_OTLP_ENDPOINT" envDefault:"localhost:4317"`
	LogLevel     string `env:"LOG_LEVEL" envDefault:"info"`
}

// Sync configura o scheduler de sincronização (no worker).
//
// JobTimeout é o orçamento por job na fila. Com o fan-out (sync_platform só lista a biblioteca e
// emite um sync_game por jogo), cada job é pequeno; o default cobre folgado mesmo com STEAM_RPS baixo.
type Sync struct {
	Enabled    bool          `env:"SYNC_ENABLED" envDefault:"true"`
	Interval   time.Duration `env:"SYNC_INTERVAL" envDefault:"6h"`
	JobTimeout time.Duration `env:"JOB_TIMEOUT" envDefault:"5m"`
}

// PSN configura o cliente da API (não oficial) de troféus da PlayStation.
//
//   - AuthBaseURL/APIBaseURL são configuráveis para servidores fake em testes.
//   - Lang é o Accept-Language (nomes/descrições localizados).
//   - RPS/Burst: a PSN limita ~300 req/15 min por conta; 0.5 rps sustentado com pico 5 fica abaixo.
type PSN struct {
	AuthBaseURL string  `env:"PSN_AUTH_BASE_URL" envDefault:"https://ca.account.sony.com/api/authz/v3/oauth"`
	APIBaseURL  string  `env:"PSN_API_BASE_URL" envDefault:"https://m.np.playstation.com/api"`
	Lang        string  `env:"PSN_LANG" envDefault:"pt-BR"`
	RPS         float64 `env:"PSN_RPS" envDefault:"0.5"`
	Burst       int     `env:"PSN_BURST" envDefault:"5"`
}

// Steam configura o cliente da Steam Web API.
//
//   - BaseURL é configurável para apontar a um servidor fake em testes/smoke (STEAM_API_BASE_URL).
//   - Lang é o idioma dos textos (l=; default "brazilian").
//   - RPS é o teto de requests por segundo do token bucket por processo. Com o fan-out, cada
//     sync_game faz ~3 chamadas; 4 rps mantém o sync responsivo sem estourar a cota da Steam.
type Steam struct {
	BaseURL string `env:"STEAM_API_BASE_URL" envDefault:"https://api.steampowered.com"`
	// ServerAPIKey: key do servidor para contas sem key própria (amigos só informam o perfil).
	// SEGREDO (Vault k3s/trophy-hunter/prod/app).
	ServerAPIKey string  `env:"STEAM_SERVER_API_KEY"`
	Lang         string  `env:"STEAM_LANG" envDefault:"brazilian"`
	RPS          float64 `env:"STEAM_RPS" envDefault:"4"`
}

// HLTB configura as estimativas de tempo (HowLongToBeat), preenchidas pelo worker em segundo plano.
type HLTB struct {
	Enabled  bool          `env:"HLTB_ENABLED" envDefault:"true"`
	BaseURL  string        `env:"HLTB_BASE_URL" envDefault:"https://howlongtobeat.com"`
	Interval time.Duration `env:"HLTB_INTERVAL" envDefault:"10m"`
	Batch    int           `env:"HLTB_BATCH" envDefault:"15"`
}

// Prices configura o backlog (preços nas lojas). ITAD_API_KEY é opcional: sem ela, Steam usa o
// preço da própria loja (sem menor histórico). PSSTORE_SEARCH_HASH vazio = hash conhecido (psstore).
type Prices struct {
	Enabled     bool          `env:"PRICES_ENABLED" envDefault:"true"`
	Interval    time.Duration `env:"PRICES_INTERVAL" envDefault:"15m"`
	Batch       int           `env:"PRICES_BATCH" envDefault:"10"`
	ITADKey     string        `env:"ITAD_API_KEY"`
	PSStoreHash string        `env:"PSSTORE_SEARCH_HASH"`
}

// Option ajusta o carregamento.
type Option func(*loadOptions)

type loadOptions struct{ withoutCrypto bool }

// WithoutCrypto dispensa a TOKEN_ENC_KEY: o ai-worker não toca nas credenciais das integrações, então
// não recebe a chave (menor privilégio). API e worker exigem a chave.
func WithoutCrypto() Option { return func(o *loadOptions) { o.withoutCrypto = true } }

// Load lê a configuração do ambiente, valida campos obrigatórios e decodifica a chave mestra.
func Load(opts ...Option) (Config, error) {
	var o loadOptions
	for _, opt := range opts {
		opt(&o)
	}
	var cfg Config
	if err := env.Parse(&cfg); err != nil {
		return Config{}, fmt.Errorf("carregar config: %w", err)
	}
	if err := validateAuth(cfg); err != nil {
		return Config{}, err
	}
	if o.withoutCrypto {
		cfg.Crypto = Crypto{}
		return cfg, validateLimits(cfg)
	}
	if cfg.Crypto.KeyB64 == "" {
		return Config{}, fmt.Errorf("carregar config: TOKEN_ENC_KEY é obrigatória")
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

	oldKeys, err := parseOldKeys(cfg.Crypto.OldKeysB64)
	if err != nil {
		return Config{}, err
	}
	cfg.Crypto.OldKeys = oldKeys
	cfg.Crypto.OldKeysB64 = ""
	return cfg, validateLimits(cfg)
}

// validateLimits confere os limites numéricos da config.
func validateLimits(cfg Config) error {
	if cfg.Steam.RPS <= 0 {
		return fmt.Errorf("carregar config: STEAM_RPS deve ser > 0")
	}
	if cfg.WorkerConcurrency <= 0 {
		return fmt.Errorf("carregar config: WORKER_CONCURRENCY deve ser > 0")
	}
	if cfg.Sync.JobTimeout <= 0 {
		return fmt.Errorf("carregar config: JOB_TIMEOUT deve ser > 0")
	}
	return nil
}

// validateAuth aplica a política de segurança do modo de autenticação (ver type Auth).
func validateAuth(cfg Config) error {
	switch cfg.Auth.Mode {
	case AuthModeOIDC:
		if cfg.Auth.Issuer == "" {
			return fmt.Errorf("carregar config: AUTH_ISSUER é obrigatório quando AUTH_MODE=oidc")
		}
		if cfg.Auth.Audience == "" {
			return fmt.Errorf("carregar config: AUTH_AUDIENCE é obrigatório quando AUTH_MODE=oidc")
		}
		return nil
	case AuthModeDev:
		// dev bypass só é aceito em dev local e sem issuer configurado (evita escorregar p/ prod).
		if cfg.Env != "dev" {
			return fmt.Errorf("carregar config: AUTH_MODE=dev só é permitido com APP_ENV=dev (APP_ENV=%q)", cfg.Env)
		}
		if cfg.Auth.Issuer != "" {
			return fmt.Errorf("carregar config: AUTH_MODE=dev exige AUTH_ISSUER vazio (há um issuer configurado — use AUTH_MODE=oidc)")
		}
		return nil
	default:
		return fmt.Errorf("carregar config: AUTH_MODE inválido %q (use oidc ou dev)", cfg.Auth.Mode)
	}
}

// parseOldKeys decodifica TOKEN_ENC_KEYS_OLD ("v:base64,v:base64") em version → chave de 32 bytes.
func parseOldKeys(raw string) (map[int][]byte, error) {
	out := map[int][]byte{}
	if raw == "" {
		return out, nil
	}
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		version, keyB64, ok := strings.Cut(entry, ":")
		if !ok {
			return nil, fmt.Errorf("carregar config: TOKEN_ENC_KEYS_OLD inválido (esperado version:base64)")
		}
		v, err := strconv.Atoi(strings.TrimSpace(version))
		if err != nil {
			return nil, fmt.Errorf("carregar config: TOKEN_ENC_KEYS_OLD com versão inválida")
		}
		keyBytes, err := base64.StdEncoding.DecodeString(strings.TrimSpace(keyB64))
		if err != nil {
			return nil, fmt.Errorf("carregar config: TOKEN_ENC_KEYS_OLD versão %d não é base64 válido", v)
		}
		if len(keyBytes) != 32 {
			return nil, fmt.Errorf("carregar config: TOKEN_ENC_KEYS_OLD versão %d deve ter 32 bytes, tem %d", v, len(keyBytes))
		}
		out[v] = keyBytes
	}
	return out, nil
}

// IsProd indica se o serviço roda em produção.
func (c Config) IsProd() bool { return c.Env == "prod" }

// AI configura o ai-worker (CLI do Claude Code headless). A API não executa a CLI: só enfileira e
// repassa a stream. OAuthToken é o CLAUDE_CODE_OAUTH_TOKEN da assinatura (SEGREDO: só vai para o
// processo da CLI, nunca é logado) e só é exigido pelo ai-worker.
type AI struct {
	ClaudeBin  string `env:"AI_CLAUDE_BIN" envDefault:"claude"`
	OAuthToken string `env:"CLAUDE_CODE_OAUTH_TOKEN"`
	// Model é o do chat (resposta rápida); GuideModel o dos guias/roadmaps (ficam salvos: qualidade
	// vale mais que velocidade).
	Model      string `env:"AI_MODEL" envDefault:"sonnet"`
	GuideModel string `env:"AI_MODEL_GUIDE" envDefault:"opus"`
	// DailyLimit: pedidos de IA por membro em 24 h (0 = sem teto). O dono não tem teto.
	DailyLimit int           `env:"AI_DAILY_LIMIT" envDefault:"30"`
	JobTimeout time.Duration `env:"AI_JOB_TIMEOUT" envDefault:"6m"`
	// Home é o HOME da CLI (~/.claude guarda as sessões do --resume do chat).
	Home string `env:"AI_HOME" envDefault:"/home/node"`
	// Workdir é a raiz dos cwd da CLI (um por conversa: o --resume exige o mesmo cwd).
	Workdir string `env:"AI_WORKDIR" envDefault:"/home/node/work"`
}

// Push configura o Web Push (VAPID). Sem as chaves o push fica desligado (a UI mostra isso).
// A chave privada é SEGREDO (Vault k3s/trophy-hunter/prod/app); a pública vai para o navegador.
type Push struct {
	VAPIDPublicKey  string `env:"VAPID_PUBLIC_KEY"`
	VAPIDPrivateKey string `env:"VAPID_PRIVATE_KEY"`
	// VAPIDSubject identifica o remetente para os push services (URL do app; não expor e-mail).
	VAPIDSubject string `env:"VAPID_SUBJECT" envDefault:"https://trophy.rodolfodebonis.com.br"`
}

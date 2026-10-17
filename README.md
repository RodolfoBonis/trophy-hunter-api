# Trophy Hunter API

Backend pessoal (single-user) para acompanhar conquistas **Steam** (troféus PSN chegam na fase 2).
Mostra a biblioteca, o progresso por jogo e o que falta para 100%. Go 1.26 · chi · pgx/sqlc · goose.

- **`cmd/api`** — API REST (`/api/v1`), valida o JWT do Keycloak (realm `platform`) e enfileira syncs.
- **`cmd/worker`** — processa a fila (Postgres) e agenda syncs incrementais; expõe `/metrics` e `/healthz`.

O contrato vive em [`api/openapi.yaml`](api/openapi.yaml) (OpenAPI 3.1) — o frontend gera tipos dele.

## Setup local

Pré-requisitos: Go 1.26, Docker (Colima no macOS), `sqlc`, `goose`, `golangci-lint` (via `make tools`).

```bash
cp .env.example .env            # ajuste se necessário
make db-up                      # Postgres + Redis (docker compose)
make run                        # sobe a API em :8080 (migra no boot)
make run-worker                 # em outro terminal: worker + scheduler
```

Em dev (`AUTH_MODE=dev`) não há Keycloak: envie o header `X-Debug-User: <id>` em cada request. O
modo `dev` só é aceito com `APP_ENV=dev` **e** `AUTH_ISSUER` vazio — em produção use `AUTH_MODE=oidc`
(default) com `AUTH_ISSUER`/`AUTH_AUDIENCE` preenchidos.

```bash
curl -H 'X-Debug-User: me' localhost:8080/api/v1/me
```

## Obter as credenciais Steam

1. **API key:** acesse <https://steamcommunity.com/dev/apikey>, logado na sua conta Steam, e gere a
   chave (qualquer domínio serve). Ela é um segredo — a API a guarda **cifrada** (AES-256-GCM) e
   **nunca** a devolve.
2. **SteamID64:** o número de 17 dígitos do seu perfil (ex.: `76561197960000000`). Se você usa uma
   *vanity URL* (`steamcommunity.com/id/seunome`), pode informar só `seunome` — a API resolve.
3. **Perfil público:** em *Privacidade do perfil*, deixe **Detalhes do jogo** e **Perfil** como
   *Público* — senão a integração fica em `error_private` e o sync não lê suas conquistas.

Conecte (várias contas por plataforma são suportadas — ex.: uma principal e uma alt):

```bash
# 1ª conta Steam: api_key obrigatória
curl -X POST localhost:8080/api/v1/integrations/steam \
  -H 'X-Debug-User: me' -H 'Content-Type: application/json' \
  -d '{"label":"Principal","api_key":"SUA_CHAVE","steam_id_or_vanity":"76561197960000000"}'

# contas seguintes: api_key opcional — a mesma key lê qualquer perfil PÚBLICO
curl -X POST localhost:8080/api/v1/integrations/steam \
  -H 'X-Debug-User: me' -H 'Content-Type: application/json' \
  -d '{"label":"Alt","steam_id_or_vanity":"minha-alt"}'
```

Renomear/trocar a key: `PATCH /api/v1/integrations/{id}`; remover (com os jogos dela):
`DELETE /api/v1/integrations/{id}`.

### Várias contas na biblioteca

Conquistas são por conta, então o mesmo jogo em duas contas são duas linhas em `games`. A
biblioteca (`GET /games`) devolve **um item por jogo** representado pela conta **mais avançada**
(maior % → mais troféus → desbloqueio mais recente), com `accounts[]` listando todas as contas. As
estatísticas somam as contas, mas cada jogo conta uma vez (o da conta mais avançada).
`?integration_id=` restringe a uma conta.

## Como o sync funciona

O sync usa **fan-out** para caber no orçamento de cada job (`JOB_TIMEOUT`) mesmo com rate limit baixo:

- **Conexão** valida a chave/ID com a Steam, grava cifrado e enfileira um **full sync** da conta
  (cada conta tem seus próprios jobs, runs e `initial_sync_done`).
- **`sync_platform`** lista a biblioteca (`GetOwnedGames`, 1 chamada), faz upsert da *identidade* de
  cada jogo e **emite um job `sync_game` por jogo** que precisa de trabalho. Cria o `sync_run` **pai**.
- **`sync_game`** sincroniza schema/conquistas/raridade de **um** jogo. A *marca d'água* do
  incremental (`remote_updated_at`) só é gravada **após** o jogo sincronizar com sucesso — um jogo que
  falhou é re-tentado no próximo incremental (não fica "pra trás" por ter marcado a marca cedo demais).
- O `sync_run` agrega o progresso dos filhos (`games_total`, `games_done`, `games_failed`,
  `items_changed`); o último filho a terminar **fecha** o run. `initial_sync_done` só liga quando o
  full inicial fecha com **zero** jogos falhos. O sync inicial **não** gera eventos de desbloqueio.
- **Incremental** (agendado a cada `SYNC_INTERVAL`, default 6h; 1ª passada ~1min após o boot): só
  re-busca jogos cujo `rtime_last_played` mudou; schema a cada 7 dias; raridade a cada 24h. Quando um
  troféu passa de não-obtido → obtido **após** o sync inicial, grava um `unlock_event` (toast/timeline,
  único por troféu).
- **Sync manual de um jogo** reaproveita o mesmo job `sync_game` (com um run próprio, scope `game`).
- **Rate limit:** token bucket por `STEAM_RPS` (default 4 req/s) + retry com backoff em 429/5xx
  (respeitando `Retry-After`). Perfil privado → `error_private`; chave inválida → `error`.
- **Fila robusta:** lease com `lease_token` (um worker "zumbi" não processa duas vezes), jobs
  envenenados vão para `dead` no claim, erros permanentes (chave inválida/perfil privado) morrem na
  hora. Um *reaper* no worker fecha `sync_runs` presos sem jobs vivos; jobs terminais > 7 dias são
  limpos periodicamente. O scheduler usa lock no Redis (`SET NX`) para só uma réplica agendar por tick.

## Disparar sync manualmente

```bash
# uma conta inteira (scope: full | incremental)
curl -X POST localhost:8080/api/v1/integrations/<integration-id>/sync \
  -H 'X-Debug-User: me' -H 'Content-Type: application/json' -d '{"scope":"incremental"}'

# um único jogo
curl -X POST localhost:8080/api/v1/games/<game-id>/sync -H 'X-Debug-User: me'

# ver o log de sync
curl -H 'X-Debug-User: me' 'localhost:8080/api/v1/sync-runs?platform=steam'
```

## Rotação da chave de criptografia

As credenciais de integração são cifradas com `TOKEN_ENC_KEY` (AES-256-GCM), e o `key_version`
prefixa cada blob. Para rotacionar **sem** re-cifrar tudo de uma vez:

1. Gere a nova chave: `openssl rand -base64 32`.
2. Promova-a a primária: `TOKEN_ENC_KEY=<nova>` e **incremente** `TOKEN_ENC_KEY_VERSION`.
3. Mantenha a(s) chave(s) antiga(s) em `TOKEN_ENC_KEYS_OLD` no formato `versão:base64` (vírgula separa
   múltiplas), p/ a API ainda **decifrar** blobs já gravados:
   `TOKEN_ENC_KEYS_OLD=1:<base64-da-chave-v1>`.
4. A cifragem nova passa a usar a chave primária. Quando todas as credenciais tiverem sido re-gravadas
   (ao reconectar as integrações), a chave antiga pode sair de `TOKEN_ENC_KEYS_OLD`.

## Testes e qualidade

```bash
make lint          # golangci-lint (0 issues)
make test          # go test -race (integração via testcontainers/Colima)
make test-short    # só unit (sem Docker)
```

Os testes de integração (fila, sync, API) sobem Postgres efêmero via **testcontainers**; no macOS
isso usa o Colima (`DOCKER_HOST` já é setado pelo Makefile). **O CI roda a suíte completa com
`-race`** (o runner ARC tem um sidecar dind), além de `sqlc generate` + `git diff` (código gerado em
dia) e lint do OpenAPI.

## Variáveis de ambiente

Todas documentadas em [`.env.example`](.env.example). Resumo dos principais grupos: HTTP
(`HTTP_ADDR`, `METRICS_ADDR`), banco (`DATABASE_URL`, `DB_MIGRATE_ON_START`), Redis (`REDIS_ADDR`…),
auth (`AUTH_MODE` — `oidc` default / `dev` só em dev, `AUTH_ISSUER`, `AUTH_AUDIENCE`,
`AUTH_REQUIRED_ROLE`), cripto (`TOKEN_ENC_KEY` base64 de 32 bytes, `TOKEN_ENC_KEY_VERSION`,
`TOKEN_ENC_KEYS_OLD` p/ rotação), sync (`SYNC_ENABLED`, `SYNC_INTERVAL`, `JOB_TIMEOUT`,
`WORKER_CONCURRENCY`), Steam (`STEAM_API_BASE_URL`, `STEAM_LANG`, `STEAM_RPS` default 4) e telemetria
(`OTEL_ENABLED`, `OTEL_SERVICE_NAME`, `LOG_LEVEL`).

> Segurança: segredos nunca são logados (redação por `ReplaceAttr` em chaves `npsso|token|secret|key|
> authorization|password`) nem retornados pela API; a API key da Steam é removida de erros/URLs.

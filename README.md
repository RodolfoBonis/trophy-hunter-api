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

Em dev (`AUTH_MODE=dev`) não há Keycloak: envie o header `X-Debug-User: <id>` em cada request.

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

Conecte:

```bash
curl -X PUT localhost:8080/api/v1/integrations/steam \
  -H 'X-Debug-User: me' -H 'Content-Type: application/json' \
  -d '{"api_key":"SUA_CHAVE","steam_id_or_vanity":"76561197960000000"}'
```

## Como o sync funciona

- **Conexão** valida a chave/ID com a Steam, grava cifrado e enfileira um **full sync**.
- **Full sync:** varre toda a biblioteca (`GetOwnedGames`), e para cada jogo busca o schema das
  conquistas, as conquistas do jogador e a raridade global. Jogos sem conquistas ficam com
  `has_achievements=false`. O sync inicial **não** gera eventos de desbloqueio.
- **Incremental** (agendado a cada `SYNC_INTERVAL`, default 6h): só re-busca jogos cujo tempo de
  jogo / último jogado mudou; schema a cada 7 dias; raridade a cada 24h (jogos não 100%). Quando um
  troféu passa de não-obtido → obtido **após** o sync inicial, grava um `unlock_event` (toast/timeline).
- **Rate limit:** token bucket por `STEAM_RPS` (default 1 req/s) + retry com backoff em 429/5xx.
- O scheduler usa um lock no Redis (`SET NX`) para que só uma réplica agende por tick.

## Disparar sync manualmente

```bash
# plataforma inteira (scope: full | incremental)
curl -X POST localhost:8080/api/v1/integrations/steam/sync \
  -H 'X-Debug-User: me' -H 'Content-Type: application/json' -d '{"scope":"incremental"}'

# um único jogo
curl -X POST localhost:8080/api/v1/games/<game-id>/sync -H 'X-Debug-User: me'

# ver o log de sync
curl -H 'X-Debug-User: me' 'localhost:8080/api/v1/sync-runs?platform=steam'
```

## Testes e qualidade

```bash
make lint          # golangci-lint (0 issues)
make test          # go test -race (integração via testcontainers/Colima)
make test-short    # só unit (sem Docker) — é o que o CI roda
```

Os testes de integração (fila, sync, API) sobem Postgres efêmero via **testcontainers**; no macOS
isso usa o Colima (`DOCKER_HOST` já é setado pelo Makefile).

## Variáveis de ambiente

Todas documentadas em [`.env.example`](.env.example). Resumo dos principais grupos: HTTP
(`HTTP_ADDR`, `METRICS_ADDR`), banco (`DATABASE_URL`, `DB_MIGRATE_ON_START`), Redis (`REDIS_ADDR`…),
auth (`AUTH_MODE`, `AUTH_ISSUER`, `AUTH_AUDIENCE`, `AUTH_REQUIRED_ROLE`), cripto (`TOKEN_ENC_KEY`
base64 de 32 bytes, `TOKEN_ENC_KEY_VERSION`), sync (`SYNC_ENABLED`, `SYNC_INTERVAL`,
`WORKER_CONCURRENCY`), Steam (`STEAM_API_BASE_URL`, `STEAM_LANG`, `STEAM_RPS`) e telemetria
(`OTEL_ENABLED`, `OTEL_SERVICE_NAME`, `LOG_LEVEL`).

> Segurança: segredos nunca são logados (redação por `ReplaceAttr` em chaves `npsso|token|secret|key|
> authorization|password`) nem retornados pela API; a API key da Steam é removida de erros/URLs.

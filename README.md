# Cline Proxy

A single Go binary that turns free upstream LLM quotas (Cline accounts + opencode zen free models) into one clean **OpenAI-compatible `/v1` gateway** for coding IDEs — Cursor, ZCode, Cline, Claude Code, OpenClaw — with account pooling, per-request proxy rotation, and an English admin panel.

Fork-in-progress of [YuJunZhiXue/Cline-proxy](https://github.com/YuJunZhiXue/Cline-proxy), reworked for solo public deployment. This project is not affiliated with Cline or opencode; use their free tiers respectfully.

## What it does

```
Cursor / ZCode / Cline / Claude Code ──►  cline-proxy  ──►  cline account pool (round-robin)
   /v1/chat/completions                        │
   /v1/messages (Anthropic)                    ├─►  opencode zen free models (multi-key, proxy pool)
   /v1/responses (OpenAI Responses)            │
   /v1/models                                  └─►  egress socks5/http proxy pool (one proxy pinned per key/account)
```

- **One endpoint, both upstreams.** The gateway inspects the requested `model` and routes it: zen free models (`big-pickle`, `mimo-v2.6-flash-free`, `nemotron-3-ultra-free`, …) go to opencode zen, paid zen models are rejected with a clear 400, everything else goes to the Cline account pool. Combos let you define your own alias model IDs on either platform.
- **Three client dialects, one upstream language.** Both upstreams are OpenAI chat-completions servers. `/v1/chat/completions` is a near-passthrough; `/v1/responses` and `/v1/messages` (Anthropic) are translated in and converted back, including streaming events, tool calls, and usage.
- **IDE-controlled parameters are respected.** `max_tokens`, `temperature`, `top_p`, `stop`, `tools`, `tool_choice`, `reasoning_effort`, … all pass through from the client. The gateway only fills gaps (128k default output budget when the client sends none) and enforces semantics upstreams get wrong: `tool_choice: "none"` is enforced by stripping tools, unknown model names return a clean 400 (`STRICT_MODEL_MATCH=false` restores the old catch-all), and `stop` sequences are deterministically truncated client-side for non-stream responses.
- **Pool everything.** Cline accounts round-robin with automatic 429 cooldown ("Try again in 17h 59m" parsing) and auto-recovery. Zen keys rotate the same way. Egress proxies rotate per request so IP-based rate limits don't bottleneck one address; proxy failures never poison account state.
- **Function calling that actually works.** Battle-tested against live upstreams across chat stream/non-stream, parallel tool calls, `/v1/responses` and Anthropic streaming, with repair logic for upstream tool-argument quirks. See the test matrix below.
- **English admin panel** at `/admin/`: Dashboard, Accounts, Add accounts (OAuth / refreshToken / static `sk_` API key — auto-detected, also batch), Gateway settings (API keys, models, headers), **Proxy pool** (one shared socks5/http egress list with per-upstream toggles for Cline and zen), opencode free models, Combos, request logs, live stats.

## Quick start (Docker)

```bash
docker run -d --name cline-proxy \
  -p 3457:3457 \
  -v cline-proxy-data:/app/data \
  -e PORT=3457 \
  -e API_KEY=change-me \
  -e ADMIN_PASSWORD=change-me-too \
  -e ZEN_KEYS=sk-your-zen-key-1,sk-your-zen-key-2 \
  ghcr.io/foxy1402/cline-proxy:latest

curl http://127.0.0.1:3457/health
```

Then open `http://127.0.0.1:3457/admin/`, log in with `ADMIN_PASSWORD`, and add accounts or zen keys (keys can also be pasted in the panel instead of `ZEN_KEYS`). The image is multi-arch (`linux/amd64`, `linux/arm64`); `:latest` tracks `main`, version tags (`vX.Y.Z`) and `:sha-xxxxxx` tags are also published. It contains only the Go binary on Alpine (~43 MB) — session IDs for zen are minted locally, so there is no bundled CLI or Node runtime.

> The image name follows the GitHub repository name. If your repo is named differently, replace `cline-proxy` with the repo name (GHCR images are always lowercase).

## Portainer stack template

Paste into Portainer → **Stacks → Add stack**, fill in the environment variables in the UI (Portainer interpolates them), then deploy:

```yaml
services:
  cline-proxy:
    image: ghcr.io/foxy1402/cline-proxy:latest
    container_name: cline-proxy
    restart: unless-stopped
    ports:
      - "${PROXY_PORT:-3457}:${PROXY_PORT:-3457}"
    volumes:
      - cline-proxy-data:/app/data
    environment:
      - PORT=${PROXY_PORT:-3457}
      - API_KEY=${API_KEY:?set API_KEY in the stack environment}
      - ADMIN_PASSWORD=${ADMIN_PASSWORD:?set ADMIN_PASSWORD in the stack environment}
      # opencode zen keys, comma-separated (optional; can also be set in the
      # admin panel later — panel keys survive restarts in the data volume)
      - ZEN_KEYS=${ZEN_KEYS:-}
      # OAuth account org id, sent as x-opencode-org-id (optional; only needed
      # when the key is an OAuth token rather than a raw API key)
      - ZEN_ORG_ID=${ZEN_ORG_ID:-}
      # cline account seed file inside the volume (optional; import runs when
      # the pool is empty — drop cline-seed.json into the volume to use it)
      - CLINE_ACCOUNTS_SEED_FILE=/app/data/cline-seed.json
      # Unknown model names return 400 instead of silently serving a default
      - STRICT_MODEL_MATCH=${STRICT_MODEL_MATCH:-true}
      # Request log (metadata only), on by default
      - LOG_REQUESTS=${LOG_REQUESTS:-true}
    healthcheck:
      test: ["CMD", "wget", "-q", "-O", "/dev/null", "http://127.0.0.1:${PROXY_PORT:-3457}/health"]
      interval: 30s
      timeout: 5s
      start_period: 10s
      retries: 3

volumes:
  cline-proxy-data:
```

Environment variables to define in the Portainer stack UI:

| Variable | Required | Example |
|---|---|---|
| `API_KEY` | yes | any long random string — the only key accepted on `/v1/*` |
| `ADMIN_PASSWORD` | yes | admin panel login password |
| `PROXY_PORT` | no | defaults to `3457`; changes both the container listen port and the host mapping |
| `ZEN_KEYS` | no | comma-separated opencode zen keys (e.g. `sk-aaa...,sk-bbb...`); leave empty to use the anonymous `public` key or paste keys in the admin panel |

No other variables are needed for a working stack: zen session IDs are minted locally (no harvester env), and everything else has a sane default (see the full table below).

To seed Cline accounts on first boot, drop a `cline-seed.json` file into the volume (see [Seeding accounts](#seeding-accounts)).

> **Public HTTPS:** `docker-compose.yml` ships an optional `caddy` service (profile `tls`) with a ready `Caddyfile` for automatic Let's Encrypt certificates. Set `DOMAIN` in `.env`, then `docker compose --profile tls up -d`. On Portainer/NAS, put the proxy behind your existing reverse proxy instead and forward to port `3457`.

> **Bind-mount note (Portainer/NAS users):** the container runs as a non-root user, so a **named volume** (`-v cline-proxy-data:/app/data`) is preferred. If you bind-mount a host directory (e.g. `-v /opt/cline-proxy-data:/app/data`), pre-create it and chown it to UID 1000 (`chown -R 1000:1000 /opt/cline-proxy-data`) or the gateway cannot write its state files and will fail to persist accounts.

## Configuration

All state lives in the `/app/data` volume — dotfiles written `0600`: `.cline-accounts.json` (account pool), `.zen-config.json` (zen keys/settings), `.zen-sessions.json` (per-key sticky session IDs), `.zen-endpoints.json` (learned chat-vs-responses routes), `.zen-model-aliases.json` (410 deprecation remaps), `.egress-bindings.json` (key/account → egress proxy, hashed keys), `.session-secret`, plus `combos.json`, `requests.jsonl`, `zen-stats.jsonl` and `cline-proxy.log`. Backup = copy the volume.

| Variable | Default | Description |
|---|---|---|
| `PORT` | `3457` | Listen port (`-port` flag wins over the env var) |
| `DATA_DIR` | `/app/data` | State directory inside the container |
| `API_KEY` / `API_KEY_FILE` | empty | The single valid `/v1` key (via `Authorization: Bearer` or `x-api-key`). When set, admin-panel-generated keys are ignored for `/v1` |
| `ADMIN_PASSWORD` / `ADMIN_PASSWORD_FILE` | empty | Admin panel password; when set, every `/admin/*` route requires login |
| `REQUIRE_ADMIN_AUTH` | `true` | Set `false` only when a reverse proxy already handles auth |
| `PROXY_PORT` | — | Compose/Portainer convenience: sets host mapping + `PORT` together |
| `STRICT_MODEL_MATCH` | `true` | `400` for unknown model names instead of silently serving the default model |
| `POOL_STRATEGY` | `round_robin` | Cline account strategy: `round_robin` / `fill` / `random` (env wins over panel config) |
| `ZEN_KEYS` | empty | opencode zen keys, comma-separated; panel config is not overwritten when it already has keys |
| `ZEN_ORG_ID` | empty | OAuth account org id, sent as `x-opencode-org-id`; not needed for raw API keys (the CLI only sends it for OAuth accounts) |
| `ZEN_SESSION_ROTATE_MINUTES` | `120` | Legacy: sticky-session rotation interval. Sessions are minted per API call since 2026-10-27, so the rotator is disabled and this value is inert |
| `CLINE_ACCOUNTS_SEED_FILE` | empty | Seed JSON imported at boot when the pool is empty |
| `CLINE_USE_PROXIES` | `false` | Route the Cline upstream through the egress proxy pool |
| `LOG_REQUESTS` | `true` | Request logging (metadata only: IP, path, model, status, duration — never conversation content) |
| `LOG_FILE_MAX_MB` | `10` | Size cap for `requests.jsonl` (wiped when exceeded) and for `cline-proxy.log` (rotated when exceeded) |
| `MAX_BODY_MB` | `32` | Request body limit; larger bodies get `413` |
| `APPLY_SYSTEM_PROMPT_OVERRIDE` | `false` | `true` enables replacing client system prompts with `override.md` |
| `STREAM_LOG` | `false` | Dump raw Anthropic-path SSE to disk (full conversations — debugging only) |
| `CLIENT_IP_HEADER` | empty | Trust this header for client IP behind a reverse proxy (e.g. `X-Real-IP`); by default only `RemoteAddr` is used |

Fail-closed startup: binding a non-loopback address without `API_KEY` and `ADMIN_PASSWORD` refuses to start.

### opencode zen session IDs

Zen session IDs are **minted locally, one per API call** — the upstream's free-tier gate is a format check (`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`), so the gateway generates a compatible ID per request. There is no embedded CLI, no harvester, and no related environment variables.

Since the v2 CLI (`2.1.4`) observation on 2026-10-27, the gateway no longer reuses one sticky session across unrelated requests: the official CLI creates a fresh session per `run` invocation, and a session reused across unrelated history-less requests gets flagged to midnight UTC after the first call passes. Each user API call therefore gets its own session, which is reused only for retries of that same call and re-minted if the key switches mid-call or a 403 forces a refresh. IDEs and agents (Cursor, ZCode, OpenClaw, Hermes) already send full history in every request, so nothing is lost — and it costs no extra upstream calls. The legacy sticky table is still persisted for the admin panel snapshot, but its age-based rotator is disabled.

The same format check also applies to the per-request id and to the injected gate tools — the request id is minted with the same descending construction as the session id, and the twelve gate tools carry the real opencode CLI v2 parameter schemas (an empty `{"type":"object","properties":{}}` schema is rejected with `403 FreeTierError`; the tool descriptions are not checked). This is what the upstream's 2026-10 gateway change tightened.

On repeated FreeTier `403`s the session for that call is re-minted automatically (one retry per call); a sustained rise in the refresh rate in the logs is the tripwire that zen has tightened its gate.

Requests also must carry a system message whose `<env>` block contains a `Working directory:` line — the CLI always appends this block and the free tier rejects bodies without it. The gateway appends it (using its own working directory) to an existing system/developer message when one is present, matching the client's own layout instead of adding a second system message.

The zen route table is seeded deterministically from the model catalog's SDK package name (`provider.npm`: `@ai-sdk/openai` → native `/v1/responses`, `@ai-sdk/openai-compatible` → `/chat/completions`) and refined per model by the endpoint learner, whose decisions persist in `.zen-endpoints.json` and are never overwritten by later catalog syncs.

The upstream base URL is `https://opencode.ai/inference/openai/v1`. It is not guessed: every catalog sync reads the opencode provider's `api` field from `models.opencode.ai` (the same data the real CLI reads) and corrects the saved config to match — but only when that config still holds the project default. A URL you typed into the admin panel is treated as deliberate and never rewritten. The sync refuses any value that is not `https://opencode.ai/inference/…`, so a tampered catalog response cannot redirect a request carrying your `Authorization` header.

A 429 cools the key that hit it for the upstream `Retry-After` (capped at 24h, since the value usually points at the daily reset). One client request may cool at most `1 + retries` keys — default 5 — because switching keys is irreversible while waiting is not. Without that bound a single oversized request walks the whole pool and cools every key in seconds; the log line `key-switch budget exhausted` marks where it stopped.

Context compaction fires at `context − max(output, buffer)` tokens, with `buffer` defaulting to 80000. The old 20000 never fired for a 200k-window / 32k-output model when the client itself caps input around 128k, so full-size requests went upstream untouched. Raise `buffer` further if your client's cap is lower than `context − 80000`; a saved config still holding exactly 20000 is migrated automatically, and any other value you set is left alone.

On load, a saved config pointing at one of the two historical defaults is migrated and logged as `zen baseURL migrated: …`: `https://opencode.ai/zen/v1` (the pre-2026-10 console shim, which strips the validated headers and 403s anonymous free-tier traffic) and `https://opencode.ai/inference/v1` (a stale default that omitted the `/openai` segment — corrected 2026-10-30 after capturing the CLI's real traffic).

| Variable | Default | Description |
|---|---|---|
| `ZEN_PIN_KEY` | empty | Troubleshooting: pin all upstream attempts to key number *n* (1-based) instead of rotating, so one key can be tested in isolation |
| `ZEN_ORG_ID` | empty | OAuth account org id, sent as `x-opencode-org-id` when set; omit for raw API keys |
| `ZEN_SESSION_ROTATE_MINUTES` | `120` | Legacy: sticky-session rotation interval. Sessions are minted per API call since 2026-10-27, so the rotator is disabled |

### One egress proxy per key

opencode zen now records the egress IP per api key and flags a key seen from several IPs as suspicious — with pure round-robin every key cycles through the whole proxy pool, so the whole pool gets marked even when the proxies themselves are clean. The gateway therefore **pins one proxy per identity**:

- each zen key is assigned one proxy and always leaves through it; a proxy serves one identity at a time
- if there are fewer proxies than keys, the surplus keys are **skipped** rather than sent direct or borrowing another key's exit (either would reintroduce the "one key, many IPs" fingerprint) — the request fails with an explicit capacity error, and the panel marks those keys `no egress`
- the assignment persists in `.egress-bindings.json`, so a restart does not reshuffle it (a reshuffle is exactly the IP change that triggered the flag). zen keys are stored as a truncated sha256, so the file holds no plaintext credentials
- adding a proxy gives it to the next unassigned key; removing a proxy unbinds its keys instead of silently moving them
- zen and cline are separate namespaces: the same proxy may serve one zen key **and** one cline account, since each upstream only ever sees one identity behind it

Cline accounts get the same treatment whenever the cline pool uses proxies (`CLINE_USE_PROXIES` or the panel switch), so the rule is already in place if cline adopts it. The switch is **Pin one egress proxy per key** on the opencode tab (default on; also stored as `pinProxyPerKey` in `.zen-config.json`). With no proxies configured the binding is inert and traffic goes direct, as before.

The admin panel's opencode tab shows per-key session liveness, age and next rotation, plus a per-key **Test** button with a probe-model picker (default **auto**: big-pickle first, then live-synced models; any free zen model can be chosen): it sends one real probe request pinned to that key, and on success clears the key's cooldown immediately (a rate-limited answer — 429, or a 403/503 whose body zen words as a limit — reports the upstream's expected recovery time instead). Note a successful probe consumes one request of that key's quota — the same trade-off as the cline Test button.

## Connecting your IDE

**Cursor / ZCode / anything OpenAI-flavored** — `POST /v1/chat/completions`:

```
Base URL:  http://<host>:3457/v1
API Key:   <API_KEY>
Model:     cline-free/deepseek-v4.1-flash   (cline pool)
           big-pickle                       (opencode zen)
```

**Claude Code / Cline** — Anthropic dialect `POST /v1/messages`, same base URL and key.

**OpenAI Responses dialect** — `POST /v1/responses` (tool calls, streaming events, and usage are fully translated).

`GET /v1/models` lists every model the gateway can serve, including your Combos. Free-model feeds sync automatically (cline official feed every 60s, zen catalog every 10min): the live list is authoritative and the built-in seeds only cover a cold start / offline boot, so delisted or newly-freed models appear or disappear on their own. If the zen catalog is unreachable at boot, the gateway starts with the built-in seed models and picks up the live list at the next successful sync.

### Seeding accounts

Two account kinds are supported and can be mixed:

```json
[
  {"refreshToken": "workos-oauth-refresh-token", "email": "acc1@example.com"},
  {"apiToken": "sk_...", "email": "acc2@example.com"}
]
```

Mount the file anywhere in the container and point `CLINE_ACCOUNTS_SEED_FILE` at it; it's imported when the pool is empty. Static `sk_` API keys are used directly as Bearer tokens (no refresh; a 401 marks them expired). OAuth accounts can also be added via the panel's browser login flow, manual token paste, or batch import.

### Combos (alias models)

Create user-defined alias IDs in the panel (e.g. `my-cline-flash` → `cline-free/deepseek-v4.1-flash` on the cline platform, or any zen free model). Strictly same-platform targets; aliases show up in `/v1/models` so IDEs can pick them directly.

## Battle-tested

Verified against live upstreams with real free-tier credentials (16-probe chat matrix + 12-probe function-calling matrix, all passing):

- Streaming shape (role chunk, `[DONE]`, `finish_reason` `tool_calls` vs `stop`), full tool round-trips, parallel tool calls with distinct indices/ids
- `stream_options.include_usage`, array content parts, long multi-turn histories, parameter passthrough, clean 4xx errors
- Client aborts propagate (no account/key cooldown pollution), 8-way parallel load, container healthcheck, seed import, key rotation

Known upstream quirks (not gateway bugs): zen's `muse-spark-*` models only work on the native `/v1/responses` endpoint (the gateway routes them there automatically — seeded from the catalog's SDK package name and confirmed by the endpoint learner — and re-emits chat/Anthropic shapes); a deprecated free model that answers `410 Gone` with a `replacement` hint is transparently remapped to its successor (alias persisted across restarts), and a `400 "Model is unavailable"` marks the model dead until the next successful catalog sync instead of burning an upstream call per request; cline's `stop` handling wipes content when the model's reasoning echoes the stop word (gateway truncates non-stream output as compensation); some reasoning-heavy models eat small `max_tokens` budgets before producing visible text (raise the budget if you get empty content with `finish_reason: length`).

## Development

```bash
go build ./... && go vet ./...
./cline-proxy -host 127.0.0.1 -port 3457   # or: go run . 
./start.sh                                  # build-or-docker wrapper
docker compose up -d --build                # build from source (PROXY_PORT to change the port)
```

One more env var is dev-only and deliberately off by default: `KILL_PORT_ON_START=true` makes the gateway force-kill whatever holds the listen port before binding (a Windows convenience, implemented with `Stop-Process`). Leave it unset in production — it kills a process it did not start, and that may be a legitimate service.

CI: every push to `main` runs `go build` + `go vet`, then publishes the multi-arch image to GHCR via Buildx (amd64 compiled natively, arm64 cross-compiled via `TARGETARCH` — the whole build is QEMU-free now that the embedded opencode CLI stage is gone). Tag a release with `v*` to publish `:vX.Y.Z` alongside `:latest`.

Project layout:

```
├── main.go                  entry point, CLI flags, running detection
├── internal/app/
│   ├── proxy.go             /v1 routing, chat handler, upstream calls, aggregation
│   ├── responses.go         /v1/responses dialect translation
│   ├── zen.go               opencode zen upstream, routing, rate-limit defense
│   ├── zen_session.go       sticky zen sessions (locally minted ses_, per-key identity)
│   ├── zen_deprecated.go    410 auto-remap aliases + 400 unavailable dead-marking
│   ├── zen_endpoint.go      endpoint seeding (catalog npm) + auto-learn (chat vs /v1/responses)
│   ├── tls_bun.go           uTLS ClientHello mimicry for the zen upstream
│   ├── compact.go           opencode-style context compaction for zen free models
│   ├── proxy_pool.go        egress proxy pool (uTLS/h1 client cache, per-request rotation)
│   ├── models.go            cline free-model feed sync + default model
│   ├── pool.go              cline account pool (OAuth refresh, static API keys, cooldowns)
│   ├── seed.go              boot-time account seeding
│   ├── combos.go            alias model IDs
│   ├── admin.go/_auth/_html/_zen  admin panel: API, session auth, UI, zen page
│   ├── config.go            env-driven configuration (fail-closed checks)
│   ├── logs.go / stats.go   request logging + token statistics
│   └── types.go             shared data structures
├── internal/cline/          cline upstream auth (WorkOS OAuth refresh)
├── internal/kit/            HTTP client, random IDs, zen session-ID minting, data paths
├── Dockerfile               multi-arch (amd64 native + arm64 cross-compile), ~43 MB runtime image
├── docker-compose.yml       source build, PROXY_PORT-parameterized
└── Caddyfile                optional TLS reverse proxy (compose `tls` profile)
```

## Credits

Built on [YuJunZhiXue/Cline-proxy](https://github.com/YuJunZhiXue/Cline-proxy). Thanks to the [LINUX DO](https://linux.do) community.

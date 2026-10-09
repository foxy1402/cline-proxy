# TODO — Public container deployment: stateless /v1 proxy with multi-account rotation

## Scope (the product)

Expose `/v1` as a simple stateless OpenAI-compatible endpoint for coding IDEs
(Cursor, ZCode) and personal agents (OpenClaw). Run in a container, publicly
exposed, long-lived and lightweight. Rotate multiple **Cline** accounts and
multiple **OpenCode Zen** accounts round-robin to maximize free-tier limits.
**Combos**: dashboard-defined alias model IDs (e.g. `cline-proxy`,
`opencode-proxy`) that upstream to a chosen concrete model — same platform only.
The admin panel is setup/maintenance only, not part of daily operation.

Release blockers before ANY public exposure: **M1 (env config + /v1 API key)**
and **M2 (admin auth)**. Everything else can land incrementally.

## Zen sessions minted locally; harvester deleted (2026-10-08)

Live probes (2026-10-09, through this repo's exact direct stack — Bun TLS
fingerprint + CLI headers) proved zen's free-tier session gate is a **stateless
format check**: any ID matching `^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$` passes
(200 ×3 incl. reuse); the old `sess_`+26 placeholder 403'd purely on format.
The harvester's premise ("only CLI-minted sessions pass") was confounded by
that malformed placeholder. Consequences, all landed:

- **`kit.MintZenSessionID`** ports opencode's descending `SessionID.create()`
  (inverted `ts_ms<<12|counter` → 12 hex + 14 crypto/rand base62 chars);
  `kit.ValidZenSessionID` encodes the gate regex. Sticky sessions
  (`zen_session.go`) mint locally; no embedded CLI, no per-key HOME dance.
- **Migration on load**: legacy `sess_*` placeholders are replaced with freshly
  minted IDs (they 403'd forever); legacy `ses_*` entries kept as-is;
  `harvestedAt` renamed to `createdAt` on write.
- **403 recovery is local** (`refreshZenSession`): 2 consecutive FreeTier 403s
  per key → fresh minted session, 1-minute per-key backoff (the old 10-minute
  cooldown existed only because CLI mints were expensive). If zen ever tightens
  the gate, the refresh rate in the logs is the tripwire.
- **Harvester deleted**: `zen_harvest.go`, its tests, the admin mint buttons /
  `/sessions/mint` endpoints, all six `ZEN_HARVEST*` env vars, the Dockerfile
  CLI stage (~185MB off the image; QEMU no longer used at build time) and
  `docs/zen-harvester.md`. The registry-unreachable model-sync fallback through
  `opencode models` is gone too — offline startup serves the seed list only.
- **Deterministic route seeding**: the catalog's per-model `provider.npm` now
  seeds `Upstream` (`@ai-sdk/openai` → native `/responses`,
  `@ai-sdk/openai-compatible` → chat/completions), replacing the muse-spark
  name heuristic; the endpoint learner stays as the correction layer for drift.
- **Deprecated-model handling**: upstream `410` with a `replacement` hint
  transparently remaps to the successor (persisted in
  `.zen-model-aliases.json`, one retry per request); `400` "Model is
  unavailable" marks the model dead until the next catalog sync.


## Current state (audit 2026-09-15)

- Admin panel (`/admin/`, all `/admin/api/*`) has **no authentication at all**;
  CORS `*` everywhere (`internal/app/proxy.go:328`).
- Proxy endpoints check client API keys **only if at least one key exists** —
  empty key list = wide open (`internal/app/proxy.go:107`).
- Go code reads **no env vars** (`PORT` in the Dockerfile is never read; flags only).
- Secrets/config in `data/`:
  - `.cline-accounts.json` — Cline account refresh tokens + client proxy `Keys`
  - `.zen-config.json` — **OpenCode Zen** config: single `Key` (`zen.go:158`),
    baseURL, outbound proxy list, failover/compaction settings
- Cline account rotation already exists: `pickAccount()` (`internal/app/pool.go:154`)
  with `round_robin` (default) / `fill` / `random`, per-account daily token counters.
- OpenCode Zen has a **single** key — no multi-account rotation (`internal/app/zen.go`).
- `override.md` (replaces the client's system prompt, `internal/app/proxy.go:349`)
  is applied whenever the file exists; docker-compose mounts it by default.
- `requests.jsonl` logs per-request **metadata only** — time, client IP, method,
  path, model name, route tag, status, duration (`internal/app/logs.go:18`); no
  message content. Mounted on every route incl. `/admin` (`proxy.go:298`);
  in-memory ring of 500 + 10MB file cap that wipes (not rotates); always on,
  no toggle.
- Dockerfile: root user, no HEALTHCHECK, no `.dockerignore`.

## M1 — Env-var configuration layer + /v1 API key (blocker)

- [ ] Env config loader (new `internal/app/config.go`): `PORT`, `DATA_DIR`,
      `API_KEY`, `ADMIN_PASSWORD`, `POOL_STRATEGY`, `LOG_REQUESTS`,
      `LOG_FILE_MAX_MB`, `APPLY_SYSTEM_PROMPT_OVERRIDE`, `ZEN_KEYS`,
      `REQUIRE_ADMIN_AUTH`. Flags keep precedence (document the rule).
- [ ] `API_KEY` env → the only valid credential for all `/v1/*` endpoints
      (accept `Authorization: Bearer` and `x-api-key`). When set, it overrides
      the admin-generated key list entirely — stateless, no DB lookup.
      Compare with `crypto/subtle`.
- [ ] Fail closed: publicly listening (non-loopback host) without `API_KEY`
      → refuse to start (matches your "only my env key can call upstream" rule).

## M2 — Admin auth via env var (blocker)

- [ ] `POST /admin/api/login` → HMAC-signed session cookie (`HttpOnly`,
      `SameSite=Strict`, `Secure` behind TLS), ~7d expiry; also accept
      `Authorization: Bearer <session>` for curl/scripting.
- [ ] Wrap ALL admin surfaces: `adminStaticHandler`, every `/admin/api/*`,
      `/admin/zen/` panel (`internal/app/admin.go:81` — easy to miss).
- [ ] `ADMIN_PASSWORD` env (or `ADMIN_PASSWORD_FILE` for docker secrets);
      `crypto/subtle` compare; login rate limit per IP (5/min).
- [ ] Fail closed: non-loopback host + no `ADMIN_PASSWORD` + `REQUIRE_ADMIN_AUTH=true`
      → refuse to start. Logout endpoint clearing the cookie.

## M3 — Multi-account round-robin (core scope)

Cline pool (mostly done, wire it up):
- [ ] **Round-robin is the load-balancing default** — already true in code
      (`defaultProxyConfig` admin.go:807 sets it, and `pickAccount()`'s switch
      falls through to round_robin, pool.go:187). Keep it that way.
- [ ] `POOL_STRATEGY` env → `cfg.Strategy`, **default `round_robin`**; env value
      wins over any persisted config so a redeployed container always comes back
      to round-robin (your multiple accounts spread every call to maximize
      per-account limits).
- [ ] Verify round-robin distributes by call (not by token count) and that
      cooldown/expired accounts are skipped cleanly (`pool.go:179`).
- [ ] Startup seeding for disposable containers: `CLINE_ACCOUNTS_SEED_FILE`
      (mounted JSON array of `{refreshToken, email}`) auto-imported once at boot
      if the pool is empty — makes `docker compose up` on a fresh host fully
      reproducible without OAuth clicks.

OpenCode Zen (new feature, mirrors the Cline pool):
- [ ] Multi-key support: `zenConfigData.Key string` → `Keys []string` with
      per-key usage/cooldown state (`internal/app/zen.go:156`).
- [ ] **Round-robin is the default (and only) strategy for the zen key pool**,
      same as the cline pool: keys rotate per call so your multiple zen accounts
      share load evenly and per-account limits are maximized. 429/quota errors
      additionally trigger cooldown + instant retry on the next key.
- [ ] Round-robin across zen keys on each upstream call (`zen.go:403`, `zen.go:510`);
      on 429/quota errors, mark key cooling and immediately retry on next key.
- [ ] `ZEN_KEYS` env (comma-separated) seeds the key list at boot when config is empty.
- [ ] Admin UI: show zen key list with per-key usage; keep single-key input
      working as a 1-element list (backward compat with existing `.zen-config.json`).

## M3.5 — Proxy routing & abort hardening (done 2026-09-15)

Per-request proxy rotation for all upstreams + client-abort safety:

- [x] **Per-request rotation**: each upstream attempt explicitly picks an exit
      (`pickUpstreamProxy`, round_robin default) via a per-proxy pinned client
      cache (uTLS Chrome fingerprint + h2). Old behavior rotated per *dial*,
      which with HTTP/2 connection reuse meant far less rotation than expected.
- [x] **Proxies apply to cline upstream too** (before: zen only). Enabled by
      `CLINE_USE_PROXIES=true` env (provider-wide) or per-combo `useProxies`
      toggle (dashboard checkbox). zen upstream uses the pool whenever
      `proxies` is configured in its config (unchanged), now per-request.
- [x] **Dead-proxy handling**: failed exit gets a 5-min cooldown and the retry
      automatically uses the next one; proxy failures never mark accounts/keys.
- [x] **Client aborts (IDE cancel/abort) can't poison state**: client context is
      propagated into both upstreams — a cancel terminates the upstream call (no
      wasted quota), aborts bypass retry loops, and never count as rate-limit or
      network failures on keys/accounts/proxies. Background summary generation
      uses its own context (unaffected by client aborts).
- [x] Cooldown poisoning audit: only genuine upstream 429/limit signals cool
      keys/accounts/proxies; stream write errors to a dead client were verified
      to touch no cooldown state.

## M4 — Combo model aliases (dashboard)

Virtual model IDs the IDE calls; each combo maps to one concrete upstream model
on **its own platform only** (a cline combo selects from cline `/models`, a zen
combo from zen's list — cross-platform selection must be impossible).

- [ ] Data model: `combo = {id, platform: "cline"|"zen", target, createdAt}`,
      persisted under `data/`. The alias `id` is **fully user-chosen at creation**
      — you type any name you want (`cline-glm-5.3`, `opencode-proxy`,
      `cline-deepseek`, ...); the only restriction is it must not collide with a
      real upstream model ID (rejected at save with a clear error).
- [ ] CRUD API: `GET /admin/api/combos` (list), `POST /admin/api/combos`
      (create), `DELETE /admin/api/combos/{id}` (delete). Renaming = delete +
      recreate; no hidden magic IDs, all combos are user-defined.
- [ ] Routing hook at the top of `routeModel()` (`internal/app/zen.go:108`):
      combo ID → route to its platform and rewrite the upstream model to `target`.
- [ ] Enforce same-platform at the API layer, not just the UI: on save, a `cline`
      combo's `target` must exist in the cline model cache, a `zen` combo's in
      `resolveZenFreeModel` (free models only).
- [ ] Fallback: if `target` is inactive/expired at call time, fall through to that
      platform's existing default-model logic instead of erroring the request.
- [ ] Include combo IDs in `/v1/models` output so Cursor/ZCode/OpenClaw can pick
      them directly.
- [ ] Dashboard UI (Models section): create/delete combos — free-text alias field
      (your own name), platform picker, then a model dropdown locked to that
      platform's model list. Show target model + platform per combo in the list.
- [ ] Seed defaults after first deploy: `cline-proxy` → `deepseek/deepseek-v4-pro`
      (cline), `opencode-proxy` → chosen zen free model.

## M5 — Import / export all credentials

- [ ] Unified backup: `{version, exportedAt, accounts[], keys[], zenConfig{keys, baseURL, proxies, strategies}, combos[]}`
      — `GET /admin/api/backup/export`, `POST /admin/api/backup/import`
      (`mode: merge|replace`, per-item validation results).
- [ ] Admin UI buttons in Settings (file download / file picker + confirm on replace).
- [ ] Keep legacy `/admin/api/accounts/export` + `/batch-import` working.

## M6 — Lightweight long-run defaults

- [ ] `LOG_REQUESTS` env, **default true (enabled)** — the log is metadata-only
      (no message content, no keys) and disk-bounded (next item), so keeping it
      on costs almost nothing. Set `LOG_REQUESTS=false` to disable entirely:
      no `requests.jsonl` writes, admin log tab shows empty, and the
      request-body probe is skipped too (`logs.go:144` buffers the whole body
      just to extract `model` for the log — wasted work on the hot path when off).
- [ ] Log cap: **keep the existing wipe-on-cap behavior** — when `requests.jsonl`
      passes the cap the file is emptied (`logs.go:58`). Accepted trade-off:
      losing old metadata logs is fine, the admin panel still shows the
      in-memory last 500 entries, and this avoids complicating the logging code
      with a rolling-rewrite scheme. Only change: make the cap size
      configurable via `LOG_FILE_MAX_MB` (default 10). The in-memory 500-entry
      ring stays as is.
- [ ] When logging is off, also skip the request-body probe (`logs.go:144`
      buffers the entire body just to extract `model` for the log) — avoids
      per-request buffering work on the hot path.
- [ ] System-prompt override off by default: `APPLY_SYSTEM_PROMPT_OVERRIDE` env
      (default false) gates `applyOverride` (`proxy.go:349`); Cursor/ZCode/OpenClaw
      keep their own prompts. Remove the `override.md` mount from the default
      docker-compose (opt back in by uncommenting).
- [ ] Rotation of any other growing files (stats jsonl) — cap or disable by default.

## M7 — Container polish

- [ ] Read `PORT` env in `main.go` (fix the Dockerfile mismatch); `DATA_DIR` respected.
- [ ] Dockerfile: non-root `USER`, `HEALTHCHECK` on `/health`, keep CGO_ENABLED=0.
- [ ] `.dockerignore`: `.git`, `data/`, `*.log`, `capture*`, `dist/`, `override.md`.
- [ ] docker-compose: env-driven (`API_KEY`, `ADMIN_PASSWORD`, `ZEN_KEYS`, ...),
      healthcheck, no override.md mount by default.
- [ ] Optional compose profile: Caddy sidecar for automatic TLS in front of /v1 + admin.
- [ ] README "Public deployment" section: required env vars, TLS, backup/restore,
      upgrade flow (volume holds the only state).

## M8 — Later / nice to have

- [ ] Change admin password from the UI (hash in data/, overrides env).
- [ ] Per-key/per-account usage dashboard improvements; zen key cooldown display.
- [ ] Multi-arch images (arm64) via GitHub Actions.

## M9 — Hardening round 1 (2026-09-15, done)

Full-codebase audit (3 parallel review passes: proxy/zen core, protocol
conversion, admin/auth/deploy) then patch of all confirmed findings.
Commit `5b60cc3`. Highlights:

- [x] Request body cap (MAX_BODY_MB=32) + 413 before handlers; envBool
      fail-closed; admin keys from crypto/rand; wildcard CORS removed from
      /admin/api/*; esc() quote-safe + data-attr event sinks; oauthSessions
      and loginFails lifecycle; config update copy-on-write.
- [x] callClineAPI rebuilds request per attempt (retry loops reused the
      consumed body); account refresh single-flight + only 400/401/403
      expires an account; token-expiry parse failure falls back to 55min
      TTL; cooldowns capped 24h; mid-stream errors surface as 500.
- [x] Anthropic/Responses translators: real usage passthrough, parallel
      tool calls, deterministic tool order, orphaned tool-message guards
      in compaction, temperature=0 / stop_sequences no longer dropped.
- [x] Graceful shutdown (SIGTERM, 10s drain); Dockerfile go.sum; CI vet
      gate; cline upstream UA + crypto-random session IDs; auth JSON
      client 60s timeout; .env gitignored; raw SSE dump (STREAM_LOG)
      default off.

Known deferred (low risk, revisit if needed):
- zen-stats.jsonl still grows unbounded (aggregate-rewrite idea).
- Dead credentials subsystem in internal/cline/auth.go (GetToken /
  LoadCredentials) — delete or mutex-guard before ever reusing.
- main.go "already running" detection is Windows-only; releases auto-tag
  every push to main.

## Settled trade-offs (do not re-propose)
- /v1/responses with a native-responses zen model (muse-spark) goes through
  the chat-completions upstream and 500s — by design. The Upstream-aware
  path for spark is /v1/chat/completions (routes to native /v1/responses
  upstream automatically). README's "spark via /v1/responses" claim refers
  to that routing. Fixing /v1/responses client-dialect for spark would
  require a responses-dialect emitter for the native upstream; deferred.
- ZEN_DEBUG_BODY was a temporary probe hook, removed before commit.
- zen chat tool_choice policy (2026-09-18, supersedes the "text-QA gateway"
  note in zen.go history): split by client intent, verified by direct
  upstream A/B on ling — tool_choice=none (no client tools) returns text;
  tool_choice=auto (client tools present) lets models call the client's
  own tools. Keep the split: making auto unconditional regressed ling
  multi-turn to empty text. Responses path (spark) must always send auto
  (gate rejects none) and passes client tools through as flat items.

## Audit of 8a9e2c3 (2026-09-18, fixed in b50191f)
Reviewed the tool-call/IDE changes with three parallel reviewers, then fixed
and re-probed (spark-1.3 + mimo, 21/21 on a fresh container). Points worth
remembering, either as behaviour or as "don't re-propose":
- Client tool definitions intentionally win over the gate stubs of the same
  name (the gate only checks that the *names* exist; the stub's empty schema
  was what made a client `read` call lose `file_path`). Gate names still all
  ship in the request, so the FreeTier check is unaffected.
- A delta whose item_id/output_index belongs to no accumulator is dropped on
  purpose (the later done/output_item event carries the full arguments);
  adopting it into another call is what produced the {"query":..}{"url":..}
  jam. Do not "fix" this by falling back to the last call again.
- SSE comment/heartbeat lines (": keep-alive", mimo sends them before the
  first chunk) are valid SSE; the non-SSE detector must only classify a body
  as JSON when the first non-empty line is not an SSE field line.
- Compaction summary generation now issues stream=true and aggregates it
  (both zen endpoints 403 on stream=false). Remaining limitation: for a
  responses-native model (spark) the summary request still carries the gate
  tools with tool_choice=auto, so the model may answer with a tool call and
  the summary falls back to truncation. Accepted; revisit only if spark
  compaction quality becomes a real complaint.
- Reasoning-heavy models (spark) need real max_tokens headroom: with
  max_tokens≈400 the reasoning consumes the budget and the client gets empty
  content with finish_reason=length. That is upstream model behaviour, not a
  gateway bug — probes should use ≥1500-2000 tokens for tool turns.

## Live catalog as the source of truth (2026-09-18, 9499f99)
The model tables used to be a hybrid: seed ids cross-checked against live
sources. Settled the other way — live is authoritative, seeds are bootstrap.
- zen: the pricing gate (models.opencode.ai `opencode` provider, cost 0/0,
  status != deprecated) *is* the real list. `opencode models` reads the same
  data (probed: CLI 7 = registry free 7), so spawning the CLI per sync adds
  nothing — it is now only the registry-down fallback, paired with the
  `-free` suffix heuristic. Do not reintroduce a seed cross-check: a seed id
  that no longer passes the gate only kept dead models listed.
- cline: /ai/cline/recommended-models is quota-independent — probed 200 on
  three accounts while every pool account was 429'd for inference. Hence
  pickAccountAny() for the sync; with only pickAccount() the whole sync
  skipped during a quota outage and the list froze on the seed snapshot.
- Seeds (both platforms) are pruned like everything else once a live list
  arrives; the existing guard stays (skip the prune when live < half of the
  known table) so a truncated feed cannot wipe the list.
- `defaultModel` is now empty = no preference, and getDefaultModel() picks
  the lexicographically smallest active id. The old random map pick made a
  pruned preference drift to a different model per request. The panel's
  DefaultModel still overrides.
- Consequence to expect: ids that only existed as seeds are gone
  (deepseek/deepseek-v4-flash, stepfun/step-3.7-flash). Requests for them
  return 400 "not available on this gateway (see /v1/models)" unless
  STRICT_MODEL_MATCH=false. A combo pinned to a stale id needs re-pointing.

## cline "requires stream" self-learning (2026-09-18, a4de61a)
The free-model feed publishes only id/name/description/tags, so "does this
model have to be called with stream=true" is inferred from the id shape
(models.go: no ":" in the id -> force upstream streaming). That inference
can only be wrong one way, and the upstream says so explicitly: 500
{"error":"empty response content"}.
- callClineAutoStream (cline_stream.go) wraps callClineAPI on the three
  non-stream ingresses. On exactly that fingerprint it learns the id into
  DATA_DIR/.cline-stream-required.json and retries with stream=true,
  returning streamed=true so the caller aggregates the SSE - the client
  still gets plain JSON with status 200.
- Only the exact fingerprint is learned. Other 5xx pass through untouched:
  a transient overload must never be persisted as a model property (unit
  test guards this). One-way learning is deliberate - over-forcing is
  transparent, under-forcing is the only visible failure.
- To un-learn a model, delete it from that JSON file (or the file) and
  restart; there is no reverse learning and none is planned.
- do not replace the colon heuristic with this: learning costs one failed
  non-stream request per model, the heuristic costs nothing when right.
  They are complementary - heuristic first, learning as the safety net.
- clineCallFn is a package-level seam so the retry path is testable against
  a fake upstream; production value is always callClineAPI.

## arm64 image: QEMU at build time only, no native runner needed (2026-09-18, verified)
Question was whether supporting arm64 means wrapping the Dockerfile in QEMU
instead of a native arm64 build. Measured answer: both are true already and
nothing needs to change.
- opencode-ai@1.18.31 (still npm `latest`) DOES publish arm64 binaries,
  including opencode-linux-arm64-musl (os linux / cpu arm64 / libc musl) -
  so the "CLI is amd64-only" premise is wrong for the pinned version.
- Verified locally with `docker buildx build --platform linux/arm64`: the
  whole build succeeds (exit 0), including the emulated `npm i -g
  opencode-ai` + `opencode --version` (262s under QEMU). The resulting image
  boots, /health returns ok, the banner auto-detects the default model, and
  /app/bin/opencode inside it is a native aarch64 ELF that prints 1.18.31.
- Cost model: QEMU is used ONLY at build time on GitHub's amd64 runners, and
  only for the npm stage (the Go binary is cross-compiled natively via
  GOOS/GOARCH from BuildKit args). At runtime on an arm64 host everything is
  native - no emulation, no performance penalty.
- No CI change needed: the existing setup-qemu + platforms
  linux/amd64,linux/arm64 already produces a working arm64 image. Image size
  is ~540MB on both arches (dominated by the ~185MB opencode binary).
- Caveat if a future opencode-ai release drops arm64: the npm stage would
  fail the arm64 leg and CI would go red (not silently degrade). If that
  happens, gate the CLI install on $TARGETARCH and copy from a directory so
  an empty dir still satisfies COPY - the gateway itself is arch-independent
  and harvestEnabled() already degrades gracefully when the binary is absent.

### Correction (same day, after the real push): the local arm64 build was misleading
The paragraph above said "no CI change needed" on the strength of a local
`buildx --platform linux/arm64` build that passed. The actual CI run failed
on the arm64 leg with exit code 132 — QEMU: uncaught target signal 4
(Illegal instruction) inside `[linux/arm64 opencode-cli 2/2]`. Root cause
is in opencode-ai's own postinstall: it ends with `verifyBinary()`, which
spawns `opencode --version` and treats any non-zero exit as "the package
manager installed the wrong binary", then exits 1 — so `npm i` fails, not
just the check. Docker Desktop's QEMU runs that Bun binary; GitHub's does
not. **Lesson: a foreign-arch build that passes locally under Docker
Desktop's QEMU proves nothing about GitHub's QEMU — for arm64 the only
trustworthy local check is the artifact (ELF arch + native-arch run), and
the real gate is CI.**
Fix (commit `b95e749`, CI green, GHCR verified): the CLI stage branches on
TARGETARCH. Same arch keeps the official `npm i -g opencode-ai` (its
verification works there). Cross arch fetches the same published npm
tarball (opencode-linux-arm64-musl) with busybox wget, extracts the binary,
and validates the architecture from the ELF header (e_machine 0xB7) instead
of executing it — nothing foreign runs at build time, and a wrong-arch
artifact fails the build. Version is pinned once via `OPENCODE_VERSION`
ARG for both branches. Unpinned arches (arm/v7) build without a CLI and
degrade to gateway-only, as harvestEnabled() already documented.
Verified end to end: both CI jobs green, manifest publishes amd64 + arm64,
and the pulled `ghcr.io/foxy1402/cline-proxy:latest` arm64 image boots with
/health ok and its embedded CLI printing 1.18.31.

## Whole-codebase audit (2026-09-18) — findings, fixes, and settled trade-offs

Four parallel audit agents covered the tree (~14.7k lines) partitioned by
subsystem: zen path, proxy + cline_stream, admin/panel/pool, responses +
compact + kit. Every P0/P1 was re-verified against the code before patching;
each fix below was then re-verified in a container against the real upstreams
(21/21 behaviour regression + 8/8 new patch cases).

### P0 — features that were dead or destructive in production
- **cline "requires stream" self-learning never fired.** `callClineAPI` returns
  a nil response for every non-200 (body read and closed), but
  `callClineAutoStream` only sniffed the body when it got a 500 *response* —
  a shape the real callee cannot produce. The unit test passed because the
  fake upstream returned `(500 response, nil error)`. Fixed by adding a typed
  `clineAPIError{Status, Body}` and classifying by status, and by rewriting
  the test fake to mirror the real contract. A new end-to-end test drives the
  **real** `callClineAPI` against a local upstream (injectable `clineAPIBase`).
- **"Refresh all tokens" expired every static-key account.** The admin loop
  called `refreshAccountToken` for all accounts; `doRefreshAccountToken` marks
  any `APIToken` account `expired` ("static key cannot refresh"). The startup
  prewarm already guarded this, the admin path did not. Now skips them and
  reports the count.
- **A transient zen 500 permanently misrouted a model.** `isWrongEndpoint`
  substring-matched the whole error text against a keyword list containing
  `"no such"` and `"endpoint"` — and Go's DNS failure text is "dial tcp:
  lookup …: no such host", so one network blip was learned and persisted as
  "this model needs /responses", which the catalog sync never rewrites.
  Classification is now status-code based (`zenHTTPError`), and 502/503/504
  are excluded.
- **Learning now requires the retry to succeed.** Probing live showed
  muse-spark returning a bare 500 on its *correct* responses endpoint twice in
  five minutes, so a bare 500 is not proof of a wrong endpoint.
  `handleZenResponsesNative` returns whether the request was actually served,
  and the flip is persisted only then.

### P1 — silently wrong behaviour
- SSE-level `error` events were swallowed by `collectStreamResponse` (empty
  200 instead of an error).
- `emitChatAsSSE` sliced content at byte 2048, splitting multi-byte runes into
  U+FFFD; it now retreats to a rune boundary. Verified live with 372 CJK
  characters over the SSE path (0 replacement chars).
- `chatStreamToResponses` sent `response.completed` with empty output when the
  upstream answered 200 with a non-SSE JSON body; it now emits
  `response.failed`, matching `collectStreamResponse`.
- The zen Anthropic non-stream path skipped `truncateAtStopSequences`, so
  `stop_sequences` silently did not apply there.
- `ZenModel` fields were mutated in place while request paths read the same
  pointer outside the lock. Writers now use copy-on-write and the invariant is
  documented on the type.
- `harvestOnForbidden` did a split check-and-set (N concurrent 403s each
  spawned a CLI harvest) and did not skip the `"public"` sentinel, so it would
  overwrite the admin's real `auth.json` credential with `key:"public"`.
- `Retries` had no cap: `delay *= 2` overflowed to a negative duration and
  retries became a no-backoff hot loop. Now capped at 30s via
  `zenRetryDelay`.
- `buildTransport` set no `TLSHandshakeTimeout`/`ResponseHeaderTimeout`, so a
  hung upstream could hold all 8 zen semaphore slots forever. Set to 15s/5min
  — **5 minutes, not 90s**, because this transport is shared with cline, whose
  non-stream requests legitimately take minutes to first byte. Deliberately a
  guard against "never responds", not a latency budget.
- `ADMIN_PASSWORD_FILE` / `API_KEY_FILE` failed **open**: if the file became
  unreadable at runtime the value became "" (meaning "not configured"), which
  disabled panel auth and made `/v1` fall through to "no key required". Both
  now return a per-process random un-matchable sentinel, logging once.
- `collectStreamResponse` never closed the response body (its sibling
  `responsesSSEToChat` always did).
- `fallbackTruncate` `continue`d past non-fitting messages, keeping older
  history and dropping the newest turn; it now breaks, keeping a contiguous
  tail.
- `compactThreshold` had no floor, so a small-window model with a large
  declared output re-summarised on every single turn.
- `/v1/responses` ignored the admin "zen disabled" switch; the `429` panel
  toast double-escaped (showing `&quot;error&quot;:` literally), zero times
  rendered as year 1, and proxy cooldowns used a clock-only formatter for
  cooldowns that can reach 24h.
- `getNested` accepted negative indices (latent panic).

### Settled trade-offs (do not re-propose)
- **Endpoint learning is confirmation-based, not fingerprint-based.** An extra
  retry per request for the affected model, in exchange for never
  permanently misrouting a healthy model. Rejected alternatives: trusting a
  bare 500 (disproved live), and sniffing `provider.npm` from the catalog
  (23 of 29 free models have no npm field, no pattern).
- **`ResponseHeaderTimeout` stays at 5 minutes.** A tighter value would break
  cline's legitimate long non-stream generations; the goal is only to break
  the "accepts the connection and never answers" case.
- **Secret files seal instead of falling back.** An explicit `ADMIN_PASSWORD`
  is still ignored while `ADMIN_PASSWORD_FILE` is set but unreadable — this is
  intentional: a broken secret mount must not silently downgrade auth.
- **The cline naming convention stays** (`":" ⇒ no forced stream) and learning
  remains one-directional: a false positive only costs one server-side
  aggregation, a false negative is what the learning path exists to repair.
- **`zenSessions` / harvest counters are pruned on key removal**; the learn
  cooldown map is not (bounded by the model count, which the catalog sync owns).

## Zen first-boot session minting: parallel + per-key HOME (2026-09-18)

Reported from the live instance: with 11 zen keys, the first calls after a
fresh deploy were extremely slow, and the logs showed a chain of
`session rejected (403)` across keys #5..#11 with harvests interleaved.
Root cause was two-independent things:

- **The request path had no notion of session liveness.** `pickZenKey` only
  skipped rate-limit cooldowns, so it happily handed out keys whose session
  was a locally random `sess_` placeholder — which is a *guaranteed* 403
  (verified 2026-09-17: the server only accepts sessions it has seen). One
  client request therefore walked key after key, each costing a full upstream
  round trip (~15-20s in the reported logs), before failing.
- **Minting was strictly serial.** One global mutex for the whole harvest,
  a shared CLI `HOME` (so parallelism was impossible: every key rewrote the
  same `auth.json`), a 5s stagger between keys, and a failure path that could
  burn 3 models × 3 attempts × 60s = 9 minutes *per key*.

### Changes
- **Per-key CLI HOME** (`ZEN_HARVEST_HOME/keys/<sha256(key)[:6]>`): auth.json
  and CLI logs are isolated, so keys mint in parallel and the shared HOME is
  never touched. The directory name is a hash — the key itself must not end
  up in a path (logs, `ps`, `ls`).
- **Bounded parallel minting** (`mintZenSessions`, `ZEN_HARVEST_CONCURRENCY`,
  default 3) used by all three triggers: startup, the hourly stale-key pass,
  and the new manual button.
- **Per-key budget** (`ZEN_HARVEST_KEY_TIMEOUT_SECONDS`, default 150s) so the
  9-minute failure path can't hold a worker.
- **`pickZenKey` prefers keys with a live session** (two passes: not-cooling +
  live, then not-cooling) instead of handing out certain-403 keys.
- **Panel**: new "Live session IDs" section on the opencode tab — `N/M live`
  summary, a per-key table (live / stale / not minted + last mint time), and
  two buttons: *Mint missing sessions* and *Force mint / refresh all*.
  `POST /admin/api/{opencode,zen}/sessions/mint` runs in the background and
  returns immediately; `GET .../sessions` reports per-key state plus
  per-key progress of the running job (the panel polls it every 2s).
- A key is only marked live by an actual CLI mint, and a **failed mint never
  overwrites** an existing live session, so a mis-clicked force refresh can't
  cost a working session.

### Measured (container, real upstream, 4 real zen keys, fresh volume)
- Startup mint: all 4 keys live **~65-78s** after boot (3 in parallel within
  ~35s; the slow key needs the 60s run timeout once). Previously serial.
- `Force mint / refresh all`: 4/4 OK, per-key 7-13s except one 60s.
- Warm restart with an existing session file: no mint work at all, first
  request served immediately (no 403 chain).
- 20/21 IDE regression probe; the single failure is the known-flaky
  `parallel_calls_no_jam` on muse-spark (200 with 0 tool calls), which passes
  on re-run with both tools called — upstream model variance, not the gateway.

### Settled trade-offs (do not re-propose)
- ~~**`ZEN_HARVEST_CONCURRENCY` defaults to 3, not "as many as keys".**~~ The CLI
  is a Bun runtime; the limit exists to protect container CPU/memory, not to cap
  throughput. **Superseded 2026-09-18: the default is now 1 (serial).** Three
  still-starved a 1-2 core cloud instance — see "Zen harvest must be serial by
  default" below.
- **Background job + polling, not a synchronous endpoint.** A force refresh of
  11 keys is ~1 minute when healthy and up to ~10 in a pathological case;
  holding an admin HTTP request open that long invites reverse-proxy timeouts.
- **Live-session preference is a two-pass rotation, not a filter.** When no key
  has a live session (the first seconds of a fresh boot) requests must still
  go out — the 403 is then the only signal available, and the harvester is
  already minting.
- **Skip-if-live is the default; force is explicit.** Re-minting a working
  session wastes upstream quota for no benefit, so the non-force button is the
  normal action and only the force path re-mints live keys.

## Session refresh interval vs the zen 5h quota window (2026-09-18)

Operator question: zen's free tier is ~200 requests per egress IP per 5 hours,
so are session IDs only valid inside that window — should the refresh interval
change? **Half right, and the half that's wrong matters:** the quota is keyed on
the egress **IP**, not on the session, so re-minting a session does not reset or
refill anything. Extra capacity comes from more exits (13 socks5 IPs × 200 ≈
2600/5h), never from fresher sessions. Minting in fact *spends* quota: harvest
runs deliberately go direct, so they egress the container's own IP and share
that IP's bucket with request traffic (and with any socks5 exit served from the
same host).

The half that's right is the *upper bound* on session lifetime. If sessions die
with the window, an interval above it guarantees a stretch where every key's
session is stale: requests start 403ing and recovery depends on
`harvestOnForbidden` (2 consecutive 403s per key, 10-minute per-key cooldown),
so failure rate and first-byte latency spike every cycle.

### Changes
- **Default `ZEN_HARVEST_INTERVAL_HOURS` 6 → 4.** Under the window, so sessions
  are replaced before they expire. Cost is ~66 mints/day for 11 keys against a
  ~960/day bucket — noise. Going to 1h would be ~264/day and would actually
  compete with request traffic for quota.
- **Periodic tick 1h → 10min.** The check runs on a ticker, so the real refresh
  time is interval + up to one tick: a 4h target with an hourly ticker lands at
  4h-5h, i.e. right on the window boundary. A 10-minute tick pins it at 4h-4h10m.
- **Overlapping periodic batches are skipped** (`periodicBatchMu.TryLock`): the
  stale list is computed before a batch runs, so a second tick during a long
  batch would re-mint the same keys (the batch hasn't written `HarvestedAt`
  back yet). The next tick recomputes the list anyway.
- **403 logs now carry the rejected session's age** —
  `[placeholder (never minted, always 403)]` vs `[minted 5h12m ago]`. Without
  the distinction a 403 tells you nothing about session lifetime; with it, the
  first `minted ... ago` rejection in the logs *is* the measured lifetime, and
  the interval can be tuned from evidence instead of assumption.
- Panel shows the active cadence ("Auto-refresh every 4h … concurrency 3") so
  the setting is visible without reading env vars.

### Settled trade-offs (do not re-propose)
- **Refresh interval stays a fixed timer, not a per-session TTL probe.** We do
  not yet know the real lifetime; until the new 403 log lines produce one, any
  probe would be guessing with extra requests. 4h is safe under either outcome.
- **Harvest runs stay direct (no proxy pool).** Routing the CLI through socks5
  would spread mint traffic across exits, but the mint is the CLI's own
  registration handshake and going direct is what has been verified to work;
  mint volume is small enough that the quota sharing is not worth the risk.

## Session-mint / session audit over the 3 unpushed commits (2026-09-18)

Scope: `ea41b6a` + `4c9d367` + `e77b432` (session minting, sticky sessions, the
panel button, the refresh interval). Three independent review passes (concurrency
& resources, session lifecycle, API/panel/security) over the same diff; every
finding below was re-verified against the code before fixing, and the fixes are
in the tree after those 3 commits.

### Fixed
- **Data race on the mint-job snapshot (P1).** `startZenMintJob` returned
  `zenMintJobSnapshotLocked(job)` *after* unlocking, while the job goroutine
  writes `job.outcomes[idx]` under the same mutex — reading that slice unlocked
  is a race. Confirmed with the race detector: reverting the fix makes
  `go test -race -run TestMintJobStatusHasNoSnapshotRace` report
  `WARNING: DATA RACE` (write in the worker vs read in the start path); with the
  fix the whole suite is race-clean. Snapshot is now taken inside the critical
  section.
- **Per-key mint locks were pruned while possibly held (P1).**
  `pruneZenKeyState` deleted `harvestKeyLocks[key]` for removed keys; an in-flight
  mint holding that mutex would then let `lockHarvestKey` hand a *second*
  goroutine a fresh mutex for the same key — two concurrent mints sharing one
  per-key HOME (auth.json and CLI logs overwrite each other, sessions cross-read).
  Locks are no longer pruned (entry count is bounded by distinct keys ever
  configured); the counters still are.
- **Never-minted keys could never be swept (P2).** The periodic scan used
  `HarvestedAt` with a fallback to `Updated`, and `Updated` is refreshed on every
  request — so an actively requested key with only a local placeholder was
  "fresh" forever and could only be repaired by the 403 path. Freshness is now
  `HarvestedAt` only (0 ⇒ stale), with a per-key `harvestLastSweep` throttle so a
  key that keeps failing to mint is retried once per interval rather than every
  10-minute tick (the old 1h ticker made that retry rate 6× cheaper). Extracted
  as `periodicSweepCandidates` so the rule is unit-testable.
- **Legacy session files re-minted the whole pool on upgrade (P2).** Files
  written before `minted` existed have no such field, so every key looked
  unminted and first boot of the new version re-minted all of them. Loading now
  back-fills `minted` from the session-ID prefix (`ses_*` = CLI-minted,
  `sess_*` = local placeholder; `sess_` does not start with `ses_`) and dates
  `harvestedAt` from `updated`, so an upgrade boots with zero mint traffic.
  Verified live: legacy-shaped file → `zen sessions migrated: 4 key(s)`, no
  startup mint, first request 200.
- **Corrupt session file was silently discarded.** Parse failure now renames the
  file to `.corrupt` before starting empty (same as `zen-config.json`); a
  non-ENOENT read error is logged, since the next save would otherwise overwrite
  an intact file. Verified live in the container.
- **Panel poll could stop permanently.** A second mint click / early return left
  a cleared-but-non-null `ocSessPoll`, and the visible-tab poll (`!ocSessPoll`)
  then never restarted. Handle is now nulled on every exit path.
- **Mint buttons are disabled when the CLI is absent** (`harvestEnabled=false`)
  instead of only failing with a 400 after the click.
- **Key masks no longer leak short keys.** `kit.Truncate(k,8)+"…"` returns the
  whole string for keys ≤8 chars and double-ellipsised longer ones; replaced by
  `maskZenKey` at all four display sites (also renders the `public` sentinel).
- Empty `ua` on a loaded entry is back-filled with the current CLI UA, and the
  swallowed body-read error in the mint handler now returns 400.

### Accepted (verified, deliberately not changed)
- `saveZenSessionsLocked` holds `zenSessMu` across a small file write: that
  serialisation is exactly what makes concurrent harvest saves safe. All writers
  hold the lock, so the `.tmp`+rename cannot interleave.
- A mint that succeeds in memory but fails to persist is still reported OK: the
  session is live for this process and only a restart loses it (then re-minted).
- An in-flight mint can re-insert a key that config just removed; the entry is
  inert (never picked, not in `cfg.Keys`) and is pruned on the next config change.
- Session-ID length differs between logs (24) and panel (12) — cosmetic.
- gofmt has pre-existing comment-format drift in untouched lines (CI only runs
  `go vet`); not reformatted to keep the audit diff reviewable.

## Zen harvest must be serial by default (2026-09-18, field failure)

Reported from the cloud instance (1-2 cores): after deleting the volume and
seeding 4 zen keys, the first boot minted **nothing** — every key logged
`harvest: no session minted (tried opencode/big-pickle, …-free)` after burning
the full 150s per-key budget. The same 4 keys minted fine locally in ~64s, and
the published GHCR image was verified to be identical, so the artifact was not
the problem.

Root cause (measured in a 0.5-core / 600MB container on the same keys):

| concurrency | observed (0.5 core / 600MB, 4 real keys, fresh volume) |
|---|---|
| 5 | first attempt of 3 of the 4 keys **killed at the 60s per-attempt timeout** (`exit=-1 err=signal: killed elapsed=60.5s`); an earlier run on the same shape had per-key mints at 70-131s, the last key landing on the 150s budget edge |
| 1 (new default) | 4 keys serial, one at a time: key#1 72s (that run also pays the CLI model-list fetch), keys #2-#4 at 19/17/17s, ~125s total, no kills |

The concurrency-5 run above still finished (all 4 live, ~132s) because the retry
had a fresh 60s; the field box was weaker and had *all* keys fail. Either way the
signal is the same: at concurrency >1 the CLI gets starved, at 1 it never does.

Each `opencode run` is a Bun process that bursts a large block of CPU and
memory on startup. On a small instance the runs starve each other, every mint
slides past the per-key budget, and the pool reports "no session minted" — which
reads as "the harvester is broken" rather than "the box is oversubscribed".
Minting is early and network-independent (the CLI writes `message=created
id=ses_` ~4s in even with a blackholed network), so a missing session means the
CLI never got that far, not that the upstream refused it.

### Changes
- `harvestConcurrency()` default 3 → **1**; range still 1..8, so an instance
  with real headroom can still opt in.
- `harvestBatchTimeout(nKeys)` replaces the fixed 15-minute batch cap at all
  three trigger sites. Serial worst case is `keys × 150s` (11 keys ≈ 27 min),
  which the old cap would have truncated mid-batch, leaving the tail keys with
  no attempt at all. Formula: `ceil(keys/concurrency) × budget + 2min`, clamped
  to 15..45 min.
- **Per-attempt timeout restored real retries.** Each CLI run now gets its own
  60s context; before, the per-key budget spanned all attempts, so attempt 1
  hanging consumed the budget and attempts 2-3 never ran (which is why the
  fallback models were never actually tried).
- **Failures now carry the CLI's own output.** `harvestRunResult` captures
  exit code, elapsed and the tail of stdout+stderr (ANSI/control stripped,
  any `sk-…` redacted, 400 bytes); the per-attempt log line and the aggregate
  error include it, so "CLI wouldn't start" / "starved past timeout" /
  "upstream refused" are distinguishable without guessing.
- Startup logs an explicit hint when **all** keys fail on a multi-key pool,
  pointing at the concurrency knob.
- Admin API exposes `keyTimeoutSeconds` and the panel derives its poll cap from
  `keys ÷ concurrency × keyTimeout` instead of a hardcoded 450 ticks, so a
  serial batch of many keys is polled to completion.

### Settled trade-offs (do not re-propose)
- **Serial (`1`) is the default, not 2-3.** The extra wall-clock is
  `keys × ~18s` (4 keys ≈ 1 min); the failure mode it avoids is a pool that
  cannot mint at all on a small instance. Do not raise the default.
- **Do not "fix" this by raising the per-key budget instead of serialising.**
  A starved run does not converge — it just occupies the worker longer, and the
  budget is what bounds the *failure* path. Serialising removes the starvation.
- **Docs' old advice "raise `ZEN_HARVEST_CONCURRENCY` to 5-8 for many keys" was
  wrong for small instances** and has been removed; the guidance is now "2-3,
  and only if the box has headroom — if all keys fail while parallel, go back
  to 1."

## Audit of cc48b4c (2026-09-18, fixed in the follow-up commit)

Reviewed the serial-default change set line by line, re-ran the constrained-box
A/B, and checked every claim in the message against the code. The change set's
core is right (serial default + work-derived batch bound + real per-attempt
retries), and the field failure is reproduced and fixed. Four defects found:

### Fixed
- **`no session minted` could report `exit=0 err=<nil>`.** `last` is only
  assigned inside the attempt loop, so when the per-key budget is spent before
  the first attempt (batch timeout / cancel landing between the auth.json write
  and the first run), the error rendered a zero-value result — which reads as
  "the CLI ran fine and the upstream gave nothing" and sends the next debugger
  to the upstream. `harvestNoSessionErr` now names that case explicitly
  (`no CLI run started: per-key budget 2m30s was already exhausted`). A real
  run and every stub both set `Elapsed`, so zero-value is unambiguous.
  (Compared field-by-field, not `== harvestRunResult{}`: `Err` is an interface
  and comparing it panics when the dynamic type is uncomparable.)
- **`cliModelIDs` had no pipe bound.** It used `CombinedOutput` with a 30s ctx
  and no `WaitDelay`: when the ctx kills `opencode models`, any quietly alive
  grandchild holding the inherited pipe keeps the read open — and this call
  holds `mintModelsMu`, so a hang there stalls the whole batch, not just one
  key. Same `WaitDelay = 10s` as `runHarvestCLI` now. (Could not reproduce the
  hold with the real binary inside the container — `opencode models` releases
  its pipe promptly when killed — so this is a latent hazard, fixed for
  consistency with the run path rather than for a reproduced failure.)
- **Panel poll cap was missing the backend's 15-minute floor.** The JS formula
  was `ceil(total/workers) × perKey + 120`, which for 1-4 keys is 5.5-13 min,
  while `harvestBatchTimeout` floors at 15 min. The polling stopped before the
  backend could still be working, so the progress panel silently froze. Now
  `Math.max(900, …)`. Verified all sizes 1-50 keys: panel cap >= backend bound.
- **Package-level doc comments still advertised the old default.** Two spots
  said `ZEN_HARVEST_CONCURRENCY 默认 3`; the change set updated the function
  comments but not the file header. Also corrected the adjacent stale claims
  (`每小时检查 … 默认 6h` → every 10 minutes, default 4h) and "隔离天然并发"
  (isolation makes concurrency *possible*; it is not the default).

### Verified, not defects
- **The concurrency path is race-free.** `go test -race` on the whole package
  inside `golang:1.26-alpine`: clean. `TestMintRespectsConcurrencyLimit` also
  run 10× for flakiness: stable.
- **Minting uses one goroutine per key up to `harvestConcurrency`, not one per
  key plus a semaphore.** `workers` is clamped to `len(todo)`, and `harvestSem`
  (sized `harvestConcurrency`) is the belt to that suspenders. With the default
  of 1 this is exactly one goroutine per batch, and `wg.Wait()` means a timed-out
  batch still returns (remaining keys are marked `canceled`), so no goroutine or
  slot leak. Verified by reading both paths.
- **A key minted by the 60s-kill path is genuinely usable.** In both serial
  runs key#1's session was captured *after* its 60s attempt was killed, so this
  was worth proving: a real `/v1/chat/completions` (big-pickle) through the
  gateway returned **200 OK in 3.6s** on those sessions, and the container log
  has no 403 / session-rejected lines. The CLI registers the session upstream
  (~4s in) well before the run is killed, so truncating the run does not
  produce a half-registered session.
- **Measured numbers are reproducible.** Serial, 0.5 core / 600MB: 4/4 minted
  twice in a row (125s and 124s; key#1 ~60s including the model-list fetch,
  the rest 16-19s). At concurrency 5 on the same shape: 3 of 4 first attempts
  killed at `elapsed=60.5s`. Both match the commit message.
- **A pre-existing 150s literal.** `harvestOnForbidden` still uses
  `150*time.Second` instead of `harvestKeyBudget()`, so
  `ZEN_HARVEST_KEY_TIMEOUT_SECONDS` doesn't affect that path. Pre-existing, not
  introduced here, and behaviourally identical at the default — left alone to
  keep this diff scoped.
- **gofmt "failures" are the known pre-existing comment reformat drift**, not
  this change: 21 files flagged both at cc48b4c and in the current tree, and
  CI runs only `go build` + `go vet`.

### Settled trade-offs (do not re-propose)
- **A batch's 45-minute ceiling means at most ~17 keys at the default budget.**
  Beyond that the batch is truncated and the tail keys are reported `canceled`
  (never falsely OK); they are re-minted by the next 403 trigger or periodic
  sweep. The ceiling exists so one wedged job cannot hold the worker forever.
  Raising it is the wrong fix — the keys that need re-minting are exactly the
  ones the 403/sweep triggers already cover.
- **At concurrency 1 the global harvest slot serialises the manual mint job
  against 403-triggered per-key mints.** A reactive mint that cannot get the
  slot inside its own 150s budget fails and is remembered by the 10-minute
  throttle, then covered by the periodic sweep. Accepted cost of not letting
  the CLI saturate a 1-core box.

## Zen per-key "Test" button (2026-09-19)

Parity with the cline accounts tab: the opencode zen keys used to be a single
comma/line box plus a one-line status summary, and a cooling key had no
recovery action — the cooldown came from the upstream `Retry-After` (measured
twice: zen's `FreeUsageLimitError` returns a Retry-After that runs until 00:00
UTC — an integer seconds count (`Retry-After: 35609` at 14:06:31 UTC = exactly
midnight), not an HTTP date; up to ~24h) and all you could do was wait.

### What was built
- `POST /admin/api/zen/keys/test` (`{"index": n}`): one real probe request
  ("Reply with exactly: OK", smallest free zen model, routed by the model's
  `Upstream` field like any normal request). The whole call is pinned to the
  probed key via a new variadic `zenCallOpts{pinKey}` on `callZenAPI` /
  `callZenResponsesAPI` — pinned calls never switch keys on 429/403 and return
  immediately with the typed error, so the verdict belongs to that key alone
  and rotation is untouched (unlike `ZEN_PIN_KEY`, which is process-wide).
- 2xx → `active` and the key's cooldown is cleared (`uncoolZenKey`) plus its
  403 fail counter reset and usage bumped — same "test success resets state"
  semantics as the cline Test button. 429 → `cooldown` with `cooldownUntil` /
  `remaining` read back from the cooldown the call itself just set. 403 →
  `error` "session no longer live" (the call already triggered the harvester).
- Panel: the one-line key summary is now a per-key table (mask / usage /
  session state / cooldown incl. expected recovery time in the browser's
  timezone / Test button); the result toast carries model + latency + reason.
- Verified in-container against the real upstream: 3 keys → active (~700ms,
  cooldown cleared, usage bumped), 1 key → cooldown with the real upstream
  body (`FreeUsageLimitError`, recovery at 00:00 UTC). Full suite + `-race`
  green; 6 new unit tests cover pin semantics, uncool, and the handler.

### Settled trade-offs (do not re-propose)
- **A successful probe spends one request of that key's quota.** Same trade-off
  as the cline Test button; a probe that reports cooldown spends nothing (the
  429 is free). Do not "fix" this by faking the probe with a lighter call —
  anything that doesn't pass the FreeTier gate proves nothing.
- **The probe model is chosen, not the client's model, in priority order:
  big-pickle (the free-tier default alias, same first choice as the mint
  path) → smallest-ID `Source=="live"` model (upstream is provably serving
  it) → smallest-ID free model (cold-boot seed fallback).** The button tests
  key+session+quota, not model quality; pinning per-model would duplicate
  endpoint learning for no diagnostic gain. Same day, superseded in part: the
  panel has a probe-model dropdown, and an explicit pick overrides the auto
  order (validated as a resolvable free zen model; unknown names error out
  instead of silently falling back). The "smallest free model" rule
  alone was replaced 2026-09-19: it only landed on big-pickle by alphabet
  luck and would drift as the live catalog grows.
- **Pin returns on first 429/403 instead of retrying.** A retried probe would
  either test a different key (switch) or re-enter a cooldown the operator was
  asking about. One call, one verdict.

## Audit of 624bec2 + 6e9bb5e (2026-09-19, fixed in the follow-up commit)

Three parallel reviewers over the Test-button feature and the Retry-After
logging; every finding re-verified against the code before fixing.

### Fixed
- **(P1) The panel toast was wrong for every outcome.** The JS reads
  `r.status`, but the handler only put the verdict in `apiResponse.Message`,
  never in `Data` — so every click rendered "Key #N — undefined" in the red
  error style, and the headline "cooldown cleared" line never appeared. The
  cline counterpart works because `testAccount` embeds `status` in every
  result map; zen now does the same (`result["status"] = status` in the
  handler), with a handler-level test guarding the wire shape.
- **(P2) A "rate-limit-shaped 403" was misreported as "session dead".**
  `isRateLimited` sends keyword-bearing 403/502s (and all 503s) down the
  rate-limit branch — key cooled, harvester NOT triggered — but `testZenKey`
  switched on status alone, so such a 403 got the "session no longer live;
  the harvester was just triggered" message plus hidden cooldown state.
  `zenHTTPError` now carries `RateLimited bool` (set at the top of the branch
  so every exit path has it) and the handler classifies branch-first: any
  rate-limited error → cooldown verdict with recovery time; only a "clean"
  FreeTier 403 → session-dead verdict. Docs updated to the same distinction.
- **(P2) A pinned probe's upstream 5xx polluted global failover.** The 5xx
  `markZenFail()` ran before the pinned early-return, so three Test clicks on
  a 500-ing key would route ALL free-zen traffic to the cline pool for 5
  minutes. Both loops now skip `markZenFail` when pinned — consistent with
  the 429 path, which already did.
- **(P3) Successful probes double-counted usage.** The upstream 200 paths
  already run `markZenKeySuccess` + `harvestMarkSuccess`; `testZenKey` called
  both again (+2 per probe in the panel's usage column). Only `uncoolZenKey`
  remains in the handler; the call path owns the rest.
- **(P3) The 429 log line could misreport the applied cooldown.** It printed
  the raw parsed value, but `cooldownZenKey` applies the 1-minute default,
  the 24h cap, and the public-sentinel no-op. `cooldownZenKey` now returns
  the applied duration and the log prints that.
- **(P3) `zenProbeModel` wasn't self-sufficient.** The model table is filled
  lazily; probing on a fresh process before any model page/sync could report
  "no zen model available". It now calls `initZenModels()` first.
- **(nit) A pinned probe advanced the round-robin cursor** (chat path called
  `pickZenKey()` before overriding). Pinned calls no longer touch the cursor.

### Tests added
Status-in-Data (wire shape), rate-limit-shaped-403 → cooldown (and harvester
NOT triggered), the responses-upstream probe path (previously zero coverage),
failover-untouched-by-probe, public-key rejection; probe-test setup now clears
the package-level cooldown/harvest maps so state can't leak between tests.
Full suite + `-race` green.

### Accepted (verified, deliberately not changed)
- The 60s probe timeout uses `context.Background()`, not the request context —
  closing the tab leaves the probe running ≤60s. Matches the cline
  `testAccount` precedent and is strictly bounded.
- `resp != nil && err == nil && StatusCode != 200` is unreachable (every
  non-200 returns a typed error); the defensive branch in `testZenKey` stays.
- Endpoint learning is untouched by probes: it lives in the proxy handlers,
  none of which pass `zenCallOpts`, and `testZenKey` performs no learning
  retry.


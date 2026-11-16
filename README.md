# FastRouter

A fast local AI gateway, written in Go, that runs your existing 9Router setup.
One binary, SQLite storage, everything hot kept in memory.

## Run

```sh
go build -o fastrouter ./cmd/fastrouter
./fastrouter import ~/9router-backup.json   # one-time: bring over your 9Router setup
./fastrouter                                # serve on http://127.0.0.1:20128
```

- API: `http://127.0.0.1:20128`. Use your existing 9Router local API key.
  - OpenAI Chat: `/v1/chat/completions`
  - OpenAI Responses: `/v1/responses`
  - Anthropic Messages: `/v1/messages`, plus `/v1/messages/count_tokens` (a local estimate)
  - Gemini: `/v1beta/models/{model}:generateContent` and `:streamGenerateContent` (key in `x-goog-api-key` or `?key=`)
  - Model lists: `/v1/models`, `/v1beta/models`
- Usage: `/api/usage?window=24h|7d|30d|all` (also shown on the dashboard).
- Dashboard (read-only): `http://127.0.0.1:20128/dashboard`. Log in with your 9Router dashboard password (any username).
- Health: `/health`.
- Config: `~/.fastrouter/fastrouter.json` (created on first run). Use `--config` to pick another file, and `--host`/`--port` to override it. Listening on `0.0.0.0` is opt-in.
- After re-importing, send `SIGHUP` to reload without restarting.

Model names are `prefix/model`, for example `ds/deepseek-v4.1-flash`, or a combo name like `my-combo-team`.

## How it works

- **State:** On startup the configuration loads from SQLite into an immutable routing table, which a reload swaps atomically. Each provider has its own account pool with a small lock of its own, so there is no global lock. Account health uses atomics. Usage is written to SQLite by a background batch writer, never on the request path.
- **Passthrough:** When the client and provider formats match, the body is forwarded as-is. Only the top-level `model` field is rewritten, with a single byte scan. SSE is copied chunk by chunk with pooled buffers and flushed immediately.
- **Fallback:**
  - If an upstream fails before sending its first byte (a connection error, 401/402/403/404/408/429/5xx, or the first-byte timeout), the router tries the next account in the pool, then the next combo model.
  - Failed accounts cool down with exponential backoff and honour `Retry-After`.
  - A 400 is returned to the client unchanged.
- **Pools and combos:** Pools are round-robin with a sticky limit, or fill-first by priority, taken from your 9Router provider strategies. Combos are fallback or round-robin.
- **Timeouts:** There is a first-byte timeout and an idle timeout between chunks. A client disconnect cancels the upstream request.
- **Secrets:** API keys, tokens and the dashboard password are AES-256-GCM encrypted in SQLite, using `secret.key` (0600) in the data dir. Logs pass through a redactor.

## Formats and translation

Any client format can call any provider. When the client and provider speak the same format the body is passed through untouched (only `model` is swapped). Otherwise the request is decoded once into a shared in-memory form and encoded once for the provider, with no intermediate JSON. Answers and streams are translated back the same way, event by event.

- Covered: text, system prompts, tools and parallel tool calls, tool results, images, thinking/reasoning blocks and their signatures, redacted thinking, `cache_control`, stop/finish reasons and token usage (including cache and reasoning tokens).
- Thinking:
  - Effort levels and token budgets convert both ways (minimal 1024, low 4096, medium 10000, high 24576, max 32000).
  - For Anthropic, thinking is switched off when the history would be rejected (a tool turn with no signed thinking block), and unsigned thinking is dropped.
  - Signatures are only replayed to the provider family that issued them. Gemini gets its documented placeholder signature on tool calls that have none.
- Gemini tool schemas are cleaned of keywords Gemini rejects.
- If a provider streams when the client wanted JSON (or the other way round), the answer is folded or replayed in the client's mode.
- Errors from a translated upstream come back in the client format's error shape.

Which endpoint a provider uses: built-ins know their OpenAI, Anthropic, Responses and Gemini endpoints (e.g. DeepSeek takes Anthropic natively; Gemini prefers its native API). Custom endpoints use their 9Router type (`anthropic-compatible`, or `apiType: responses`).

## Imported 9Router behaviour

- **Thinking modes (`providerThinking`):** when the client didn't choose a thinking setting, the provider's mode is applied. `on` means a 10000-token budget, `off` disables thinking, and any other value is an effort level (your DeepSeek and OpenCode are set to `max`).
- **Vision fallback (`capacityAdapter.vision`):** a request with images whose models can't read images is routed to your vision list first (round-robin if set). Vision support comes from custom-model caps, then the model name.
- **Claude Code housekeeping:** for `claude-cli` clients, warmup, `count` and title-extraction calls are answered locally, as 9Router does. Topic naming is also answered locally if `ccFilterNaming` is on.
- **Empty-answer failover:** a stream is held back until its first real content, for up to `emptyResponseWait` (45 s). An empty answer moves on to the next account or model. A content-filter stop is not retried.
- **Per-key access:** 9Router's `access.restricted` allow-lists are enforced.

## Token savers

The four 9Router token savers run on every upstream request, in 9Router's order (RTK, Headroom, Caveman, Ponytail). Each defaults to your imported 9Router setting and can be overridden under `tokenSavers` in the config file:

```json
"tokenSavers": { "rtk": true, "caveman": "ultra", "ponytail": "off", "headroom": true,
                 "headroomUrl": "http://localhost:8787", "headroomCodeAware": true, "headroomAutostart": false }
```

- **RTK** compresses tool output (git diff/log/status, build and test logs, grep, find, ls, tree, long lists) in OpenAI, Anthropic, Gemini and Responses bodies. Its filters are ported from 9Router and tested against vectors generated by 9Router's own code (`internal/saver/testdata`).
- **Caveman** and **Ponytail** add 9Router's output-reduction prompts (verbatim) at the configured level to the system prompt. Your import sets both to `ultra`. Adding them is idempotent.
- **Headroom** sends the conversation to a Headroom proxy's `/v1/compress` (`headroomUrl`, 3 s timeout) and keeps the original body if the proxy fails, pausing calls for a while after a failure. With `headroomAutostart` on (off by default), FastRouter runs `headroom proxy --port N` for a localhost URL, adding `--code-aware` when `headroomCodeAware` is on and `--disable-kompress` when `headroomKompress` is false, the same as 9Router. Headroom must already be installed; FastRouter never installs it.
- **Opt-out:** send `X-9Router-Token-Saver: off` (9Router's header) or `X-FastRouter-Token-Saver: off` to skip every saver for one request.
- The response header `X-FastRouter-Token-Saver` lists what ran, for example `RTK:2,CAVEMAN:ultra,PONYTAIL:ultra`.

## Usage and limits

- Every attempt records account, model, key, status, latency and token counts (input, cached, cache-write, output, reasoning) from the provider's usage fields.
  - It is written in the background and batched once a second.
  - Cost is estimated from 9Router's price table plus your imported `pricing` overrides.
  - Reasoning tokens are not charged twice.
- Optional per-key limits in the config file. They are off by default:

  ```json
  "keyLimits": { "my combo": { "rpm": 60, "models": ["ds/*", "my-combo-team"] } }
  ```

## Importer

`fastrouter import <backup.json>` brings in:

- settings, provider strategies, thinking and capacity-adapter settings
- accounts, keeping `isActive` and `priority`
- custom endpoints
- combos and their strategies
- custom models
- local API keys
- the dashboard password, with its bcrypt hash unchanged

Custom endpoints that share a prefix are renamed so each one is reachable, and the importer prints a warning listing every rename. For your setup: `deepseek` keeps `custom` and also answers to `ds2`, `neon` becomes `neon`, `atria` becomes `atria`, and `deepseek ultr fast` becomes `og` (9Router's own prefix for it). Any other clash gets a prefix made from the endpoint name. An endpoint is always reachable by its id too.

Custom models whose provider no longer exists are flagged, not imported. Login-based accounts (antigravity, codex, cursor, glm, opencode) are stored but marked `needs-plugin`. The summary prints names and counts only.

## Tests and benchmarks

```sh
go test ./...      # importer, translators + goldens, passthrough, translated paths, fallback, usage, limits
go test ./internal/translate -update   # regenerate golden files after an intended change
go test -run '^$' -bench . -benchtime 3s ./internal/server/
```

`testdata/9router-backup.redacted.json` was produced by `scripts/redact_fixture.py`. Every key, token, password, cookie, email and account id in it is replaced.

## Credits

Behaviour follows [9Router](https://github.com/decolua/9router) (MIT, Copyright (c) 2024-2026 decolua and contributors): provider endpoints and aliases, translator behaviour (used as a reference; the Go translators are written fresh), the bypass rules, vision name patterns and pricing lookup order. The default price table in `internal/pricing/pricing_9router.json` is 9Router's data; its MIT licence is kept in `internal/pricing/LICENSE-9router`.

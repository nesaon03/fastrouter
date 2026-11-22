# FastRouter

A fast local AI gateway, written in Go, that runs your existing 9Router setup.
One binary, SQLite storage, everything hot kept in memory.

## Install (npm)

```sh
npm i -g fastrouter          # downloads the binary for this platform
fastrouter import ~/9router-backup.json
fastrouter                   # serve on http://127.0.0.1:20128
```

Updates are the normal npm flow, and the dashboard sidebar shows the same
command with a copy button:

```sh
npm i -g fastrouter@latest
```

The npm package is a small launcher: it downloads the matching release binary
from GitHub, verifies its SHA-256 against the release `checksums.txt`, and runs
it. Supported platforms: Windows x64, Linux x64/arm64, macOS x64/arm64.
`FASTROUTER_SKIP_DOWNLOAD=1` skips the download, `FASTROUTER_DOWNLOAD_BASE`
points at a mirror, and `FASTROUTER_STRICT_INSTALL=1` makes a failed download
fail the install instead of warning.

Source is MIT licensed (see `LICENSE`) at <https://github.com/nesaon03/fastrouter>; releases and the npm package are
published by the maintainer. Building from source is below.

## Run from source

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
  - Decisions: `/v1/decisions` (TypeSafe Jev / OpenAI Decisions; accepts either engine's shape)
  - Non-chat OpenAI routes: `/v1/embeddings`, `/v1/images/generations`, `/v1/audio/transcriptions`, `/v1/audio/speech`
  - Model lists: `/v1/models`, `/v1beta/models`
- Usage: `/api/usage?period=today|24h|7d|30d|60d|all` (totals by provider, model, account and key, a time series, the last 20 requests and in-flight counts) and `/api/usage/requests?period=…&provider=…&limit=…&offset=…` (one row per attempt). Both need a dashboard session or the old dashboard auth; account emails are masked.
- Dashboard: `http://127.0.0.1:20128/dashboard/`. Log in with your 9Router dashboard password. Laid out like 9Router's (orange theme, light/dark), it is embedded in the binary as prebuilt static files (Preact + htm, no Node at run time):
  - **Dashboard:** endpoint URLs with copy buttons, provider/account/combo counts, 24h traffic and import notes.
  - **Providers:** every provider FastRouter can route, configured or not (the 14 core ones plus 45 API-key providers generated from 9Router's registry, such as Groq, Cerebras, Mistral, Together and Fireworks), with search and an All / Configured / Not configured filter. Then sign-in providers, your custom endpoints, and the other 9Router providers that FastRouter doesn't support (greyed out). Add, edit, pause and delete accounts; drag to set account priority; add/edit/delete OpenAI- or Anthropic-compatible endpoints; Codex/GLM sign-in.
  - **Combos & Vision:** create, edit and delete combos with six strategies (Fallback, Round-robin, Smart Router with a chat or decision router, Fusion, Swarm Agent, Cascade) and the vision-fallback model list. A swarm combo shows a live debate board under its chain - one row per agent, one speech card per round, peer-exchange lanes, the judge at the end - fed by `/api/admin/swarm`, so each agent's message, thinking/answered/timeout state, who it read and any early consensus are visible while the debate runs; the Transcript button opens the full back-and-forth. Models are chosen in a searchable picker grouped by provider, with logos and friendly endpoint names; search supports `-exclude` and `/regex/`. The same picker is used for key allow-lists.
  - **Usage:** Overview and Details tabs; Today / 24h / 7D / 30D / 60D / All; cards for requests, input, cached and output tokens and estimated cost; a provider topology graph (FastRouter in the middle, the last request's edge in orange, failures in red, in-flight edges animated, zoom and pan); recent requests; a tokens / requests / cost chart; and per-provider, model, account and key tables. Details is a paginated request log.
  - **Token Saver:** RTK, Headroom, Caveman, Ponytail, tool-output compaction and dedupe, with per-request savings accounting and an RTK preview.
  - **Console Log:** the server log live over SSE (`/api/admin/logs/stream`), coloured by level, with filter, pause and Clear. The last 1,000 lines are kept in memory, redacted (keys, tokens, emails) before they are stored.
  - **Quota Tracker** (per-account usage read from the provider's own API for Codex, GLM, DeepSeek, OpenCode Go, OpenRouter and Antigravity, with reset countdowns and opt-in auto-ping for Codex and Antigravity), **API Keys** (keys are generated in the browser and shown once), **CLI Tools** (copy-paste setup for Claude Code, Codex, OpenCode, Cline, Roo, Kilo, Continue, Droid, Cursor, cURL and the SDKs).
  - **Settings:** per-provider thinking and account rotation, vision fallback, decision cache/gate, dashboard password.
  - Every change goes through `/api/admin/*`, which needs a session cookie (HttpOnly, SameSite=Strict, 24h), an `X-CSRF-Token` header, a JSON body and a same-origin `Origin`. Inputs are validated, secrets are write-only (only `••••` plus the last four characters come back), and changes are saved to SQLite and applied live without a restart. Five wrong passwords lock an IP out for a minute. Token-saver fields set in the config file show as locked.
- Health: `/health`.
- Config: `~/.fastrouter/fastrouter.json` (created on first run). Use `--config` to pick another file, and `--host`/`--port` to override it. Listening on `0.0.0.0` is opt-in.
- After re-importing, send `SIGHUP` to reload without restarting.

## Model names are `prefix/model`, for example `ds/deepseek-v4.1-flash`, or a combo name like `my-combo-team`.

## Model discovery (new models without a new build)

A model id is not validated on the request path: `ds/anything` routes as long as
the prefix is a real provider, so a model released yesterday already works. The
compiled catalog only decides what `/v1/models` and the dashboard picker
advertise — which used to mean waiting for a new binary.

FastRouter now also asks each provider what it serves:

- Every six hours (`modelDiscovery.every`, `modelDiscovery.enabled` to opt out),
  each provider with an active account is asked, in whatever way it answers:

  | Kind | How | Providers |
  |---|---|---|
  | API key | `GET …/models` with the account's key | OpenAI, DeepSeek, xAI, Groq, OpenRouter, NVIDIA, custom endpoints, … |
  | Sign-in | the vendor's own list, carrying the same headers the chat request uses | Claude Code, Kimi, Grok CLI, Copilot, Cline family |
  | Private protocol | the vendor's own API call | Cursor (`GetUsableModels`), Antigravity / Gemini CLI (`:fetchAvailableModels`), Codex (`backend-api/models`) |

  The header shape is tried one at a time: a gateway that routes on which
  credential header it sees (NVIDIA returns an empty list if an Anthropic
  version header is present) cannot be confused by extra ones.
- Antigravity and Gemini CLI also report each model's capabilities, so
  `supportsImages` feeds the vision fallback the moment a model is discovered.
- Results go to a `discovered_models` table, not to the import: a re-import or a
  dashboard edit never forgets them.
- `fastrouter models [--refresh] [--provider id] [--json]` prints the overlay;
  the provider page has **Refresh** (one provider, now) and
  `POST /api/admin/providers/{id}/refresh-models`.
- A provider that publishes nothing (OpenCode Free, Kiro, Qoder, Zed) keeps the
  catalog as its only source.

Two guards keep the overlay honest:

- **The catalog's `Upstream` map always wins, both ways.** DeepSeek and
  Antigravity list the *served* id (`deepseek-flash`,
  `gemini-3.8-flash-low(low)`); the catalog maps a public id onto it. A served
  id is mapped back to its public name rather than dropped, so those providers
  yield usable models, and no model ever has two names with different pricing.
  An id that is only an adapter suffix (`…(high)`) is not a name anyone can call,
  so it is dropped.
- **Non-chat ids are not stored.** A provider's list mixes in embeddings, speech
  and image models; those are filtered by name (`LooksLikeChat`), so the picker
  stays a chat picker. The count skipped is logged.

A model listed twice in a row and then missing twice is marked `dead` and drops
out of `/v1/models`; a 404 `model_not_found` for a discovered id marks it dead
immediately. A dead id is never revived by the list alone — only a request that
succeeds again does that. Catalog ids are never touched by any of this.

## Adding a model by hand

Discovery only finds what a provider lists. A preview, an alias or a private
deployment needs the provider page's **Add Model**: type the id, test it with
the account, mark Vision and Reasoning, and it is stored as a custom model, so
it shows in the picker and routes like any other id. `POST /api/admin/models`
and `DELETE /api/admin/models?provider=…&id=…` are the endpoints behind it.

## Swarm Agent combos

A swarm is a team of models producing one answer:

1. **Round 1** — every agent (up to 4) answers the user on its own, like fusion's
   panel.
2. **Rounds 2-3** — each agent is shown its own previous answer and every peer's,
   and told to revise: keep what is right, drop what it cannot defend, and adopt
   a peer's point only if it can verify it. Rounds are **synchronized** — every
   agent finishes a round before the next starts, so all of them argue from the
   same state.
3. **Judge** — reads the final answers and writes the reply the client receives,
   streamed if the client asked for a stream.

Fusion samples the panel once and averages it, so three wrong models outvote one
right one. Swarm lets the right one argue for itself first.

- `rounds`: 1 (independent answers only, no debate) to 3. Default 2.
- Cost is `agents × rounds + 1`. Set `judgeModel` to something cheaper than the
  agents if the answers are the expensive part.
- Debate answers are capped at 1,200 tokens (a smaller client `max_tokens` still
  wins) and peer answers carried into round 2 are clipped to 2,500 characters, so
  the debate stays cheap; only the judge uses the full budget.
- Once a quorum (2 agents) answers a round, stragglers get 8 seconds instead of
  holding the round for the full 45s deadline. The whole swarm is capped at 150s.
- If every agent agrees exactly (same normalized text), the judge is skipped and
  the longest copy is returned, saving one call and its latency.
- Tools are stripped for the agents (they cannot call a tool mid-debate).
- An agent that fails a round keeps its previous answer and stays in the debate;
  one that never answers at all drops out. One survivor is returned directly — a
  debate needs two voices — and every agent failing is a 503.
- Prompts tell the agents and the judge never to mention the swarm, so the user
  sees one answer.

`comboStrategies[name].rounds` stores the round count, beside the other combo
options.

## How it works

- **State:** On startup the configuration loads from SQLite into an immutable routing table, which a reload swaps atomically. Each provider has its own account pool with a small lock of its own, so there is no global lock. Account health uses atomics. Usage is written to SQLite by a background batch writer, never on the request path.
- **Passthrough:** When the client and provider formats match, the body is forwarded as-is. Only the top-level `model` field is rewritten, with a single byte scan. SSE is copied chunk by chunk with pooled buffers and flushed immediately.
- **Fallback:**
  - If an upstream fails before sending its first byte (a connection error, 401/402/403/404/408/429/5xx, or the first-byte timeout), the router tries the next account in the pool, then the next combo model.
  - Failed accounts cool down with exponential backoff and honour `Retry-After`.
  - A 400 is returned to the client unchanged.
- **Pools and combos:** Pools are round-robin with a sticky limit, or fill-first by priority, taken from your 9Router provider strategies. Combos are fallback, round-robin, smart or fusion:
  - **Smart Router** (9Router's System One combo): a router model (default: the combo's first model) gets the last user message and the candidate list and answers with JSON `{choice, confidence}`. The pick is tried first, then the rest in order. Picks are sticky per conversation for 2 hours (by `session_id`/`x-session-id` header, or a hash of key and first message). Policies: `smart` switches only when confidence ≥ the threshold (0.8), `strict` never switches, `per-turn` follows every pick. The router call has an 8-second limit and fails open to the listed order.
  - **Fusion:** every model answers in parallel (non-streaming, tools stripped). Once two have answered, stragglers get 8 seconds more, with a 90-second cap. No answers gives 503 and one answer is returned as-is. Otherwise a judge model (default: the first model) gets the panel's answers and replies in the client's format, streaming if asked.
  - Options live in settings `comboStrategies[name]` (`smartPolicy`, `confidenceThreshold`, `routerModel`, `judgeModel`), the same place 9Router keeps them, so imported smart and fusion combos work.
- **Timeouts:** There is a first-byte timeout and an idle timeout between chunks. A client disconnect cancels the upstream request.
- **Secrets:** API keys, tokens and the dashboard password are AES-256-GCM encrypted in SQLite, using `secret.key` (0600) in the data dir. Logs pass through a redactor.
- **Keyless mode:** with no local API key (or no dashboard password), only loopback clients are trusted, and only when the `Host` is `localhost`, `127.0.0.1` or `::1` and any `Origin` is one of those too. That blocks DNS rebinding and browser CSRF. Add a tunnel host name to `allowedHosts` in the config to trust it, or better, create an API key.
- **Limits:** request bodies over 64 MB get a 413. A key's `keyLimits.models` list in the config narrows its 9Router access list (both must allow the model), and the vision fallback only uses models the key may call.

## Formats and translation

Any client format can call any provider. When the client and provider speak the same format the body is passed through untouched (only `model` is swapped). Otherwise the request is decoded once into a shared in-memory form and encoded once for the provider, with no intermediate JSON. Answers and streams are translated back the same way, event by event.

- Covered: text, system prompts, tools and parallel tool calls, tool results, images, thinking/reasoning blocks and their signatures, redacted thinking, `cache_control`, stop/finish reasons and token usage (including cache and reasoning tokens).
- Thinking:
  - Effort levels and token budgets convert both ways (minimal 1024, low 4096, medium 10000, high 24576, max 32000).
  - For Anthropic, thinking is switched off when the history would be rejected (a tool turn with no signed thinking block), and unsigned thinking is dropped.
  - Signatures are only replayed to the provider family that issued them, even across mixed combos. A signature handed to a client in another format is tagged `frsig:<family>:…`. On a later turn it is restored for that family and dropped for any other, and a passthrough body carrying a tag is re-encoded. Gemini gets its documented placeholder signature on tool calls that have none.
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

The 9Router token savers run on every upstream request, in 9Router's order (RTK, Headroom, Caveman, Ponytail), after FastRouter's optional tool-output compaction. Each defaults to your imported 9Router setting and can be overridden under `tokenSavers` in the config file:

```json
"tokenSavers": { "rtk": true, "caveman": "ultra", "ponytail": "off", "headroom": true,
                 "headroomUrl": "http://localhost:8787", "headroomCodeAware": true, "headroomAutostart": false }
```

- **RTK** compresses tool output (git diff/log/status, build and test logs, grep, find, ls, tree, long lists) in OpenAI, Anthropic, Gemini and Responses bodies. Its filters are ported from 9Router and tested against vectors generated by 9Router's own code (`internal/saver/testdata`).
- **Caveman** and **Ponytail** add 9Router's output-reduction prompts (verbatim) at the configured level to the system prompt. Your import sets both to `ultra`. Adding them is idempotent.
- **Headroom** sends the conversation to a Headroom proxy's `/v1/compress` (`headroomUrl`, 3 s timeout) and keeps the original body if the proxy fails, pausing calls for a while after a failure. With `headroomAutostart` on (off by default), FastRouter runs `headroom proxy --port N` for a localhost URL, adding `--code-aware` when `headroomCodeAware` is on and `--disable-kompress` when `headroomKompress` is false, the same as 9Router. Headroom must already be installed; FastRouter never installs it.
- **Tool-output compaction** (`compactEnabled`, `compactKeepToolResults`): keep the last N tool results and replace older ones with a short marker. **Dedupe** (`dedupeEnabled`) replaces a repeated identical tool result with a reference. Both are lossy and off by default; error results are never touched.
- **Opt-out:** send `X-9Router-Token-Saver: off` (9Router's header) or `X-FastRouter-Token-Saver: off` to skip every saver for one request.
- The response header `X-FastRouter-Token-Saver` lists what ran, for example `COMPACT:1784,RTK:2,CAVEMAN:ultra,PONYTAIL:ultra`.
- **Savings accounting:** every request records estimated input tokens saved and prompt tokens added (4 bytes ≈ 1 token), priced with the same rate table as usage. The Token Saver page shows the 30-day totals, net after prompt cost, and a break-even hint when a prompt saver adds more tokens than the average answer. Usage → Details has a Saved column; the RTK card has a paste-and-preview box that runs the real filters without sending a request.

## Official logins (Codex, GLM)

`internal/login` handles the two providers that use their vendor's own sign-in. It uses only their documented OAuth flows and APIs: no browser-session scraping, fingerprint spoofing, traffic interception or anti-ban tricks.

- **Codex (ChatGPT sign-in)**, prefix `cx/`: requests go to `chatgpt.com/backend-api/codex/responses` in Responses format and are shaped like 9Router's codex executor. That means always streamed (folded back to JSON for non-stream clients), `store:false`, default Codex instructions, server item ids stripped, flat function tools, Codex reasoning defaults, the responses-lite form for the GPT-6.x models, and only the fields Codex accepts. The `ChatGPT-Account-ID`, `session_id` and `originator` headers are set as 9Router does.
- **GLM (Z.ai coding plan)**, prefix `glm/`: requests go to `api.z.ai`'s OpenAI endpoint (`/api/coding/paas/v4`) or Anthropic endpoint, whichever matches the client, with the coding-plan key.
- **Token refresh:** a background job checks every minute and refreshes a Codex access token 10 minutes before it expires, or when its last refresh is over 8 days old (9Router's rules). A refresh token that OpenAI rejects marks the account `needs-reauth`. GLM's coding-plan key has no refresh grant (the same as the official CLI), so a GLM account only needs a new sign-in if Z.ai revokes the key.
- **Imported Codex accounts are not refreshed by default.** OpenAI rotates refresh tokens, and the running 9Router holds the same one, so a FastRouter refresh would sign 9Router out. The imported access token keeps working until it expires. Then use **Sign in again** on the dashboard, which creates FastRouter's own session that does refresh. Once 9Router is retired you can set `"plugins": {"refreshImported": true}` instead.
- **Dashboard sign-in:** the Sign-ins section lists Codex/GLM accounts with **Sign in again** and **Add account** buttons.
  - Codex opens the ChatGPT sign-in and catches the redirect on `http://localhost:1455/auth/callback`, the port registered for the Codex client, only while a sign-in is pending. If that port is busy (for example, the Codex CLI is signing in), paste the address the browser ends on into the dashboard.
  - GLM opens Z.ai's sign-in page, and FastRouter polls until it finishes, then creates or reuses the `zcode-api-key` coding-plan key.
  - Tokens are stored encrypted and never shown. Sign-in POSTs need a dashboard session and its CSRF token, like every other dashboard write.
- Re-importing a 9Router backup replaces the accounts, including tokens refreshed or signed in through FastRouter.

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

Custom models whose provider no longer exists are flagged, not imported. Codex and GLM accounts are usable through the official-login plugin (below). Antigravity, Cursor and OpenCode accounts are stored but marked `needs-plugin`; FastRouter builds no workaround for them. The summary prints names and counts only.

## Tests and benchmarks

```sh
go test ./...      # importer, translators + goldens, passthrough, translated paths, fallback, smart/fusion combos, usage, limits, admin API, log stream
go test ./internal/translate -update   # regenerate golden files after an intended change
go test -run '^$' -bench . -benchtime 3s ./internal/server/
```

`testdata/9router-backup.redacted.json` was produced by `scripts/redact_fixture.py`. Every key, token, password, cookie, email and account id in it is replaced.

## Credits

Behaviour follows [9Router](https://github.com/decolua/9router) (MIT, Copyright (c) 2024-2026 decolua and contributors): provider endpoints and aliases, translator behaviour (used as a reference; the Go translators are written fresh), the bypass rules, vision name patterns and pricing lookup order. The default price table in `internal/pricing/pricing_9router.json` is 9Router's data; its MIT licence is kept in `internal/pricing/LICENSE-9router`.

## License

MIT — see [LICENSE](LICENSE). Releases and the npm package are published by the maintainer; the source is open, so you can also build and run your own binary.

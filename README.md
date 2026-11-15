# FastRouter

A fast local AI gateway, written in Go, that runs your existing 9Router setup.
One binary, SQLite storage, everything hot kept in memory.

## Run

```sh
go build -o fastrouter ./cmd/fastrouter
./fastrouter import ~/9router-backup.json   # one-time: bring over your 9Router setup
./fastrouter                                # serve on http://127.0.0.1:20128
```

- API: `http://127.0.0.1:20128/v1` (`/v1/chat/completions`, `/v1/messages`, `/v1/models`). Use your existing 9Router local API key.
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

## Importer

`fastrouter import <backup.json>` brings in:

- settings, provider strategies, thinking and capacity-adapter settings
- accounts, keeping `isActive` and `priority`
- custom endpoints
- combos and their strategies
- custom models
- local API keys
- the dashboard password, with its bcrypt hash unchanged

Custom models whose provider no longer exists are flagged, not imported. Login-based accounts (antigravity, codex, cursor, glm, opencode) are stored but marked `needs-plugin`. The summary prints names and counts only.

## Tests and benchmarks

```sh
go test ./...                                          # importer, passthrough, fallback, auth, cancel
go test -run '^$' -bench . -benchtime 3s ./internal/server/
```

`testdata/9router-backup.redacted.json` was produced by `scripts/redact_fixture.py`. Every key, token, password, cookie, email and account id in it is replaced.

## Credits

Provider endpoints and aliases follow [9Router](https://github.com/decolua/9router) (MIT, © decolua). No 9Router code is included.

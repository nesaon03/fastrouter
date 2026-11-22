# Packaging and release

Everything here is local. Nothing is pushed or published by a normal build;
releasing is one tag push (see below).

## Version info

`internal/server.Version` is the source of truth:

```go
var Version = "0.7.0"
```

`package.json` must carry the same value — `TestPackageVersionMatchesServer`
fails otherwise, and the release workflow refuses a tag that disagrees with
either. A plain `go build` from a git checkout falls back to Go's embedded VCS
info for the commit and date. The same version is reported by `/health`, the
dashboard sidebar, the npm package and the upstream `User-Agent` header.

## Build

`scripts/build.sh` (or `make dist`) cross-compiles static binaries with `CGO_ENABLED=0` (the SQLite driver is pure Go) into `dist/`, along with `dist/SHA256SUMS`:

```sh
./scripts/build.sh                          # windows/amd64, linux/amd64, darwin/arm64
TARGETS="linux/arm64" ./scripts/build.sh    # other targets
VERSION=1.2.3 ./scripts/build.sh            # explicit version
```

- Without `VERSION`, an exact git tag is used if there is one. Otherwise the version is `0.0.0-dev+<commit>`, with `.dirty` added when the tree has changes.
- Files are named `dist/fastrouter_<version>_<os>_<arch>[.exe]`.
- `dist/` is gitignored.

### GoReleaser (optional)

`.goreleaser.yaml` builds the same three targets with the same ldflags. It produces `.tar.gz` archives (`.zip` for Windows) with README, this file and `install.ps1`, plus `checksums.txt`. Release publishing and the changelog are disabled.

```sh
goreleaser check
goreleaser release --snapshot --clean     # into dist/; uploads nothing
# without installing goreleaser (downloads into your Go module cache only):
go run github.com/goreleaser/goreleaser/v2@v2.20.2 release --snapshot --clean
```

In a repo with no git remote, GoReleaser has no git info. The config then leaves the commit and date flags unset, and the binary reports Go's embedded VCS revision and commit time instead.

For a real version, tag first (`git tag v1.2.3`) and run `goreleaser release --clean`. Uploading stays disabled, so the output stays in `dist/`.

## Test

```sh
go build ./... && go vet ./... && go test ./...      # or: make test
node --test npm/test.js                              # npm wrapper (offline)
./dist/fastrouter_*_linux_amd64 version
```

## npm package

`package.json` publishes the `@zenpo/fastrouter` package (MIT). It contains no binary:
`npm/fastrouter.js` is a launcher, and `npm/install.js` runs as `postinstall` to
download the matching raw release asset from
`https://github.com/nesaon03/fastrouter/releases/download/v<version>/` and verify it
against `checksums.txt`.

- Assets: `fastrouter_<version>_<os>_<arch>[.exe]` for windows/amd64,
  linux/amd64, linux/arm64, darwin/amd64, darwin/arm64.
- `FASTROUTER_DOWNLOAD_BASE` overrides the download base (mirror or local test).
- `FASTROUTER_SKIP_DOWNLOAD=1` skips the postinstall download; the launcher
  retries on first run.
- `FASTROUTER_STRICT_INSTALL=1` turns a failed download into a failed install.
- The downloaded binary lands in `npm/vendor/` (gitignored) and is chmod 0755.

Check the package without publishing:

```sh
npm pack --dry-run     # list the files that would ship
npm pack               # zenpo-fastrouter-<version>.tgz
node --test npm/test.js
```

Update flow for users is plain npm, and the dashboard sidebar shows the command
with a copy button:

```sh
npm i -g @zenpo/fastrouter@latest
```

## Release

1. Bump `internal/server.Version` and `package.json` to the same `X.Y.Z`.
2. Commit, then tag and push:

   ```sh
   git tag v0.7.0
   git push origin v0.7.0
   ```

3. `.github/workflows/release.yml` runs the tests, verifies the tag against both
   version fields, builds the five raw binaries, writes `checksums.txt`, creates
   the GitHub release, and publishes the npm package (`NPM_TOKEN` repository
   secret required).

The repository name in `npm/install.js` and the workflow (`nesaon03/fastrouter`) is
the release channel: if the repo moves, update both constants.

## Docker

The image is multi-stage. The final stage is `gcr.io/distroless/static-debian13:nonroot`, so there is no shell and it runs as uid 65532. No config or secrets are baked in. State lives in the `/data` volume.

```sh
docker build -t fastrouter --build-arg VERSION=1.2.3 --build-arg COMMIT=$(git rev-parse --short=12 HEAD) .
docker run -d --name fastrouter -p 127.0.0.1:20128:20128 -v fastrouter-data:/data \
  fastrouter serve --config /data/fastrouter.json --host 0.0.0.0
docker run --rm -v fastrouter-data:/data -v ~/9router-backup.json:/backup.json:ro \
  fastrouter import /backup.json --config /data/fastrouter.json
```

**Binding:** FastRouter defaults to `127.0.0.1` inside the container too. That is the container's own loopback, so a published port can't reach it until you opt in with `--host 0.0.0.0` (or `"host": "0.0.0.0"` in `/data/fastrouter.json`). Keep the published side on the host's loopback (`-p 127.0.0.1:20128:20128`) unless you really mean to expose it. If you override the command, pass `--config /data/fastrouter.json` again, because otherwise state goes to `/home/nonroot/.fastrouter`, outside the volume.

## Install on Windows (per user, no admin)

Put `install.ps1` next to `fastrouter_*_windows_amd64.exe` (or run it from `scripts\` with the binary in `dist\`):

```powershell
powershell -ExecutionPolicy Bypass -File .\install.ps1                         # copy to %LOCALAPPDATA%\FastRouter
powershell -ExecutionPolicy Bypass -File .\install.ps1 -AddToPath -Startup -Run
powershell -ExecutionPolicy Bypass -File .\install.ps1 -Uninstall              # removes binary, PATH entry, startup
```

| Option | What it does |
|---|---|
| `-Source <exe>` | Binary to install. Default: the newest `fastrouter*.exe` next to the script or in `..\dist`. |
| `-InstallDir <dir>` | Install folder. Default: `%LOCALAPPDATA%\FastRouter`. |
| `-AddToPath` | Adds the folder to the user PATH (HKCU only). |
| `-Startup` | Runs `fastrouter service install`. |
| `-Run` | Starts FastRouter now, minimized. |
| `-Force` | Stops a running FastRouter from the install folder before updating or uninstalling. Without it the script aborts. |

Your config and data in `%USERPROFILE%\.fastrouter` are never touched, including on uninstall.

## Run at startup (per user)

```sh
fastrouter service install [--config path] [--host h] [--port n] [--dry-run]
fastrouter service status
fastrouter service uninstall
```

`--dry-run` prints the files and commands without changing anything. The registered command is the absolute path of the running binary plus `serve --config <absolute config path>`. Run `service install` from the installed binary, not from `go run`.

| OS | Mechanism | Notes |
|---|---|---|
| Linux | `~/.config/systemd/user/fastrouter.service`, then `systemctl --user enable --now` | `systemctl --user reload fastrouter` sends SIGHUP. To keep it running when logged out: `loginctl enable-linger $USER`. Logs: `journalctl --user -u fastrouter`. |
| macOS | `~/Library/LaunchAgents/com.zenpo.fastrouter.plist`, then `launchctl bootstrap gui/$UID` | RunAtLoad, restarted if it crashes. Logs: `~/Library/Logs/fastrouter.log`. |
| Windows | `FastRouter.lnk` in the user Startup folder (minimized window) | Starts at next logon. A Startup shortcut was chosen over Task Scheduler because logon-trigger tasks usually need admin. |

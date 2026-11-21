#!/bin/sh
# Cross-compile static FastRouter binaries into dist/ (CGO off; SQLite is pure Go).
#
#   scripts/build.sh                     # default targets
#   TARGETS="linux/arm64" scripts/build.sh
#   VERSION=1.0.0 scripts/build.sh       # must match internal/server.Version
set -eu
cd "$(dirname "$0")/.."

# The version lives in the source; a -X on this non-main package symbol is
# silently dropped by the toolchain, so never pass one here.
SRC_VERSION=$(./scripts/check-version.sh)
if [ -n "${VERSION:-}" ] && [ "${VERSION#v}" != "$SRC_VERSION" ]; then
	echo "VERSION=$VERSION does not match internal/server.Version=$SRC_VERSION" >&2
	exit 1
fi
VERSION=$SRC_VERSION

TARGETS=${TARGETS:-"windows/amd64 linux/amd64 darwin/arm64"}
COMMIT=${COMMIT:-$(git rev-parse --short=12 HEAD 2>/dev/null || echo unknown)}
# Commit time keeps builds reproducible; override with DATE=...
DATE=${DATE:-$(git log -1 --format=%cI 2>/dev/null || date -u +%Y-%m-%dT%H:%M:%SZ)}

LDFLAGS="-s -w -X main.commit=$COMMIT -X main.date=$DATE"

# Every Material Symbol the dashboard uses must be in the subset font (else it shows as text).
go test -count=1 -run TestIconFontHasEveryUsedIcon ./internal/dashboard >/dev/null || { echo "icon font is missing glyphs: run python3 scripts/icons.py" >&2; exit 1; }

mkdir -p dist
for t in $TARGETS; do
	os=${t%/*}
	arch=${t#*/}
	ext=""
	[ "$os" = windows ] && ext=".exe"
	out="dist/fastrouter_${VERSION}_${os}_${arch}${ext}"
	echo "build $out"
	CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -ldflags "$LDFLAGS" -o "$out" ./cmd/fastrouter
done

(cd dist && rm -f SHA256SUMS && sha256sum fastrouter_"${VERSION}"_* > SHA256SUMS && cat SHA256SUMS)

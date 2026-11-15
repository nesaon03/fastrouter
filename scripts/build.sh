#!/bin/sh
# Cross-compile static FastRouter binaries into dist/ (CGO off; SQLite is pure Go).
#
#   scripts/build.sh                     # default targets
#   TARGETS="linux/arm64" scripts/build.sh
#   VERSION=1.0.0 scripts/build.sh
set -eu
cd "$(dirname "$0")/.."

PKG=github.com/zenpo/fastrouter
TARGETS=${TARGETS:-"windows/amd64 linux/amd64 darwin/arm64"}
COMMIT=${COMMIT:-$(git rev-parse --short=12 HEAD 2>/dev/null || echo unknown)}
if [ -z "${VERSION:-}" ]; then
	VERSION=$(git describe --tags --exact-match 2>/dev/null || true)
	[ -n "$VERSION" ] || VERSION="0.0.0-dev+$COMMIT"
	if [ -n "$(git status --porcelain 2>/dev/null)" ]; then VERSION="$VERSION.dirty"; fi
fi
# Commit time keeps builds reproducible; override with DATE=...
DATE=${DATE:-$(git log -1 --format=%cI 2>/dev/null || date -u +%Y-%m-%dT%H:%M:%SZ)}

LDFLAGS="-s -w -X $PKG/internal/server.Version=$VERSION -X main.commit=$COMMIT -X main.date=$DATE"

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

#!/bin/sh
# Verifies that the version is consistent everywhere it is published:
# internal/server/server.go (source of truth), package.json (npm), and an
# optional expected version (a release tag).
#
#   scripts/check-version.sh          # prints the version, fails on mismatch
#   scripts/check-version.sh v1.2.3   # also requires the tag to match
set -eu
cd "$(dirname "$0")/.."

SRC=$(sed -n 's/^var Version = "\([^"]*\)"/\1/p' internal/server/server.go)
PKG=$(sed -n 's/^  "version": "\([^"]*\)",$/\1/p' package.json | head -n 1)

[ -n "$SRC" ] || { echo "no version found in internal/server/server.go" >&2; exit 1; }
[ -n "$PKG" ] || { echo "no version found in package.json" >&2; exit 1; }
[ "$SRC" = "$PKG" ] || { echo "package.json $PKG != internal/server.Version $SRC" >&2; exit 1; }

if [ "${1:-}" != "" ] && [ "${1#v}" != "$SRC" ]; then
	echo "expected ${1#v} != source $SRC" >&2
	exit 1
fi
echo "$SRC"

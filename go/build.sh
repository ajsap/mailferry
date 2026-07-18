#!/bin/sh
# MailFerry v2.0.0 — reproducible release build (all six targets).
#
# Usage: ./build.sh [output-dir]        (default: ./dist)
#
# Produces mailferry-v<version>-<os>-<arch>[.exe] + SHA256SUMS.
# CGO is disabled (pure Go, static binaries); -trimpath and a cleared
# buildid keep builds reproducible across machines with the same Go
# toolchain. macOS signing/notarisation happens AFTER this step - see
# docs/RELEASING-MACOS.md.
set -e
VERSION=$(grep -o 'Version *= *"[^"]*"' internal/identity/identity.go | head -1 | cut -d'"' -f2)
OUT=${1:-dist}
mkdir -p "$OUT"
# the changelog command embeds the repo CHANGELOG.md - keep it in sync
cp ../CHANGELOG.md cmd/mailferry/CHANGELOG.md
export CGO_ENABLED=0
export SOURCE_DATE_EPOCH=${SOURCE_DATE_EPOCH:-$(date +%s)}
for t in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64 windows/arm64; do
  GOOS=${t%/*}; GOARCH=${t#*/}; EXT=""
  [ "$GOOS" = "windows" ] && EXT=".exe"
  NAME="mailferry-v$VERSION-$GOOS-$GOARCH$EXT"
  echo "building $NAME"
  GOOS=$GOOS GOARCH=$GOARCH go build -trimpath \
    -ldflags="-s -w -buildid=" -o "$OUT/$NAME" ./cmd/mailferry
done
( cd "$OUT" && sha256sum mailferry-v"$VERSION"-* > SHA256SUMS )
echo "done: $OUT"

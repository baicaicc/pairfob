#!/usr/bin/env bash
# Build this fork's daemon for the kidsbox machines (docs/kidsbox.md).
#
# The version is deliberately not a release number and no release key is
# embedded: neither the daemon nor the PWA orders it against the hosted
# channel, so no automatic or manual update can replace it with an official
# build that would reconnect to pairfob.com. Deployments also set
# PAIRFOB_AUTO_UPDATE=0.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

sha="$(git rev-parse --short=7 HEAD)"
if [[ -n "$(git status --porcelain)" ]]; then sha="${sha}-dirty"; fi
VERSION="kidsbox-$(date -u +%Y%m%d)-${sha}"
OUT="${OUT:-$ROOT/dist/kidsbox}"
mkdir -p "$OUT"

build_env=(CGO_ENABLED=0)
if [[ "$(uname -s)" == Darwin ]]; then
  # The native Keychain reader needs cgo on macOS; match release.sh's floor.
  build_env=(CGO_ENABLED=1 MACOSX_DEPLOYMENT_TARGET=12.0
    "CGO_CFLAGS=-O2 -g -mmacosx-version-min=12.0"
    "CGO_LDFLAGS=-O2 -g -mmacosx-version-min=12.0")
fi
env "${build_env[@]}" go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o "$OUT/pairfob" ./cmd/pairfob
echo "$OUT/pairfob $VERSION"

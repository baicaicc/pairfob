#!/usr/bin/env bash
# Build the PWA, pack it as the only static surface (/ and /pair), apply D1
# migrations remotely and deploy the origin to https://pair.kidsbox.cc.
# Secrets (IP_HASH_PEPPER, OPERATOR_TOKEN) are set once with `wrangler secret put`.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
ORIGIN_DIR="$ROOT/workers/pairfob-origin"
CONFIG="wrangler.kidsbox.jsonc"
export WRANGLER_SEND_METRICS=false

BUILD="${BUILD:-$(git -C "$ROOT" rev-parse --short HEAD 2>/dev/null || echo dev)}"
if [[ -n "$(git -C "$ROOT" status --porcelain 2>/dev/null)" ]]; then BUILD="${BUILD}-dirty"; fi

echo "building PWA…"
(cd "$ROOT/pwa" && bun run build >/dev/null)

echo "packing PWA into public-dist (/ and /pair)…"
DEST="$ORIGIN_DIR/public-dist"
rm -rf "$DEST"
mkdir -p "$DEST"
cp -R "$ROOT/pwa/dist/." "$DEST/"
cp "$ROOT/pwa/dist/index.html" "$DEST/pair-shell.asset"

cd "$ORIGIN_DIR"
echo "applying D1 migrations (remote)…"
bunx wrangler d1 migrations apply pairfob --remote --config "$CONFIG"

echo "deploying build $BUILD…"
bunx wrangler deploy --config "$CONFIG" --var "BUILD:$BUILD"

echo "verifying…"
sleep 3
curl -sS --max-time 20 https://pair.kidsbox.cc/api/config; echo
curl -sS --max-time 20 https://pair.kidsbox.cc/v2/grants; echo

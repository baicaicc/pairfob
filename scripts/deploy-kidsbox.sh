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
config="$(curl -sS --max-time 20 -w '\n%{http_code}' https://pair.kidsbox.cc/api/config)"
echo "api/config http=${config##*$'\n'} ${config%$'\n'*}"
[[ "${config##*$'\n'}" == "200" ]] || { echo "api/config expected 200" >&2; exit 1; }
# /v2/grants was retired with grantless enroll (src/worker.ts): it must answer
# 404 "unbound", never mint a join grant.
grants="$(curl -sS --max-time 20 -w '\n%{http_code}' https://pair.kidsbox.cc/v2/grants)"
echo "v2/grants http=${grants##*$'\n'} ${grants%$'\n'*}"
[[ "${grants##*$'\n'}" == "404" ]] || { echo "v2/grants expected retired 404" >&2; exit 1; }

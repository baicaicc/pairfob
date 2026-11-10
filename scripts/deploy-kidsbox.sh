#!/usr/bin/env bash
# Deploy this fork's origin to https://pair.kidsbox.cc (docs/kidsbox.md):
# build the PWA, pack it as the only static surface (/ and /pair, no marketing
# site, docs or /dl), apply D1 migrations remotely, deploy and check the live
# entry points. Secrets (IP_HASH_PEPPER, OPERATOR_TOKEN) are set once with
# `wrangler secret put`.
#
# A D1 migration changes the shared database, so pending migrations stop the
# deploy before anything is built unless PAIRFOB_APPLY_MIGRATIONS=1. Export a
# backup first: wrangler d1 export pairfob --remote --config wrangler.kidsbox.jsonc --output <file>.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
ORIGIN_DIR="$ROOT/workers/pairfob-origin"
CONFIG="wrangler.kidsbox.jsonc"
ORIGIN_URL="${PAIRFOB_KIDSBOX_ORIGIN:-https://pair.kidsbox.cc}"
export WRANGLER_SEND_METRICS=false

BUILD="${BUILD:-kidsbox-$(git -C "$ROOT" rev-parse --short HEAD 2>/dev/null || echo dev)}"
if [[ -n "$(git -C "$ROOT" status --porcelain 2>/dev/null)" ]]; then BUILD="${BUILD}-dirty"; fi

echo "checking D1 migrations (remote)…"
pending="$(cd "$ORIGIN_DIR" && bunx wrangler d1 migrations list pairfob --remote --config "$CONFIG")"
pending_names="$(grep -oE '[0-9]{4}_[A-Za-z0-9_]+\.sql' <<<"$pending" | sort -u || true)"
if [[ -n "$pending_names" ]]; then
  echo "pending D1 migrations:"
  sed 's/^/  /' <<<"$pending_names"
  if [[ "${PAIRFOB_APPLY_MIGRATIONS:-}" != "1" ]]; then
    echo "deploy stopped: export a D1 backup, get the migration approved, then rerun with PAIRFOB_APPLY_MIGRATIONS=1" >&2
    exit 3
  fi
fi

echo "building PWA…"
(cd "$ROOT/pwa" && bun run build >/dev/null)

echo "packing PWA into public-dist (/ and /pair)…"
DEST="$ORIGIN_DIR/public-dist"
staging="$(mktemp -d)"
trap 'rm -rf "$staging"' EXIT
# Keep the hashed chunks of the last two packed builds, so a tab opened before
# this deploy can still load its lazy chunks (same rule as pack-origin-assets).
bun "$ROOT/scripts/retain-pwa-assets.ts" "${PAIRFOB_PREVIOUS_ASSETS:-$DEST}" "$ROOT/pwa/dist" "$staging"
rm -rf "$DEST"
mkdir -p "$DEST"
cp -R "$ROOT/pwa/dist/." "$DEST/"
cp -R "$staging/assets/." "$DEST/assets/"
rm -rf "$DEST/.vite"
# Cloudflare Assets canonicalizes *.html; the Worker fetches these opaque copies.
cp "$ROOT/pwa/dist/index.html" "$DEST/pair-shell.asset"
cp "$ROOT/pwa/dist/preview/runner.html" "$DEST/preview-runner.asset"

if [[ -n "$pending_names" ]]; then
  echo "applying D1 migrations (remote)…"
  (cd "$ORIGIN_DIR" && bunx wrangler d1 migrations apply pairfob --remote --config "$CONFIG")
fi

echo "deploying build $BUILD…"
(cd "$ORIGIN_DIR" && bunx wrangler deploy --config "$CONFIG" --var "BUILD:$BUILD")

echo "verifying…"
sleep 3
check() {
  local path="$1" want="$2" body code
  body="$(curl -sS --max-time 20 -w '\n%{http_code}' "$ORIGIN_URL$path")"
  code="${body##*$'\n'}"
  echo "$path http=$code" >&2
  [[ "$code" == "$want" ]] || { echo "$path expected $want" >&2; exit 1; }
  printf '%s' "${body%$'\n'*}"
}
config="$(check /api/config 200)"
echo "$config"
grep -q "\"build\":\"$BUILD\"" <<<"$config" || { echo "api/config does not report build $BUILD" >&2; exit 1; }
# /v2/grants was retired with grantless enroll: it must answer 404, never mint a grant.
check /v2/grants 404 >/dev/null
entry="$(grep -oE 'assets/index-[A-Za-z0-9_-]+\.js' "$ROOT/pwa/dist/index.html" | head -1)"
for path in / /pair; do
  check "$path" 200 | grep -q "$entry" || { echo "$path does not serve the new PWA ($entry)" >&2; exit 1; }
done
check /preview/runner.html 200 >/dev/null
echo "deployed $BUILD to $ORIGIN_URL"

#!/usr/bin/env bash
# Publish a baked PMTiles release: generate + sign the manifest and upload in the
# durability-safe order (artifact, then signature, then manifest.json last).
#
# The Ed25519 signing PRIVATE key stays offline and never enters the cluster.
# Generate one once with:
#   openssl genpkey -algorithm ed25519 -out tiles-manifest.key
#   scripts/publish-release.sh --pubkey tiles-manifest.key   # prints raw hex pubkey
#
# Usage:
#   scripts/publish-release.sh <version> <planet.pmtiles> <public-base-url>
set -euo pipefail

if [[ "${1:-}" == "--pubkey" ]]; then
  # Print the raw 32-byte Ed25519 public key as hex for the Helm value.
  openssl pkey -in "$2" -pubout -outform DER | tail -c 32 | xxd -p -c 64
  exit 0
fi

VERSION="$1"
PMTILES="$2"
BASE_URL="${3:?public base URL required, e.g. https://artifacts.example.com}"
KEY="${SIGNING_KEY:-tiles-manifest.key}"
MINZOOM="${MINZOOM:-0}"
MAXZOOM="${MAXZOOM:-14}"

SAFE="$(printf '%s' "$VERSION" | tr -c 'A-Za-z0-9._-' '-')"
REMOTE_NAME="planet-$SAFE.pmtiles"

SIZE="$(stat -c%s "$PMTILES" 2>/dev/null || stat -f%z "$PMTILES")"
SHA="$(sha256sum "$PMTILES" | cut -d' ' -f1)"
CREATED="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

# Build the manifest with EXACT byte layout that will be signed.
MANIFEST="$(cat <<JSON
{
  "schema_version": 1,
  "version": "$VERSION",
  "url": "$BASE_URL/$REMOTE_NAME",
  "sha256": "$SHA",
  "size": $SIZE,
  "min_zoom": $MINZOOM,
  "max_zoom": $MAXZOOM,
  "tile_schema": "openmaptiles",
  "created_at": "$CREATED"
}
JSON
)"

printf '%s' "$MANIFEST" > manifest.json
openssl pkeyutl -sign -inkey "$KEY" -rawin -in manifest.json -out manifest.sig

echo "== Local verification =="
scripts/verify.sh "$PMTILES" manifest.json "$(dirname "$KEY")/$(basename "$KEY" .key).pub" 2>/dev/null || true

echo "== Upload order: artifact -> signature -> manifest.json (last) =="
: "${UPLOAD_CMD:?Set UPLOAD_CMD to a function/command that takes <local> <remote-name>}"
$UPLOAD_CMD "$PMTILES" "$REMOTE_NAME"
$UPLOAD_CMD manifest.sig "manifest.sig"
$UPLOAD_CMD manifest.json "manifest.json"

echo "Published $VERSION. Bootstrap/updater will discover it via manifest.json."

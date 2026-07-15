#!/usr/bin/env bash
# Verify a local PMTiles + manifest + signature triple before publishing.
# Usage: scripts/verify.sh <planet.pmtiles> <manifest.json> <pubkey.pem-or-hex> [manifest.sig]
set -euo pipefail

PMTILES="$1"
MANIFEST="$2"
PUBKEY="$3"
SIG="${4:-manifest.sig}"

echo "== Signature =="
if [[ -f "$PUBKEY" && "$PUBKEY" == *.pem ]]; then
  openssl pkeyutl -verify -pubin -inkey "$PUBKEY" -rawin -in "$MANIFEST" -sigfile "$SIG"
else
  echo "(skipping openssl verify; pass a .pem public key to check the signature)"
fi

echo "== Digest / size =="
WANT_SHA="$(grep -o '"sha256": *"[0-9a-fA-F]*"' "$MANIFEST" | grep -o '[0-9a-fA-F]\{64\}')"
WANT_SIZE="$(grep -o '"size": *[0-9]*' "$MANIFEST" | grep -o '[0-9]*')"
GOT_SHA="$(sha256sum "$PMTILES" | cut -d' ' -f1)"
GOT_SIZE="$(stat -c%s "$PMTILES" 2>/dev/null || stat -f%z "$PMTILES")"
[[ "$WANT_SHA" == "$GOT_SHA" ]] || { echo "SHA mismatch: $GOT_SHA != $WANT_SHA"; exit 1; }
[[ "$WANT_SIZE" == "$GOT_SIZE" ]] || { echo "size mismatch: $GOT_SIZE != $WANT_SIZE"; exit 1; }
echo "sha256 OK ($GOT_SHA)"
echo "size OK ($GOT_SIZE)"

echo "== PMTiles header magic =="
head -c 7 "$PMTILES" | grep -q "PMTiles" || { echo "not a PMTiles file"; exit 1; }
echo "PMTiles magic OK"
echo "All checks passed."

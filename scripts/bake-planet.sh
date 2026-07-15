#!/usr/bin/env bash
# Bake the world-wide OpenMapTiles planet to a versioned PMTiles file with
# Planetiler. Run OUTSIDE Kubernetes on a workstation/CI box: ~80 GB download,
# ~60-100 GB output, ~64 GB RAM recommended, potentially a day of processing.
#
# Do NOT run this as a Helm hook, init container, sidecar, or normal K8s Job.
set -euo pipefail

VERSION="${1:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}"
OUT_DIR="${OUT_DIR:-./data}"
PLANETILER_JAR="${PLANETILER_JAR:-planetiler.jar}"
MAXZOOM="${MAXZOOM:-14}"

mkdir -p "$OUT_DIR"
SAFE="$(printf '%s' "$VERSION" | tr -c 'A-Za-z0-9._-' '-')"
OUT="$OUT_DIR/planet-$SAFE.pmtiles"

echo "Baking planet version=$VERSION -> $OUT"
java -Xmx56g -jar "$PLANETILER_JAR" \
  --download \
  --area=planet \
  --maxzoom="$MAXZOOM" \
  --nodemap-type=array \
  --storage=mmap \
  --output="$OUT"

echo "Done: $OUT"
echo "Next: scripts/publish-release.sh \"$VERSION\" \"$OUT\""

# streetCryptid map server

A privacy-quantized map tile API for the streetCryptid app. One custom Go server
is the **only** public map endpoint; [Martin](https://github.com/maplibre/martin)
runs as a private localhost-only sidecar serving a world-wide OpenMapTiles
`planet.pmtiles` dataset (z0–14).

The server enforces a hard privacy boundary that **cannot be weakened by
configuration**: raw XYZ is public only for z0–10, and fine detail (z11–14) is
reachable exclusively through a fixed-z10 privacy bundle. The API learns a
request's ~25 km z10 ancestor but never which child tile the user is looking at.

> This provides coarse location quantization, not anonymity or private
> information retrieval.

## How it works

```
Ingress / Service (only :8080)
        │
        ▼
   map-api :8080 ──┐
        │          └── bundle cache (emptyDir/PVC)
        ▼
  localhost:3000  Martin sidecar ── read-only active planet release
        ▲
 init container (bootstrap) ── activates a verified release on the tile PVC
 updater sidecar ─────────── stages verified releases, patches this Deployment
```

### Public API

| Method | Path | Zoom | Behavior |
| ------ | ---- | ---- | -------- |
| `GET`/`HEAD` | `/planet/{z}/{x}/{y}` | 0–10 | Proxies Martin MVT bytes. |
| `GET`/`HEAD` | `/planet/{z}/{x}/{y}` | 11–14 | **404 without contacting Martin.** |
| `GET` | `/planet/bundle/v1/{x10}/{y10}/{tileZoom}` | 11–14 | Returns the complete descendant set as one SCB1 bundle. |
| `GET` | `/livez` `/readyz` `/metrics` | — | Health + Prometheus metrics (metrics is cluster-internal). |

A z`N` bundle contains every descendant under the fixed z10 anchor:
z11→4, z12→16, z13→64, z14→256 entries, deterministic row-major order. The wire
format (`SCB1`) is the server side of the app parser in
`streetCryptid/src/features/map/tiles/tile-bundle.ts` — the golden fixture in
`testdata/scb1-z11.golden` locks the byte layout.

### The privacy boundary is compiled in

`internal/privacy` holds three `const` values (`privacyAnchorZoom=10`,
`maxPublicRawZoom=10`, `maxBundleZoom=14`). They are **not** env vars, flags,
Helm values, or ConfigMap fields. `helm/.../values.schema.json` rejects the keys
`maxPublicRawZoom`, `privacyAnchorZoom`, `allowRawFineTiles`, `disablePrivacy`,
`bundleAnchorZoom`, and `maxBundleZoom`. Tests prove the boundary can't move.

## Self-bootstrapping tile data

The planet is **never** baked into an image or the chart. An external
workstation/CI job bakes `planet.pmtiles` and publishes a signed manifest
(`scripts/bake-planet.*`, `scripts/publish-release.*`). In-cluster:

- **init container** (`tiles bootstrap`) activates a verified release before
  Martin starts; an empty PVC bootstraps from the signed artifact. A valid local
  release starts even when the artifact host is down.
- **updater sidecar** (`tiles watch`, on by default) polls the small signed
  manifest, stages a newer verified release, and patches **only this**
  Deployment's pod-template annotation. Kubernetes recreates the pod
  (`Recreate` strategy) and the init container activates the pending release.

Every release is verified end to end: Ed25519 signature over the exact
`manifest.json` bytes, size, SHA-256, PMTiles zoom range, and OpenMapTiles
metadata. A failed download/signature/hash leaves the active release untouched;
at least two releases are retained for rollback. The signing private key never
enters the cluster.

## The binary

One static, non-root, distroless image; four subcommands:

```
streetcryptid-map-server serve            # the public API
streetcryptid-map-server tiles bootstrap  # init container
streetcryptid-map-server tiles watch      # updater sidecar
streetcryptid-map-server tiles verify     # verify the active local release
```

## Build & test

```bash
go test ./...            # unit + golden + integration (fake Martin, signed manifest)
go test -race ./...      # concurrency (needs cgo; runs in CI/Dockerfile build stage)
go vet ./...
docker build -t streetcryptid-map-server .
```

Runtime configuration is operational only (see `internal/httpapi`, `cmd/server`):
`MARTIN_URL`, `BUNDLE_CACHE_DIR`, `BUNDLE_WORKERS`, `TILE_DATA_DIR`,
`TILE_MANIFEST_URL`, `TILE_MANIFEST_PUBLIC_KEY_FILE`, `TILE_UPDATE_INTERVAL`, etc.
The privacy constants are absent from configuration by design.

## Deploy

```bash
helm upgrade --install maps helm/streetcryptid-map-server \
  --set image.tag=<semver> \
  --set tiles.manifestURL=https://artifacts.example.com/manifest.json \
  --set tiles.publicKey=<ed25519-hex> \
  --set ingress.enabled=true --set ingress.host=martin.junephilip.com
```

Only port 8080 is exposed; Martin's 3000 is never in a Service, Ingress,
`hostPort`, or `LoadBalancer`. The chart ships a scoped Role (get/patch this one
Deployment), NetworkPolicy, optional ServiceMonitor/PDB/Ingress, and a `helm
test` connection pod.

## CI/CD

`.github/workflows/ci.yml` runs `go vet` + `go test -race` on every push/PR.
`.github/workflows/release.yml` builds a multi-arch image and pushes to GHCR with
semver tags on git tags `v*` (plus `sha-<commit>`), so a cluster running Flux
`ImagePolicy`/`ImageUpdateAutomation` can auto-bump to new semver releases.

```
docker pull ghcr.io/unrealjune/streetcryptid-map-server:<semver>
```

## Layout

```
cmd/server            process, config, subcommands, graceful shutdown
internal/privacy      compile-time boundary, XYZ validation, descendant math
internal/scb1         strict SCB1 encoder + size accounting
internal/martin       bounded localhost Martin client
internal/cache        disk LRU bundle cache + in-flight dedup
internal/httpapi      routes, coarse + bundle handlers, health, metrics, limits
internal/tilesync     signed manifest, resumable download, releases, updater, k8s patch
helm/                 chart (API + private Martin + bootstrap + updater)
scripts/              bake / publish / verify (workstation/CI)
```

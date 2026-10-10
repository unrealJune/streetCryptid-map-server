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
        │          ├── persistent checksummed bundle cache (PVC)
        │          └── direct read-only active PMTiles (fine bundles)
        ▼
  localhost:3000  Martin sidecar ── read-only active planet release
        ▲
 init container (bootstrap) ── activates a verified release on the tile PVC
 updater sidecar ─────────── stages verified releases, patches this Deployment
```

### Public API

| Method | Path | Zoom | Behavior |
| ------ | ---- | ---- | -------- |
| `GET`/`HEAD` | `/planet/{z}/{x}/{y}` | 0–10 | Proxies Martin MVT bytes, gzip-encoded as stored when the client accepts gzip (inflated otherwise); `Cache-Control: public, max-age=86400`, `Vary: Accept-Encoding`. |
| `GET`/`HEAD` | `/planet/{z}/{x}/{y}` | 11–14 | **404 without contacting Martin.** |
| `GET` | `/planet/bundle/v1/{x10}/{y10}/{tileZoom}` | 11–14 | Returns the complete descendant set as one SCB1 bundle. |
| `GET`/`HEAD` | `/planet/bundle/v2/{x10}/{y10}/{tileZoom}` | 11–14 | Progressive SCB2 stages; complete cached representations support byte ranges. |
| `GET`/`HEAD` | `/planet/bundle/v3/{x10}/{y10}/{tileZoom}` | 11–14 | Progressive SCB3: per-tile gzip entries, z14 split into structure and labels stages; byte ranges as v2. |
| `GET`/`HEAD` | `/terrain/{z}/{x}/{y}` | 0–10 | One Terrarium WebP DEM tile (204 over open sea). **404 above z10**, and on every terrain route when no terrain archive is installed. |
| `GET` | `/terrain/bundle/v1/{x10}/{y10}/{tileZoom}` | 11–12 | Every DEM tile under the z10 anchor as one SCB1 bundle (raw entries, each an image). |
| `GET` | `/livez` `/readyz` `/metrics` | — | Health + Prometheus metrics (metrics is cluster-internal). |

A z`N` bundle contains every descendant under the fixed z10 anchor:
z11→4, z12→16, z13→64, z14→256 entries, deterministic row-major order. The wire
format (`SCB1`) is the server side of the app parser in
`streetCryptid/src/features/map/tiles/tile-bundle.ts` — the golden fixture in
`testdata/scb1-z11.golden` locks the byte layout.

Fine bundles read the active PMTiles file **directly**, without per-descendant
HTTP fanout through Martin. Martin still serves raw z0–10 and supplies the
readiness catalog. The v1 media type, outer gzip, SCB1 layout, deterministic
descendant ordering, and ETag format are unchanged.

### Progressive/resumable SCB2

V2 accepts only the same fixed z10 anchor and requested zoom. Query parameters
(including child hints), request bodies, and non-z10 anchors are not supported.
It never accepts a viewport, child tile, or target-dependent priority.

The response is `application/vnd.streetcryptid.tile-stream`, with
`Cache-Control: public, max-age=86400, no-transform` and **no HTTP
Content-Encoding**. Gzip belongs to each stage, not to the HTTP representation.
All integers below are unsigned big-endian:

| Header offset | Bytes | Value |
| --- | --- | --- |
| 0 | 4 | ASCII `SCB2` |
| 4 | 1 | Version `2` |
| 5 | 1 | Anchor zoom `10` |
| 6 | 1 | Requested tile zoom |
| 7 | 1 | Reserved `0` |
| 8 | 4 | Anchor x10 |
| 12 | 4 | Anchor y10 |
| 16 | 4 | Stage count |

The header is exactly 20 bytes. Requested z14 has stages **[13, 14]**; all other
requests have one stage **[tileZoom]**. Each frame has a 40-byte prefix:
compressed length at offset 0 (4 bytes), raw SCB1 length at offset 4 (4 bytes),
and SHA-256 of the raw SCB1 at offset 8 (32 bytes). The complete gzip-compressed
SCB1 payload immediately follows. Each stage contains **all** descendants of
the same anchor, validated in row-major order. Raw stages are capped at 64 MiB,
compressed stages at 65 MiB, and the full stream at
`2 * (65 MiB + 40) + 20` bytes.

On an ordinary cold GET, the header and complete z13 overview are written and
flushed **before** starting z14. Concurrent requests for that same stream share
the build; followers can receive the completed representation. A disconnected
client does not cancel the bounded build. A failure after streaming starts
aborts the HTTP stream; clients must require every expected stage, check each
raw length and digest, and never treat a truncated prefix as a complete bundle.
Only a successful full representation enters the v2 cache.

Completed responses advertise `Accept-Ranges: bytes` and support a **single**
`Range: bytes=...` via Go's `http.ServeContent`: closed, open-ended and suffix
ranges, 206/Content-Range, 416 for unsatisfiable ranges, HEAD and conditional
GET. Multipart ranges are ignored (a full 200 response). A cold Range request
may build the whole representation before answering. Resume with the stored
**strong ETag** in `If-Range`; a changed dataset or codec, weak ETag, or date
validator produces a full 200 rather than mixing versions. There is no
Last-Modified validator. The v2 ETag/cache namespace includes the release label,
artifact SHA-256, z10 anchor, requested zoom and
`v2-scb1-gzip-go1.26-r1`. Gzip has timestamp zero; bump the codec identifier when
compressor/encoding changes alter bytes.

A resume at exactly the completed representation length returns 416 with the
current strong ETag and `Content-Range: bytes */N`, including on a cold cache.
This lets a client recognize a fully saved transfer after a crash, but only
when both the saved ETag and byte count match. SCB2 preserves the ETag on 416
without changing Go's global error-header policy; that error response uses
`Cache-Control: no-store, no-transform` and no Content-Encoding. A changed
If-Range still returns the full 200 representation, even at the old end offset.

V2 includes public CORS response headers and an OPTIONS preflight allowing
Range/If-Range/If-None-Match, with ETag/range/length headers exposed. The server
sends `X-Accel-Buffering: no`; any ingress/CDN must also honor no-transform and
avoid response buffering/compression on this route for early stages to arrive
progressively. Proxy timeouts must cover the configured build duration.

Cross-language conformance fixtures for **10/164/357/11**, four empty sentinels:
`testdata/scb2-z11-empty.json` contains raw SCB1, gzip, raw SHA-256 and full SCB2
as hex; `testdata/scb2-z11-empty.scb2` is the exact binary response.
The golden test regenerates them only with `UPDATE_SCB2_FIXTURE=1`.

For local app integration, run the production HTTP handler, direct PMTiles
reader and cache against a tiny generated empty archive (no planet download,
sidecar installation, or deployment):

```powershell
Set-Location C:\Users\june\streetCryptid-map-server
go run .\cmd\fixture-server -listen 127.0.0.1:8089
```

The golden endpoint is `http://127.0.0.1:8089/planet/bundle/v2/164/357/11`
(107 bytes); z14 returns empty, complete stages [13,14]. The command refuses
non-loopback bindings. It uses a temporary cache by default; `-cache-dir` can
select an isolated persistent fixture cache for restart testing. `/readyz`
checks a local empty Martin stub; fine bundles always use the real PMTiles
reader. The fixture command is not included in the production image.

With an existing app checkout and its dependencies installed, a second terminal
can run the cross-language check without modifying app source:

```powershell
bun run .\scripts\check-app-conformance.ts C:\Users\june\streetCryptid http://127.0.0.1:8089
```

This imports the app's actual `TileStreamDecoder`, `StreamingBundleSource` and
`SqliteBundleResumeStore`. It checks golden decoding under byte fragmentation,
live HTTP bytes, SQLite close/reopen prefix resumes, 206, matching end-offset
416, changed-ETag full replacement, and complete z13/z14 stage delivery.
It uses host Bun fetch, SHA-256 and SQLite adapters, not the device-native
Expo implementations. No Bun dependencies are added to the Go server.

### Progressive SCB3

V3 is served alongside v1 and v2, which keep working unchanged. It shares
v2's request validation (fixed z10 anchor and requested zoom; no query string
or body), its response headers (`Cache-Control: public, max-age=86400,
no-transform`, strong ETag, `Accept-Ranges: bytes`, CORS, `X-Accel-Buffering:
no`), its Range / If-Range / 416 behaviour, progressive flushing, build
admission and "only a complete stream is cached" rule. The media type is
`application/vnd.streetcryptid.tile-stream3`.

Two things differ. There is **no gzip around a stage**: each tile entry is its
own gzip member, copied byte for byte from the PMTiles archive (an uncompressed
archive is gzipped per tile), so the app inflates only the tiles it draws. And
the **z14 stage is split by MVT layer** into a `structure` stage and a `labels`
stage, so streets and buildings arrive before house numbers and POIs. Both
stages still contain every descendant in the same row-major order; the split
changes what is inside an entry, never which entries exist. A tile is a
protobuf whose only field is `repeated Layer layers = 3`, so concatenating the
inflated structure and labels entries gives back a tile with every layer.

| Header offset | Bytes | Value |
| --- | --- | --- |
| 0 | 4 | ASCII `SCB3` |
| 4 | 1 | Version `3` |
| 5 | 1 | Anchor zoom `10` |
| 6 | 1 | Requested tile zoom (11–14) |
| 7 | 1 | Reserved `0` |
| 8 | 4 | Anchor x10 |
| 12 | 4 | Anchor y10 |
| 16 | 4 | Stage count |

| Requested zoom | Stages, in order (zoom, part) |
| --- | --- |
| 11, 12, 13 | (z, 0 full) |
| 14 | (13, 0 full), (14, 1 structure), (14, 2 labels) |

Part `0` is every layer, `1` every layer not in the label set, `2` only the
label set. The label set is the constant `{housenumber, poi}`
(`mvt.LabelLayers`); changing it requires a new `scb3.CodecVersion`.

Each frame has a 40-byte prefix: payload length (4 bytes), stage zoom (1),
part (1), reserved `0` (2), SHA-256 of the payload (32). The payload follows
as-is. It is an SCB1 body with flags byte `0x01` (every non-empty entry is one
complete gzip member, starting `1f 8b 08`; an entry inflates to at most
16 MiB). A part with no layers, or a tile empty upstream, is the empty
sentinel. Payloads are capped at 64 MiB and the stream at
`3 * (64 MiB + 40) + 20` bytes.

On a cold z14 GET the header and z13 frame are flushed before z14 is read,
then the structure frame, then the labels frame. Both z14 parts come from one
pass over the 256 tiles and are cached as separate stages. The ETag/cache
namespace includes `v3-scb1gz-go1.26-r1`; recompressed entries use Go's
default gzip level with no name or timestamp, so they are deterministic for a
Go version. `mapapi_bundle_v3_total` counts v3 requests.

Fixtures for **10/164/357** with every descendant empty:
`testdata/scb3-z11-empty.scb3` (one stage) and `testdata/scb3-z14-empty.scb3`
(three stages), with `.json` companions holding each stage payload and its
SHA-256 as hex. The fixture server returns them byte for byte; the golden test
regenerates them only with `UPDATE_SCB3_FIXTURE=1`. `scripts/bundle-stat.py`
reports stage, tile and per-layer sizes for any SCB2 or SCB3 stream.

### Persistent cache and bounded builds

The default cache budget is **4 GiB of representation payloads**, on a **6 GiB
ReadWriteOnce PVC** (or `persistence.cache.existingClaim`). The extra space covers
file metadata/allocation and atomic-write headroom. The chart enforces one
replica and Recreate: this cache is single-writer, not a shared multi-pod index.
The previous emptyDir default did not survive pod replacement.

Each immutable `.bundle` file atomically contains its key, size, SHA-256 and
payload. Startup verifies those records, restores LRU from last-access mtimes,
removes corrupt/incomplete cache-owned files and trims to the configured byte
budget. Legacy unindexed `.scb1gz` files are removed once. Reads recheck integrity
and use a seekable file view, not a whole-response `ReadFile`. Active readers pin
entries; eviction never drops their byte accounting or invalidates a transfer.
If all eviction candidates are pinned, insertion fails explicitly. IO failures
are reported, not silently treated as successful persistence. The index also
has a 100,000-entry cap to bound tiny-file overhead.

V1 and v2 reuse complete gzipped SCB1 stage caches; complete SCB2 files also
include the stage bytes so cache-hit transfers and ranges need only one file.
V3 caches its own stages (gzip-entry SCB1; the z14 structure and labels parts
as two) and complete SCB3 files the same way. All copies count toward the same
LRU budget. Oversized individual cache objects
and explicitly disabled persistence still allow uncached responses.

`BUNDLE_MAX_BUILDS` defaults to **1**, bounding whole-build working sets across
both versions; overload returns 503 with Retry-After. `BUNDLE_WORKERS` remains
**16**, a global tile-read bound, not increased fanout. The overall
`BUNDLE_REQUEST_TIMEOUT` (default 60s) covers both stages, including detached
work. Subscriber writes have a 5s deadline so a stalled client cannot hold a
build indefinitely. The chart gives the API a 1 GiB memory limit; increasing
build admission requires budgeting memory for additional complete stages.

The local reader supports PMTiles **v3 MVT**, internal and tile compression
**none/gzip**, Hilbert IDs, root/leaf directories, sparse entries and RLE.
It bounds individual decoded tiles at 16 MiB, decoded directories at 8 MiB /
262,144 entries, directory traversal at four levels, and its leaf-directory LRU
at 32 MiB / 4,096 directories. Invalid offsets, truncation, overflow, gzip
failures, cycles and unsupported compression fail explicitly; there is **no
Martin fallback** in the serving binary. The verified active release's file is
opened once and pinned for the server lifetime. A missing/inconsistent active
pointer or unsupported archive prevents startup rather than using an
`unknown` dataset namespace.

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

### In-cluster bake (no external download)

If you'd rather **produce** the planet on the cluster than host it externally,
enable `tiles.bake` (with `tiles.autoUpdate.enabled=false`). A **heavily
throttled** CronJob runs Planetiler onto a dedicated scratch volume, then
`tiles import` installs the result as the active release — no network fetch and
no signature (an in-cluster-baked file is trusted; the download path still
requires a signature).

The bake is boxed in so it can't starve the cluster: pinned to one node,
CPU/memory limited, Planetiler `--threads`/`-Xmx` bounded, the low-memory mmap
profile (nodemap spills to SSD), a capped OSM download bandwidth, and a
suspended-by-default schedule. A planet bake still wants ~64 GB RAM, ~500 GB
scratch, and ~a day — pin the serving Deployment to the same node so it shares
the ReadWriteOnce tile volume.

```bash
helm upgrade --install maps helm/streetcryptid-map-server \
  --set tiles.autoUpdate.enabled=false \
  --set tiles.bake.enabled=true \
  --set nodeSelector."kubernetes\.io/hostname"=delphinium
# trigger the bake on demand:
kubectl create job maps-bake-now --from=cronjob/maps-streetcryptid-map-server-bake
```

### Terrain (the /terrain DEM)

The app shades parkland from elevation — hillshade, dotted contours, a treeline.
That elevation is the Copernicus DEM GLO-30 (GLO-90 where the 30 m product is
withheld), baked by `terrain/bake_terrain.py` into **Terrarium-encoded WebP tiles,
z0–12, integer metres** and served from `<dataDir>/terrain/terrain.pmtiles`:

- **Same privacy boundary as the vectors.** z0–10 is public XYZ; z11–12 only
  leaves as the complete z10-anchored bundle (`privacy.MaxTerrainZoom = 12`,
  compiled in like every other boundary). An archive deeper than z12 is refused
  at import and at startup.
- **Optional.** Without the archive every `/terrain` route answers 404 and the
  app draws parkland flat (its "canopy" texture). A present-but-invalid archive
  fails startup rather than silently looking absent.
- **Integer metres, not terrain-RGB's 0.1 m.** The app quantizes elevation to
  8-bit contour bands; the tenths are noise that defeats WebP. A hilly z12 tile
  is ~15–30 KB, a full z0–12 bake on the order of 100 GB — size
  `persistence.tiles.size` for it. All-sea tiles are not written (the app reads
  a missing tile as 0 m).
- **Bake** in-cluster with `tiles.terrainBake.enabled=true` (a suspended
  CronJob; image `streetcryptid-terrain-bake`, built from `terrain/`). It reads
  the public COGs in place over HTTPS and is **resumable**: progress lives in a
  SQLite staging DB on its scratch volume, so a retried pod continues at the next
  1-degree cell. The Job's last step is `tiles import-terrain`, which moves the
  archive into place and recreates the pod. `--bbox` (`tiles.terrainBake.bbox`)
  bakes a region; CI bakes one Kyoto cell and imports it with the Go server on
  every PR.

```bash
kubectl create job maps-terrain-now --from=cronjob/maps-streetcryptid-map-server-terrain-bake
# locally, against a regional bake:
docker build -t terrain-bake terrain
docker run --rm -v "$PWD/out:/work" terrain-bake --output /work/terrain.pmtiles \
  --workdir /work/wd --bbox 135,34,137,36
go run ./cmd/fixture-server -terrain out/terrain.pmtiles   # serves /terrain on 127.0.0.1:8089
```

Copernicus attribution (required by its licence): "© DLR e.V. 2010-2014 and
© Airbus Defence and Space GmbH 2014-2018 provided under COPERNICUS by the
European Union and ESA; all rights reserved".

## The binary

One static, non-root, distroless image; four subcommands:

```
streetcryptid-map-server serve            # the public API
streetcryptid-map-server tiles bootstrap  # init container
streetcryptid-map-server tiles watch      # updater sidecar
streetcryptid-map-server tiles verify     # verify the active local release
streetcryptid-map-server tiles import-terrain <file> [version]  # install a terrain bake
```

## Build & test

```bash
go test ./...            # unit + golden + integration (fake Martin, signed manifest)
go test -race ./...      # concurrency (needs cgo; runs in CI/Dockerfile build stage)
go vet ./...
docker build -t streetcryptid-map-server .
```

Runtime configuration is operational only (see `internal/httpapi`, `cmd/server`):
`MARTIN_URL`, `BUNDLE_CACHE_DIR`, `BUNDLE_CACHE_MAX_BYTES`, `BUNDLE_MAX_BUILDS`,
`BUNDLE_WORKERS`, `TILE_DATA_DIR`,
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
internal/scb2         progressive framing + raw stage integrity + golden fixture
internal/scb3         SCB3 framing (gzip-entry stages, z14 layer split) + golden fixtures
internal/mvt          MVT layer split (structure / labels) without re-encoding
internal/pmtiles      bounded direct PMTiles v3 file reader + directory LRU
internal/martin       bounded localhost Martin client
internal/cache        disk LRU bundle cache + in-flight dedup
internal/httpapi      routes, coarse + bundle handlers, health, metrics, limits
internal/tilesync     signed manifest, resumable download, releases, updater, k8s patch
helm/                 chart (API + private Martin + bootstrap + updater)
scripts/              bake / publish / verify (workstation/CI)
terrain/              Copernicus DEM → Terrarium WebP PMTiles bake (Python + GDAL image)
scripts/bundle-stat.py  stage / tile / layer size report for SCB2 and SCB3 streams
```

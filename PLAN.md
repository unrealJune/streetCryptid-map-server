# streetCryptid map server — implementation plan

## Architecture decision

Do not ship Caddy or another application-level reverse proxy.

The custom Go API server is the only public map endpoint. It owns:

- Coarse XYZ routing.
- The z10 SCB1 bundle endpoint.
- Input validation.
- Privacy enforcement.
- Bundle generation and caching.
- Health and metrics endpoints.

Martin runs as a private sidecar in the same Kubernetes pod. It has no Service
and no public route. The API server reaches it over localhost.

TLS is supplied by the Kubernetes platform through an optional Ingress or a
`LoadBalancer` Service. The privacy boundary must remain enforced inside the Go
API regardless of ingress configuration.

## Non-configurable privacy invariants

Compile these values into the API server:

```go
const (
    privacyAnchorZoom = 10
    maxPublicRawZoom  = 10
    maxBundleZoom     = 14
)
```

They must not be environment variables, command-line flags, Helm values, ConfigMap
fields, or runtime feature flags.

The server must:

1. Allow public raw XYZ only for z0–10.
2. Return 404 for every raw z11–14 request before contacting Martin.
3. Accept fine detail only through the fixed-z10 bundle endpoint.
4. Never accept arbitrary child lists, viewports, centers, or bounding boxes.
5. Never expose Martin directly through a Kubernetes Service.

Tests must prove the block cannot be disabled through configuration.

## Objective

Serve one world-wide OpenMapTiles dataset from z0 through z14 while ensuring fine
child XYZ coordinates never reach the public API or its access logs.

The completed deployment consists of:

- A custom Go map API image built from this repository's `Dockerfile`.
- A Martin sidecar using the upstream Martin image.
- A Helm chart deploying both containers in one pod.
- A bootstrap init container using the same Go image.
- An updater sidecar, enabled by default, using the same Go image.
- One verified `planet.pmtiles` release activated from shared persistent storage.
- Optional bundle-cache storage mounted into the API container.

The app-side protocol source of truth is:

```text
C:\Users\june\streetCryptid\src\features\map\tiles\tile-bundle.ts
C:\Users\june\streetCryptid\src\features\map\tiles\bundle-fetch.ts
```

## Privacy model

For a requested z11–14 tile, the app sends only its fixed z10 ancestor and data
zoom. The API returns every descendant at that zoom:

| Data zoom | Descendant grid | Entries |
| --------- | --------------- | ------: |
| z11       | 2×2             |       4 |
| z12       | 4×4             |      16 |
| z13       | 8×8             |      64 |
| z14       | 16×16           |     256 |

The API may learn the z10 anchor, request time, adjacent anchors, and network
identity. It must not learn which child caused the request.

This provides coarse location quantization, not anonymity or private information
retrieval.

## Self-bootstrap and data updates

The server must bootstrap and update its **map data**, not bake the planet inside
Kubernetes.

The full Planetiler bake remains an external workstation/CI job. It publishes:

```text
planet.pmtiles
manifest.json
manifest.sig
```

The signed manifest contains:

```json
{
  "schema_version": 1,
  "version": "2026-07-15T00:00:00Z",
  "url": "https://artifacts.example.com/planet-2026-07-15.pmtiles",
  "sha256": "<hex digest>",
  "size": 0,
  "min_zoom": 0,
  "max_zoom": 14,
  "tile_schema": "openmaptiles",
  "created_at": "2026-07-15T00:00:00Z"
}
```

`manifest.sig` is an Ed25519 signature over the exact `manifest.json` bytes. The
verification public key is mounted through Helm; the signing private key never
enters the cluster.

### Bootstrap behavior

An init container runs:

```text
streetcryptid-map-server tiles bootstrap
```

It must:

1. Acquire an exclusive lock on the tile-data volume.
2. Validate any active release and its signed manifest.
3. Continue immediately when the active release is valid.
4. If no valid release exists, fetch and verify the remote signed manifest.
5. Check free space before downloading.
6. Download with HTTP Range resume into a versioned `.part` file.
7. Verify size, SHA-256, signature, zoom range, and OpenMapTiles metadata.
8. Atomically rename the completed artifact.
9. Atomically activate the release through a `current` symlink or manifest pointer.
10. Retain the previous valid release for rollback.

If no valid local release exists and bootstrap fails, the pod must not start.
If a valid local release exists but the update source is unavailable, startup
continues with the local release.

### Update behavior

The updater sidecar, enabled by default, runs:

```text
streetcryptid-map-server tiles watch
```

It periodically:

1. Fetches the small signed manifest.
2. Does nothing when the active version matches.
3. Downloads and verifies a newer release into staging.
4. Writes an atomic pending-release marker.
5. Patches only this Helm release's Deployment pod-template annotation with the
   pending version.
6. Lets Kubernetes restart the complete pod.
7. The new init container activates the verified pending release before Martin
   starts.
8. The API derives its dataset version from the active local manifest and naturally
   uses a new bundle-cache namespace.

The updater requires narrowly scoped RBAC to `get` and `patch` only its own
Deployment. It must not have cluster-wide write permissions.

Use Deployment strategy `Recreate` by default so old and new Martin processes do
not serve different datasets concurrently and so ReadWriteOnce volumes remain
safe. This trades a short update outage for deterministic consistency.

Keep at least two verified releases. A failed download, signature, hash, metadata,
or rollout preparation must leave the current release untouched.

Server software images are updated through Helm/GitOps using immutable image
digests. The running process must not self-replace its executable.

## Public API

Production source root:

```text
https://martin.junephilip.com/planet
```

### Coarse XYZ

```http
GET /planet/{z}/{x}/{y}
HEAD /planet/{z}/{x}/{y}
```

Behavior:

- Parse all three path fields as bounded integers.
- Permit only z0–10.
- Validate x/y against `[0, 2^z - 1]`.
- Reconstruct the internal Martin URL from validated integers.
- Proxy Martin's MVT bytes, status, `Content-Type`, `Content-Encoding`, ETag, and
  cache headers.
- Preserve 204/404 for empty tiles.
- Return 404 for z11–14 without calling Martin.
- Do not log the rejected fine path's x/y fields.

The block must execute before any generic proxy behavior.

### Fine-detail privacy bundle

```http
GET /planet/bundle/v1/{x10}/{y10}/{tileZoom}
Accept: application/vnd.streetcryptid.tile-bundle
```

Validation:

- `x10` and `y10` are integers in `[0, 1023]`.
- `tileZoom` is one of 11, 12, 13, or 14.
- No body, query parameters, child list, target-child header, center, bbox, or
  viewport is accepted.
- Invalid input returns 400.
- Martin/read failure returns 502 or 503.
- A partial bundle is never returned as 200.

Response:

```http
Content-Type: application/vnd.streetcryptid.tile-bundle
Content-Encoding: gzip
ETag: "<dataset-version>:10:<x10>:<y10>:<tileZoom>"
```

The decompressed body must not exceed 64 MiB.

### Health and metrics

```http
GET /livez
GET /readyz
GET /metrics
```

- `livez`: API process is responsive.
- `readyz`: `planet.pmtiles` is mounted and Martin can serve the `planet` source.
- `metrics`: intended for cluster-internal scraping; do not expose it through the
  public Ingress by default.

Do not expose Martin's `/catalog` publicly. Readiness may query it over localhost.

## SCB1 wire format

All integers are unsigned big-endian.

### Header

| Offset | Size | Value |
| -----: | ---: | ----- |
| 0      | 4    | ASCII `SCB1` |
| 4      | 1    | Version `1` |
| 5      | 1    | Anchor zoom `10` |
| 6      | 1    | Requested tile zoom |
| 7      | 1    | Flags `0` |
| 8      | 4    | z10 x |
| 12     | 4    | z10 y |
| 16     | 4    | Entry count |

### Entries

Entries use deterministic row-major order: y outer, x inner.

```text
d    = tileZoom - 10
side = 1 << d
x0   = x10 << d
y0   = y10 << d
```

Entry `i` represents:

```text
x = x0 + (i % side)
y = y0 + floor(i / side)
```

Each entry:

| Size | Value |
| ---: | ----- |
| 4    | Byte length, or `0xFFFFFFFF` for a known-empty tile |
| N    | Raw, transfer-decoded MVT bytes |

Every descendant must have an entry. Martin 204/404 responses use the empty
sentinel.

## Kubernetes pod architecture

```text
Kubernetes Service / optional Ingress
                  |
                  v
       map-api container :8080
          |            |
          |            `-- bundle cache volume
          v
    localhost:3000
      Martin sidecar
          |
          `-- read-only active planet release

init container
  `-- bootstrap/activate verified release on tile PVC

updater sidecar
  |-- stage verified releases on tile PVC
  `-- patch this Deployment to restart the whole pod
```

All containers share the pod network and mounted tile volume. Only bootstrap and
updater mounts are writable; Martin receives the active release read-only.

Only port 8080 is declared by the Kubernetes Service. Martin's port 3000 is
container-local and must not have a Service, Ingress, `hostPort`, or
`LoadBalancer`.

## Go API implementation

### Package responsibilities

```text
cmd/server
  process startup, configuration, HTTP server, shutdown

internal/httpapi
  route registration, coarse handler, bundle handler, health, metrics

internal/privacy
  non-configurable zoom constants, request validation, descendant math

internal/scb1
  strict encoder and response-size accounting

internal/martin
  localhost client with bounded timeouts and exact status handling

internal/cache
  completed-bundle cache, ETag, in-flight deduplication, LRU eviction

internal/tilesync
  signed manifest validation, resumable download, release activation, rollback,
  updater loop, narrowly scoped Deployment patch
```

### Coarse handler

The handler must parse and validate the route itself. Do not use a catch-all HTTP
proxy whose behavior could be changed by configuration.

Required order:

1. Parse z.
2. Reject z above `maxPublicRawZoom`.
3. Parse and validate x/y.
4. Reconstruct the Martin path.
5. Fetch from localhost Martin.
6. Stream the response.

### Bundle handler

- Compute the complete fixed descendant set.
- Read every descendant from Martin with bounded concurrency.
- Treat 204/404 as empty.
- Fail the whole bundle on any other Martin failure.
- Encode deterministic SCB1.
- Gzip the complete response.
- Continue fixed-set generation independently of an individual client
  disconnect, or fail without committing a partial cache entry.
- Never stop after locating one child.

### Operational limits

Runtime configuration may control operational behavior only:

```text
MARTIN_URL=http://127.0.0.1:3000/planet
BUNDLE_CACHE_DIR=/cache
BUNDLE_CACHE_MAX_BYTES=<bounded allocation>
BUNDLE_WORKERS=16
BUNDLE_REQUEST_TIMEOUT=60s
BUNDLE_MAX_BYTES=67108864
HTTP_READ_TIMEOUT=15s
HTTP_WRITE_TIMEOUT=90s
HTTP_IDLE_TIMEOUT=60s
TILE_DATA_DIR=/data
TILE_MANIFEST_URL=https://artifacts.example.com/manifest.json
TILE_MANIFEST_PUBLIC_KEY_FILE=/config/tiles-manifest.pub
TILE_AUTH_TOKEN_FILE=/secrets/artifact-token
TILE_UPDATE_INTERVAL=6h
TILE_RETAIN_RELEASES=2
POD_NAMESPACE=<downward API>
DEPLOYMENT_NAME=<Helm-generated name>
```

`privacyAnchorZoom`, `maxPublicRawZoom`, and `maxBundleZoom` are absent from
runtime configuration.

The API derives `TILESET_VERSION` from the active signed manifest. It must refuse
startup for missing or malformed required operational values.

## Dockerfile

Create one root `Dockerfile` for the Go API.

Requirements:

- Multi-stage build.
- Pin the Go major/minor version.
- Copy `go.mod` and `go.sum` before source for layer caching.
- Run `go mod download`.
- Build with `CGO_ENABLED=0`.
- Use `-trimpath` and inject version/commit metadata with `-ldflags`.
- Run `go test ./...` in the build stage or CI before image publication.
- Final image is distroless/static or equivalent minimal non-root image.
- No shell, compiler, source tree, PMTiles artifact, secrets, or cache data in the
  final image.
- Expose port 8080.
- Include OCI source, revision, and version labels.
- The same binary supports:
  - `serve`
  - `tiles bootstrap`
  - `tiles watch`
  - `tiles verify`

The Martin image is configured separately in Helm using the upstream pinned image.

## Helm chart

Create:

```text
helm/streetcryptid-map-server/
  Chart.yaml
  values.yaml
  values.schema.json
  templates/
    _helpers.tpl
    serviceaccount.yaml
    configmap.yaml
    deployment.yaml
    service.yaml
    ingress.yaml
    networkpolicy.yaml
    role.yaml
    rolebinding.yaml
    pvc.yaml
    poddisruptionbudget.yaml
    servicemonitor.yaml
    tests/
      test-connection.yaml
```

### Deployment

One Deployment and one pod template.

Init container:

- Uses the custom API image with `tiles bootstrap`.
- Mounts the tile PVC read-write.
- Mounts the manifest public key and optional artifact credentials.
- Activates a valid release before Martin starts.

Primary containers:

1. `map-api`
   - Custom image from this repository.
   - Port 8080.
   - Liveness `/livez`.
   - Readiness `/readyz`.
   - Bundle-cache volume.

2. `martin`
   - Pinned `ghcr.io/maplibre/martin` image.
   - Command points at `/data/current/planet.pmtiles`.
   - Port 3000 is not exposed by a Service.
   - Read-only tile volume.

Third container:

3. `tile-updater`
   - Uses the custom API image with `tiles watch`.
   - Mounts the tile PVC read-write.
   - Mounts public key and optional artifact credentials.
   - Uses downward-API namespace values.
   - Patches only this Deployment when a verified update is staged.

Pod requirements:

- Non-root security contexts.
- Read-only root filesystems where supported.
- Dropped Linux capabilities.
- Seccomp `RuntimeDefault`.
- Resource requests and limits for both containers.
- Graceful API shutdown.
- Read-only PMTiles mount shared with Martin.
- Optional writable cache mount for the API.
- Default `Recreate` Deployment strategy.
- Enough ephemeral/persistent storage for current, staged, and previous releases.

### Service

- Expose only API port 8080.
- Default `ClusterIP`.
- Allow `LoadBalancer` through values for clusters without Ingress.
- Never include Martin port 3000.

### Ingress

- Disabled by default.
- When enabled, route the host directly to the API Service.
- TLS secret and ingress-class annotations are configurable.
- No path-based privacy rules are required in the Ingress because the API server
  enforces them unconditionally.

### NetworkPolicy

- Permit ingress only to API port 8080.
- Permit monitoring access to `/metrics` according to namespace policy.
- Permit required DNS/egress.
- Martin remains reachable only over pod localhost.

### Storage

Tile data:

- Support either `persistence.tiles.existingClaim` or a chart-created PVC.
- Bootstrap may populate an empty PVC from the signed artifact manifest.
- Mount `/data/current/planet.pmtiles` read-only into Martin.
- Fail readiness when the file or Martin source is unavailable.
- Do not package map data into either container image or Helm chart.
- Size production storage for at least two full releases plus one resumable staging
  file.

Bundle cache:

- Default to `emptyDir` for a single replica.
- Optionally allow an existing PVC.
- Keep one replica by default until cache coordination across replicas is proven.

Updater:

- `tiles.autoUpdate.enabled` defaults to true.
- When enabled, require manifest URL and verification public key.
- Artifact credentials come from an existing Secret, never plain Helm values.
- Create a namespaced Role/RoleBinding scoped to patch only this Deployment.
- Air-gapped/manual deployments may disable automatic updates explicitly; bootstrap
  and signed local-release validation remain mandatory.

### Helm values that must not exist

The chart schema must reject or omit:

```text
maxPublicRawZoom
privacyAnchorZoom
allowRawFineTiles
disablePrivacy
bundleAnchorZoom
maxBundleZoom
```

There is no supported way to weaken the privacy block through Helm.

## Planned repository layout

```text
streetCryptid-map-server/
  PLAN.md
  README.md
  Dockerfile
  .dockerignore
  .gitignore
  go.mod
  go.sum
  cmd/
    server/
      main.go
  internal/
    cache/
    httpapi/
    martin/
    privacy/
    scb1/
    tilesync/
  helm/
    streetcryptid-map-server/
      Chart.yaml
      values.yaml
      values.schema.json
      templates/
  scripts/
    bake-planet.ps1
    bake-planet.sh
    publish-release.ps1
    publish-release.sh
    verify.ps1
    verify.sh
  testdata/
    scb1-z11.golden
  data/
    .gitkeep
```

Ignore:

- `data/*`
- `planet.pmtiles`
- Bundle cache files.
- Local environment files.
- Rendered Helm secrets.
- Logs and profiling output.

## Planet bake and artifact publication

Use Planetiler outside Kubernetes for the full bake:

```text
--download
--area=planet
--maxzoom=14
--nodemap-type=array
--storage=mmap
--output=/data/planet.pmtiles
```

Expected workstation requirements:

- Approximately 80 GB source download.
- Approximately 60–100 GB final PMTiles output.
- About 64 GB RAM recommended.
- Substantial temporary disk and potentially a day of processing.

Do not run the full planet bake as a Helm hook, init container, sidecar, or normal
Kubernetes Job.

Publication process:

1. Bake to a versioned filename.
2. Verify z0, z4, z7, z10, z13, and z14 locally.
3. Record SHA-256 and byte size.
4. Upload the PMTiles artifact first.
5. Generate the versioned manifest.
6. Sign the exact manifest bytes with the offline Ed25519 key.
7. Upload `manifest.sig`.
8. Publish `manifest.json` last, making the release discoverable only after every
   other object is durable.
9. Let bootstrap/updater download and activate it.

Artifact hosting must support HTTPS, content length, and HTTP Range requests.

## Implementation phases

### Phase 1 — Repository and privacy core

- Initialize Go module and package structure.
- Add compile-time privacy constants.
- Implement XYZ validation.
- Implement z10 descendant math.
- Implement SCB1 encoding and size accounting.
- Add golden compatibility fixture for the app parser.

Required tests:

- Raw z0–10 accepted.
- Raw z11–14 rejected.
- Rejection occurs without invoking the Martin fake.
- No configuration object can change the boundary.
- z11/z12/z13/z14 bundles contain 4/16/64/256 entries.
- Seattle anchor `10/164/357` produces the expected coordinate ranges.
- Malformed values never overflow or reach Martin.

### Phase 2 — Martin client and HTTP API

- Add bounded localhost Martin client.
- Implement coarse streaming handler.
- Implement complete-set bundle handler.
- Add `/livez`, `/readyz`, and `/metrics`.
- Add structured logs that omit fine child paths and coordinate labels.
- Add graceful shutdown and server timeouts.

### Phase 3 — Bundle cache and abuse controls

- Atomic cache writes.
- Dataset-versioned keys.
- Header verification before cache hits.
- In-flight request deduplication.
- LRU disk cap.
- Global worker bound.
- Per-client rate limits, especially for z14.
- Fail deployment checks if valid z14 bundles exceed 64 MiB.

### Phase 4 — Tile bootstrap and updater

- Define and test the signed manifest schema.
- Verify Ed25519 signatures.
- Implement resumable Range downloads.
- Verify free space, size, SHA-256, zoom range, and tile schema.
- Implement volume locking, versioned release directories, atomic activation, and
  previous-release retention.
- Implement bootstrap behavior for empty and already-valid volumes.
- Implement update polling and pending-release staging.
- Implement narrowly scoped Kubernetes Deployment patching.
- Guarantee failed updates leave the active release unchanged.

### Phase 5 — Container image

- Implement multi-stage Dockerfile.
- Add `.dockerignore`.
- Build and run as non-root.
- Scan image and generate an SBOM in CI.
- Publish immutable image tags and digest.
- Verify all `serve` and `tiles` subcommands work in the same image.

### Phase 6 — Helm chart

- Add chart metadata and JSON schema.
- Deploy API + Martin sidecar.
- Add bootstrap init container and updater sidecar.
- Expose API-only Service.
- Add optional Ingress and TLS values.
- Add tile PVC and cache volume options.
- Add probes, resources, security contexts, NetworkPolicy, PDB, and optional
  ServiceMonitor.
- Add scoped updater Role/RoleBinding.
- Add manifest public-key and artifact-Secret mounts.
- Add Helm test pod.
- Confirm rendered manifests never expose Martin port 3000.

### Phase 7 — Deployment verification

Run:

```text
go test ./...
go test -race ./...
docker build .
helm lint helm/streetcryptid-map-server
helm template test helm/streetcryptid-map-server --values <test-values>
```

Cluster checks:

- `/livez` and `/readyz` succeed.
- Public raw z10 succeeds.
- Public raw z11–14 returns 404.
- A blocked raw request causes zero Martin requests.
- z13 bundle returns exactly 64 entries.
- z14 bundle returns exactly 256 entries.
- Bundle entries byte-match Martin internally.
- Repeated bundle request hits cache with stable ETag.
- Only API port 8080 exists in the Service.
- Martin port 3000 is unreachable through cluster Services and Ingress.
- Public logs contain no fine child XYZ paths.
- An empty PVC bootstraps a verified release without manual copying.
- A valid local release starts successfully while the manifest host is unavailable.
- A newer signed manifest stages a release and restarts the complete pod.
- A bad signature, wrong hash, truncated download, or insufficient disk leaves the
  active release untouched.
- The new pod reports the updated dataset version and uses a new bundle-cache
  namespace.
- The updater ServiceAccount cannot patch other Deployments.

App coordination:

- Update the app verifier to test coarse public XYZ plus SCB1 bundles; it must no
  longer require public raw z12–14 access or a public Martin catalog.
- Point `EXPO_PUBLIC_TILE_URL` at:

  ```text
  https://martin.junephilip.com/planet
  ```

- Perform a real-device street-zoom pan and confirm only z10 bundle URLs reach
  the API.

## Definition of done

- One Dockerfile builds the non-root Go API image.
- One Helm chart deploys the API and private Martin sidecar.
- An empty persistent volume can bootstrap from a signed artifact release.
- The updater can stage and activate new map data without manual file copying.
- Failed updates preserve the last valid release.
- The privacy cutoff is compiled into the API and cannot be weakened by values,
  environment, flags, or ingress changes.
- Raw z11–14 is impossible through the public API.
- Fine requests produce one fixed-z10 SCB1 bundle.
- `planet.pmtiles` remains outside images and source control.
- Health, metrics, resource limits, security contexts, NetworkPolicy, and
  deployment verification are complete.

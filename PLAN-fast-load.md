# Fast-load plan: smaller bundles, compressed coarse tiles, SCB3

This is an implementation plan for making the streetCryptid map load faster on
the phone. It spans two repositories:

- **server**: this repo (`streetcryptid-map-server`, Go).
- **app**: `unrealJune/streetCryptid` (Expo / React Native / TypeScript, with a
  Rust native module at `modules/iroh-location/rust`).

It is written for an implementing agent that has not seen the analysis behind
it. Everything that needed a decision has been decided here. Do the workstreams
in order; each one ships on its own. Do not start Workstream 3 until
Workstream 2 is merged and deployed, because the app must fall back to the
still-served v2 endpoint on servers that lack v3.

---

## 0. Read this first

### 0.1 What was measured (production, Seattle anchor z10 164/357, 2026-10-09)

| Thing | Measured | Meaning |
| --- | --- | --- |
| `GET /planet/bundle/v2/164/357/14` | 16.7 MB | the whole z14 download the app does for one 25 km cell |
| z14 stage alone, raw SCB1 / gzip | 22.3 MB / 14.2 MB | 256 tiles, median 82 KB raw, max 402 KB |
| z13 stage | 2.4 MB | fine as is |
| z14 raw bytes by MVT layer | building 27%, **housenumber 24%, poi 19%**, transportation 14%, transportation_name 6%, rest 10% | house numbers and POIs are 43% of the download |
| `GET /planet/10/164/357` (coarse tile) | 111 KB, **no `Content-Encoding`, no `Cache-Control`** | gzip would be 84 KB. PMTiles already stores it gzipped |
| cold z13 build TTFB | 0.77 s | server-side build speed is not the bottleneck |
| per-tile gzip vs whole-stage gzip | within 0.2% | cross-tile compression buys nothing |
| brotli q5 / q11 whole stage | -9% / -19% | q11 takes 72 s per z14 stage, over the 60 s build timeout |
| zstd -19 with trained dictionary, per tile | -3% | not worth a new codec |

The app only draws house numbers at camera zoom 17 and above
(`HOUSENUMBER_MIN_ZOOM = 17` in `src/features/map/core/map-labels.ts`); the
initial camera zoom is 15. The app inflates each stage in JavaScript on the JS
thread with `fflate.gunzipSync` (14 MB in, 22 MB out) and stores the raw
22 MB per z14 anchor in SQLite.

Reproduce any number with `scripts/bundle-stat.py` (stdlib only):

```bash
curl -sS https://martin.junephilip.com/planet/bundle/v2/164/357/14 -o /tmp/s.scb2
python3 -I scripts/bundle-stat.py /tmp/s.scb2
```

### 0.2 Decisions already made. Do not relitigate them.

1. **No WebSockets.** The transfer is one resumable byte stream using Range,
   If-Range, strong ETags and a SQLite resume journal in the app. HTTP/2 is
   already on at the edge (Caddy). Nothing here changes transport.
2. **No brotli, no zstd.** With per-tile compression (decision 3) whole-stage
   codecs are off the table and per-tile brotli measured -4%. Stay on gzip.
3. **Entries are shipped gzip-compressed per tile**, byte-for-byte as stored in
   the PMTiles archive where possible. Server builds for z11–13 become byte
   copies. The app inflates only the tiles it draws, and SQLite stores
   compressed bytes.
4. **The z14 stage is split into two stages by MVT layer**: `structure`
   (every layer except `housenumber` and `poi`) and `labels` (`housenumber`
   and `poi` only). Both stages cover every descendant of the same z10 anchor
   in the same fixed row-major order. The split is done on the server at
   build time, not in the Planetiler bake.
5. **New endpoint `/planet/bundle/v3/...` with format SCB3. v1 and v2 keep
   working unchanged** until the app has been on v3 for a release cycle.
6. **The privacy boundary does not move.** See 0.3.

### 0.3 Invariants. Any change that violates one of these is wrong.

- `internal/privacy` constants stay `const` and stay 10 / 10 / 14. No new env
  var, flag, Helm value or query parameter may influence which tiles a bundle
  contains or in which order.
- Raw XYZ stays public for z0–10 only; z11–14 raw stays 404 before any
  upstream contact.
- Every bundle endpoint (v1, v2, v3) accepts exactly `{x10}/{y10}/{tileZoom}`
  and rejects query strings and bodies (`bundleRequest` in
  `internal/httpapi/server.go` already does this; reuse it).
- Every stage contains **all** descendants of the anchor in row-major order.
  The layer split changes what is inside each entry, never which entries exist.
- The server never learns which child tile the user is looking at. Nothing in
  this plan adds a per-tile or per-viewport request.
- The app never sends a z11–14 XYZ request. The app's existing check in
  `bundle-fetch.ts` (`tile.z <= anchorZoom` chooses the coarse path) stays.

### 0.4 How to verify work in each repo

Server (run from the repo root; CI runs the same):

```bash
go vet ./... && go test ./... && go test -race ./...
go run ./cmd/fixture-server -listen 127.0.0.1:8089      # local empty-planet server
```

App (from the app repo root; `just check` runs these):

```bash
bun run typecheck && bun run lint && bun run test
just test-rust                                           # cargo test in modules/iroh-location/rust
```

Cross-language conformance (server repo, needs an app checkout and a running
fixture server):

```bash
bun run ./scripts/check-app-conformance.ts /path/to/streetCryptid http://127.0.0.1:8089
```

---

## Workstream 1: coarse tiles compressed and cacheable (server only)

Ship this first and alone. It is a bug fix.

### Cause

`internal/martin/martin.go` `GetTile` builds the upstream request without an
`Accept-Encoding` header. Go's `http.Client` then adds `Accept-Encoding: gzip`
itself, transparently inflates the response, and **removes the
`Content-Encoding` header**. `handleCoarse` in `internal/httpapi/server.go`
copies the now-absent header and writes the inflated body. Martin also sends no
`Cache-Control`, and the server forwards that absence.

### Changes

**`internal/martin/martin.go`**

1. In `GetTile`, after creating the request:
   `req.Header.Set("Accept-Encoding", "gzip")`. Setting it explicitly disables
   Go's transparent decompression, so `resp.Body` is the stored gzip bytes and
   `Content-Encoding: gzip` survives.
2. `maxTileBytes` now bounds the compressed body. Keep the bound as is.
3. Add a method on `*TileResponse`:

   ```go
   // RawBody returns the tile bytes with any gzip transfer encoding removed.
   // The inflated size is bounded by limit; larger tiles are an error.
   func (t *TileResponse) RawBody(limit int64) ([]byte, error)
   ```

   If `ContentEncoding` is `""` or `identity`, return `Body`. If `gzip`,
   inflate with `compress/gzip` through `io.LimitReader(zr, limit+1)` and fail
   when more than `limit` bytes come out. Any other encoding is an error.
4. `GetTileBytes` (used by v1/v2 bundle builds when the Martin client is the
   `TileSource`, which happens in tests and the fixture fallback) must return
   **raw MVT** exactly as before: call `RawBody(c.maxTileBytes)`.
5. Add, for Workstream 2:

   ```go
   // GetTileStored returns the tile as the upstream stores it plus whether it
   // is a complete gzip member. (nil, false, nil) means an empty tile.
   func (c *Client) GetTileStored(ctx context.Context, t privacy.TileCoord) ([]byte, bool, error)
   ```

   Returns `resp.Body, resp.ContentEncoding == "gzip"`.

**`internal/httpapi/server.go` `handleCoarse`**

After the upstream call succeeds:

1. Decide the client's acceptance: `acceptsGzip := strings.Contains(r.Header.Get("Accept-Encoding"), "gzip")`.
2. If `resp.ContentEncoding == "gzip"` and `!acceptsGzip`: inflate with
   `resp.RawBody(16 << 20)` and send with no `Content-Encoding`. Count it in a
   new metric `mapapi_coarse_inflated_total`. (Real clients all accept gzip;
   this branch exists so a `curl` without `--compressed` still works.)
3. Otherwise forward `Content-Encoding` as returned.
4. Always set:
   - `Vary: Accept-Encoding`
   - `Cache-Control: public, max-age=86400` (override whatever Martin sent;
     the ETag from Martin changes on every rebake, so a day is safe)
   - `Content-Length` to the byte count actually sent
   - `Content-Type` and `ETag` forwarded as today
5. Add metric `mapapi_coarse_upstream_gzip_total`, incremented when Martin
   returned `Content-Encoding: gzip`. This is how you confirm after deploy that
   Martin honours the header: the counter must climb on `/metrics`.

Do **not** compress on the server when Martin returns identity. That costs CPU
per request for a case that should not happen; the metric above will tell you
if it does.

### Tests (`internal/httpapi/server_test.go`)

The fake Martin (`newFakeMartin`) must gain gzip behaviour: when the incoming
request has `Accept-Encoding` containing `gzip`, write the gzip of the
synthetic tile bytes with `Content-Encoding: gzip`; otherwise write them plain.
Then:

- `TestCoarseGzipPassthrough`: request with `Accept-Encoding: gzip` →
  `Content-Encoding: gzip`, `Vary: Accept-Encoding`,
  `Cache-Control: public, max-age=86400`, body equals the fake's gzip bytes,
  `Content-Length` matches.
- `TestCoarseInflatesForClientWithoutGzip`: request without the header → no
  `Content-Encoding`, body equals the raw synthetic bytes.
- `TestBundleBytesMatchMartin` and `TestDirectPMTilesV1ParityWithoutMartinFanout`
  must still pass unchanged: bundle entries remain raw MVT.
- A unit test for `RawBody` rejecting a gzip stream that inflates past `limit`.

### Acceptance (after deploy)

```bash
curl -sI -H 'Accept-Encoding: gzip' https://martin.junephilip.com/planet/10/164/357 | grep -i -E 'content-encoding|cache-control|vary|content-length'
# content-encoding: gzip, cache-control: public, max-age=86400, vary: Accept-Encoding, content-length ≈ 84000
curl -s -H 'Accept-Encoding: gzip' https://martin.junephilip.com/planet/10/164/357 | gunzip | wc -c   # ≈ 110975
```

The app needs no change: `fetch` inflates `Content-Encoding: gzip` natively.

README: update the Public API table row for z0–10 to say the response is
gzip-encoded when accepted and cacheable for a day.

---

## Workstream 2: SCB3 on the server

### 2.1 Wire format (normative)

All integers unsigned big-endian, same conventions as SCB2.

**Stream header, 20 bytes**

| Offset | Bytes | Value |
| --- | --- | --- |
| 0 | 4 | ASCII `SCB3` |
| 4 | 1 | Version `3` |
| 5 | 1 | Anchor zoom `10` |
| 6 | 1 | Requested tile zoom (11–14) |
| 7 | 1 | Reserved `0` |
| 8 | 4 | Anchor x10 |
| 12 | 4 | Anchor y10 |
| 16 | 4 | Stage count |

**Stage list** (fixed by requested zoom, never by anything else)

| Requested zoom | Stages, in order (zoom, part) |
| --- | --- |
| 11, 12, 13 | (z, 0 full) |
| 14 | (13, 0 full), (14, 1 structure), (14, 2 labels) |

Part codes: `0` = all layers, `1` = structure = every layer whose name is not
in the label set, `2` = labels = only layers in the label set. The label set is
the constant `{"housenumber", "poi"}`. Changing it requires a new codec
identifier (2.6) because cached bytes change.

**Frame prefix, 40 bytes, followed immediately by the payload**

| Offset | Bytes | Value |
| --- | --- | --- |
| 0 | 4 | Payload length |
| 4 | 1 | Stage zoom |
| 5 | 1 | Part (0, 1, 2) |
| 6 | 2 | Reserved `0` |
| 8 | 32 | SHA-256 of the payload |

There is no outer gzip. The payload is written as-is.

**Payload** = an SCB1 body with flags byte `0x01`:

| Offset | Bytes | Value |
| --- | --- | --- |
| 0 | 4 | ASCII `SCB1` |
| 4 | 1 | `1` |
| 5 | 1 | `10` |
| 6 | 1 | Stage zoom |
| 7 | 1 | Flags `0x01` = every non-empty entry is one complete gzip member |
| 8 | 4 | Anchor x10 |
| 12 | 4 | Anchor y10 |
| 16 | 4 | Entry count (4, 16, 64, 256) |
| 20… | | entries: 4-byte length then bytes; `0xFFFFFFFF` = empty tile, no bytes |

Entry rules: a non-empty entry starts with bytes `1f 8b 08` and inflates to at
most 16 MiB of MVT. An entry whose part would contain zero layers is written as
the empty sentinel. A tile that is empty upstream is empty in every part.

Bounds: payload ≤ 64 MiB (`scb1.MaxDecompressedBytes` reused as the payload
bound); stream ≤ `20 + 3 * (40 + 64 MiB)`.

Response headers: `Content-Type: application/vnd.streetcryptid.tile-stream3`,
and otherwise identical to v2 (`streamHeaders`: `Cache-Control: public,
max-age=86400, no-transform`, strong ETag, `Accept-Ranges: bytes`, CORS,
`X-Accel-Buffering: no`). Same Range / If-Range / 416 behaviour as v2 via the
existing `serveStreamContent`.

Why an MVT concatenation works: a Mapbox Vector Tile is a protobuf message
whose only field is `repeated Layer layers = 3`. Concatenating the serialized
structure part and labels part yields a valid tile containing both layer sets.
Both the JS decoder (`@mapbox/vector-tile`) and the Rust decoder read repeated
fields sequentially. Layer names never collide because the two sets are
disjoint.

### 2.2 PMTiles reader: stored bytes without inflating

`internal/pmtiles/reader.go`. Today `GetTileBytes` calls `r.read(..., r.tileCompression, ...)`
which inflates. Add:

```go
// GetTileStored returns the tile bytes exactly as stored in the archive and
// whether they are a gzip member (tile compression 2). (nil, false, nil) is an
// empty tile. Stored size is bounded by maxTileBytes.
func (r *Reader) GetTileStored(ctx context.Context, t privacy.TileCoord) ([]byte, bool, error)
```

Implement by factoring the directory walk out of `GetTileBytes` into a helper
that returns the `entry` for a tile id (or not-found), then reading the section
with compression byte `1` (none) regardless of `r.tileCompression`, and
returning `gzipped := r.tileCompression == 2`. Keep `GetTileBytes` for v1/v2.

Tests (`reader_test.go`): on the existing synthetic archives, `GetTileStored`
on a gzip-compressed archive returns bytes that `gzip.NewReader` inflates to the
same output as `GetTileBytes`; on an uncompressed archive returns the identical
bytes with `gzipped == false`; empty tile returns `(nil, false, nil)`.

### 2.3 New package `internal/mvt`: layer split

File `internal/mvt/split.go`, no third-party dependencies, hand-rolled
protobuf wire walking (varint tags, wire types 0/1/2/5). Functions:

```go
// LabelLayers is the fixed set that goes into part 2. Changing it changes
// representation bytes: bump scb3.CodecVersion.
var LabelLayers = map[string]bool{"housenumber": true, "poi": true}

// Split copies the top-level fields of an MVT tile into two tiles. Field 3
// (Layer) goes to labels when its name (Layer field 1) is in LabelLayers,
// else to structure. Any other top-level field is copied to structure only.
// A result with no layers is returned as nil. Malformed input is an error.
func Split(tile []byte) (structure, labels []byte, err error)

// LayerNames lists layer names in order; used by tests and bundle-stat parity.
func LayerNames(tile []byte) ([]string, error)
```

Rules the implementation must follow:

- Walk only the top level of the tile and, inside a layer, only far enough to
  find field 1 (name). Never re-encode a layer; copy its original bytes
  including its tag and length prefix. Output bytes must be byte-identical
  slices of the input in the original order.
- Reject: a wire type of 3, 4, 6 or 7; a truncated varint or length; a layer
  with no field 1. On any error return `err`, never a partial result.
- Bound the input at 16 MiB (`ErrTooLarge`).

Tests (`split_test.go`): build tiny tiles by hand (two layers `building`
and `poi` with one dummy feature each) and assert the split outputs
re-concatenate to the input, each output has the expected `LayerNames`, a tile
with only `poi` gives `structure == nil`, a tile with no label layers gives
`labels == nil`, and malformed inputs error. Add one golden test using
`testdata/` tiles from the empty fixture archive (empty tiles split to nil/nil).

### 2.4 `internal/scb1`: gzip-entry flag

- Add `const FlagGzipEntries = 0x01`.
- Add `EncodeFlags(req privacy.BundleRequest, flags byte, entries []Entry) ([]byte, error)`;
  make `Encode` call it with `0`. Flags go in byte 7.
- Add `ValidateFlags(req, raw []byte, flags byte) error`; make `Validate` call
  it with `0`. When `flags & FlagGzipEntries != 0`, additionally check that
  every non-empty entry has length ≥ 18 and starts with `1f 8b 08`. Do not
  inflate during validation.
- Golden fixture `testdata/scb1-z11.golden` is for flags `0` and must not
  change.

### 2.5 New package `internal/scb3`

Mirror `internal/scb2/scb2.go`:

```go
const (
    MediaType    = "application/vnd.streetcryptid.tile-stream3"
    CodecVersion = "v3-scb1gz-go1.26-r1"
    HeaderBytes  = 20
    FrameBytes   = 40
    MaxPayloadBytes = scb1.MaxDecompressedBytes
    MaxStreamBytes  = 3*(MaxPayloadBytes+FrameBytes) + HeaderBytes
)

type Part uint8
const ( PartFull Part = 0; PartStructure Part = 1; PartLabels Part = 2 )

type Stage struct { Req privacy.BundleRequest; Part Part }

func Stages(req privacy.BundleRequest) []Stage     // table in 2.1
func Header(req privacy.BundleRequest) []byte
func Frame(stage Stage, payload []byte) ([]byte, error) // validates via scb1.ValidateFlags(..., FlagGzipEntries), bounds, then prefixes
```

Golden fixture: `testdata/scb3-z11-empty.scb3` and `.json` for 10/164/357/11
(four empty sentinels), generated by a test guarded by
`UPDATE_SCB3_FIXTURE=1`, exactly like `scb2_test.go` does. Also add a z14
golden for the same empty anchor: three stages, all sentinels. The app tests
will import these files.

### 2.6 `internal/httpapi`: the v3 handler

Add to `Handler()`:

```go
mux.HandleFunc("GET /planet/bundle/v3/{x10}/{y10}/{tileZoom}", s.handleBundleV3)
mux.HandleFunc("OPTIONS /planet/bundle/v3/{x10}/{y10}/{tileZoom}", s.handleBundleOptions)
```

New source interface, alongside `TileSource`:

```go
// StoredTileSource returns tiles as stored plus whether they are gzip members.
type StoredTileSource interface {
    GetTileStored(context.Context, privacy.TileCoord) ([]byte, bool, error)
}
```

`*pmtiles.Reader` and `*martin.Client` both satisfy it after 2.2 and
Workstream 1. In `New`, if `cfg.BundleSource` implements it, keep it as
`s.stored`; otherwise v3 returns 501 (only possible in a misconfigured test).

**Keys.** Stage key:
`fmt.Sprintf("%s:%s:%s:10:%d:%d:%d:%d:stage", scb3.CodecVersion, url.PathEscape(DatasetVersion), DatasetDigest, x, y, zoom, part)`.
Stream key: same without `:stage` and without the part, plus the requested
zoom. ETag = quoted stream key, like v2.

**Building a full stage (part 0)** — `buildStoredStage(ctx, req)`: reuse the
worker-pool shape of `buildBundle` but call `s.stored.GetTileStored`. For each
tile: empty → `Entry{nil}`; `gzipped` → `Entry{bytes}` unchanged; not gzipped →
gzip it with `compress/gzip` default level and a zero header (Go's
`gzip.NewWriter` writes no timestamp unless `ModTime` is set). Encode with
`scb1.EncodeFlags(req, scb1.FlagGzipEntries, entries)`. Size bound: sum of
entry lengths + header must stay ≤ `MaxPayloadBytes`, same check as today.

**Building the z14 parts (1 and 2)** — `buildSplitStages(ctx, req14)`: one
pass over the 256 tiles that, per tile: `GetTileStored` → inflate if gzipped
(bounded 16 MiB; reuse the reader's limit style) → `mvt.Split` → gzip each
non-nil part → two entries. Return two encoded payloads. Cache both under their
stage keys with `s.cache.Put`. Serve either from `stage()` by key; if either
key misses, build both. A rare double build when two requests race on
different parts is acceptable; `cache.Do` dedups per key and `Put` is
idempotent.

Entry gzip for recompressed parts must be deterministic for a given input:
same Go version, `gzip.DefaultCompression`, no header fields. This is why
`CodecVersion` carries the Go version string, as v2's does.

**Handler flow.** Copy `handleBundleV2` and change: key/ETag/media type;
`scb3.Header`, `scb3.Stages`, `scb3.Frame`; the per-stage builder picks
`buildStoredStage` for part 0 and the split builder for parts 1 and 2; the
progressive flush after each frame stays exactly as in v2 (flush header plus
first frame, then each later frame). Keep the `progressive` condition, the
5-second subscriber write deadline, the abort-on-failure `panic(http.ErrAbortHandler)`
and the "only a complete stream enters the cache" rule. Keep
`admitted` and `BUNDLE_MAX_BUILDS` semantics: one v3 request holds one build
slot for the whole stream.

Metrics: reuse the existing bundle counters; add
`mapapi_bundle_v3_total` so adoption is visible.

### 2.7 Fixture server and conformance

- `cmd/fixture-server/main.go` already uses `*pmtiles.Reader` as
  `BundleSource`; nothing to wire. Confirm `GET /planet/bundle/v3/164/357/11`
  returns exactly `testdata/scb3-z11-empty.scb3` and that the z14 request
  returns the three-stage empty golden.
- `scripts/check-app-conformance.ts`: after Workstream 3 lands in the app, add
  the v3 checks (golden decode under fragmentation, live bytes, resume 206/416,
  three stages for z14, structure/labels delivered in order). Until then,
  leave the v2 checks as they are so the script keeps passing.

### 2.8 Tests to add (`internal/httpapi/stream_test.go` or a new `v3_test.go`)

Follow the v2 tests one-for-one; they are the spec of the streaming behaviour.

- `TestV3GoldenEmpty`: z11 and z14 bytes equal the goldens.
- `TestV3EntriesAreStoredGzipMembers`: with a gzip-compressed synthetic
  archive, every non-empty entry equals the archive's stored bytes exactly;
  with an uncompressed archive, every entry inflates to the stored bytes.
- `TestV3Z14SplitCoversEveryLayer`: for a synthetic archive whose tiles carry
  layers `building`, `poi`, `housenumber`, `transportation`: inflate structure
  and labels entries, `mvt.LayerNames` of structure is the non-label set, of
  labels is the label set, and concatenating them re-parses to the original
  layer list. Entry counts are 256 in both parts; empty tiles are sentinel in
  both.
- `TestV3OverviewFlushedBeforeDetail`: port of
  `TestOverviewFlushedBeforeDetailAndDisconnectCaches`; assert the client sees
  header + z13 frame before the z14 builder is allowed to proceed, and the
  structure frame before labels.
- `TestV3RangesAndConditions`, `TestV3FullySavedTransfer416`,
  `TestV3PrivacyAndCORS`, `TestV3CacheSurvivesServerRestart`: ports.
- `TestV3RejectsQueryAndBody`: shares `bundleRequest`, so a one-line port.
- `TestV2UnchangedByV3`: v2 golden bytes and ETag unchanged after this work.

### 2.9 README

Add a "Progressive SCB3" section mirroring the SCB2 one with the tables from
2.1, the stage list, the label set, and the statement that v1/v2 remain. Add
`scripts/bundle-stat.py` to the Layout list.

### 2.10 Acceptance (after deploy)

```bash
curl -sS https://martin.junephilip.com/planet/bundle/v3/164/357/14 -o /tmp/s.scb3
python3 -I scripts/bundle-stat.py /tmp/s.scb3
```

Expected for Seattle: stage z13 full ≈ 2.4 MB; stage z14 structure ≈ 9 MB;
stage z14 labels ≈ 5–6 MB; total ≈ 17 MB (slightly above v2 because per-tile
gzip loses nothing but gains nothing). The win is that the app can draw full
z14 streets and buildings after ≈ 11.4 MB instead of 16.7 MB, and never
inflates the whole stage in JavaScript. `curl -sI` on the same URL must show
the v3 media type, a strong ETag and `Accept-Ranges: bytes`; a
`Range: bytes=100-` request must return 206.

---

## Workstream 3: the app adopts SCB3

All paths below are under `src/features/map/` in the app repo unless stated.

### 3.1 Decoders accept gzip-compressed MVT, and several parts

Change the decoder seam in `tiles/decode-source.ts`:

```ts
export type TileDecoder = (
  parts: readonly Uint8Array[],   // was: bytes: Uint8Array
  tile: TileCoord
) => Promise<PackedGeometry> | PackedGeometry;
```

Each part is either raw MVT or one complete gzip member (sniff: first two
bytes `0x1f 0x8b`; a raw MVT can never start with `0x1f` because that would be
field 3 with wire type 7, which is invalid). Decoded output is the MVT parse of
the **concatenation of the inflated parts**.

- **JS decoder** (`jsTileDecoder`): for each part, if gzip → `fflate.gunzipSync(part)`,
  else use as is; concatenate into one `Uint8Array`; then `decodeMvtTile` as
  today. Reject any inflated part over 16 MiB. **Do not concatenate gzip
  members before inflating**: `fflate.gunzipSync` only reads the first member
  and sizes its output from the last member's trailer (verified, it returns a
  truncated result silently).
- **Native decoder** (`tiles/native-tile-decoder.ts`): concatenate the parts
  into one buffer and call the existing `decodeMvtTile(bytes, z, x, y)`. The
  FFI signature does not change, so no `just bindgen-ios` / Kotlin regeneration.
- **Rust** (`modules/iroh-location/rust/src/mvt.rs`, `decode_tile_into`):
  if the input starts with `1f 8b`, inflate it with
  `flate2::read::MultiGzDecoder` (handles concatenated members) wrapped in
  `.take(16 * 1024 * 1024 + 1)`, error if over the bound, then decode as
  before. Add to `Cargo.toml`:
  `flate2 = { version = "1", default-features = false, features = ["rust_backend"] }`
  (pure Rust backend so iOS, Android and the wasm target all build without a C
  toolchain). Add a Rust test: gzip two tiny tiles, concatenate, decode, assert
  layers from both are present. Run `just test-rust` and `just check-bindings`.
- `DecodingGeometrySource.getTile` passes `[raw]` for a single buffer. The
  coarse path (`MartinByteSource`) returns raw MVT because `fetch` inflates
  `Content-Encoding` itself; nothing changes there.

Tests: `__tests__/decode-source.test.ts` gets cases for raw, single gzip
member, and two gzip members whose layers must both appear.

### 3.2 `tiles/tile-bundle.ts`: accept flags `0x01`

- Export `TILE_BUNDLE_FLAG_GZIP_ENTRIES = 0x01`.
- `decodeTileBundle` accepts flags `0` or `0x01` (reject anything else). When
  the flag is set, each non-empty entry must be ≥ 18 bytes and start with
  `1f 8b 08`; otherwise throw. Entry bytes are returned as-is (compressed).
- Entry type stays `{ tile, bytes: Uint8Array | null }`.

### 3.3 `tiles/bundle-stream.ts`: SCB3 decoder

Add a `format: 2 | 3` constructor option to `TileStreamDecoder` (default 3)
and a stage descriptor type:

```ts
export type StagePart = 'full' | 'structure' | 'labels';
export interface StreamStage { readonly tileZoom: number; readonly part: StagePart }
export type StageListener = (stage: StreamStage, request: TileBundleRequest, entries: readonly TileBundleEntry[]) => Promise<void>;
```

Format 3 behaviour, matching 2.1 exactly:

- header: magic `SCB3`, version 3, anchor 10, tileZoom, flags 0, x, y, stage
  count equals the fixed list length (3 for z14, else 1).
- frame: `length` (u32), `zoom` (u8), `part` (u8), reserved u16 == 0, 32-byte
  hash. Validate `zoom`/`part` equal the expected next stage in the fixed
  list; any deviation throws `InvalidTileStream`.
- payload: `await this.hash(payload)` must equal the frame hash; then
  `decodeTileBundle(payload, { ...request, tileZoom: zoom })` and require flags
  `0x01`.
- Bounds: `STREAM3_MAX_BYTES = 20 + 3 * (40 + TILE_BUNDLE_MAX_BYTES)`.

Format 2 keeps today's code path unchanged (it is the fallback).

Tests (`__tests__/bundle-stream.test.ts`): copy the fixture
`testdata/scb3-z11-empty.scb3` and the z14 empty golden from the server repo
into `tiles/__fixtures__/`; decode under fragment sizes 1, 7, 20, 40, whole;
stage order `[13 full, 14 structure, 14 labels]` for z14; corrupted hash,
wrong part order, wrong flags each throw.

### 3.4 `tiles/streaming-bundle-source.ts`: v3 URL with v2 fallback

- Request `${sourceUrl}/bundle/v3/${x}/${y}/${z}` with
  `Accept: application/vnd.streetcryptid.tile-stream3`.
- On 404 or 405 for v3: set a `v2Only` flag on the instance and retry the same
  request through the existing v2 code path (which itself falls back to v1 on
  404). Log once: `[map] tile server has no v3 stream endpoint; using v2`.
- `STREAM_MAX_BYTES` checks use the v3 bound when on v3.
- Resume journal keys are the URL, so v3 and v2 journals never mix.
- The `onStage` callback now receives the `StreamStage` first (3.3). Keep the
  `TileBundleSource.getBundle` return type as it is: it resolves with the
  entries of the **last** stage. Every stage, the last one included, is
  delivered to `onStage` before `getBundle` resolves, so `bundle-fetch.ts`
  collects the z14 structure entries from `onStage` and receives the labels
  entries as the resolved value. The v2 code path and the v1 legacy source
  report their single z14 stage as `{ tileZoom: 14, part: 'full' }`.

### 3.5 `tiles/bundle-fetch.ts`: storage rule, completeness, previews

**Storage.** Keep the SQLite schema. Use the `source` column as the namespace:

- Full stages (z11–13 and the z13 overview) and the **z14 structure** part go
  under `sourceId`.
- The **z14 labels** part goes under `${sourceId}#labels`.
- Bump the planet `sourceId` in `config.ts` from `planet-z10-v1` to
  `planet-z10-v3` because row bytes are now compressed and z14 rows are split.
  On first open, run `DELETE FROM tiles WHERE source = 'planet-z10-v1'` once
  (add a `deleteSource(sourceId)` to `TileByteStore`, implemented in
  `sqlite-tile-store.ts` and the in-memory fake).

**Completeness rule for a z14 tile.** `getTileBytes(tile)` for `z === 14`
reads both rows. The tile counts as stored only if **both** rows exist and the
structure row is fresh. It then returns `[structure, labels]` as parts (each
may be `null` for an empty sentinel; drop nulls; an all-null tile is
`null`). If either row is missing, treat as a miss and go through
`fetchBundle`, which resumes from the journal. This keeps today's guarantee
that a decoded, memory-cached z14 tile is complete, so no cache invalidation
is needed when labels arrive later.

Because `TileByteSource.getTileBytes` now returns parts, change its return type
to `Promise<readonly Uint8Array[] | null>` and update `DecodingGeometrySource`
and the tests. For z ≤ 13 it returns a one-element array.

**Writing stages as they land**, in the `onStage` callback:

| Stage | Action |
| --- | --- |
| (13, full) on a z14 request | `putMany(sourceId, entries)`; emit preview stage `{ tileZoom: 13, part: 'full' }` |
| (14, structure) | `putMany(sourceId, entries)`; emit preview stage `{ tileZoom: 14, part: 'structure' }` |
| (14, labels) | `putMany(sourceId#labels, entries)`; resolve the bundle promise with the merged per-tile parts |
| (14, full) from the v2 or v1 fallback | `putMany(sourceId, entries)` **and** `putMany(sourceId#labels, entries with bytes null)` so the completeness rule holds; the structure row then carries the whole tile (raw or one gzip member, the decoder sniffs) |
| (z, full), z ≤ 13 request | `putMany(sourceId, entries)`; resolve |

**Previews.** Replace the one-shot `getPreviewTiles(tiles)` with a callback
form so the engine can draw twice:

```ts
getPreviewTiles?(
  tiles: readonly TileCoord[],
  onStage: (stage: StreamStage, entries: readonly TileBundleEntry[]) => Promise<void>
): Promise<void>;   // resolves when no further preview stages will come
```

Filter each stage's entries to the ones the caller's `tiles` need (today's
`wanted` logic: z13 parents for the 13-full stage; the z14 tiles themselves
for 14-structure). Preserve today's rule that preview is only attempted for
all-z14 requests, and that a bundle whose preview promise is gone yields
nothing.

### 3.6 Engine: two previews

`engine/map-engine.ts` around the `getPreview` call (line ~341): pass a
callback; for each stage build a preview with
`buildFromGeometry(request, { ...spec, tileZoom: stage.tileZoom }, geometry, …)`
and call `onPreview(preview)`; keep `this.last = preview`. Keep the existing
catch-and-warn. Order is guaranteed by the stream, so no reordering logic.
`tiles/geometry-source.ts` and `tiles/tile-cache.ts` forward the new
signature; `decode-source.ts` decodes each stage's entries with the decoder
(each entry becomes a one-part array) and merges as today.

### 3.7 Tests to update or add in the app

- `__tests__/bundle-fetch.test.ts`: two-row storage; a z14 tile with a missing
  labels row is a miss; both rows present returns two parts; preview callback
  fires 13-full then 14-structure; old source rows deleted on open.
- `__tests__/streaming-bundle-source.test.ts`: v3 happy path, v3 404 → v2
  fallback once, resume with Range on the v3 URL, 416 complete case.
- `__tests__/sqlite-tile-store.test.ts`: `deleteSource`.
- `__tests__/decode-source.test.ts`, `__tests__/tile-bundle.test.ts`,
  `__tests__/bundle-stream.test.ts` as described above.
- Rust: `cargo test` in `modules/iroh-location/rust` for the gzip path.
- Finally run the server repo's `scripts/check-app-conformance.ts` against the
  fixture server with the v3 checks added (2.7).

### 3.8 Rollout order

1. Server Workstream 1 → deploy → confirm metrics and curl acceptance.
2. Server Workstream 2 → deploy. v3 is live but unused; v2 unchanged.
3. App Workstream 3 → release. New builds use v3; old builds keep using v2.
4. After one release cycle with no v2 traffic in `mapapi_bundle_*` metrics,
   a separate change may remove v1 and v2. Not part of this plan.

---

## Workstream 4 (optional, ops only): edge configuration

Production fronts the Go server with Caddy (`via: 1.1 Caddy` in responses).
Check its config, not this repo:

- `encode` must not apply to `/planet/bundle/*` (responses carry
  `no-transform`; Caddy honours it by default, verify).
- Streaming: Caddy must not buffer `/planet/bundle/*` responses; the server
  sends `X-Accel-Buffering: no`, which Caddy ignores, so confirm `flush_interval -1`
  on the reverse_proxy for that path.
- Upstream read timeout ≥ `BUNDLE_REQUEST_TIMEOUT` (60 s).
- A CDN (for example Cloudflare) in front is safe for privacy: it sees only the
  same z10 anchor the origin sees. It will buffer cold-miss streams, so early
  stages arrive late on first touch only. Optional; decide after measuring
  real-user TTFB.

---

## Explicitly out of scope, with reasons

- **Brotli / zstd**: see 0.2 (2). Whole-stage codecs conflict with per-tile
  entries; per-tile gains measured at 3–4%.
- **Trimming MVT attributes in the bake**: measured -5% gzip at z14. Real but
  small, and it couples the bake to the app's renderer. Revisit after SCB3.
- **Dropping layers in the bake**: the split delivers the same first-paint win
  without losing data for later zooms.
- **Pre-baking every bundle offline**: strong long-term option (zero-CPU
  serving, CDN-friendly) but a separate design; the cache already handles warm
  anchors in under a second.
- **Storing coarse (z0–10) tiles compressed in SQLite**: `fetch` inflates
  them before the app sees bytes; recompressing in JS is not worth it.

## Definition of done

- Workstream 1: tests in 1 pass; production curl acceptance in 1 holds;
  `mapapi_coarse_upstream_gzip_total` climbs.
- Workstream 2: `go test -race ./...` green; goldens committed; production
  `bundle-stat.py` output matches 2.10; v2 bytes and ETags unchanged.
- Workstream 3: `just check` and `just test-rust` green; conformance script
  green against the fixture server; on a device with a cold cache, the map
  shows z14 streets and buildings before POI names appear, and the JS thread
  never inflates a whole stage (confirm via the `tileDecodeMs` /
  `bundleParseMs` perf metrics dropping).

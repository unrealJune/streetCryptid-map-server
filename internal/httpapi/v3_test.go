package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/junephilip/streetcryptid-map-server/internal/cache"
	"github.com/junephilip/streetcryptid-map-server/internal/mvt"
	"github.com/junephilip/streetcryptid-map-server/internal/pmtiles"
	"github.com/junephilip/streetcryptid-map-server/internal/privacy"
	"github.com/junephilip/streetcryptid-map-server/internal/scb1"
	"github.com/junephilip/streetcryptid-map-server/internal/scb2"
	"github.com/junephilip/streetcryptid-map-server/internal/scb3"
)

type storedFunc func(context.Context, privacy.TileCoord) ([]byte, bool, error)

func (f storedFunc) GetTileStored(ctx context.Context, t privacy.TileCoord) ([]byte, bool, error) {
	return f(ctx, t)
}

var emptyStored = storedFunc(func(context.Context, privacy.TileCoord) ([]byte, bool, error) { return nil, false, nil })

func mvtLayer(name string) []byte {
	var l []byte
	l = append(l, 0x0a, byte(len(name)))
	l = append(l, name...)
	l = append(l, 0x12, 0x02, 0x18, 0x01) // one POINT feature
	return append([]byte{0x1a, byte(len(l))}, l...)
}

func mvtTile(names ...string) []byte {
	var t []byte
	for _, n := range names {
		t = append(t, mvtLayer(n)...)
	}
	return t
}

// layeredSource serves gzip-stored tiles with every layer kind, leaving tiles
// with x%5 == 0 empty, and counts reads.
func layeredSource(calls *atomic.Int64) storedFunc {
	return func(_ context.Context, t privacy.TileCoord) ([]byte, bool, error) {
		calls.Add(1)
		if t.X%5 == 0 {
			return nil, false, nil
		}
		gz, err := gzipBytes(mvtTile("building", "poi", "housenumber", "transportation"))
		return gz, true, err
	}
}

type v3Stage struct {
	stage   scb3.Stage
	entries [][]byte // nil entry = empty sentinel
}

// checkStream3 strictly parses an SCB3 stream against the fixed stage list.
func checkStream3(t *testing.T, req privacy.BundleRequest, stream []byte) []v3Stage {
	t.Helper()
	if len(stream) < scb3.HeaderBytes || !bytes.Equal(stream[:scb3.HeaderBytes], scb3.Header(req)) {
		t.Fatalf("invalid header %x", stream[:min(len(stream), 20)])
	}
	off := scb3.HeaderBytes
	var out []v3Stage
	for _, stage := range scb3.Stages(req) {
		if len(stream)-off < scb3.FrameBytes {
			t.Fatal("missing frame")
		}
		n := int(binary.BigEndian.Uint32(stream[off:]))
		if int(stream[off+4]) != stage.Req.TileZoom || scb3.Part(stream[off+5]) != stage.Part || stream[off+6] != 0 || stream[off+7] != 0 {
			t.Fatalf("frame %x, want %v", stream[off:off+8], stage)
		}
		if n > len(stream)-off-scb3.FrameBytes {
			t.Fatal("truncated payload")
		}
		payload := stream[off+scb3.FrameBytes : off+scb3.FrameBytes+n]
		hash := sha256.Sum256(payload)
		if !bytes.Equal(hash[:], stream[off+8:off+40]) {
			t.Fatal("bad payload hash")
		}
		if err := scb1.ValidateFlags(stage.Req, payload, scb1.FlagGzipEntries); err != nil {
			t.Fatal(err)
		}
		out = append(out, v3Stage{stage, scb1Entries(payload)})
		off += scb3.FrameBytes + n
	}
	if off != len(stream) {
		t.Fatal("trailing bytes")
	}
	return out
}

func scb1Entries(payload []byte) [][]byte {
	count := int(binary.BigEndian.Uint32(payload[16:]))
	entries := make([][]byte, count)
	off := scb1.HeaderBytes
	for i := range entries {
		n := binary.BigEndian.Uint32(payload[off:])
		off += 4
		if n == scb1.EmptyTileLength {
			continue
		}
		entries[i] = payload[off : off+int(n)]
		off += int(n)
	}
	return entries
}

func newV3Server(t *testing.T, f *fakeMartin, src StoredTileSource) *Server {
	t.Helper()
	s := newTestServer(t, f)
	s.stored = src
	return s
}

func TestV3GoldenEmpty(t *testing.T) {
	f := newFakeMartin()
	defer f.close()
	s := newV3Server(t, f, emptyStored)
	for _, z := range []int{11, 14} {
		golden, err := os.ReadFile(filepath.Join("..", "..", "testdata", fmt.Sprintf("scb3-z%d-empty.scb3", z)))
		if err != nil {
			t.Fatal(err)
		}
		w := streamRequest(s.Handler(), fmt.Sprintf("/planet/bundle/v3/164/357/%d", z), "", "")
		if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), golden) {
			t.Fatalf("z%d: %d, golden mismatch", z, w.Code)
		}
		h := w.Header()
		if h.Get("Content-Type") != scb3.MediaType || h.Get("Content-Encoding") != "" ||
			h.Get("Cache-Control") != "public, max-age=86400, no-transform" || h.Get("Accept-Ranges") != "bytes" ||
			h.Get("Access-Control-Allow-Origin") != "*" || h.Get("X-Accel-Buffering") != "no" ||
			!strings.Contains(h.Get("ETag"), scb3.CodecVersion) {
			t.Fatalf("headers %v", h)
		}
	}
	if atomic.LoadInt64(&f.calls) != 0 {
		t.Fatal("v3 fanned out to Martin")
	}
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(rr.Body.String(), "mapapi_bundle_v3_total 2") {
		t.Fatalf("metrics missing v3 counter:\n%s", rr.Body.String())
	}
}

// tinyArchive is a PMTiles file whose one RLE run covers every z11 tile.
func tinyArchive(t *testing.T, stored []byte, tileCompression byte) *pmtiles.Reader {
	t.Helper()
	first := ((uint64(1) << 22) - 1) / 3
	dir := binary.AppendUvarint(nil, 1)
	for _, v := range []uint64{first, 1 << 22, uint64(len(stored)), 1} {
		dir = binary.AppendUvarint(dir, v)
	}
	metadata := []byte(`{"vector_layers":[]}`)
	h := make([]byte, 127)
	copy(h, "PMTiles")
	h[7], h[97], h[98], h[99], h[101] = 3, 1, tileCompression, 1, 14
	offset := uint64(127)
	for i, b := range [][]byte{dir, metadata, nil, stored} {
		binary.LittleEndian.PutUint64(h[8+i*16:], offset)
		binary.LittleEndian.PutUint64(h[16+i*16:], uint64(len(b)))
		offset += uint64(len(b))
	}
	archive := append(append(append(h, dir...), metadata...), stored...)
	path := filepath.Join(t.TempDir(), "tiny.pmtiles")
	if err := os.WriteFile(path, archive, 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := pmtiles.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func TestV3EntriesAreStoredGzipMembers(t *testing.T) {
	raw := mvtTile("building", "poi")
	req := privacy.BundleRequest{AnchorX: 164, AnchorY: 357, TileZoom: 11}
	f := newFakeMartin()
	defer f.close()

	// Gzip archive: every entry is the archive's stored member, byte for byte.
	member, err := gzipBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	member[9] = 3 // a different OS byte proves the bytes are copied, not recompressed
	s := newV3Server(t, f, tinyArchive(t, member, 2))
	w := streamRequest(s.Handler(), "/planet/bundle/v3/164/357/11", "", "")
	for _, e := range checkStream3(t, req, w.Body.Bytes())[0].entries {
		if !bytes.Equal(e, member) {
			t.Fatalf("entry %x is not the stored member", e)
		}
	}

	// Uncompressed archive: every entry is a gzip member of the stored bytes.
	s = newV3Server(t, f, tinyArchive(t, raw, 1))
	w = streamRequest(s.Handler(), "/planet/bundle/v3/164/357/11", "", "")
	for _, e := range checkStream3(t, req, w.Body.Bytes())[0].entries {
		got, err := inflateStored(e, true)
		if err != nil || !bytes.Equal(got, raw) {
			t.Fatal("entry does not inflate to the stored tile", err)
		}
	}

	// A stored gzip member of an empty tile is the sentinel, as in v1/v2.
	empty, err := gzipBytes(nil)
	if err != nil {
		t.Fatal(err)
	}
	s = newV3Server(t, f, tinyArchive(t, empty, 2))
	w = streamRequest(s.Handler(), "/planet/bundle/v3/164/357/11", "", "")
	for _, e := range checkStream3(t, req, w.Body.Bytes())[0].entries {
		if e != nil {
			t.Fatal("empty stored tile not sent as sentinel")
		}
	}
	if atomic.LoadInt64(&f.calls) != 0 {
		t.Fatal("v3 fanned out to Martin")
	}
}

func TestV3Z14SplitCoversEveryLayer(t *testing.T) {
	f := newFakeMartin()
	defer f.close()
	var calls atomic.Int64
	s := newV3Server(t, f, layeredSource(&calls))
	req := privacy.BundleRequest{AnchorX: 164, AnchorY: 357, TileZoom: 14}
	w := streamRequest(s.Handler(), "/planet/bundle/v3/164/357/14", "", "")
	stages := checkStream3(t, req, w.Body.Bytes())
	if len(stages) != 3 || len(stages[0].entries) != 64 || len(stages[1].entries) != 256 || len(stages[2].entries) != 256 {
		t.Fatal("wrong stage shapes")
	}
	// The z13 overview is full: every layer in one entry.
	for i, coord := range stages[0].stage.Req.Descendants() {
		e := stages[0].entries[i]
		if (coord.X%5 == 0) != (e == nil) {
			t.Fatal("overview sentinel mismatch")
		}
	}
	for i, coord := range req.Descendants() {
		structure, labels := stages[1].entries[i], stages[2].entries[i]
		if coord.X%5 == 0 {
			if structure != nil || labels != nil {
				t.Fatal("empty tile not empty in every part")
			}
			continue
		}
		rs, err := inflateStored(structure, true)
		if err != nil {
			t.Fatal(err)
		}
		rl, err := inflateStored(labels, true)
		if err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			tile []byte
			want []string
		}{
			{rs, []string{"building", "transportation"}},
			{rl, []string{"poi", "housenumber"}},
			{append(bytes.Clone(rs), rl...), []string{"building", "transportation", "poi", "housenumber"}},
		} {
			got, err := mvt.LayerNames(tc.tile)
			if err != nil || !slices.Equal(got, tc.want) {
				t.Fatalf("layers %v %v, want %v", got, err, tc.want)
			}
		}
	}
	// 64 overview reads plus one pass of 256 for both parts.
	if calls.Load() != 64+256 {
		t.Fatalf("split read the archive %d times", calls.Load())
	}
	// Cached: no further reads, identical bytes.
	again := streamRequest(s.Handler(), "/planet/bundle/v3/164/357/14", "", "")
	if !bytes.Equal(again.Body.Bytes(), w.Body.Bytes()) || calls.Load() != 64+256 {
		t.Fatal("cache reuse or deterministic bytes failed")
	}
}

func TestV3SplitRebuildsDeterministicallyWithoutCache(t *testing.T) {
	f := newFakeMartin()
	defer f.close()
	var calls atomic.Int64
	s := newV3Server(t, f, layeredSource(&calls))
	s.cache, _ = cache.New("", 0)
	first := streamRequest(s.Handler(), "/planet/bundle/v3/164/357/14", "", "")
	checkStream3(t, privacy.BundleRequest{AnchorX: 164, AnchorY: 357, TileZoom: 14}, first.Body.Bytes())
	if calls.Load() != 64+256 {
		t.Fatalf("labels rebuilt the split without a cache: %d reads", calls.Load())
	}
	second := streamRequest(s.Handler(), "/planet/bundle/v3/164/357/14", "", "")
	if !bytes.Equal(first.Body.Bytes(), second.Body.Bytes()) {
		t.Fatal("rebuild is not byte-identical")
	}
}

func TestV3SplitRejectsMalformedTile(t *testing.T) {
	f := newFakeMartin()
	defer f.close()
	s := newV3Server(t, f, storedFunc(func(_ context.Context, c privacy.TileCoord) ([]byte, bool, error) {
		if c.Z == 14 {
			return []byte{0x1b}, false, nil // field 3, wire type 3
		}
		return nil, false, nil
	}))
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/planet/bundle/v3/164/357/14")
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(resp.Body)
	resp.Body.Close()
	if err == nil {
		t.Fatal("stream with malformed detail ended successfully")
	}
	if _, ok := s.cache.Get(s.v3StageKey(scb3.Stage{Req: privacy.BundleRequest{AnchorX: 164, AnchorY: 357, TileZoom: 14}, Part: scb3.PartStructure})); ok {
		t.Fatal("failed stage cached")
	}
}

// chunkRecorder records what a client has received at each flush.
type chunkRecorder struct {
	*httptest.ResponseRecorder
	mu      sync.Mutex
	pending []byte
	chunks  [][]byte
	onFlush func(n int)
}

func (w *chunkRecorder) Write(b []byte) (int, error) {
	w.mu.Lock()
	w.pending = append(w.pending, b...)
	w.mu.Unlock()
	return w.ResponseRecorder.Write(b)
}

func (w *chunkRecorder) Flush() {
	w.mu.Lock()
	w.chunks = append(w.chunks, w.pending)
	w.pending = nil
	n := len(w.chunks)
	w.mu.Unlock()
	w.ResponseRecorder.Flush()
	if w.onFlush != nil {
		w.onFlush(n)
	}
}

func (w *chunkRecorder) SetWriteDeadline(time.Time) error { return nil }

func TestV3OverviewFlushedBeforeDetail(t *testing.T) {
	f := newFakeMartin()
	defer f.close()
	var flushed atomic.Int64
	s := newV3Server(t, f, storedFunc(func(_ context.Context, c privacy.TileCoord) ([]byte, bool, error) {
		if c.Z == 14 && flushed.Load() < 1 {
			return nil, false, errors.New("detail read before overview flush")
		}
		gz, err := gzipBytes(mvtTile("building", "poi"))
		return gz, true, err
	}))
	w := &chunkRecorder{ResponseRecorder: httptest.NewRecorder(), onFlush: func(n int) { flushed.Store(int64(n)) }}
	s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/planet/bundle/v3/164/357/14", nil))
	req := privacy.BundleRequest{AnchorX: 164, AnchorY: 357, TileZoom: 14}
	stages := checkStream3(t, req, w.Body.Bytes())
	if len(w.chunks) != 3 {
		t.Fatalf("%d flushes, want header+z13, structure, labels", len(w.chunks))
	}
	// Each flush ends exactly on a frame boundary, in stage order.
	off := scb3.HeaderBytes
	for i, chunk := range w.chunks {
		start := off
		if i == 0 {
			start = 0
		}
		n := int(binary.BigEndian.Uint32(w.Body.Bytes()[off:]))
		off += scb3.FrameBytes + n
		if !bytes.Equal(chunk, w.Body.Bytes()[start:off]) {
			t.Fatalf("flush %d is not stage %v", i, stages[i].stage)
		}
	}
}

func TestV3DisconnectedClientStillCaches(t *testing.T) {
	f := newFakeMartin()
	defer f.close()
	detailEntered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var calls atomic.Int64
	s := newV3Server(t, f, storedFunc(func(ctx context.Context, c privacy.TileCoord) ([]byte, bool, error) {
		calls.Add(1)
		if c.Z == 14 {
			once.Do(func() { close(detailEntered) })
			select {
			case <-release:
			case <-ctx.Done():
				return nil, false, ctx.Err()
			}
		}
		return nil, false, nil
	}))
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/planet/bundle/v3/164/357/14")
	if err != nil {
		t.Fatal(err)
	}
	head := make([]byte, scb3.HeaderBytes+scb3.FrameBytes)
	if _, err := io.ReadFull(resp.Body, head); err != nil {
		t.Fatal(err)
	}
	if head[scb3.HeaderBytes+4] != 13 || head[scb3.HeaderBytes+5] != 0 {
		t.Fatal("first frame is not the z13 overview")
	}
	select {
	case <-detailEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("detail never started")
	}
	resp.Body.Close()
	close(release)
	complete, err := http.Get(ts.URL + "/planet/bundle/v3/164/357/14")
	if err != nil {
		t.Fatal(err)
	}
	full, err := io.ReadAll(complete.Body)
	complete.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	checkStream3(t, privacy.BundleRequest{AnchorX: 164, AnchorY: 357, TileZoom: 14}, full)
	if calls.Load() != 64+256 {
		t.Fatalf("rebuilt after disconnect: %d reads", calls.Load())
	}
}

func TestV3RangesAndConditions(t *testing.T) {
	f := newFakeMartin()
	defer f.close()
	path := "/planet/bundle/v3/164/357/14"
	var calls atomic.Int64
	for _, cold := range []bool{false, true} {
		s := newV3Server(t, f, layeredSource(&calls))
		base := streamRequest(s.Handler(), path, "", "")
		body := bytes.Clone(base.Body.Bytes())
		etag := base.Header().Get("ETag")
		for _, tc := range []struct {
			span, ifRange string
			status        int
			start, end    int
		}{
			{"bytes=100-", etag, 206, 100, len(body)},
			{"bytes=20-59", etag, 206, 20, 60},
			{"bytes=-10", etag, 206, len(body) - 10, len(body)},
			{"bytes=20-59", `"old-dataset"`, 200, 0, len(body)},
			{"bytes=20-59", "W/" + etag, 200, 0, len(body)},
			{"bytes=0-1,3-4", etag, 200, 0, len(body)},
			{"bytes=3-2", etag, 416, 0, 0},
		} {
			if cold {
				s = newV3Server(t, f, layeredSource(&calls))
			}
			w := streamRequest(s.Handler(), path, tc.span, tc.ifRange)
			if w.Code != tc.status {
				t.Fatalf("cold=%v %s %s got %d", cold, tc.span, tc.ifRange, w.Code)
			}
			if tc.status == 416 {
				continue
			}
			if !bytes.Equal(w.Body.Bytes(), body[tc.start:tc.end]) {
				t.Fatal("wrong range bytes")
			}
			if tc.status == 206 && w.Header().Get("Content-Range") != fmt.Sprintf("bytes %d-%d/%d", tc.start, tc.end-1, len(body)) {
				t.Fatal(w.Header())
			}
			if w.Header().Get("Content-Encoding") != "" || w.Header().Get("Accept-Ranges") != "bytes" || w.Header().Get("Content-Type") != scb3.MediaType {
				t.Fatal("range representation transformed")
			}
		}
		for _, tag := range []string{etag, "W/" + etag, "*"} {
			r := httptest.NewRequest("GET", path, nil)
			r.Header.Set("If-None-Match", tag)
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, r)
			if w.Code != 304 || w.Body.Len() != 0 {
				t.Fatal("conditional GET failed", w.Code)
			}
		}
		s.cfg.DatasetDigest = "changed"
		w := streamRequest(s.Handler(), path, "bytes=20-59", etag)
		if w.Code != 200 || w.Header().Get("ETag") == etag {
			t.Fatal("changed dataset range not replaced")
		}
	}
}

func TestV3FullySavedTransfer416(t *testing.T) {
	f := newFakeMartin()
	defer f.close()
	s := newV3Server(t, f, emptyStored)
	path := "/planet/bundle/v3/164/357/11"
	base := streamRequest(s.Handler(), path, "", "")
	etag := base.Header().Get("ETag")
	n := base.Body.Len()
	w := streamRequest(s.Handler(), path, fmt.Sprintf("bytes=%d-", n), etag)
	if w.Code != 416 || w.Header().Get("Content-Range") != fmt.Sprintf("bytes */%d", n) ||
		w.Header().Get("ETag") != etag || w.Header().Get("Cache-Control") != "no-store, no-transform" {
		t.Fatal("complete transfer not recognisable", w.Code, w.Header())
	}
}

func TestV3PrivacyAndCORS(t *testing.T) {
	f := newFakeMartin()
	defer f.close()
	var calls atomic.Int64
	s := newV3Server(t, f, layeredSource(&calls))
	for _, path := range []string{
		"/planet/bundle/v3/164/357/10", "/planet/bundle/v3/164/357/15",
		"/planet/bundle/v3/1024/357/11", "/planet/bundle/v3/-1/357/11",
		"/planet/bundle/v3/164/357/11?child=1", "/planet/bundle/v3/164/357/11?",
		"/planet/bundle/v3/164/357/14?part=labels",
	} {
		w := streamRequest(s.Handler(), path, "", "")
		if w.Code != 400 {
			t.Fatalf("%s: %d", path, w.Code)
		}
	}
	r := httptest.NewRequest("GET", "/planet/bundle/v3/164/357/11", strings.NewReader("child"))
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 400 {
		t.Fatal("body accepted")
	}
	if calls.Load() != 0 || atomic.LoadInt64(&f.calls) != 0 {
		t.Fatal("invalid request reached source")
	}
	r = httptest.NewRequest("OPTIONS", "/planet/bundle/v3/164/357/11", nil)
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 204 || !strings.Contains(w.Header().Get("Access-Control-Allow-Headers"), "If-Range") {
		t.Fatal("CORS range preflight")
	}
	w = streamRequest(s.Handler(), "/planet/bundle/v3/164/357/11", "", "")
	if !strings.Contains(w.Header().Get("Access-Control-Expose-Headers"), "Content-Range") {
		t.Fatal("range headers not exposed")
	}
}

func TestV3WithoutStoredSourceIs501(t *testing.T) {
	f := newFakeMartin()
	defer f.close()
	s := newV3Server(t, f, nil)
	if w := streamRequest(s.Handler(), "/planet/bundle/v3/164/357/11", "", ""); w.Code != http.StatusNotImplemented {
		t.Fatal(w.Code)
	}
	// Validation still runs first.
	if w := streamRequest(s.Handler(), "/planet/bundle/v3/164/357/15", "", ""); w.Code != 400 {
		t.Fatal(w.Code)
	}
	// Both production sources are stored sources.
	if newTestServer(t, f).stored == nil {
		t.Fatal("Martin client not detected as a stored source")
	}
	var _ StoredTileSource = (*pmtiles.Reader)(nil)
}

func TestV3CorruptStageCannotBecomeStream(t *testing.T) {
	f := newFakeMartin()
	defer f.close()
	s := newV3Server(t, f, emptyStored)
	req := privacy.BundleRequest{AnchorX: 164, AnchorY: 357, TileZoom: 11}
	// A flags-0 body is valid SCB1 but not a v3 payload.
	raw, err := scb1.Encode(req, make([]scb1.Entry, 4))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.cache.Put(s.v3StageKey(scb3.Stage{Req: req}), raw); err != nil {
		t.Fatal(err)
	}
	w := streamRequest(s.Handler(), "/planet/bundle/v3/164/357/11", "", "")
	if w.Code != 502 || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("ETag") != "" {
		t.Fatal("corrupt stage accepted", w.Code)
	}
}

func TestV3CacheSurvivesServerRestart(t *testing.T) {
	f := newFakeMartin()
	defer f.close()
	dir := t.TempDir()
	c, err := cache.New(dir, 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	s := newV3Server(t, f, layeredSource(&calls))
	s.cache = c
	first := streamRequest(s.Handler(), "/planet/bundle/v3/164/357/14", "", "")
	c, err = cache.New(dir, 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	restarted := newV3Server(t, f, storedFunc(func(context.Context, privacy.TileCoord) ([]byte, bool, error) {
		return nil, false, errors.New("source offline")
	}))
	restarted.cache = c
	got := streamRequest(restarted.Handler(), "/planet/bundle/v3/164/357/14", "bytes=100-", first.Header().Get("ETag"))
	if got.Code != 206 || !bytes.Equal(got.Body.Bytes(), first.Body.Bytes()[100:]) {
		t.Fatal("restart did not restore resumable representation", got.Code)
	}
}

func TestV2UnchangedByV3(t *testing.T) {
	f := newFakeMartin()
	defer f.close()
	var calls atomic.Int64
	s := newV3Server(t, f, layeredSource(&calls))
	s.source = tileFunc(func(context.Context, privacy.TileCoord) ([]byte, error) { return nil, nil })
	streamRequest(s.Handler(), "/planet/bundle/v3/164/357/11", "", "")
	w := streamRequest(s.Handler(), "/planet/bundle/v2/164/357/11", "", "")
	golden, err := os.ReadFile(filepath.Join("..", "..", "testdata", "scb2-z11-empty.scb2"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(w.Body.Bytes(), golden) || w.Header().Get("Content-Type") != scb2.MediaType {
		t.Fatal("v2 bytes changed")
	}
	if want := `"v2-scb1-gzip-go1.26-r1:test-v1::10:164:357:11"`; w.Header().Get("ETag") != want {
		t.Fatalf("v2 ETag %s, want %s", w.Header().Get("ETag"), want)
	}
}

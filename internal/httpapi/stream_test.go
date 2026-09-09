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
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/junephilip/streetcryptid-map-server/internal/cache"
	"github.com/junephilip/streetcryptid-map-server/internal/pmtiles"
	"github.com/junephilip/streetcryptid-map-server/internal/privacy"
	"github.com/junephilip/streetcryptid-map-server/internal/scb1"
	"github.com/junephilip/streetcryptid-map-server/internal/scb2"
)

type tileFunc func(context.Context, privacy.TileCoord) ([]byte, error)

func (f tileFunc) GetTileBytes(ctx context.Context, t privacy.TileCoord) ([]byte, error) {
	return f(ctx, t)
}

func streamRequest(h http.Handler, path, span, ifRange string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", path, nil)
	if span != "" {
		r.Header.Set("Range", span)
	}
	if ifRange != "" {
		r.Header.Set("If-Range", ifRange)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func checkStream(t *testing.T, req privacy.BundleRequest, stream []byte) [][]byte {
	t.Helper()
	if len(stream) < 20 || !bytes.Equal(stream[:20], scb2.Header(req)) {
		t.Fatalf("invalid header %x", stream)
	}
	off := 20
	var rawStages [][]byte
	for _, stage := range scb2.Stages(req) {
		if len(stream)-off < 40 {
			t.Fatal("missing frame")
		}
		n := int(binary.BigEndian.Uint32(stream[off:]))
		rawN := int(binary.BigEndian.Uint32(stream[off+4:]))
		if n > scb2.MaxCompressedBytes || rawN > scb1.MaxDecompressedBytes || n > len(stream)-off-40 {
			t.Fatal("invalid lengths")
		}
		raw := gunzip(t, stream[off+40:off+40+n])
		hash := sha256.Sum256(raw)
		if rawN != len(raw) || !bytes.Equal(hash[:], stream[off+8:off+40]) {
			t.Fatal("bad raw hash/length")
		}
		if err := scb1.Validate(stage, raw); err != nil {
			t.Fatal(err)
		}
		rawStages = append(rawStages, raw)
		off += 40 + n
	}
	if off != len(stream) {
		t.Fatal("trailing bytes")
	}
	return rawStages
}

func TestV2FullSetsV1ParityAndReuse(t *testing.T) {
	f := newFakeMartin()
	defer f.close()
	s := newTestServer(t, f)
	h := s.Handler()
	for z := 11; z <= 14; z++ {
		path := fmt.Sprintf("/planet/bundle/v2/164/357/%d", z)
		w := streamRequest(h, path, "", "")
		if w.Code != 200 || w.Header().Get("Content-Type") != scb2.MediaType || w.Header().Get("Content-Encoding") != "" {
			t.Fatalf("response %d %v", w.Code, w.Header())
		}
		if w.Header().Get("Cache-Control") != "public, max-age=86400, no-transform" || !strings.Contains(w.Header().Get("ETag"), scb2.CodecVersion) {
			t.Fatal("wrong cache contract")
		}
		req, _ := privacy.ValidateBundle(164, 357, z)
		stages := checkStream(t, req, w.Body.Bytes())
		calls := atomic.LoadInt64(&f.calls)
		for i, stage := range scb2.Stages(req) {
			v1 := streamRequest(h, fmt.Sprintf("/planet/bundle/v1/164/357/%d", stage.TileZoom), "", "")
			if !bytes.Equal(gunzip(t, v1.Body.Bytes()), stages[i]) {
				t.Fatal("v1 parity failed")
			}
			// Every payload must match its deterministic row-major coordinate.
			off := 20
			for _, coord := range stage.Descendants() {
				if binary.BigEndian.Uint32(stages[i][off:]) != 12 {
					t.Fatal("wrong entry length")
				}
				off += 4
				for j, want := range []int{coord.Z, coord.X, coord.Y} {
					if binary.BigEndian.Uint32(stages[i][off+j*4:]) != uint32(want) {
						t.Fatal("wrong descendant")
					}
				}
				off += 12
			}
		}
		cached := streamRequest(h, path, "", "")
		if !bytes.Equal(w.Body.Bytes(), cached.Body.Bytes()) || atomic.LoadInt64(&f.calls) != calls {
			t.Fatal("cache reuse or deterministic bytes failed")
		}
	}
}

func TestV2RangesAndConditions(t *testing.T) {
	f := newFakeMartin()
	defer f.close()
	path := "/planet/bundle/v2/164/357/11"
	for _, cold := range []bool{false, true} {
		s := newTestServer(t, f)
		base := streamRequest(s.Handler(), path, "", "")
		body := bytes.Clone(base.Body.Bytes())
		etag := base.Header().Get("ETag")
		for _, tc := range []struct {
			span, ifRange string
			status        int
			start, end    int
		}{
			{"bytes=20-59", etag, 206, 20, 60},
			{"bytes=20-", etag, 206, 20, len(body)},
			{"bytes=-10", etag, 206, len(body) - 10, len(body)},
			{"bytes=0-99999", etag, 206, 0, len(body)},
			{"bytes=20-59", `"old-dataset"`, 200, 0, len(body)},
			{"bytes=20-59", "W/" + etag, 200, 0, len(body)},
			{"bytes=20-59", "Wed, 01 Jan 2020 00:00:00 GMT", 200, 0, len(body)},
			{"bytes=99999-", etag, 416, 0, 0},
			{"bytes=3-2", etag, 416, 0, 0},
			{"bytes=0-1,3-4", etag, 200, 0, len(body)},
		} {
			if cold {
				s = newTestServer(t, f)
			}
			w := streamRequest(s.Handler(), path, tc.span, tc.ifRange)
			if w.Code != tc.status {
				t.Fatalf("cold=%v %s %s got %d", cold, tc.span, tc.ifRange, w.Code)
			}
			if tc.status == 416 {
				if tc.span == "bytes=99999-" && w.Header().Get("Content-Range") != fmt.Sprintf("bytes */%d", len(body)) {
					t.Fatal(w.Header())
				}
				continue
			}
			if !bytes.Equal(w.Body.Bytes(), body[tc.start:tc.end]) {
				t.Fatal("wrong range bytes")
			}
			if tc.status == 206 && w.Header().Get("Content-Range") != fmt.Sprintf("bytes %d-%d/%d", tc.start, tc.end-1, len(body)) {
				t.Fatal(w.Header())
			}
			if w.Header().Get("Content-Encoding") != "" || w.Header().Get("Accept-Ranges") != "bytes" {
				t.Fatal("range representation transformed")
			}
		}
		for _, tag := range []string{etag, "W/" + etag, `"other", ` + etag, "*"} {
			r := httptest.NewRequest("GET", path, nil)
			r.Header.Set("If-None-Match", tag)
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, r)
			if w.Code != 304 || w.Body.Len() != 0 {
				t.Fatal("conditional GET failed", w.Code)
			}
		}
		// Same release label, different artifact digest must not mix bytes.
		s.cfg.DatasetDigest = "changed"
		w := streamRequest(s.Handler(), path, "bytes=20-59", etag)
		if w.Code != 200 || w.Header().Get("ETag") == etag {
			t.Fatal("changed dataset range not replaced")
		}
		checkStream(t, privacy.BundleRequest{AnchorX: 164, AnchorY: 357, TileZoom: 11}, w.Body.Bytes())
	}
}

func TestOverviewFlushedBeforeDetailAndDisconnectCaches(t *testing.T) {
	for _, disconnect := range []bool{false, true} {
		t.Run(fmt.Sprint(disconnect), func(t *testing.T) {
			f := newFakeMartin()
			defer f.close()
			s := newTestServer(t, f)
			detailEntered := make(chan struct{})
			release := make(chan struct{})
			var once sync.Once
			var calls atomic.Int64
			s.source = tileFunc(func(ctx context.Context, coord privacy.TileCoord) ([]byte, error) {
				calls.Add(1)
				if coord.Z == 14 {
					once.Do(func() { close(detailEntered) })
					select {
					case <-release:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				}
				return nil, nil
			})
			ts := httptest.NewServer(s.Handler())
			defer ts.Close()
			resp, err := http.Get(ts.URL + "/planet/bundle/v2/164/357/14")
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			header := make([]byte, 60)
			if _, err := io.ReadFull(resp.Body, header); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(header[:20], scb2.Header(privacy.BundleRequest{AnchorX: 164, AnchorY: 357, TileZoom: 14})) {
				t.Fatal("wrong progressive header")
			}
			n := int(binary.BigEndian.Uint32(header[20:]))
			overview := make([]byte, n)
			if _, err := io.ReadFull(resp.Body, overview); err != nil {
				t.Fatal(err)
			}
			raw := gunzip(t, overview)
			if raw[6] != 13 || binary.BigEndian.Uint32(raw[16:]) != 64 {
				t.Fatal("not a complete overview")
			}
			select {
			case <-detailEntered:
			case <-time.After(5 * time.Second):
				t.Fatal("detail never started")
			}
			if disconnect {
				resp.Body.Close()
			}
			close(release)
			if !disconnect {
				tail, err := io.ReadAll(resp.Body)
				if err != nil {
					t.Fatal(err)
				}
				checkStream(t, privacy.BundleRequest{AnchorX: 164, AnchorY: 357, TileZoom: 14}, append(append(header, overview...), tail...))
			}
			// A concurrent client joins the detached build, then sees the full cache.
			complete, err := http.Get(ts.URL + "/planet/bundle/v2/164/357/14")
			if err != nil {
				t.Fatal(err)
			}
			full, err := io.ReadAll(complete.Body)
			complete.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			checkStream(t, privacy.BundleRequest{AnchorX: 164, AnchorY: 357, TileZoom: 14}, full)
			if calls.Load() != 320 {
				t.Fatalf("rebuilt after disconnect: %d reads", calls.Load())
			}
		})
	}
}

type flushRecorder struct {
	*httptest.ResponseRecorder
	flushed  bool
	deadline time.Time
}

func (w *flushRecorder) Flush() { w.flushed = true; w.ResponseRecorder.Flush() }

func (w *flushRecorder) SetWriteDeadline(deadline time.Time) error { w.deadline = deadline; return nil }

func TestFlushHappensBeforeDetailRead(t *testing.T) {
	f := newFakeMartin()
	defer f.close()
	s := newTestServer(t, f)
	w := &flushRecorder{ResponseRecorder: httptest.NewRecorder()}
	s.source = tileFunc(func(_ context.Context, c privacy.TileCoord) ([]byte, error) {
		if c.Z == 14 && !w.flushed {
			return nil, errors.New("detail read before overview flush")
		}
		if c.Z == 14 && time.Until(w.deadline) < 30*time.Second {
			return nil, errors.New("short subscriber deadline left active during detail build")
		}
		return nil, nil
	})
	s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/planet/bundle/v2/164/357/14", nil))
	checkStream(t, privacy.BundleRequest{AnchorX: 164, AnchorY: 357, TileZoom: 14}, w.Body.Bytes())
}

func TestFailedDetailAbortsAndIsNotCached(t *testing.T) {
	f := newFakeMartin()
	defer f.close()
	s := newTestServer(t, f)
	var fail atomic.Bool
	fail.Store(true)
	s.source = tileFunc(func(_ context.Context, c privacy.TileCoord) ([]byte, error) {
		if c.Z == 14 && fail.Load() {
			return nil, errors.New("broken archive")
		}
		return nil, nil
	})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/planet/bundle/v2/164/357/14")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err == nil {
		t.Fatal("failed stream ended successfully")
	}
	if len(raw) < 60 {
		t.Fatal("overview missing")
	}
	if _, ok := s.cache.Get(s.stageKey(privacy.BundleRequest{AnchorX: 164, AnchorY: 357, TileZoom: 14})); ok {
		t.Fatal("failed stage cached")
	}
	fail.Store(false)
	w := streamRequest(s.Handler(), "/planet/bundle/v2/164/357/14", "", "")
	checkStream(t, privacy.BundleRequest{AnchorX: 164, AnchorY: 357, TileZoom: 14}, w.Body.Bytes())
}

func TestCorruptStageCannotBecomeStream(t *testing.T) {
	f := newFakeMartin()
	defer f.close()
	s := newTestServer(t, f)
	req := privacy.BundleRequest{AnchorX: 164, AnchorY: 357, TileZoom: 11}
	// Internally checksummed but semantically incomplete SCB1 must fail framing.
	if err := s.cache.Put(s.stageKey(req), []byte("not gzip")); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		w := streamRequest(s.Handler(), "/planet/bundle/v2/164/357/11", "", "")
		if w.Code != 502 {
			t.Fatal("corrupt stage accepted", w.Code)
		}
		if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("ETag") != "" {
			t.Fatal("failed response is cacheable")
		}
	}
}

func TestAdmissionAndDeadline(t *testing.T) {
	f := newFakeMartin()
	defer f.close()
	s := newTestServer(t, f)
	s.cfg.BundleTimeout = 150 * time.Millisecond
	entered := make(chan struct{})
	var once sync.Once
	s.source = tileFunc(func(ctx context.Context, _ privacy.TileCoord) ([]byte, error) {
		once.Do(func() { close(entered) })
		<-ctx.Done()
		return nil, ctx.Err()
	})
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- streamRequest(s.Handler(), "/planet/bundle/v2/164/357/11", "", "") }()
	<-entered
	w := streamRequest(s.Handler(), "/planet/bundle/v2/165/357/11", "", "")
	if w.Code != 503 || w.Header().Get("Retry-After") == "" {
		t.Fatal("admission not bounded")
	}
	w = streamRequest(s.Handler(), "/planet/bundle/v1/166/357/11", "", "")
	if w.Code != 503 {
		t.Fatal("v1 bypassed admission")
	}
	select {
	case w = <-done:
		if w.Code != 503 {
			t.Fatal(w.Code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("deadline did not release build")
	}
	if len(s.builds) != 0 || len(s.workers) != 0 || s.cache.Bytes() != 0 {
		t.Fatal("failed build retained resources")
	}
}

func TestAggregateSizeBound(t *testing.T) {
	f := newFakeMartin()
	defer f.close()
	s := newTestServer(t, f)
	payload := make([]byte, 17<<20)
	s.source = tileFunc(func(context.Context, privacy.TileCoord) ([]byte, error) { return payload, nil })
	w := streamRequest(s.Handler(), "/planet/bundle/v2/164/357/11", "", "")
	if w.Code != 502 || s.cache.Bytes() != 0 {
		t.Fatal("oversized aggregate accepted")
	}
}

func TestV2PrivacyAndCORS(t *testing.T) {
	f := newFakeMartin()
	defer f.close()
	s := newTestServer(t, f)
	for _, path := range []string{
		"/planet/bundle/v2/164/357/10", "/planet/bundle/v2/164/357/15",
		"/planet/bundle/v2/1024/357/11", "/planet/bundle/v2/-1/357/11",
		"/planet/bundle/v2/164/357/11?child=1", "/planet/bundle/v2/164/357/11?",
	} {
		w := streamRequest(s.Handler(), path, "", "")
		if w.Code != 400 {
			t.Fatalf("%s: %d", path, w.Code)
		}
	}
	for _, v := range []string{"v1", "v2"} {
		r := httptest.NewRequest("GET", "/planet/bundle/"+v+"/164/357/11", strings.NewReader("child"))
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != 400 {
			t.Fatal("body accepted")
		}
	}
	for z := 11; z <= 14; z++ {
		w := streamRequest(s.Handler(), fmt.Sprintf("/planet/%d/1/1", z), "", "")
		if w.Code != 404 {
			t.Fatal("raw fine tile exposed")
		}
	}
	if atomic.LoadInt64(&f.calls) != 0 {
		t.Fatal("invalid request reached source")
	}
	r := httptest.NewRequest("OPTIONS", "/planet/bundle/v2/164/357/11", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 204 || !strings.Contains(w.Header().Get("Access-Control-Allow-Headers"), "If-Range") {
		t.Fatal("CORS range preflight")
	}
}

func TestDirectPMTilesV1ParityWithoutMartinFanout(t *testing.T) {
	// A real tiny PMTiles archive with an RLE covering every z11 ID.
	payload := []byte{0x1a, 0}
	compressed, err := gzipBytes(payload)
	if err != nil {
		t.Fatal(err)
	}
	first := ((uint64(1) << 22) - 1) / 3
	dir := binary.AppendUvarint(nil, 1)
	for _, v := range []uint64{first, 1 << 22, uint64(len(compressed)), 1} {
		dir = binary.AppendUvarint(dir, v)
	}
	metadata := []byte(`{"vector_layers":[]}`)
	h := make([]byte, 127)
	copy(h, "PMTiles")
	h[7], h[97], h[98], h[99], h[101] = 3, 1, 2, 1, 14
	offset := uint64(127)
	for i, b := range [][]byte{dir, metadata, nil, compressed} {
		binary.LittleEndian.PutUint64(h[8+i*16:], offset)
		binary.LittleEndian.PutUint64(h[16+i*16:], uint64(len(b)))
		offset += uint64(len(b))
	}
	archive := append(append(append(h, dir...), metadata...), compressed...)
	path := filepath.Join(t.TempDir(), "tiny.pmtiles")
	if err := os.WriteFile(path, archive, 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := pmtiles.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	f := newFakeMartin()
	defer f.close()
	s := newTestServer(t, f)
	s.source = source
	w := streamRequest(s.Handler(), "/planet/bundle/v1/164/357/11", "", "")
	req := privacy.BundleRequest{AnchorX: 164, AnchorY: 357, TileZoom: 11}
	entries := make([]scb1.Entry, 4)
	for i := range entries {
		entries[i].Bytes = payload
	}
	want, err := scb1.Encode(req, entries)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gunzip(t, w.Body.Bytes()), want) {
		t.Fatal("direct bytes differ from decoded Martin semantics")
	}
	w = streamRequest(s.Handler(), "/planet/bundle/v2/164/357/11", "", "")
	checkStream(t, req, w.Body.Bytes())
	if atomic.LoadInt64(&f.calls) != 0 {
		t.Fatal("direct bundles fanned out to Martin")
	}
	w = streamRequest(s.Handler(), "/planet/10/164/357", "", "")
	if w.Code != 200 || atomic.LoadInt64(&f.calls) != 1 {
		t.Fatal("coarse stopped using Martin")
	}
}

func TestV2CacheSurvivesServerRestart(t *testing.T) {
	f := newFakeMartin()
	defer f.close()
	dir := t.TempDir()
	c, err := cache.New(dir, 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	s := newTestServer(t, f)
	s.cache = c
	first := streamRequest(s.Handler(), "/planet/bundle/v2/164/357/14", "", "")
	c, err = cache.New(dir, 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	restarted := newTestServer(t, f)
	restarted.cache = c
	restarted.source = tileFunc(func(context.Context, privacy.TileCoord) ([]byte, error) { return nil, errors.New("source offline") })
	got := streamRequest(restarted.Handler(), "/planet/bundle/v2/164/357/14", "bytes=20-59", first.Header().Get("ETag"))
	if got.Code != 206 || !bytes.Equal(got.Body.Bytes(), first.Body.Bytes()[20:60]) {
		t.Fatal("restart did not restore resumable representation")
	}
}

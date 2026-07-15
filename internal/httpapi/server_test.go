package httpapi

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/junephilip/streetcryptid-map-server/internal/cache"
	"github.com/junephilip/streetcryptid-map-server/internal/martin"
)

// fakeMartin serves deterministic tile bytes and counts requests so tests can
// prove a blocked raw request never reaches upstream.
type fakeMartin struct {
	srv   *httptest.Server
	calls int64
	// emptyAt returns true for tiles that should be reported sparse.
	emptyAt func(z, x, y int) bool
}

func newFakeMartin() *fakeMartin {
	f := &fakeMartin{}
	mux := http.NewServeMux()
	mux.HandleFunc("/catalog", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"tiles":{"planet":{}}}`))
	})
	mux.HandleFunc("/planet/{z}/{x}/{y}", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&f.calls, 1)
		var z, x, y int
		fmt.Sscanf(r.PathValue("z")+" "+r.PathValue("x")+" "+r.PathValue("y"), "%d %d %d", &z, &x, &y)
		if f.emptyAt != nil && f.emptyAt(z, x, y) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
		// Payload encodes the coordinate so tests can byte-match.
		var b [12]byte
		binary.BigEndian.PutUint32(b[0:], uint32(z))
		binary.BigEndian.PutUint32(b[4:], uint32(x))
		binary.BigEndian.PutUint32(b[8:], uint32(y))
		w.Write(b[:])
	})
	f.srv = httptest.NewServer(mux)
	return f
}

func (f *fakeMartin) close() { f.srv.Close() }

func newTestServer(t *testing.T, f *fakeMartin) *Server {
	t.Helper()
	c := martin.New(martin.Config{BaseURL: f.srv.URL + "/planet", Timeout: 5 * time.Second})
	cch, err := cache.New(t.TempDir(), 8*1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	return New(Config{
		MartinBaseURL:    f.srv.URL + "/planet",
		MartinCatalogURL: f.srv.URL + "/catalog",
		Source:           "planet",
		DatasetVersion:   "test-v1",
		BundleWorkers:    8,
	}, c, cch)
}

func TestCoarseZoomsAccepted(t *testing.T) {
	f := newFakeMartin()
	defer f.close()
	h := newTestServer(t, f).Handler()
	for z := 0; z <= 10; z++ {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest("GET", fmt.Sprintf("/planet/%d/0/0", z), nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("z%d got %d", z, rr.Code)
		}
	}
}

func TestRawFineBlockedWithoutMartin(t *testing.T) {
	f := newFakeMartin()
	defer f.close()
	h := newTestServer(t, f).Handler()
	for z := 11; z <= 14; z++ {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest("GET", fmt.Sprintf("/planet/%d/1/1", z), nil))
		if rr.Code != http.StatusNotFound {
			t.Fatalf("raw z%d must be 404, got %d", z, rr.Code)
		}
	}
	if got := atomic.LoadInt64(&f.calls); got != 0 {
		t.Fatalf("blocked raw requests hit Martin %d times, want 0", got)
	}
}

func TestBundleEntryCounts(t *testing.T) {
	f := newFakeMartin()
	defer f.close()
	h := newTestServer(t, f).Handler()
	for _, tc := range []struct{ z, want int }{{11, 4}, {12, 16}, {13, 64}, {14, 256}} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest("GET", fmt.Sprintf("/planet/bundle/v1/164/357/%d", tc.z), nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("z%d bundle got %d", tc.z, rr.Code)
		}
		if ct := rr.Header().Get("Content-Type"); ct != TileBundleMediaType {
			t.Fatalf("content type %q", ct)
		}
		if enc := rr.Header().Get("Content-Encoding"); enc != "gzip" {
			t.Fatalf("encoding %q", enc)
		}
		count := decodeEntryCount(t, rr.Body.Bytes())
		if count != uint32(tc.want) {
			t.Fatalf("z%d entry count %d, want %d", tc.z, count, tc.want)
		}
	}
}

func TestBundleBytesMatchMartin(t *testing.T) {
	f := newFakeMartin()
	// Make one descendant empty to exercise the sentinel.
	f.emptyAt = func(z, x, y int) bool { return x == 328 && y == 714 }
	defer f.close()
	h := newTestServer(t, f).Handler()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/planet/bundle/v1/164/357/11", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("got %d", rr.Code)
	}
	raw := gunzip(t, rr.Body.Bytes())
	// Header 20 bytes; then 4 entries row-major from (328,714).
	off := 20
	// entry0 is (328,714) => empty sentinel
	l0 := binary.BigEndian.Uint32(raw[off:])
	if l0 != 0xFFFFFFFF {
		t.Fatalf("entry0 should be empty sentinel, got len %d", l0)
	}
	off += 4
	// entry1 is (329,714) => 12 bytes encoding z=11,x=329,y=714
	l1 := binary.BigEndian.Uint32(raw[off:])
	off += 4
	if l1 != 12 {
		t.Fatalf("entry1 len %d, want 12", l1)
	}
	z := binary.BigEndian.Uint32(raw[off:])
	x := binary.BigEndian.Uint32(raw[off+4:])
	y := binary.BigEndian.Uint32(raw[off+8:])
	if z != 11 || x != 329 || y != 714 {
		t.Fatalf("entry1 payload %d/%d/%d, want 11/329/714", z, x, y)
	}
}

func TestBundleCacheHitStableETag(t *testing.T) {
	f := newFakeMartin()
	defer f.close()
	h := newTestServer(t, f).Handler()
	req := func() *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest("GET", "/planet/bundle/v1/164/357/12", nil))
		return rr
	}
	first := req()
	callsAfterFirst := atomic.LoadInt64(&f.calls)
	second := req()
	if first.Header().Get("ETag") != second.Header().Get("ETag") {
		t.Fatal("ETag not stable across requests")
	}
	if atomic.LoadInt64(&f.calls) != callsAfterFirst {
		t.Fatal("second request hit Martin; cache miss")
	}
	// If-None-Match => 304
	rr := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/planet/bundle/v1/164/357/12", nil)
	r.Header.Set("If-None-Match", first.Header().Get("ETag"))
	h.ServeHTTP(rr, r)
	if rr.Code != http.StatusNotModified {
		t.Fatalf("If-None-Match got %d, want 304", rr.Code)
	}
}

func TestBundleInvalidInput(t *testing.T) {
	f := newFakeMartin()
	defer f.close()
	h := newTestServer(t, f).Handler()
	for _, path := range []string{
		"/planet/bundle/v1/164/357/10",         // zoom too low
		"/planet/bundle/v1/164/357/15",         // zoom too high
		"/planet/bundle/v1/1024/357/12",        // anchor out of range
		"/planet/bundle/v1/164/357/12?child=3", // query param
	} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest("GET", path, nil))
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("%s got %d, want 400", path, rr.Code)
		}
	}
}

func TestReadyzChecksCatalog(t *testing.T) {
	f := newFakeMartin()
	defer f.close()
	h := newTestServer(t, f).Handler()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/readyz", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("readyz got %d", rr.Code)
	}
}

func TestMetricsCountsBlocked(t *testing.T) {
	f := newFakeMartin()
	defer f.close()
	h := newTestServer(t, f).Handler()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/planet/13/1/1", nil))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(rr.Body.String(), "mapapi_coarse_blocked_total 1") {
		t.Fatalf("metrics missing blocked counter:\n%s", rr.Body.String())
	}
}

func decodeEntryCount(t *testing.T, gz []byte) uint32 {
	t.Helper()
	raw := gunzip(t, gz)
	if len(raw) < 20 {
		t.Fatal("short bundle")
	}
	return binary.BigEndian.Uint32(raw[16:20])
}

func gunzip(t *testing.T, gz []byte) []byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

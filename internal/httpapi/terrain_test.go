package httpapi

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/junephilip/streetcryptid-map-server/internal/privacy"
	"github.com/junephilip/streetcryptid-map-server/internal/scb1"
)

// fakeTerrain returns a payload naming its coordinate, empty for "sea" tiles,
// and counts every read so tests can prove a blocked request never reads.
type fakeTerrain struct {
	reads int64
	seaAt func(privacy.TileCoord) bool
	fail  bool
}

func (f *fakeTerrain) GetTileBytes(_ context.Context, t privacy.TileCoord) ([]byte, error) {
	atomic.AddInt64(&f.reads, 1)
	if f.fail {
		return nil, errors.New("disk on fire")
	}
	if f.seaAt != nil && f.seaAt(t) {
		return nil, nil
	}
	return []byte(fmt.Sprintf("webp:%d/%d/%d", t.Z, t.X, t.Y)), nil
}

func newTerrainServer(t *testing.T, terrain *fakeTerrain) *Server {
	t.Helper()
	f := newFakeMartin()
	t.Cleanup(f.close)
	s := newTestServer(t, f)
	if terrain != nil {
		s.cfg.Terrain = &Terrain{Source: terrain, ContentType: "image/webp", Version: "dem-v1", MaxZoom: 12}
	}
	return s
}

func TestTerrainCoarseServesPublicZooms(t *testing.T) {
	terrain := &fakeTerrain{}
	h := newTerrainServer(t, terrain).Handler()
	for z := 0; z <= privacy.MaxPublicRawZoom; z++ {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest("GET", fmt.Sprintf("/terrain/%d/0/0", z), nil))
		if rr.Code != 200 || rr.Body.String() != fmt.Sprintf("webp:%d/0/0", z) {
			t.Fatalf("z%d: %d %q", z, rr.Code, rr.Body.String())
		}
		if rr.Header().Get("Content-Type") != "image/webp" || rr.Header().Get("ETag") == "" ||
			rr.Header().Get("Access-Control-Allow-Origin") != "*" {
			t.Fatalf("z%d headers: %v", z, rr.Header())
		}
	}
}

func TestTerrainCoarseBlocksFineZoomsWithoutReading(t *testing.T) {
	terrain := &fakeTerrain{}
	h := newTerrainServer(t, terrain).Handler()
	for z := 11; z <= 14; z++ {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest("GET", fmt.Sprintf("/terrain/%d/1/1", z), nil))
		if rr.Code != 404 {
			t.Fatalf("raw terrain z%d answered %d", z, rr.Code)
		}
	}
	if terrain.reads != 0 {
		t.Fatalf("blocked requests read %d tiles", terrain.reads)
	}
}

func TestTerrainSeaIsNoContent(t *testing.T) {
	h := newTerrainServer(t, &fakeTerrain{seaAt: func(privacy.TileCoord) bool { return true }}).Handler()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/terrain/5/1/1", nil))
	if rr.Code != 204 || rr.Header().Get("Cache-Control") == "" {
		t.Fatalf("sea tile: %d %v", rr.Code, rr.Header())
	}
}

func TestTerrainAbsentIs404(t *testing.T) {
	h := newTerrainServer(t, nil).Handler()
	for _, path := range []string{"/terrain/5/1/1", "/terrain/bundle/v1/164/357/12"} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest("GET", path, nil))
		if rr.Code != 404 {
			t.Fatalf("%s without terrain answered %d", path, rr.Code)
		}
	}
}

func TestTerrainBundleIsTheWholeAnchor(t *testing.T) {
	terrain := &fakeTerrain{seaAt: func(c privacy.TileCoord) bool { return c.X%2 == 1 }}
	h := newTerrainServer(t, terrain).Handler()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/terrain/bundle/v1/164/357/12", nil))
	if rr.Code != 200 || rr.Header().Get("Content-Type") != TileBundleMediaType {
		t.Fatalf("bundle: %d %v", rr.Code, rr.Header())
	}
	body := rr.Body.Bytes()
	if string(body[:4]) != scb1.Magic {
		t.Fatalf("not SCB1: %q", body[:4])
	}
	req, _ := privacy.ValidateTerrainBundle(164, 357, 12)
	tiles := req.Descendants()
	if len(tiles) != 16 || terrain.reads != 16 {
		t.Fatalf("read %d of %d descendants", terrain.reads, len(tiles))
	}
	// Walk the entries: empties carry the sentinel, the rest name their tile.
	off := scb1.HeaderBytes
	for _, tc := range tiles {
		n := binary.BigEndian.Uint32(body[off:])
		off += 4
		if tc.X%2 == 1 {
			if n != scb1.EmptyTileLength {
				t.Fatalf("%v should be empty", tc)
			}
			continue
		}
		if got := string(body[off : off+int(n)]); got != fmt.Sprintf("webp:%d/%d/%d", tc.Z, tc.X, tc.Y) {
			t.Fatalf("%v: %q", tc, got)
		}
		off += int(n)
	}
	if off != len(body) {
		t.Fatalf("trailing bytes: %d of %d", off, len(body))
	}
}

func TestTerrainBundleZoomBoundary(t *testing.T) {
	terrain := &fakeTerrain{}
	h := newTerrainServer(t, terrain).Handler()
	for _, z := range []int{10, 13, 14} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest("GET", fmt.Sprintf("/terrain/bundle/v1/164/357/%d", z), nil))
		if rr.Code != 404 {
			t.Fatalf("terrain bundle z%d answered %d", z, rr.Code)
		}
	}
	if terrain.reads != 0 {
		t.Fatalf("rejected bundles read %d tiles", terrain.reads)
	}
}

func TestTerrainReadFailureIsNotAnEmptyTile(t *testing.T) {
	h := newTerrainServer(t, &fakeTerrain{fail: true}).Handler()
	for _, path := range []string{"/terrain/5/1/1", "/terrain/bundle/v1/164/357/11"} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest("GET", path, nil))
		if rr.Code != 502 {
			t.Fatalf("%s on a failing archive answered %d", path, rr.Code)
		}
	}
}

func TestTerrainNotModified(t *testing.T) {
	h := newTerrainServer(t, &fakeTerrain{}).Handler()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/terrain/bundle/v1/164/357/11", nil))
	req := httptest.NewRequest("GET", "/terrain/bundle/v1/164/357/11", nil)
	req.Header.Set("If-None-Match", rr.Header().Get("ETag"))
	again := httptest.NewRecorder()
	h.ServeHTTP(again, req)
	if again.Code != 304 {
		t.Fatalf("revalidation answered %d", again.Code)
	}
}

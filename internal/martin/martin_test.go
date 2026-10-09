package martin

import (
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/junephilip/streetcryptid-map-server/internal/privacy"
)

func gzipTest(t *testing.T, raw []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	if _, err := zw.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestRawBodyBoundsAndEncodings(t *testing.T) {
	raw := bytes.Repeat([]byte{7}, 100)
	gz := gzipTest(t, raw)
	for _, enc := range []string{"", "identity"} {
		got, err := (&TileResponse{Body: raw, ContentEncoding: enc}).RawBody(10)
		if err != nil || !bytes.Equal(got, raw) {
			t.Fatalf("%q: identity body changed: %v", enc, err)
		}
	}
	got, err := (&TileResponse{Body: gz, ContentEncoding: "gzip"}).RawBody(100)
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("gzip inflate: %v", err)
	}
	if _, err := (&TileResponse{Body: gz, ContentEncoding: "gzip"}).RawBody(99); err == nil {
		t.Fatal("inflated past limit")
	}
	if _, err := (&TileResponse{Body: gz[:len(gz)-1], ContentEncoding: "gzip"}).RawBody(100); err == nil {
		t.Fatal("truncated gzip accepted")
	}
	if _, err := (&TileResponse{Body: raw, ContentEncoding: "br"}).RawBody(100); err == nil {
		t.Fatal("unknown encoding accepted")
	}
}

func TestStoredAndRawTileReads(t *testing.T) {
	raw := []byte("mvt bytes")
	gz := gzipTest(t, raw)
	var sawAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAccept = r.Header.Get("Accept-Encoding")
		if r.URL.Path == "/planet/14/0/0" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Encoding", "gzip")
		w.Write(gz)
	}))
	defer srv.Close()
	c := New(Config{BaseURL: srv.URL + "/planet/"})
	ctx := context.Background()
	stored, gzipped, err := c.GetTileStored(ctx, privacy.TileCoord{Z: 14, X: 1, Y: 1})
	if err != nil || !gzipped || !bytes.Equal(stored, gz) {
		t.Fatalf("stored read inflated or lost encoding: %v %v", gzipped, err)
	}
	if sawAccept != "gzip" {
		t.Fatalf("Accept-Encoding %q", sawAccept)
	}
	got, err := c.GetTileBytes(ctx, privacy.TileCoord{Z: 14, X: 1, Y: 1})
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("bundle bytes not raw MVT: %v", err)
	}
	stored, gzipped, err = c.GetTileStored(ctx, privacy.TileCoord{Z: 14})
	if stored != nil || gzipped || err != nil {
		t.Fatal("empty tile not (nil, false, nil)")
	}
}

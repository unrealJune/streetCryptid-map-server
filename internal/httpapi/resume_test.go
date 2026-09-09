package httpapi

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/junephilip/streetcryptid-map-server/internal/cache"
)

func TestV2FullySavedTransfer416(t *testing.T) {
	f := newFakeMartin()
	defer f.close()
	path := "/planet/bundle/v2/164/357/11"
	base := streamRequest(newTestServer(t, f).Handler(), path, "", "")
	body := base.Body.Bytes()
	etag := base.Header().Get("ETag")
	for _, mode := range []string{"cached", "cold", "uncached"} {
		t.Run(mode, func(t *testing.T) {
			s := newTestServer(t, f)
			if mode == "uncached" {
				c, err := cache.New("", 0)
				if err != nil {
					t.Fatal(err)
				}
				s.cache = c
			}
			if mode == "cached" {
				streamRequest(s.Handler(), path, "", "")
			}
			span := fmt.Sprintf("bytes=%d-", len(body))
			w := streamRequest(s.Handler(), path, span, etag)
			// Inspect the committed HTTP headers, not subsequent map changes.
			response := w.Result()
			defer response.Body.Close()
			if response.StatusCode != http.StatusRequestedRangeNotSatisfiable ||
				response.Header.Get("ETag") != etag ||
				response.Header.Get("Content-Range") != fmt.Sprintf("bytes */%d", len(body)) {
				t.Fatalf("cannot confirm fully saved transfer: %d %v", response.StatusCode, response.Header)
			}
			if response.Header.Get("Content-Encoding") != "" ||
				!strings.Contains(response.Header.Get("Cache-Control"), "no-transform") ||
				response.Header.Get("Access-Control-Allow-Origin") != "*" {
				t.Fatal("416 lost identity/CORS headers", response.Header)
			}
			for _, header := range []string{"ETag", "Content-Range", "Accept-Ranges"} {
				if !strings.Contains(response.Header.Get("Access-Control-Expose-Headers"), header) {
					t.Fatal("416 does not expose", header)
				}
			}
			// The same end offset must not certify bytes from another dataset.
			s.cfg.DatasetDigest = "replacement-artifact"
			w = streamRequest(s.Handler(), path, span, etag)
			if w.Code != http.StatusOK || w.Header().Get("ETag") == etag || !bytes.Equal(w.Body.Bytes(), body) {
				t.Fatal("changed If-Range did not return full representation", w.Code, w.Header())
			}
		})
	}
}

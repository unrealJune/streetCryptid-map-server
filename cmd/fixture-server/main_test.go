package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestFixtureProductionHandlerMatchesGoldenAndResume(t *testing.T) {
	dir := t.TempDir()
	handler, cleanup, err := newFixture(dir, filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	server := httptest.NewServer(handler)
	defer server.Close()
	golden, err := os.ReadFile(filepath.Join("..", "..", "testdata", "scb2-z11-empty.scb2"))
	if err != nil {
		t.Fatal(err)
	}
	url := server.URL + "/planet/bundle/v2/164/357/11"
	for range 2 {
		resp, err := http.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || resp.StatusCode != 200 || !bytes.Equal(body, golden) {
			t.Fatalf("golden mismatch: %d %x %v", resp.StatusCode, body, err)
		}
		etag := resp.Header.Get("ETag")
		for _, offset := range []int{1, 19, 20, 39, 60, 106, 107} {
			request, err := http.NewRequest("GET", url, nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
			request.Header.Set("If-Range", etag)
			r, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			b, err := io.ReadAll(r.Body)
			r.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if r.Header.Get("ETag") != etag || r.Header.Get("Content-Encoding") != "" {
				t.Fatal("range validator/encoding mismatch", r.Header)
			}
			if offset == len(golden) {
				if r.StatusCode != 416 || r.Header.Get("Content-Range") != "bytes */107" {
					t.Fatal("complete-prefix recovery failed", r.StatusCode, r.Header)
				}
			} else if r.StatusCode != 206 || !bytes.Equal(b, golden[offset:]) {
				t.Fatal("arbitrary prefix resume failed", offset, r.StatusCode)
			}
		}
	}
}

func TestFixtureRejectsPublicBind(t *testing.T) {
	for _, addr := range []string{":8089", "0.0.0.0:8089", "[::]:8089", "192.168.1.1:8089", "example.com:8089"} {
		if err := loopbackAddress(addr); err == nil {
			t.Fatal("accepted public bind", addr)
		}
	}
	for _, addr := range []string{"127.0.0.1:8089", "[::1]:8089"} {
		if err := loopbackAddress(addr); err != nil {
			t.Fatal(err)
		}
	}
}

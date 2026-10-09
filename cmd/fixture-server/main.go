// Command fixture-server runs the production API against a tiny, empty PMTiles
// archive for local cross-language conformance. It never accepts a public bind.
package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/junephilip/streetcryptid-map-server/internal/cache"
	"github.com/junephilip/streetcryptid-map-server/internal/httpapi"
	"github.com/junephilip/streetcryptid-map-server/internal/martin"
	"github.com/junephilip/streetcryptid-map-server/internal/pmtiles"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8089", "loopback address")
	cacheDir := flag.String("cache-dir", "", "optional persistent fixture cache directory")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, *listen, *cacheDir); err != nil {
		slog.Error("fixture server failed", "err", err)
		os.Exit(1)
	}
}

func loopbackAddress(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return errors.New("fixture server requires a literal loopback IP")
	}
	return nil
}

func run(ctx context.Context, addr, cacheDir string) error {
	if err := loopbackAddress(addr); err != nil {
		return err
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	defer listener.Close()
	work, err := os.MkdirTemp("", "scb2-fixture-")
	if err != nil {
		return err
	}
	defer func() {
		if err := os.RemoveAll(work); err != nil {
			slog.Error("fixture cleanup failed", "err", err)
		}
	}()
	if cacheDir == "" {
		cacheDir = filepath.Join(work, "cache")
	}
	handler, closeFixture, err := newFixture(work, cacheDir)
	if err != nil {
		return err
	}
	defer closeFixture()
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      90 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	fmt.Printf("SCB2 fixture http://%s/planet/bundle/v2/164/357/11\n", listener.Addr())
	fmt.Printf("SCB3 fixture http://%s/planet/bundle/v3/164/357/11\n", listener.Addr())
	fmt.Printf("cache=%s; all fine tiles empty; production handler, PMTiles reader and cache\n", cacheDir)
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			server.Close()
			return err
		}
		err := <-done
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}

func newFixture(work, cacheDir string) (http.Handler, func(), error) {
	archive, err := emptyArchive()
	if err != nil {
		return nil, nil, err
	}
	path := filepath.Join(work, "fixture.pmtiles")
	if err := os.WriteFile(path, archive, 0o600); err != nil {
		return nil, nil, err
	}
	reader, err := pmtiles.Open(path)
	if err != nil {
		return nil, nil, err
	}
	c, err := cache.New(cacheDir, 4<<30)
	if err != nil {
		reader.Close()
		return nil, nil, err
	}
	// Only coarse/health use this local empty Martin stub. Fine requests read
	// the actual tiny archive through the production PMTiles reader.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/catalog" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"tiles":{"planet":{}}}`))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	digest := sha256.Sum256(archive)
	client := martin.New(martin.Config{BaseURL: upstream.URL + "/planet"})
	s := httpapi.New(httpapi.Config{
		Source:           "planet",
		MartinCatalogURL: upstream.URL + "/catalog",
		DatasetVersion:   "fixture-empty-v1",
		DatasetDigest:    hex.EncodeToString(digest[:]),
		BundleSource:     reader,
	}, client, c)
	return s.Handler(), func() {
		upstream.Close()
		if err := reader.Close(); err != nil {
			slog.Error("fixture reader close failed", "err", err)
		}
	}, nil
}

func emptyArchive() ([]byte, error) {
	var tile bytes.Buffer
	zw := gzip.NewWriter(&tile)
	if err := zw.Close(); err != nil {
		return nil, err
	}
	dir := binary.AppendUvarint(nil, 1)
	for _, v := range []uint64{0, 1, uint64(tile.Len()), 1} {
		dir = binary.AppendUvarint(dir, v)
	}
	meta := []byte(`{"name":"OpenMapTiles","vector_layers":[]}`)
	header := make([]byte, 127)
	copy(header, "PMTiles")
	header[7], header[97], header[98], header[99], header[101] = 3, 1, 2, 1, 14
	offset := uint64(127)
	for i, section := range [][]byte{dir, meta, nil, tile.Bytes()} {
		binary.LittleEndian.PutUint64(header[8+i*16:], offset)
		binary.LittleEndian.PutUint64(header[16+i*16:], uint64(len(section)))
		offset += uint64(len(section))
	}
	out := append(header, dir...)
	out = append(out, meta...)
	return append(out, tile.Bytes()...), nil
}

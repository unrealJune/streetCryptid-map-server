// Package martin is a bounded client for the private Martin sidecar reachable
// only over pod localhost. It never leaks Martin's address or catalog to the
// public API and applies exact status handling: 204/404 mean "empty tile", any
// other non-2xx is a hard failure.
package martin

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/junephilip/streetcryptid-map-server/internal/privacy"
)

// ErrEmpty signals a sparse tile: Martin returned 204 or 404. Callers encode it
// as the SCB1 empty sentinel; it is not an error condition for a bundle.
var ErrEmpty = errors.New("martin: empty tile")

// TileResponse carries the proxied bytes and the headers the coarse path
// forwards verbatim.
type TileResponse struct {
	Status          int
	Body            []byte
	ContentType     string
	ContentEncoding string
	ETag            string
	CacheControl    string
}

// Client talks to one Martin source over localhost.
type Client struct {
	baseURL string // e.g. http://127.0.0.1:3000/planet
	http    *http.Client
	// maxTileBytes caps a single tile read to protect the bundle memory budget.
	maxTileBytes int64
}

// Config configures the Martin client.
type Config struct {
	BaseURL      string
	Timeout      time.Duration
	MaxTileBytes int64
	// Transport is optional; tests inject a fake. Defaults to a pooled transport.
	Transport http.RoundTripper
}

// New builds a Martin client. It trims a trailing slash so path joins are exact.
func New(cfg Config) *Client {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.MaxTileBytes <= 0 {
		cfg.MaxTileBytes = 16 * 1024 * 1024
	}
	tr := cfg.Transport
	if tr == nil {
		tr = &http.Transport{
			MaxIdleConns:        64,
			MaxIdleConnsPerHost: 64,
			IdleConnTimeout:     90 * time.Second,
		}
	}
	return &Client{
		baseURL:      strings.TrimRight(cfg.BaseURL, "/"),
		http:         &http.Client{Timeout: cfg.Timeout, Transport: tr},
		maxTileBytes: cfg.MaxTileBytes,
	}
}

// tileURL reconstructs the internal Martin URL from already-validated integers.
// It never interpolates raw request path text.
func (c *Client) tileURL(z, x, y int) string {
	return fmt.Sprintf("%s/%d/%d/%d", c.baseURL, z, x, y)
}

// GetTile fetches one MVT tile. It returns ErrEmpty for 204/404 so both the
// coarse and bundle paths share sparse-tile handling.
func (c *Client) GetTile(ctx context.Context, z, x, y int) (*TileResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.tileURL(z, x, y), nil)
	if err != nil {
		return nil, err
	}
	// Asking explicitly disables net/http's transparent decompression, so the
	// stored gzip bytes and their Content-Encoding reach the caller intact.
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotFound {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, ErrEmpty
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("martin: unexpected status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, c.maxTileBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > c.maxTileBytes {
		return nil, fmt.Errorf("martin: tile exceeds %d bytes", c.maxTileBytes)
	}
	if len(body) == 0 {
		return nil, ErrEmpty
	}
	return &TileResponse{
		Status:          resp.StatusCode,
		Body:            body,
		ContentType:     resp.Header.Get("Content-Type"),
		ContentEncoding: resp.Header.Get("Content-Encoding"),
		ETag:            resp.Header.Get("ETag"),
		CacheControl:    resp.Header.Get("Cache-Control"),
	}, nil
}

// RawBody returns the tile bytes with any gzip transfer encoding removed.
// The inflated size is bounded by limit; larger tiles are an error.
func (t *TileResponse) RawBody(limit int64) ([]byte, error) {
	switch t.ContentEncoding {
	case "", "identity":
		return t.Body, nil
	case "gzip":
	default:
		return nil, fmt.Errorf("martin: unsupported content encoding %q", t.ContentEncoding)
	}
	zr, err := gzip.NewReader(bytes.NewReader(t.Body))
	if err != nil {
		return nil, fmt.Errorf("martin: gzip: %w", err)
	}
	defer zr.Close()
	raw, err := io.ReadAll(io.LimitReader(zr, limit+1))
	if err != nil {
		return nil, fmt.Errorf("martin: gzip: %w", err)
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("martin: inflated tile exceeds %d bytes", limit)
	}
	return raw, nil
}

// GetTileBytes fetches a descendant for a bundle. It returns (nil, nil) for an
// empty tile so the encoder can write the sentinel. Bytes are raw MVT, which
// is what SCB1 v1/v2 entries store.
func (c *Client) GetTileBytes(ctx context.Context, t privacy.TileCoord) ([]byte, error) {
	resp, err := c.GetTile(ctx, t.Z, t.X, t.Y)
	if errors.Is(err, ErrEmpty) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	raw, err := resp.RawBody(c.maxTileBytes)
	if err != nil || len(raw) == 0 {
		return nil, err
	}
	return raw, nil
}

// GetTileStored returns the tile as the upstream stores it plus whether it
// is a complete gzip member. (nil, false, nil) means an empty tile.
func (c *Client) GetTileStored(ctx context.Context, t privacy.TileCoord) ([]byte, bool, error) {
	resp, err := c.GetTile(ctx, t.Z, t.X, t.Y)
	if errors.Is(err, ErrEmpty) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	switch resp.ContentEncoding {
	case "", "identity":
		return resp.Body, false, nil
	case "gzip":
		return resp.Body, true, nil
	default:
		return nil, false, fmt.Errorf("martin: unsupported content encoding %q", resp.ContentEncoding)
	}
}

// Healthy reports whether the Martin catalog is reachable over localhost and
// lists the expected source. Used by /readyz; never exposed publicly.
func (c *Client) Healthy(ctx context.Context, catalogURL, source string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, catalogURL, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("martin catalog status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if source != "" && !strings.Contains(string(body), source) {
		return fmt.Errorf("martin catalog missing source %q", source)
	}
	return nil
}

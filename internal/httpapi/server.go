// Package httpapi registers the public routes and enforces the privacy boundary
// before any proxy behavior. The coarse handler parses and validates the route
// itself — there is no catch-all proxy whose behavior configuration could widen.
package httpapi

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/junephilip/streetcryptid-map-server/internal/cache"
	"github.com/junephilip/streetcryptid-map-server/internal/martin"
	"github.com/junephilip/streetcryptid-map-server/internal/privacy"
	"github.com/junephilip/streetcryptid-map-server/internal/scb1"
)

// TileBundleMediaType is the SCB1 response content type, matching the app.
const TileBundleMediaType = "application/vnd.streetcryptid.tile-bundle"

// Config configures the HTTP API. It carries operational values only; the
// privacy constants live in package privacy and are not reachable from here.
type Config struct {
	MartinBaseURL    string
	MartinCatalogURL string
	Source           string // Martin source id, e.g. "planet"
	DatasetVersion   string // derived from the active signed manifest
	BundleWorkers    int
	BundleTimeout    time.Duration
	// RatePerSec/Burst tune the per-client token bucket. Zero disables limits.
	RatePerSec float64
	Burst      float64
	Logger     *slog.Logger
	Now        func() time.Time
}

// Server holds handler dependencies.
type Server struct {
	cfg     Config
	client  *martin.Client
	cache   *cache.Cache
	metrics *Metrics
	limiter *rateLimiter
	log     *slog.Logger
	workers chan struct{} // global concurrency bound across all bundle builds
}

// New wires a server. workers bounds total concurrent upstream reads.
func New(cfg Config, client *martin.Client, c *cache.Cache) *Server {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.BundleWorkers <= 0 {
		cfg.BundleWorkers = 16
	}
	if cfg.BundleTimeout <= 0 {
		cfg.BundleTimeout = 60 * time.Second
	}
	m := newMetrics()
	s := &Server{
		cfg:     cfg,
		client:  client,
		cache:   c,
		metrics: m,
		log:     cfg.Logger,
		workers: make(chan struct{}, cfg.BundleWorkers),
	}
	if cfg.RatePerSec > 0 {
		s.limiter = newRateLimiter(cfg.RatePerSec, cfg.Burst, cfg.Now)
	}
	if c != nil {
		m.registerGauge("mapapi_cache_bytes", func() float64 { return float64(c.Bytes()) })
	}
	return s
}

// Handler returns the fully routed handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// Bundle pattern is more specific (6 segments) than coarse (4); no overlap.
	mux.HandleFunc("GET /planet/bundle/v1/{x10}/{y10}/{tileZoom}", s.handleBundle)
	mux.HandleFunc("GET /planet/{z}/{x}/{y}", s.handleCoarse)
	mux.HandleFunc("HEAD /planet/{z}/{x}/{y}", s.handleCoarse)
	mux.HandleFunc("GET /livez", s.handleLivez)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	return mux
}

// StartSweeper runs the rate-limiter memory sweep until ctx is done.
func (s *Server) StartSweeper(ctx context.Context) {
	if s.limiter == nil {
		return
	}
	go func() {
		t := time.NewTicker(5 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.limiter.sweep()
			}
		}
	}()
}

// --- Coarse XYZ ---

func (s *Server) handleCoarse(w http.ResponseWriter, r *http.Request) {
	// Parse zoom first. A blocked fine request must not have its x/y parsed,
	// logged, or validated — the boundary executes before anything else.
	z, err := strconv.Atoi(r.PathValue("z"))
	if err != nil {
		http.Error(w, "bad zoom", http.StatusBadRequest)
		return
	}
	if !privacy.AllowRawZoom(z) {
		// z11..z14: 404 without contacting Martin and without logging x/y.
		s.metrics.inc("mapapi_coarse_blocked_total")
		http.NotFound(w, r)
		return
	}
	x, err := strconv.Atoi(r.PathValue("x"))
	if err != nil {
		http.Error(w, "bad x", http.StatusBadRequest)
		return
	}
	y, err := strconv.Atoi(r.PathValue("y"))
	if err != nil {
		http.Error(w, "bad y", http.StatusBadRequest)
		return
	}
	if err := privacy.ValidateRawXYZ(z, x, y); err != nil {
		http.NotFound(w, r)
		return
	}
	if s.limiter != nil && !s.limiter.allow(clientIP(r), 1) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	resp, err := s.client.GetTile(ctx, z, x, y)
	if errors.Is(err, martin.ErrEmpty) {
		s.metrics.inc("mapapi_coarse_empty_total")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		s.metrics.inc("mapapi_coarse_error_total")
		s.log.Warn("coarse upstream failure", "zoom", z, "err", err.Error())
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	s.metrics.inc("mapapi_coarse_ok_total")
	copyHeader(w, "Content-Type", resp.ContentType)
	copyHeader(w, "Content-Encoding", resp.ContentEncoding)
	copyHeader(w, "ETag", resp.ETag)
	copyHeader(w, "Cache-Control", resp.CacheControl)
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	w.Write(resp.Body)
}

// --- Fine-detail privacy bundle ---

func (s *Server) handleBundle(w http.ResponseWriter, r *http.Request) {
	// Reject any request carrying query params, a body, or child hints. The
	// endpoint accepts only the fixed anchor and data zoom.
	if len(r.URL.RawQuery) != 0 {
		http.Error(w, "no query parameters allowed", http.StatusBadRequest)
		return
	}
	x10, err1 := strconv.Atoi(r.PathValue("x10"))
	y10, err2 := strconv.Atoi(r.PathValue("y10"))
	tz, err3 := strconv.Atoi(r.PathValue("tileZoom"))
	if err1 != nil || err2 != nil || err3 != nil {
		http.Error(w, "bad bundle path", http.StatusBadRequest)
		return
	}
	req, err := privacy.ValidateBundle(x10, y10, tz)
	if err != nil {
		http.Error(w, "invalid bundle request", http.StatusBadRequest)
		return
	}

	cost := float64(uint(1) << uint(tz-privacy.PrivacyAnchorZoom)) // 2,4,8,16
	if s.limiter != nil && !s.limiter.allow(clientIP(r), cost) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
		return
	}

	etag := fmt.Sprintf("%q", fmt.Sprintf("%s:10:%d:%d:%d", s.cfg.DatasetVersion, x10, y10, tz))
	if match := r.Header.Get("If-None-Match"); match != "" && match == etag {
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusNotModified)
		return
	}

	key := fmt.Sprintf("%s:%d:%d:%d", s.cfg.DatasetVersion, x10, y10, tz)

	// Cache hit: serve the stored gzipped bundle directly.
	if s.cache != nil {
		if data, ok := s.cache.Get(key); ok {
			s.metrics.inc("mapapi_bundle_cache_hit_total")
			s.writeBundle(w, etag, data)
			return
		}
	}

	// Build once per key even under concurrent callers.
	build := func() ([]byte, error) {
		gz, err := s.buildBundle(r.Context(), req)
		if err != nil {
			return nil, err
		}
		if s.cache != nil {
			s.cache.Put(key, gz)
		}
		return gz, nil
	}

	var data []byte
	if s.cache != nil {
		data, err = s.cache.Do(key, build)
	} else {
		data, err = build()
	}
	if err != nil {
		s.metrics.inc("mapapi_bundle_error_total")
		// Log without any child coordinate label; anchor+zoom only.
		s.log.Warn("bundle build failed", "tileZoom", tz, "err", err.Error())
		status := http.StatusBadGateway
		if errors.Is(err, context.DeadlineExceeded) {
			status = http.StatusServiceUnavailable
		}
		http.Error(w, "bundle unavailable", status)
		return
	}
	s.metrics.inc("mapapi_bundle_ok_total")
	s.writeBundle(w, etag, data)
}

func (s *Server) writeBundle(w http.ResponseWriter, etag string, gz []byte) {
	h := w.Header()
	h.Set("Content-Type", TileBundleMediaType)
	h.Set("Content-Encoding", "gzip")
	h.Set("ETag", etag)
	h.Set("Cache-Control", "public, max-age=86400")
	h.Set("Content-Length", strconv.Itoa(len(gz)))
	w.WriteHeader(http.StatusOK)
	w.Write(gz)
}

// buildBundle fetches the complete fixed descendant set with bounded
// concurrency, encodes deterministic SCB1, and gzips it. It never returns a
// partial bundle: any non-empty Martin failure fails the whole build. A client
// disconnect does not abort the build — members are shared and worth caching —
// but it also never commits a partial cache entry because build runs to
// completion or errors atomically.
func (s *Server) buildBundle(reqCtx context.Context, req privacy.BundleRequest) ([]byte, error) {
	// Detach from the client request so one client leaving does not cancel a
	// shared build, but keep a bounded overall deadline.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(reqCtx), s.cfg.BundleTimeout)
	defer cancel()

	tiles := req.Descendants()
	entries := make([]scb1.Entry, len(tiles))

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)
	sem := make(chan struct{}, s.cfg.BundleWorkers)

	for i, t := range tiles {
		wg.Add(1)
		go func(i int, t privacy.TileCoord) {
			defer wg.Done()
			// Two-level bound: per-request workers and a global cap.
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				mu.Lock()
				if firstErr == nil {
					firstErr = ctx.Err()
				}
				mu.Unlock()
				return
			}
			defer func() { <-sem }()
			select {
			case s.workers <- struct{}{}:
			case <-ctx.Done():
				mu.Lock()
				if firstErr == nil {
					firstErr = ctx.Err()
				}
				mu.Unlock()
				return
			}
			defer func() { <-s.workers }()

			bytes, err := s.client.GetTileBytes(ctx, t)
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
					cancel() // stop the rest; the whole bundle fails
				}
				mu.Unlock()
				return
			}
			entries[i] = scb1.Entry{Bytes: bytes} // nil bytes => empty sentinel
		}(i, t)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}

	raw, err := scb1.Encode(req, entries)
	if err != nil {
		return nil, err
	}
	return gzipBytes(raw)
}

func gzipBytes(raw []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// --- Health & metrics ---

func (s *Server) handleLivez(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte("ok"))
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := s.client.Healthy(ctx, s.cfg.MartinCatalogURL, s.cfg.Source); err != nil {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	w.Write([]byte("ready"))
}

func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	w.Write([]byte(s.metrics.render()))
}

func copyHeader(w http.ResponseWriter, key, val string) {
	if val != "" {
		w.Header().Set(key, val)
	}
}

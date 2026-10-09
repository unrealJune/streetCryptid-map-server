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
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/junephilip/streetcryptid-map-server/internal/cache"
	"github.com/junephilip/streetcryptid-map-server/internal/martin"
	"github.com/junephilip/streetcryptid-map-server/internal/privacy"
	"github.com/junephilip/streetcryptid-map-server/internal/scb1"
	"github.com/junephilip/streetcryptid-map-server/internal/scb2"
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
	DatasetDigest    string // verified artifact SHA-256; protects v2 against reused release labels
	BundleWorkers    int
	BundleTimeout    time.Duration
	BundleBuilds     int // concurrent complete builds, not individual tile reads
	BundleSource     TileSource
	// RatePerSec/Burst tune the per-client token bucket. Zero disables limits.
	RatePerSec float64
	Burst      float64
	Logger     *slog.Logger
	Now        func() time.Time
}

// TileSource returns transfer-decoded MVT or nil for a known-empty tile.
type TileSource interface {
	GetTileBytes(context.Context, privacy.TileCoord) ([]byte, error)
}

// StoredTileSource returns tiles as stored plus whether they are gzip members.
type StoredTileSource interface {
	GetTileStored(context.Context, privacy.TileCoord) ([]byte, bool, error)
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
	builds  chan struct{}
	source  TileSource
	stored  StoredTileSource // nil: v3 is unavailable (501)
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
	if cfg.BundleBuilds <= 0 {
		cfg.BundleBuilds = 1
	}
	if cfg.BundleSource == nil {
		cfg.BundleSource = client
	}
	if c == nil {
		c, _ = cache.New("", 0)
	}
	m := newMetrics()
	s := &Server{
		cfg:     cfg,
		client:  client,
		cache:   c,
		metrics: m,
		log:     cfg.Logger,
		workers: make(chan struct{}, cfg.BundleWorkers),
		builds:  make(chan struct{}, cfg.BundleBuilds),
		source:  cfg.BundleSource,
	}
	if stored, ok := cfg.BundleSource.(StoredTileSource); ok {
		s.stored = stored
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
	mux.HandleFunc("GET /planet/bundle/v2/{x10}/{y10}/{tileZoom}", s.handleBundleV2)
	mux.HandleFunc("OPTIONS /planet/bundle/v2/{x10}/{y10}/{tileZoom}", s.handleBundleOptions)
	mux.HandleFunc("GET /planet/bundle/v3/{x10}/{y10}/{tileZoom}", s.handleBundleV3)
	mux.HandleFunc("OPTIONS /planet/bundle/v3/{x10}/{y10}/{tileZoom}", s.handleBundleOptions)
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
	body, encoding := resp.Body, resp.ContentEncoding
	if encoding == "gzip" {
		s.metrics.inc("mapapi_coarse_upstream_gzip_total")
		// Real clients accept gzip; this keeps a plain curl working.
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			body, err = resp.RawBody(16 << 20)
			if err != nil {
				s.metrics.inc("mapapi_coarse_error_total")
				s.log.Warn("coarse inflate failure", "zoom", z, "err", err.Error())
				http.Error(w, "upstream error", http.StatusBadGateway)
				return
			}
			encoding = ""
			s.metrics.inc("mapapi_coarse_inflated_total")
		}
	}
	h := w.Header()
	copyHeader(w, "Content-Type", resp.ContentType)
	copyHeader(w, "Content-Encoding", encoding)
	copyHeader(w, "ETag", resp.ETag)
	h.Set("Vary", "Accept-Encoding")
	// Martin's ETag changes on every rebake, so a day is safe.
	h.Set("Cache-Control", "public, max-age=86400")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	w.Write(body)
}

// --- Fine-detail privacy bundle ---

func (s *Server) bundleRequest(w http.ResponseWriter, r *http.Request) (privacy.BundleRequest, bool) {
	// Reject any request carrying query params, a body, or child hints. The
	// endpoint accepts only the fixed anchor and data zoom.
	if r.URL.ForceQuery || len(r.URL.RawQuery) != 0 || r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
		http.Error(w, "no query parameters or body allowed", http.StatusBadRequest)
		return privacy.BundleRequest{}, false
	}
	x10, err1 := strconv.Atoi(r.PathValue("x10"))
	y10, err2 := strconv.Atoi(r.PathValue("y10"))
	tz, err3 := strconv.Atoi(r.PathValue("tileZoom"))
	if err1 != nil || err2 != nil || err3 != nil {
		http.Error(w, "bad bundle path", http.StatusBadRequest)
		return privacy.BundleRequest{}, false
	}
	req, err := privacy.ValidateBundle(x10, y10, tz)
	if err != nil {
		http.Error(w, "invalid bundle request", http.StatusBadRequest)
		return privacy.BundleRequest{}, false
	}

	cost := float64(uint(1) << uint(tz-privacy.PrivacyAnchorZoom)) // 2,4,8,16
	if s.limiter != nil && !s.limiter.allow(clientIP(r), cost) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
		return privacy.BundleRequest{}, false
	}
	return req, true
}

func (s *Server) stageKey(req privacy.BundleRequest) string {
	return fmt.Sprintf("%s:%s:%s:10:%d:%d:%d:stage", scb2.CodecVersion, url.PathEscape(s.cfg.DatasetVersion), s.cfg.DatasetDigest, req.AnchorX, req.AnchorY, req.TileZoom)
}

var errBuildBusy = errors.New("bundle build admission full")

func (s *Server) admitted(build func() ([]byte, error)) ([]byte, error) {
	select {
	case s.builds <- struct{}{}:
		defer func() { <-s.builds }()
		return build()
	default:
		return nil, errBuildBusy
	}
}

// cachedStage reads a persisted stage. A missing or corrupt entry is a miss.
func (s *Server) cachedStage(key string) ([]byte, bool, error) {
	f, err := s.cache.Open(key)
	if err == nil {
		data, readErr := io.ReadAll(f)
		if err := errors.Join(readErr, f.Close()); err != nil {
			return nil, false, err
		}
		return data, true, nil
	}
	if errors.Is(err, cache.ErrCorrupt) {
		s.log.Warn("discarded corrupt bundle stage; rebuilding", "err", err)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, false, fmt.Errorf("read bundle stage: %w", err)
	}
	return nil, false, nil
}

func (s *Server) stage(ctx context.Context, req privacy.BundleRequest) ([]byte, error) {
	key := s.stageKey(req)
	return s.cache.Do(key, func() ([]byte, error) {
		if data, ok, err := s.cachedStage(key); ok || err != nil {
			return data, err
		}
		data, err := s.buildBundle(ctx, req)
		if err != nil {
			return nil, err
		}
		if err := s.cache.Put(key, data); err != nil {
			return nil, fmt.Errorf("persist bundle stage: %w", err)
		}
		return data, nil
	})
}

func (s *Server) buildError(w http.ResponseWriter, req privacy.BundleRequest, err error) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Del("ETag")
	w.Header().Del("Accept-Ranges")
	s.metrics.inc("mapapi_bundle_error_total")
	s.log.Warn("bundle build failed", "tileZoom", req.TileZoom, "err", err)
	status := http.StatusBadGateway
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errBuildBusy) {
		status = http.StatusServiceUnavailable
		w.Header().Set("Retry-After", "2")
	}
	http.Error(w, "bundle unavailable", status)
}

func (s *Server) handleBundle(w http.ResponseWriter, r *http.Request) {
	req, ok := s.bundleRequest(w, r)
	if !ok {
		return
	}
	etag := fmt.Sprintf("%q", fmt.Sprintf("%s:10:%d:%d:%d", s.cfg.DatasetVersion, req.AnchorX, req.AnchorY, req.TileZoom))
	if match := r.Header.Get("If-None-Match"); match != "" && match == etag {
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusNotModified)
		return
	}

	key := s.stageKey(req)

	// Cache hit: serve the stored gzipped bundle directly.
	if f, err := s.cache.Open(key); err == nil {
		defer f.Close()
		s.metrics.inc("mapapi_bundle_cache_hit_total")
		s.bundleHeaders(w, etag, f.Size())
		if r.Method != http.MethodHead {
			if _, err := io.Copy(w, f); err != nil {
				s.log.Warn("bundle response interrupted", "err", err)
			}
		}
		return
	} else if !errors.Is(err, os.ErrNotExist) {
		s.log.Error("bundle cache read failed", "err", err)
		if !errors.Is(err, cache.ErrCorrupt) {
			s.buildError(w, req, err)
			return
		}
	}

	// Build once per key even under concurrent callers.
	data, err := s.cache.Do(key+":request", func() ([]byte, error) {
		return s.admitted(func() ([]byte, error) {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), s.cfg.BundleTimeout)
			defer cancel()
			return s.stage(ctx, req)
		})
	})
	if err != nil {
		s.buildError(w, req, err)
		return
	}
	s.metrics.inc("mapapi_bundle_ok_total")
	// Release the assembled buffer before a potentially slow transfer.
	if f, err := s.cache.Open(key); err == nil {
		data = nil
		defer f.Close()
		s.bundleHeaders(w, etag, f.Size())
		if r.Method != http.MethodHead {
			if _, err := io.Copy(w, f); err != nil {
				s.log.Warn("bundle response interrupted", "err", err)
			}
		}
		return
	} else if !errors.Is(err, os.ErrNotExist) {
		s.buildError(w, req, err)
		return
	}
	// When persistence is disabled/too small, count an in-memory transfer
	// against the build budget so slow readers cannot retain unbounded bundles.
	select {
	case s.builds <- struct{}{}:
		defer func() { <-s.builds }()
	default:
		s.buildError(w, req, errBuildBusy)
		return
	}
	s.bundleHeaders(w, etag, int64(len(data)))
	if r.Method != http.MethodHead {
		w.Write(data)
	}
}

func (s *Server) bundleHeaders(w http.ResponseWriter, etag string, size int64) {
	h := w.Header()
	h.Set("Content-Type", TileBundleMediaType)
	h.Set("Content-Encoding", "gzip")
	h.Set("ETag", etag)
	h.Set("Cache-Control", "public, max-age=86400")
	h.Set("Content-Length", strconv.FormatInt(size, 10))
	w.WriteHeader(http.StatusOK)
}

// eachTile runs fn for every tile under a two-level bound: per-request workers
// and the global cap. The first error cancels the remaining reads and is
// returned, so callers never see a partial descendant set.
func (s *Server) eachTile(reqCtx context.Context, tiles []privacy.TileCoord, fn func(ctx context.Context, i int, t privacy.TileCoord) error) error {
	ctx, cancel := context.WithCancel(reqCtx)
	defer cancel()

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)
	fail := func(err error) {
		mu.Lock()
		defer mu.Unlock()
		if firstErr == nil {
			firstErr = err
			cancel() // stop the rest; the whole bundle fails
		}
	}
	sem := make(chan struct{}, s.cfg.BundleWorkers)

	for i, t := range tiles {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				fail(ctx.Err())
				return
			}
			defer func() { <-sem }()
			select {
			case s.workers <- struct{}{}:
			case <-ctx.Done():
				fail(ctx.Err())
				return
			}
			defer func() { <-s.workers }()
			if err := fn(ctx, i, t); err != nil {
				fail(err)
			}
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	return reqCtx.Err()
}

// buildBundle fetches the complete fixed descendant set with bounded
// concurrency, encodes deterministic SCB1, and gzips it. It never returns a
// partial bundle: any non-empty Martin failure fails the whole build. A client
// disconnect does not abort the build — members are shared and worth caching —
// but it also never commits a partial cache entry because build runs to
// completion or errors atomically.
func (s *Server) buildBundle(ctx context.Context, req privacy.BundleRequest) ([]byte, error) {
	tiles := req.Descendants()
	entries := make([]scb1.Entry, len(tiles))
	var (
		mu    sync.Mutex
		total = scb1.HeaderBytes + req.EntryCount()*4
	)
	err := s.eachTile(ctx, tiles, func(ctx context.Context, i int, t privacy.TileCoord) error {
		payload, err := s.source.GetTileBytes(ctx, t)
		if err != nil {
			return err
		}
		mu.Lock()
		defer mu.Unlock()
		if len(payload) > scb1.MaxDecompressedBytes-total {
			return scb1.ErrTooLarge
		}
		total += len(payload)
		entries[i] = scb1.Entry{Bytes: payload}
		return nil
	})
	if err != nil {
		return nil, err
	}

	raw, err := scb1.Encode(req, entries)
	if err != nil {
		return nil, err
	}
	gz, err := gzipBytes(raw)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return gz, nil
}

func streamHeaders(w http.ResponseWriter, mediaType, etag string) {
	h := w.Header()
	h.Set("Content-Type", mediaType)
	h.Set("Cache-Control", "public, max-age=86400, no-transform")
	h.Set("ETag", etag)
	h.Set("Accept-Ranges", "bytes")
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Expose-Headers", "ETag, Accept-Ranges, Content-Range, Content-Length")
	h.Set("X-Accel-Buffering", "no")
}

func (s *Server) handleBundleOptions(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.bundleRequest(w, r); !ok {
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Range, If-Range, If-None-Match")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleBundleV2(w http.ResponseWriter, r *http.Request) {
	req, ok := s.bundleRequest(w, r)
	if !ok {
		return
	}
	stages := scb2.Stages(req)
	s.serveStream(w, r, req, streamSpec{
		key:       fmt.Sprintf("%s:%s:%s:10:%d:%d:%d", scb2.CodecVersion, url.PathEscape(s.cfg.DatasetVersion), s.cfg.DatasetDigest, req.AnchorX, req.AnchorY, req.TileZoom),
		mediaType: scb2.MediaType,
		header:    scb2.Header(req),
		stages:    len(stages),
		maxBytes:  scb2.MaxStreamBytes,
		frame: func(ctx context.Context, i int) ([]byte, error) {
			gz, err := s.stage(ctx, stages[i])
			if err != nil {
				return nil, err
			}
			return scb2.Frame(stages[i], gz)
		},
	})
}

// streamSpec describes one progressive representation: a fixed header followed
// by frames that are built, in order, inside a single admitted build.
type streamSpec struct {
	key       string // persistent namespace; the quoted key is the strong ETag
	mediaType string
	header    []byte
	stages    int
	maxBytes  int
	frame     func(ctx context.Context, i int) ([]byte, error)
}

// serveStream serves a complete cached stream with Range support, or builds it
// once, flushing each frame to a plain GET as soon as it exists. Only a
// complete stream enters the cache.
func (s *Server) serveStream(w http.ResponseWriter, r *http.Request, req privacy.BundleRequest, spec streamSpec) {
	key := spec.key
	etag := fmt.Sprintf("%q", key)
	streamHeaders(w, spec.mediaType, etag)
	// Only single byte ranges are supported. Ignore multi-range requests rather
	// than emitting a multipart representation or amplifying overlapping ranges.
	if strings.Contains(r.Header.Get("Range"), ",") {
		r = r.Clone(r.Context())
		r.Header.Del("Range")
	}
	if f, err := s.cache.Open(key); err == nil {
		defer f.Close()
		s.metrics.inc("mapapi_bundle_cache_hit_total")
		serveStreamContent(w, r, etag, f)
		return
	} else if !errors.Is(err, os.ErrNotExist) {
		s.log.Error("bundle cache read failed", "err", err)
		if !errors.Is(err, cache.ErrCorrupt) {
			s.buildError(w, req, err)
			return
		}
	}

	streamed := false
	writeFailed := false
	// Conditional requests use ServeContent after materialization so Go applies
	// all preconditions, including strong If-Range semantics, consistently.
	progressive := r.Method == http.MethodGet && r.Header.Get("Range") == "" &&
		r.Header.Get("If-None-Match") == "" && r.Header.Get("If-Match") == ""
	data, err := s.cache.Do(key, func() ([]byte, error) {
		return s.admitted(func() ([]byte, error) {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), s.cfg.BundleTimeout)
			defer cancel()
			var out bytes.Buffer
			out.Write(spec.header)
			for i := range spec.stages {
				frame, err := spec.frame(ctx, i)
				if err != nil {
					return nil, err
				}
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				if out.Len()+len(frame) > spec.maxBytes {
					return nil, errors.New("bundle stream exceeds bound")
				}
				out.Write(frame)
				if progressive && !writeFailed {
					streamed = true
					controller := http.NewResponseController(w)
					// A slow/disconnected subscriber must not wedge the shared build.
					if err := controller.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil && !errors.Is(err, http.ErrNotSupported) {
						s.log.Warn("stream deadline failed", "err", err)
						writeFailed = true
					}
					chunk := frame
					if i == 0 {
						chunk = out.Bytes()
					}
					if !writeFailed {
						_, err = w.Write(chunk)
						if err == nil {
							err = controller.Flush()
						}
						if err != nil {
							s.log.Warn("bundle stream interrupted; build continues", "err", err)
							writeFailed = true
						}
						// HTTP/2 write deadlines can expire even between writes.
						// Restore the overall deadline while detail is being built.
						deadline, _ := ctx.Deadline()
						if err := controller.SetWriteDeadline(deadline); err != nil && !errors.Is(err, http.ErrNotSupported) {
							s.log.Warn("stream deadline restore failed", "err", err)
							writeFailed = true
						}
					}
				}
				// Each frame is flushed above BEFORE the next stage is built.
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if err := s.cache.Put(key, out.Bytes()); err != nil {
				return nil, fmt.Errorf("persist stream: %w", err)
			}
			return out.Bytes(), nil
		})
	})
	if err != nil {
		if streamed {
			s.metrics.inc("mapapi_bundle_error_total")
			s.log.Warn("bundle stream aborted", "tileZoom", req.TileZoom, "err", err)
			panic(http.ErrAbortHandler)
		}
		s.buildError(w, req, err)
		return
	}
	s.metrics.inc("mapapi_bundle_ok_total")
	if streamed {
		if writeFailed {
			panic(http.ErrAbortHandler)
		}
		return
	}
	if f, err := s.cache.Open(key); err == nil {
		data = nil
		defer f.Close()
		serveStreamContent(w, r, etag, f)
		return
	} else if !errors.Is(err, os.ErrNotExist) {
		s.buildError(w, req, err)
		return
	}
	select {
	case s.builds <- struct{}{}:
		defer func() { <-s.builds }()
	default:
		s.buildError(w, req, errBuildBusy)
		return
	}
	serveStreamContent(w, r, etag, bytes.NewReader(data))
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

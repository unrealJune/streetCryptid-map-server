package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/junephilip/streetcryptid-map-server/internal/privacy"
	"github.com/junephilip/streetcryptid-map-server/internal/scb1"
)

// TerrainSource reads terrain-RGB raster tiles (the GLO-30 DEM) exactly as
// stored. Raster tiles are already compressed, so stored bytes are the payload.
type TerrainSource interface {
	GetTileBytes(context.Context, privacy.TileCoord) ([]byte, error)
}

// Terrain is the optional DEM the app shades parkland from. A server without
// one answers 404 on every terrain route, which the app treats as "no tile"
// and draws its canopy fallback.
type Terrain struct {
	Source TerrainSource
	// ContentType of every tile, e.g. "image/webp".
	ContentType string
	// Version names the terrain release; it keys the ETags.
	Version string
	// MaxZoom is the archive's deepest zoom (≤ privacy.MaxTerrainZoom).
	MaxZoom int
}

// terrainTimeout bounds one terrain request: a bundle is at most 16 local reads.
const terrainTimeout = 15 * time.Second

func (s *Server) terrainEnabled(w http.ResponseWriter, r *http.Request) bool {
	if s.cfg.Terrain == nil || s.cfg.Terrain.Source == nil {
		http.NotFound(w, r)
		return false
	}
	return true
}

// handleTerrainCoarse serves one public (≤ z10) terrain tile. The privacy gate
// runs before anything else, exactly as on the planet's coarse route.
func (s *Server) handleTerrainCoarse(w http.ResponseWriter, r *http.Request) {
	z, err := strconv.Atoi(r.PathValue("z"))
	if err != nil {
		http.Error(w, "bad zoom", http.StatusBadRequest)
		return
	}
	if !privacy.AllowRawZoom(z) {
		s.metrics.inc("mapapi_terrain_blocked_total")
		http.NotFound(w, r)
		return
	}
	x, errX := strconv.Atoi(r.PathValue("x"))
	y, errY := strconv.Atoi(r.PathValue("y"))
	if errX != nil || errY != nil {
		http.Error(w, "bad tile", http.StatusBadRequest)
		return
	}
	if err := privacy.ValidateRawXYZ(z, x, y); err != nil {
		http.NotFound(w, r)
		return
	}
	if !s.terrainEnabled(w, r) {
		return
	}
	t := s.cfg.Terrain
	if z > t.MaxZoom {
		http.NotFound(w, r)
		return
	}
	if s.limiter != nil && !s.limiter.allow(clientIP(r), 1) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
		return
	}
	etag := fmt.Sprintf("%q", fmt.Sprintf("terrain:%s:%d:%d:%d", t.Version, z, x, y))
	if match := r.Header.Get("If-None-Match"); match != "" && match == etag {
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusNotModified)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), terrainTimeout)
	defer cancel()
	tile, err := t.Source.GetTileBytes(ctx, privacy.TileCoord{Z: z, X: x, Y: y})
	if err != nil {
		s.metrics.inc("mapapi_terrain_error_total")
		s.log.Warn("terrain read failure", "zoom", z, "err", err.Error())
		http.Error(w, "terrain read failed", http.StatusBadGateway)
		return
	}
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Cache-Control", "public, max-age=86400")
	h.Set("ETag", etag)
	if tile == nil {
		// Open sea: the DEM has no tile, and that is cacheable.
		s.metrics.inc("mapapi_terrain_empty_total")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.metrics.inc("mapapi_terrain_ok_total")
	h.Set("Content-Type", t.ContentType)
	h.Set("Content-Length", strconv.Itoa(len(tile)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		w.Write(tile)
	}
}

// handleTerrainBundle serves every terrain tile under one z10 anchor at a z11 or
// z12 data zoom as an SCB1 bundle (raw entries: each is an image), so a fine
// terrain request reveals no more than a fine vector request does.
func (s *Server) handleTerrainBundle(w http.ResponseWriter, r *http.Request) {
	x10, errX := strconv.Atoi(r.PathValue("x10"))
	y10, errY := strconv.Atoi(r.PathValue("y10"))
	tileZoom, errZ := strconv.Atoi(r.PathValue("tileZoom"))
	if errX != nil || errY != nil || errZ != nil {
		http.Error(w, "bad bundle", http.StatusBadRequest)
		return
	}
	req, err := privacy.ValidateTerrainBundle(x10, y10, tileZoom)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if !s.terrainEnabled(w, r) {
		return
	}
	t := s.cfg.Terrain
	if req.TileZoom > t.MaxZoom {
		http.NotFound(w, r)
		return
	}
	// Same cost as a planet bundle of this depth.
	if s.limiter != nil && !s.limiter.allow(clientIP(r), float64(uint(1)<<uint(req.TileZoom-privacy.PrivacyAnchorZoom))) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
		return
	}
	etag := fmt.Sprintf("%q", fmt.Sprintf("terrain:%s:10:%d:%d:%d", t.Version, req.AnchorX, req.AnchorY, req.TileZoom))
	if match := r.Header.Get("If-None-Match"); match != "" && match == etag {
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusNotModified)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), terrainTimeout)
	defer cancel()
	tiles := req.Descendants()
	entries := make([]scb1.Entry, len(tiles))
	err = s.eachTile(ctx, tiles, func(ctx context.Context, i int, tc privacy.TileCoord) error {
		b, err := t.Source.GetTileBytes(ctx, tc)
		if err != nil {
			return err
		}
		entries[i] = scb1.Entry{Bytes: b}
		return nil
	})
	if err != nil {
		s.metrics.inc("mapapi_terrain_error_total")
		if errors.Is(err, context.DeadlineExceeded) {
			http.Error(w, "terrain bundle timed out", http.StatusGatewayTimeout)
			return
		}
		s.log.Warn("terrain bundle failure", "zoom", req.TileZoom, "err", err.Error())
		http.Error(w, "terrain read failed", http.StatusBadGateway)
		return
	}
	body, err := scb1.Encode(req, entries)
	if err != nil {
		s.metrics.inc("mapapi_terrain_error_total")
		http.Error(w, "terrain bundle too large", http.StatusInternalServerError)
		return
	}
	s.metrics.inc("mapapi_terrain_bundle_ok_total")
	h := w.Header()
	h.Set("Content-Type", TileBundleMediaType)
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Expose-Headers", "ETag, Content-Length")
	h.Set("Cache-Control", "public, max-age=86400")
	h.Set("ETag", etag)
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		w.Write(body)
	}
}

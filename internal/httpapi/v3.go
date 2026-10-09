package httpapi

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"

	"github.com/junephilip/streetcryptid-map-server/internal/mvt"
	"github.com/junephilip/streetcryptid-map-server/internal/privacy"
	"github.com/junephilip/streetcryptid-map-server/internal/scb1"
	"github.com/junephilip/streetcryptid-map-server/internal/scb3"
)

func (s *Server) handleBundleV3(w http.ResponseWriter, r *http.Request) {
	req, ok := s.bundleRequest(w, r)
	if !ok {
		return
	}
	if s.stored == nil {
		http.Error(w, "stored tile source unavailable", http.StatusNotImplemented)
		return
	}
	s.metrics.inc("mapapi_bundle_v3_total")
	stages := scb3.Stages(req)
	// The z14 split builds both parts at once; the build goroutine keeps the
	// part it did not ask for here so it is not rebuilt when the cache is off.
	spare := make(map[scb3.Stage][]byte)
	s.serveStream(w, r, req, streamSpec{
		key:       fmt.Sprintf("%s:%s:%s:10:%d:%d:%d", scb3.CodecVersion, url.PathEscape(s.cfg.DatasetVersion), s.cfg.DatasetDigest, req.AnchorX, req.AnchorY, req.TileZoom),
		mediaType: scb3.MediaType,
		header:    scb3.Header(req),
		stages:    len(stages),
		maxBytes:  scb3.MaxStreamBytes,
		frame: func(ctx context.Context, i int) ([]byte, error) {
			payload, err := s.v3Stage(ctx, stages[i], spare)
			if err != nil {
				return nil, err
			}
			return scb3.Frame(stages[i], payload)
		},
	})
}

func (s *Server) v3StageKey(st scb3.Stage) string {
	return fmt.Sprintf("%s:%s:%s:10:%d:%d:%d:%d:stage", scb3.CodecVersion, url.PathEscape(s.cfg.DatasetVersion), s.cfg.DatasetDigest, st.Req.AnchorX, st.Req.AnchorY, st.Req.TileZoom, st.Part)
}

// v3Stage returns one SCB1 payload with gzip-member entries, from the cache or
// built. Building a z14 part builds and caches both parts; a rare double build
// when two requests race on different parts is acceptable because Put is
// idempotent for the deterministic bytes.
func (s *Server) v3Stage(ctx context.Context, st scb3.Stage, spare map[scb3.Stage][]byte) ([]byte, error) {
	if data, ok := spare[st]; ok {
		delete(spare, st)
		return data, nil
	}
	key := s.v3StageKey(st)
	var sibling scb3.Stage
	var siblingData []byte
	data, err := s.cache.Do(key, func() ([]byte, error) {
		if data, ok, err := s.cachedStage(key); ok || err != nil {
			return data, err
		}
		if st.Part == scb3.PartFull {
			data, err := s.buildStoredStage(ctx, st.Req)
			if err != nil {
				return nil, err
			}
			if err := s.cache.Put(key, data); err != nil {
				return nil, fmt.Errorf("persist bundle stage: %w", err)
			}
			return data, nil
		}
		structure, labels, err := s.buildSplitStages(ctx, st.Req)
		if err != nil {
			return nil, err
		}
		own := structure
		sibling, siblingData = scb3.Stage{Req: st.Req, Part: scb3.PartLabels}, labels
		if st.Part == scb3.PartLabels {
			own = labels
			sibling, siblingData = scb3.Stage{Req: st.Req, Part: scb3.PartStructure}, structure
		}
		if err := s.cache.Put(key, own); err != nil {
			return nil, fmt.Errorf("persist bundle stage: %w", err)
		}
		if err := s.cache.Put(s.v3StageKey(sibling), siblingData); err != nil {
			s.log.Warn("persist sibling bundle stage failed", "err", err)
		}
		return own, nil
	})
	if err == nil && siblingData != nil && spare != nil {
		spare[sibling] = siblingData
	}
	return data, err
}

// buildStoredStage encodes the full descendant set with every non-empty entry
// a gzip member: archive bytes are copied as stored, uncompressed archives are
// gzipped here.
func (s *Server) buildStoredStage(ctx context.Context, req privacy.BundleRequest) ([]byte, error) {
	tiles := req.Descendants()
	entries := make([]scb1.Entry, len(tiles))
	var (
		mu    sync.Mutex
		total = scb1.HeaderBytes + req.EntryCount()*4
	)
	err := s.eachTile(ctx, tiles, func(ctx context.Context, i int, t privacy.TileCoord) error {
		stored, gzipped, err := s.stored.GetTileStored(ctx, t)
		if err != nil {
			return err
		}
		member, err := storedMember(stored, gzipped)
		if err != nil {
			return err
		}
		mu.Lock()
		defer mu.Unlock()
		if len(member) > scb3.MaxPayloadBytes-total {
			return scb1.ErrTooLarge
		}
		total += len(member)
		entries[i] = scb1.Entry{Bytes: member}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return scb1.EncodeFlags(req, scb1.FlagGzipEntries, entries)
}

// buildSplitStages makes one pass over the descendants and splits each tile
// by layer into the structure and labels payloads. Both cover every
// descendant in the same order; a part with no layers is the empty sentinel.
func (s *Server) buildSplitStages(ctx context.Context, req privacy.BundleRequest) (structure, labels []byte, err error) {
	tiles := req.Descendants()
	parts := [2][]scb1.Entry{make([]scb1.Entry, len(tiles)), make([]scb1.Entry, len(tiles))}
	var (
		mu     sync.Mutex
		totals = [2]int{scb1.HeaderBytes + req.EntryCount()*4, scb1.HeaderBytes + req.EntryCount()*4}
	)
	err = s.eachTile(ctx, tiles, func(ctx context.Context, i int, t privacy.TileCoord) error {
		stored, gzipped, err := s.stored.GetTileStored(ctx, t)
		if err != nil {
			return err
		}
		raw, err := inflateStored(stored, gzipped)
		if err != nil {
			return err
		}
		st, lb, err := mvt.Split(raw)
		if err != nil {
			return fmt.Errorf("split z%d tile: %w", t.Z, err)
		}
		var members [2][]byte
		for p, part := range [2][]byte{st, lb} {
			if part == nil {
				continue
			}
			if members[p], err = gzipMember(part); err != nil {
				return err
			}
		}
		mu.Lock()
		defer mu.Unlock()
		for p, member := range members {
			if len(member) > scb3.MaxPayloadBytes-totals[p] {
				return scb1.ErrTooLarge
			}
			totals[p] += len(member)
			parts[p][i] = scb1.Entry{Bytes: member}
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	if structure, err = scb1.EncodeFlags(req, scb1.FlagGzipEntries, parts[0]); err != nil {
		return nil, nil, err
	}
	if labels, err = scb1.EncodeFlags(req, scb1.FlagGzipEntries, parts[1]); err != nil {
		return nil, nil, err
	}
	return structure, labels, nil
}

// storedMember turns a stored tile into an SCB3 entry: nil for an empty tile,
// otherwise exactly one gzip member.
func storedMember(stored []byte, gzipped bool) ([]byte, error) {
	if len(stored) == 0 {
		return nil, nil
	}
	if !gzipped {
		return gzipMember(stored)
	}
	if len(stored) < 18 || stored[0] != 0x1f || stored[1] != 0x8b || stored[2] != 0x08 {
		return nil, errors.New("stored tile is not a gzip member")
	}
	// ISIZE 0 may be a stored empty tile, which v1/v2 send as the sentinel.
	// Inflating to confirm costs nothing for a genuinely empty member.
	if binary.LittleEndian.Uint32(stored[len(stored)-4:]) == 0 {
		raw, err := inflateStored(stored, true)
		if err != nil {
			return nil, err
		}
		if len(raw) == 0 {
			return nil, nil
		}
	}
	return stored, nil
}

// inflateStored returns raw MVT for a stored tile, bounded like the archive
// reader bounds decoded tiles.
func inflateStored(stored []byte, gzipped bool) ([]byte, error) {
	if !gzipped || len(stored) == 0 {
		return stored, nil
	}
	zr, err := gzip.NewReader(bytes.NewReader(stored))
	if err != nil {
		return nil, fmt.Errorf("stored tile gzip: %w", err)
	}
	defer zr.Close()
	raw, err := io.ReadAll(io.LimitReader(zr, mvt.MaxTileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("stored tile gzip: %w", err)
	}
	if len(raw) > mvt.MaxTileBytes {
		return nil, mvt.ErrTooLarge
	}
	return raw, nil
}

// gzipWriters reuses compressor state across the 512 members of a z14 split.
// Reset restores NewWriter's zero header, so output stays deterministic.
var gzipWriters = sync.Pool{New: func() any { return gzip.NewWriter(nil) }}

// gzipMember compresses one entry deterministically: default level, no name,
// no timestamp. scb3.CodecVersion names the Go version for this reason.
func gzipMember(raw []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzipWriters.Get().(*gzip.Writer)
	defer gzipWriters.Put(zw)
	zw.Reset(&buf)
	if _, err := zw.Write(raw); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

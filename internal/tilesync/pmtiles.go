package tilesync

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// pmtilesHeaderLen is the fixed PMTiles v3 header size.
const pmtilesHeaderLen = 127

// pmtilesInfo is the subset of the PMTiles v3 header and metadata this server
// verifies against the signed manifest.
type pmtilesInfo struct {
	MinZoom         int
	MaxZoom         int
	TileSchema      string // best-effort schema name from metadata
	HasVectorLayers bool
}

// readPMTilesInfo parses the PMTiles v3 header and internal metadata JSON. Only
// the fields needed to cross-check the manifest are extracted; the tile data is
// not read.
func readPMTilesInfo(path string) (*pmtilesInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	hdr := make([]byte, pmtilesHeaderLen)
	if _, err := io.ReadFull(f, hdr); err != nil {
		return nil, fmt.Errorf("pmtiles: read header: %w", err)
	}
	if string(hdr[0:7]) != "PMTiles" {
		return nil, fmt.Errorf("pmtiles: bad magic")
	}
	if hdr[7] != 3 {
		return nil, fmt.Errorf("pmtiles: unsupported version %d", hdr[7])
	}
	metaOff := binary.LittleEndian.Uint64(hdr[24:32])
	metaLen := binary.LittleEndian.Uint64(hdr[32:40])
	internalComp := hdr[97]
	minZoom := int(hdr[100])
	maxZoom := int(hdr[101])

	info := &pmtilesInfo{MinZoom: minZoom, MaxZoom: maxZoom}

	if metaLen > 0 && metaLen < 32*1024*1024 {
		raw := make([]byte, metaLen)
		if _, err := f.ReadAt(raw, int64(metaOff)); err == nil {
			if md, err := decodeMetadata(raw, internalComp); err == nil {
				info.TileSchema, info.HasVectorLayers = schemaFromMetadata(md)
			}
		}
	}
	return info, nil
}

func decodeMetadata(raw []byte, internalComp byte) ([]byte, error) {
	switch internalComp {
	case 1: // none
		return raw, nil
	case 2: // gzip
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		defer zr.Close()
		return io.ReadAll(io.LimitReader(zr, 32*1024*1024))
	default:
		// brotli/zstd not linked; treat as opaque and skip metadata checks.
		return nil, fmt.Errorf("pmtiles: unsupported internal compression %d", internalComp)
	}
}

// schemaFromMetadata pulls a schema hint out of the metadata JSON. Planetiler's
// OpenMapTiles profile emits a "name" of "OpenMapTiles" and a vector_layers
// array; either signal is accepted.
func schemaFromMetadata(md []byte) (schema string, hasVectorLayers bool) {
	var m struct {
		Name         string          `json:"name"`
		VectorLayers json.RawMessage `json:"vector_layers"`
	}
	if err := json.Unmarshal(md, &m); err != nil {
		return "", false
	}
	hasVectorLayers = len(m.VectorLayers) > 0 && string(m.VectorLayers) != "null"
	if strings.Contains(strings.ToLower(string(md)), "openmaptiles") {
		return "openmaptiles", hasVectorLayers
	}
	return strings.ToLower(m.Name), hasVectorLayers
}

// VerifyPMTilesAgainstManifest cross-checks a downloaded PMTiles file's header
// and metadata against the signed manifest. It fails on any zoom-range mismatch
// or missing OpenMapTiles signal.
func VerifyPMTilesAgainstManifest(path string, m *Manifest) error {
	info, err := readPMTilesInfo(path)
	if err != nil {
		return err
	}
	if info.MinZoom != m.MinZoom || info.MaxZoom != m.MaxZoom {
		return fmt.Errorf("pmtiles: zoom range %d-%d does not match manifest %d-%d",
			info.MinZoom, info.MaxZoom, m.MinZoom, m.MaxZoom)
	}
	if strings.EqualFold(m.TileSchema, "openmaptiles") {
		if info.TileSchema != "openmaptiles" && !info.HasVectorLayers {
			return fmt.Errorf("pmtiles: metadata is not OpenMapTiles")
		}
	}
	return nil
}

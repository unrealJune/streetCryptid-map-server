package tilesync

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// synthTerrain builds a one-tile raster PMTiles archive (z0..maxZoom, WebP,
// uncompressed) — just enough for OpenRaster.
func synthTerrain(maxZoom int) []byte {
	tile := []byte("RIFF....WEBP")
	dir := binary.AppendUvarint(nil, 1)                // one entry
	dir = binary.AppendUvarint(dir, 0)                 // tile id 0 (z0)
	dir = binary.AppendUvarint(dir, 1)                 // run length
	dir = binary.AppendUvarint(dir, uint64(len(tile))) // length
	dir = binary.AppendUvarint(dir, 1)                 // offset 0 (+1)
	meta := []byte(`{"name":"terrain-rgb"}`)

	h := make([]byte, pmtilesHeaderLen)
	copy(h, "PMTiles")
	h[7], h[97], h[98], h[99], h[101] = 3, 1, 1, 4, byte(maxZoom)
	offset := uint64(pmtilesHeaderLen)
	for i, b := range [][]byte{dir, meta, nil, tile} {
		binary.LittleEndian.PutUint64(h[8+i*16:], offset)
		binary.LittleEndian.PutUint64(h[16+i*16:], uint64(len(b)))
		offset += uint64(len(b))
	}
	out := append(h, dir...)
	out = append(out, meta...)
	return append(out, tile...)
}

func TestTerrainAbsentIsNotAnError(t *testing.T) {
	info, err := ReadTerrain(t.TempDir())
	if err != nil || info != nil {
		t.Fatalf("absent terrain: %+v %v", info, err)
	}
}

func TestImportTerrainInstallsAndVersions(t *testing.T) {
	dataDir := t.TempDir()
	s := NewSyncer(Config{DataDir: dataDir, RetainReleases: 2}, nil)
	baked := filepath.Join(t.TempDir(), "terrain.pmtiles")
	os.WriteFile(baked, synthTerrain(12), 0o644)

	info, err := s.ImportTerrain(context.Background(), baked, "glo30-2026-10")
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if info.Version != "glo30-2026-10" || info.ContentType != "image/webp" || info.MaxZoom != 12 {
		t.Fatalf("installed: %+v", info)
	}
	if _, err := os.Stat(baked); !os.IsNotExist(err) {
		t.Fatal("baked archive should have been moved into place")
	}
	again, err := ReadTerrain(dataDir)
	if err != nil || again.Path != TerrainPath(dataDir) || again.Version != "glo30-2026-10" {
		t.Fatalf("re-read: %+v %v", again, err)
	}
}

func TestImportTerrainRefusesPastTheBoundary(t *testing.T) {
	s := NewSyncer(Config{DataDir: t.TempDir(), RetainReleases: 2}, nil)
	baked := filepath.Join(t.TempDir(), "terrain.pmtiles")
	os.WriteFile(baked, synthTerrain(13), 0o644)
	if _, err := s.ImportTerrain(context.Background(), baked, "v1"); err == nil {
		t.Fatal("a z13 terrain archive must be refused")
	}
	if _, err := os.Stat(baked); err != nil {
		t.Fatal("a refused archive must be left where it was")
	}
}

func TestImportTerrainRefusesVectorArchives(t *testing.T) {
	s := NewSyncer(Config{DataDir: t.TempDir(), RetainReleases: 2}, nil)
	baked := filepath.Join(t.TempDir(), "planet.pmtiles")
	os.WriteFile(baked, synthPMTiles(0, 12), 0o644)
	if _, err := s.ImportTerrain(context.Background(), baked, "v1"); err == nil {
		t.Fatal("a vector archive is not terrain")
	}
}

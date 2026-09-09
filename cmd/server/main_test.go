package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/junephilip/streetcryptid-map-server/internal/pmtiles"
	"github.com/junephilip/streetcryptid-map-server/internal/tilesync"
)

func installTestDataset(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	release := filepath.Join(root, "releases", "release-1")
	if err := os.MkdirAll(release, 0o755); err != nil {
		t.Fatal(err)
	}
	directory := []byte{1, 0, 1, 1, 1}
	meta := []byte(`{"name":"OpenMapTiles","vector_layers":[]}`)
	h := make([]byte, 127)
	copy(h, "PMTiles")
	h[7], h[97], h[98], h[99], h[101] = 3, 1, 1, 1, 14
	offset := uint64(127)
	for i, b := range [][]byte{directory, meta, nil, {42}} {
		binary.LittleEndian.PutUint64(h[8+i*16:], offset)
		binary.LittleEndian.PutUint64(h[16+i*16:], uint64(len(b)))
		offset += uint64(len(b))
	}
	artifact := append(append(append(h, directory...), meta...), 42)
	hash := sha256.Sum256(artifact)
	m := tilesync.Manifest{SchemaVersion: 1, Version: "release-1", URL: "local:planet.pmtiles", SHA256: hex.EncodeToString(hash[:]), Size: int64(len(artifact)), MinZoom: 0, MaxZoom: 14, TileSchema: "openmaptiles", CreatedAt: "2026-09-01T00:00:00Z"}
	manifest, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(release, "planet.pmtiles")
	for file, raw := range map[string][]byte{
		path:                                    artifact,
		filepath.Join(release, "manifest.json"): manifest,
		filepath.Join(root, "active.json"):      []byte(`{"version":"release-1","safe_dir":"release-1"}`),
	} {
		if err := os.WriteFile(file, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root, path
}

func TestActiveDatasetPinsRelease(t *testing.T) {
	root, path := installTestDataset(t)
	m, got, err := activeDataset(root)
	if err != nil || got != path || m.Version != "release-1" {
		t.Fatalf("%v %s %v", m, got, err)
	}
	// No "current" symlink is needed; both version and file come from one pointer.
	r, err := pmtiles.Open(got)
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
}

func TestActiveDatasetFailsClosed(t *testing.T) {
	if _, _, err := activeDataset(""); err == nil {
		t.Fatal("unknown dataset accepted")
	}
	if _, _, err := activeDataset(t.TempDir()); err == nil {
		t.Fatal("missing pointer accepted")
	}
	for _, pointer := range []string{
		`{"version":"release-1","safe_dir":"../release-1"}`,
		`{"version":"release-1","safe_dir":".."}`,
		`{"version":"release-1","safe_dir":""}`,
		`{"version":"other","safe_dir":"release-1"}`,
		`broken`,
	} {
		root, _ := installTestDataset(t)
		if err := os.WriteFile(filepath.Join(root, "active.json"), []byte(pointer), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := activeDataset(root); err == nil {
			t.Fatal("invalid pointer accepted", pointer)
		}
	}

	root, path := installTestDataset(t)
	if err := os.WriteFile(path, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := activeDataset(root); err == nil {
		t.Fatal("partial artifact accepted")
	}
	root, path = installTestDataset(t)
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte{13}, 101); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, _, err := activeDataset(root); err == nil {
		t.Fatal("header/manifest mismatch accepted")
	}
}

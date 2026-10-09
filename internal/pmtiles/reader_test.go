package pmtiles

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/junephilip/streetcryptid-map-server/internal/privacy"
)

func compressTest(t *testing.T, raw []byte, comp byte) []byte {
	t.Helper()
	if comp == 1 {
		return raw
	}
	var b bytes.Buffer
	z := gzip.NewWriter(&b)
	if _, err := z.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func directoryBytes(entries []entry) []byte {
	b := binary.AppendUvarint(nil, uint64(len(entries)))
	var prev uint64
	for _, e := range entries {
		b = binary.AppendUvarint(b, e.id-prev)
		prev = e.id
	}
	for _, e := range entries {
		b = binary.AppendUvarint(b, e.run)
	}
	for _, e := range entries {
		b = binary.AppendUvarint(b, e.length)
	}
	for i, e := range entries {
		v := e.offset + 1
		if i > 0 && e.offset == entries[i-1].offset+entries[i-1].length {
			v = 0
		}
		b = binary.AppendUvarint(b, v)
	}
	return b
}

func archiveBytes(t *testing.T, entries []entry, tiles []byte, comp, tileComp byte, useLeaf bool) []byte {
	t.Helper()
	dir := compressTest(t, directoryBytes(entries), comp)
	var leaves []byte
	if useLeaf {
		leaves = dir
		dir = compressTest(t, directoryBytes([]entry{{id: entries[0].id, length: uint64(len(leaves))}}), comp)
	}
	meta := compressTest(t, []byte(`{"vector_layers":[]}`), comp)
	h := make([]byte, 127)
	copy(h, "PMTiles")
	h[7], h[97], h[98], h[99], h[101] = 3, comp, tileComp, 1, 14
	offset := uint64(127)
	for i, b := range [][]byte{dir, meta, leaves, tiles} {
		binary.LittleEndian.PutUint64(h[8+i*16:], offset)
		binary.LittleEndian.PutUint64(h[16+i*16:], uint64(len(b)))
		offset += uint64(len(b))
	}
	out := append(h, dir...)
	out = append(out, meta...)
	out = append(out, leaves...)
	return append(out, tiles...)
}

func openArchive(t *testing.T, archive []byte) (*Reader, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tiny.pmtiles")
	if err := os.WriteFile(path, archive, 0o600); err != nil {
		t.Fatal(err)
	}
	return Open(path)
}

func TestHilbertKnownIDs(t *testing.T) {
	for _, tc := range []struct {
		z, x, y int
		id      uint64
	}{
		{0, 0, 0, 0}, {1, 0, 0, 1}, {1, 0, 1, 2}, {1, 1, 1, 3}, {1, 1, 0, 4},
		{2, 0, 0, 5}, {12, 3423, 1763, 19078479},
	} {
		got, err := TileID(privacy.TileCoord{Z: tc.z, X: tc.x, Y: tc.y})
		if err != nil || got != tc.id {
			t.Fatalf("%+v: %d %v", tc, got, err)
		}
	}
	for _, tc := range []privacy.TileCoord{{Z: 32}, {Z: -1}, {Z: 2, X: 4}, {Z: 2, Y: -1}} {
		if _, err := TileID(tc); err == nil {
			t.Fatal("accepted invalid coordinate", tc)
		}
	}
}

func TestArchiveRootLeafRLECompression(t *testing.T) {
	for _, useLeaf := range []bool{false, true} {
		for _, comp := range []byte{1, 2} {
			for _, tileComp := range []byte{1, 2} {
				a := compressTest(t, []byte("abc"), tileComp)
				b := compressTest(t, []byte("defg"), tileComp)
				// IDs 1,2 share one blob. ID 3 is contiguous; ID 4 is sparse.
				entries := []entry{{id: 1, run: 2, length: uint64(len(a))}, {id: 3, run: 1, length: uint64(len(b)), offset: uint64(len(a))}}
				r, err := openArchive(t, archiveBytes(t, entries, append(a, b...), comp, tileComp, useLeaf))
				if err != nil {
					t.Fatal(err)
				}
				for i, tc := range []privacy.TileCoord{{Z: 1}, {Z: 1, Y: 1}, {Z: 1, X: 1, Y: 1}, {Z: 1, X: 1}} {
					got, err := r.GetTileBytes(context.Background(), tc)
					want := []string{"abc", "abc", "defg", ""}[i]
					if err != nil || string(got) != want {
						t.Fatalf("leaf=%v comp=%d tileComp=%d i=%d got=%q err=%v", useLeaf, comp, tileComp, i, got, err)
					}
					stored, gzipped, err := r.GetTileStored(context.Background(), tc)
					if err != nil || gzipped != (tileComp == 2 && want != "") {
						t.Fatalf("stored leaf=%v comp=%d tileComp=%d i=%d gzipped=%v err=%v", useLeaf, comp, tileComp, i, gzipped, err)
					}
					wantStored := map[string][]byte{"abc": a, "defg": b, "": nil}[want]
					if !bytes.Equal(stored, wantStored) || (want == "") != (stored == nil) {
						t.Fatalf("stored bytes leaf=%v tileComp=%d i=%d: %x", useLeaf, tileComp, i, stored)
					}
					if gzipped {
						zr, err := gzip.NewReader(bytes.NewReader(stored))
						if err != nil {
							t.Fatal(err)
						}
						inflated, err := io.ReadAll(zr)
						if err != nil || string(inflated) != want {
							t.Fatalf("stored gzip inflates to %q, want %q (%v)", inflated, want, err)
						}
					}
				}
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				if _, err := r.GetTileBytes(ctx, privacy.TileCoord{Z: 1}); err == nil {
					t.Fatal("ignored cancellation")
				}
				if _, _, err := r.GetTileStored(ctx, privacy.TileCoord{Z: 1}); err == nil {
					t.Fatal("stored read ignored cancellation")
				}
				r.Close()
			}
		}
	}
}

func TestBadArchiveRejected(t *testing.T) {
	base := archiveBytes(t, []entry{{id: 1, run: 1, length: 1}}, []byte{42}, 1, 1, false)
	for name, mutate := range map[string]func([]byte){
		"magic":                 func(b []byte) { b[0] = 0 },
		"version":               func(b []byte) { b[7] = 2 },
		"internal compression":  func(b []byte) { b[97] = 3 },
		"tile compression":      func(b []byte) { b[98] = 4 },
		"not MVT":               func(b []byte) { b[99] = 2 },
		"bad zoom":              func(b []byte) { b[100] = 15 },
		"section offset":        func(b []byte) { binary.LittleEndian.PutUint64(b[56:], ^uint64(0)) },
		"section length":        func(b []byte) { binary.LittleEndian.PutUint64(b[64:], ^uint64(0)) },
		"overlap":               func(b []byte) { binary.LittleEndian.PutUint64(b[56:], 127) },
		"root outside first16k": func(b []byte) { binary.LittleEndian.PutUint64(b[8:], 16385) },
		"zero count":            func(b []byte) { b[127] = 0 },
		"truncated varint":      func(b []byte) { b[127] = 255 },
		"offset outside tiles":  func(b []byte) { b[131] = 255 },
		"gzip mismatch":         func(b []byte) { b[97] = 2 },
	} {
		t.Run(name, func(t *testing.T) {
			b := bytes.Clone(base)
			mutate(b)
			r, err := openArchive(t, b)
			if err == nil {
				r.Close()
				t.Fatal("accepted corrupt archive")
			}
		})
	}
}

func TestBadDirectories(t *testing.T) {
	for _, raw := range [][]byte{
		binary.AppendUvarint(nil, maxDirectoryEntries+1),
		directoryBytes([]entry{{id: 1, run: 1, length: 0}}),
		directoryBytes([]entry{{id: 1, run: 1, length: 2}}),
		directoryBytes([]entry{{id: 1, run: 2, length: 1}, {id: 2, run: 1, length: 1}}),
		directoryBytes([]entry{{id: 1, run: 1, length: 1}, {id: 1, run: 1, length: 1}}),
		{1, 1, 1, 1, 0},
		append(directoryBytes([]entry{{id: 1, run: 1, length: 1}}), 0),
	} {
		if _, err := decodeDirectory(raw, 1, 1); err == nil {
			t.Fatalf("accepted malformed directory %x", raw)
		}
	}
}

func TestTileBoundsAndCorruption(t *testing.T) {
	for name, payload := range map[string][]byte{
		"bad gzip": {1, 2, 3},
		"bomb":     compressTest(t, make([]byte, maxTileBytes+1), 2),
	} {
		t.Run(name, func(t *testing.T) {
			r, err := openArchive(t, archiveBytes(t, []entry{{id: 1, run: 1, length: uint64(len(payload))}}, payload, 1, 2, false))
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			if _, err := r.GetTileBytes(context.Background(), privacy.TileCoord{Z: 1}); err == nil {
				t.Fatal("accepted bad tile")
			}
		})
	}
	r, err := openArchive(t, archiveBytes(t, []entry{{id: 1, run: 1, length: 1}}, []byte{1}, 1, 1, false))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.read(section{0, maxTileBytes + (1 << 20) + 1}, 1, maxTileBytes); err == nil {
		t.Fatal("accepted compressed size over bound")
	}
	// Decoded directory count and bytes are independently bounded.
	huge := compressTest(t, make([]byte, maxDirectoryBytes+1), 2)
	path := filepath.Join(t.TempDir(), "bomb")
	if err := os.WriteFile(path, huge, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	bomb := &Reader{file: f}
	if _, err := bomb.read(section{0, uint64(len(huge))}, 2, maxDirectoryBytes); err == nil {
		t.Fatal("accepted directory bomb")
	}
}

func TestLeafCacheConcurrentBound(t *testing.T) {
	r, err := openArchive(t, archiveBytes(t, []entry{{id: 1, run: 1, length: 1}}, []byte{42}, 2, 1, true))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			for range 50 {
				got, err := r.GetTileBytes(context.Background(), privacy.TileCoord{Z: 1})
				if err != nil || !bytes.Equal(got, []byte{42}) {
					t.Errorf("read %x %v", got, err)
					return
				}
			}
		})
	}
	wg.Wait()
	if len(r.directories) != 1 || r.cacheBytes > maxLeafCacheBytes {
		t.Fatal("unbounded directory cache")
	}
}

func TestLeafCycleFails(t *testing.T) {
	// A five-byte leaf directory points to itself.
	dir := directoryBytes([]entry{{id: 1, length: 5}})
	archive := archiveBytes(t, []entry{{id: 1, length: 5}}, dir, 1, 1, false)
	rootLen := binary.LittleEndian.Uint64(archive[16:])
	metaLen := binary.LittleEndian.Uint64(archive[32:])
	leafOff := 127 + rootLen + metaLen
	binary.LittleEndian.PutUint64(archive[40:], leafOff)
	binary.LittleEndian.PutUint64(archive[48:], uint64(len(dir)))
	binary.LittleEndian.PutUint64(archive[56:], uint64(len(archive)))
	binary.LittleEndian.PutUint64(archive[64:], 0)
	r, err := openArchive(t, archive)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.GetTileBytes(context.Background(), privacy.TileCoord{Z: 1}); err == nil {
		t.Fatal("cycle accepted")
	}
}

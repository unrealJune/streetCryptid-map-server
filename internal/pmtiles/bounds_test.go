package pmtiles

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"

	"github.com/junephilip/streetcryptid-map-server/internal/privacy"
)

func TestDirectoryCacheEvictsLRUByCount(t *testing.T) {
	var leaves []byte
	var keys []section
	for i := 0; i < maxCachedDirectories+1; i++ {
		b := directoryBytes([]entry{{id: uint64(i), run: 1, length: 1}})
		keys = append(keys, section{offset: uint64(len(leaves)), length: uint64(len(b))})
		leaves = append(leaves, b...)
	}
	archive := archiveBytes(t, []entry{{length: keys[0].length}}, leaves, 1, 1, false)
	leafOffset := binary.LittleEndian.Uint64(archive[56:])
	binary.LittleEndian.PutUint64(archive[40:], leafOffset)
	binary.LittleEndian.PutUint64(archive[48:], uint64(len(leaves)))
	binary.LittleEndian.PutUint64(archive[56:], uint64(len(archive)))
	binary.LittleEndian.PutUint64(archive[64:], 1)
	archive = append(archive, 42)
	r, err := openArchive(t, archive)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for _, key := range keys[:maxCachedDirectories] {
		if _, err := r.directory(key); err != nil {
			t.Fatal(err)
		}
	}
	// Touch the first directory, then push the cache over its entry cap.
	if _, err := r.directory(keys[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := r.directory(keys[maxCachedDirectories]); err != nil {
		t.Fatal(err)
	}
	if r.directories[keys[0]] == nil || r.directories[keys[1]] != nil || len(r.directories) != maxCachedDirectories || r.cacheBytes > maxLeafCacheBytes {
		t.Fatal("directory LRU/cap failed")
	}
}

func TestLeafCorruptionDoesNotBecomeEmpty(t *testing.T) {
	base := archiveBytes(t, []entry{{id: 1, run: 1, length: 1}}, []byte{42}, 1, 1, true)
	for _, which := range []string{"count", "parent", "truncation"} {
		archive := bytes.Clone(base)
		leafOffset := binary.LittleEndian.Uint64(archive[40:])
		switch which {
		case "count":
			archive[leafOffset] = 0
		case "parent":
			archive[leafOffset+1] = 2
		case "truncation":
			archive[leafOffset+4] = 255
		}
		r, err := openArchive(t, archive)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.GetTileBytes(context.Background(), privacy.TileCoord{Z: 1}); err == nil {
			t.Fatal("bad leaf treated as empty", which)
		}
		r.Close()
	}
}

func FuzzDecodeDirectory(f *testing.F) {
	f.Add(directoryBytes([]entry{{id: 1, run: 1, length: 1}}))
	f.Add([]byte{255, 255, 255})
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 8192 {
			t.Skip()
		}
		entries, err := decodeDirectory(raw, 1<<20, 1<<20)
		if err != nil {
			return
		}
		if len(entries) == 0 || len(entries) > maxDirectoryEntries {
			t.Fatal("unbounded entry count")
		}
		for i, e := range entries {
			if e.length == 0 || e.offset > 1<<20 || e.length > (1<<20)-e.offset {
				t.Fatal("unbounded entry")
			}
			if i > 0 && entries[i-1].id >= e.id {
				t.Fatal("unsorted IDs")
			}
		}
	})
}

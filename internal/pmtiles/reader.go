// Package pmtiles implements bounded local PMTiles v3 MVT reads. Compression
// modes other than none/gzip fail explicitly; they never become empty tiles.
package pmtiles

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"

	"github.com/junephilip/streetcryptid-map-server/internal/privacy"
)

const (
	maxDirectoryBytes    = 8 << 20
	maxDirectoryEntries  = 1 << 18
	maxTileBytes         = 16 << 20
	maxLeafCacheBytes    = 32 << 20
	maxCachedDirectories = 4096
	maxDepth             = 4
)

type section struct{ offset, length uint64 }
type entry struct{ id, run, length, offset uint64 }
type leaf struct {
	entries []entry
	used    uint64
}

type Reader struct {
	file                                 *os.File
	root                                 []entry
	leaves, tiles                        section
	internalCompression, tileCompression byte
	mu                                   sync.Mutex
	directories                          map[section]*leaf
	clock                                uint64
	cacheBytes                           int
}

func Open(path string) (*Reader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	r, err := openFile(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	return r, nil
}

func openFile(f *os.File) (*Reader, error) {
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	var h [127]byte
	if _, err := f.ReadAt(h[:], 0); err != nil {
		return nil, err
	}
	if string(h[:7]) != "PMTiles" || h[7] != 3 {
		return nil, errors.New("pmtiles: expected v3 archive")
	}
	if (h[97] != 1 && h[97] != 2) || (h[98] != 1 && h[98] != 2) {
		return nil, fmt.Errorf("pmtiles: unsupported compression internal=%d tile=%d", h[97], h[98])
	}
	if h[99] != 1 || h[100] > h[101] || h[101] > 31 {
		return nil, errors.New("pmtiles: invalid MVT type or zoom range")
	}
	sections := make([]section, 4)
	for i := range sections {
		p := 8 + i*16
		sections[i] = section{binary.LittleEndian.Uint64(h[p:]), binary.LittleEndian.Uint64(h[p+8:])}
		s := sections[i]
		if s.offset > uint64(fi.Size()) || s.length > uint64(fi.Size())-s.offset || (s.length > 0 && s.offset < 127) {
			return nil, errors.New("pmtiles: section outside archive")
		}
		for j := 0; j < i; j++ {
			o := sections[j]
			if s.length > 0 && o.length > 0 && s.offset < o.offset+o.length && o.offset < s.offset+s.length {
				return nil, errors.New("pmtiles: overlapping sections")
			}
		}
	}
	if sections[0].length == 0 || sections[0].offset+sections[0].length > 16384 {
		return nil, errors.New("pmtiles: invalid root extent")
	}
	r := &Reader{file: f, leaves: sections[2], tiles: sections[3], internalCompression: h[97], tileCompression: h[98], directories: make(map[section]*leaf)}
	raw, err := r.read(sections[0], h[97], maxDirectoryBytes)
	if err != nil {
		return nil, err
	}
	r.root, err = decodeDirectory(raw, r.leaves.length, r.tiles.length)
	if err != nil {
		return nil, err
	}
	return r, nil
}

func (r *Reader) Close() error { return r.file.Close() }

func (r *Reader) read(s section, compression byte, limit int64) ([]byte, error) {
	if s.length == 0 || s.length > uint64(limit)+(1<<20) {
		return nil, errors.New("pmtiles: compressed object exceeds bound")
	}
	src := io.NewSectionReader(r.file, int64(s.offset), int64(s.length))
	if compression == 1 {
		if s.length > uint64(limit) {
			return nil, errors.New("pmtiles: object exceeds bound")
		}
		raw := make([]byte, int(s.length))
		_, err := io.ReadFull(src, raw)
		return raw, err
	}
	zr, err := gzip.NewReader(src)
	if err != nil {
		return nil, fmt.Errorf("pmtiles: gzip: %w", err)
	}
	defer zr.Close()
	raw, err := io.ReadAll(io.LimitReader(zr, limit+1))
	if err != nil {
		return nil, fmt.Errorf("pmtiles: gzip: %w", err)
	}
	if int64(len(raw)) > limit {
		return nil, errors.New("pmtiles: decompressed object exceeds bound")
	}
	return raw, nil
}

func decodeDirectory(raw []byte, leafBytes, tileBytes uint64) ([]entry, error) {
	b := bytes.NewReader(raw)
	n, err := binary.ReadUvarint(b)
	if err != nil || n == 0 || n > maxDirectoryEntries || n > uint64(b.Len()/4) {
		return nil, errors.New("pmtiles: invalid directory count")
	}
	entries := make([]entry, int(n))
	var last uint64
	for i := range entries {
		d, err := binary.ReadUvarint(b)
		if err != nil || (i > 0 && d == 0) || d > ^uint64(0)-last {
			return nil, errors.New("pmtiles: invalid tile id delta")
		}
		last += d
		entries[i].id = last
	}
	for i := range entries {
		v, err := binary.ReadUvarint(b)
		if err != nil || v > ^uint64(0)-entries[i].id {
			return nil, errors.New("pmtiles: invalid run")
		}
		if i+1 < len(entries) && v > entries[i+1].id-entries[i].id {
			return nil, errors.New("pmtiles: overlapping run")
		}
		entries[i].run = v
	}
	for i := range entries {
		v, err := binary.ReadUvarint(b)
		if err != nil || v == 0 {
			return nil, errors.New("pmtiles: invalid entry length")
		}
		entries[i].length = v
	}
	for i := range entries {
		v, err := binary.ReadUvarint(b)
		if err != nil || (v == 0 && i == 0) {
			return nil, errors.New("pmtiles: invalid offset")
		}
		if v == 0 {
			prev := entries[i-1]
			if prev.length > ^uint64(0)-prev.offset {
				return nil, errors.New("pmtiles: offset overflow")
			}
			entries[i].offset = prev.offset + prev.length
		} else {
			entries[i].offset = v - 1
		}
		e := entries[i]
		bound := tileBytes
		if e.run == 0 {
			bound = leafBytes
		}
		if e.offset > bound || e.length > bound-e.offset {
			return nil, errors.New("pmtiles: entry outside section")
		}
	}
	if b.Len() != 0 {
		return nil, errors.New("pmtiles: trailing directory bytes")
	}
	return entries, nil
}

// TileID maps XYZ to the cumulative Hilbert index specified by PMTiles v3.
func TileID(t privacy.TileCoord) (uint64, error) {
	if t.Z < 0 || t.Z > 31 || t.X < 0 || t.Y < 0 || uint64(t.X) >= uint64(1)<<t.Z || uint64(t.Y) >= uint64(1)<<t.Z {
		return 0, errors.New("pmtiles: invalid coordinate")
	}
	x, y := int64(t.X), int64(t.Y)
	var d uint64
	for s := int64(1) << t.Z >> 1; s > 0; s >>= 1 {
		var rx, ry int64
		if x&s != 0 {
			rx = 1
		}
		if y&s != 0 {
			ry = 1
		}
		d += uint64(s * s * ((3 * rx) ^ ry))
		if ry == 0 {
			if rx == 1 {
				x, y = s-1-x, s-1-y
			}
			x, y = y, x
		}
	}
	return ((uint64(1)<<(2*t.Z))-1)/3 + d, nil
}

func (r *Reader) directory(s section) ([]entry, error) {
	// Serialize leaf loads to avoid decompressing the same directory per tile.
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clock++
	if cached := r.directories[s]; cached != nil {
		cached.used = r.clock
		return cached.entries, nil
	}
	raw, err := r.read(section{r.leaves.offset + s.offset, s.length}, r.internalCompression, maxDirectoryBytes)
	if err != nil {
		return nil, err
	}
	entries, err := decodeDirectory(raw, r.leaves.length, r.tiles.length)
	if err != nil {
		return nil, err
	}
	cost := len(entries) * 32
	for r.cacheBytes+cost > maxLeafCacheBytes || len(r.directories) >= maxCachedDirectories {
		var oldest section
		used := ^uint64(0)
		for k, v := range r.directories {
			if v.used < used {
				oldest, used = k, v.used
			}
		}
		r.cacheBytes -= len(r.directories[oldest].entries) * 32
		delete(r.directories, oldest)
	}
	r.directories[s] = &leaf{entries: entries, used: r.clock}
	r.cacheBytes += cost
	return entries, nil
}

func (r *Reader) GetTileBytes(ctx context.Context, t privacy.TileCoord) ([]byte, error) {
	id, err := TileID(t)
	if err != nil {
		return nil, err
	}
	entries := r.root
	for depth := 0; depth < maxDepth; depth++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		i := sort.Search(len(entries), func(i int) bool { return entries[i].id > id }) - 1
		if i < 0 {
			return nil, nil
		}
		e := entries[i]
		if e.run != 0 {
			if id-e.id >= e.run {
				return nil, nil
			}
			raw, err := r.read(section{r.tiles.offset + e.offset, e.length}, r.tileCompression, maxTileBytes)
			if err != nil {
				return nil, err
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if len(raw) == 0 {
				return nil, nil
			}
			return raw, nil
		}
		end := ^uint64(0)
		if i+1 < len(entries) {
			end = entries[i+1].id
		}
		entries, err = r.directory(section{e.offset, e.length})
		if err != nil {
			return nil, err
		}
		last := entries[len(entries)-1]
		if entries[0].id != e.id || last.id >= end || last.run > end-last.id {
			return nil, errors.New("pmtiles: leaf IDs outside parent interval")
		}
	}
	return nil, errors.New("pmtiles: directory depth exceeds bound (possible cycle)")
}

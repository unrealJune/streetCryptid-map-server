// Package scb1 implements the strict SCB1 tile-bundle wire format.
//
// The format is the server side of the app parser in
// streetCryptid/src/features/map/tiles/tile-bundle.ts. All integers are
// unsigned big-endian. The encoder refuses to emit a partial bundle: every
// descendant must have exactly one entry, and the total decompressed size is
// bounded.
package scb1

import (
	"encoding/binary"
	"fmt"

	"github.com/junephilip/streetcryptid-map-server/internal/privacy"
)

const (
	// Magic is the four-byte SCB1 marker.
	Magic = "SCB1"
	// Version is the only supported format version.
	Version = 1
	// HeaderBytes is the fixed header size.
	HeaderBytes = 20
	// EmptyTileLength marks a known-empty (204/404) descendant.
	EmptyTileLength = 0xFFFFFFFF
	// MaxDecompressedBytes bounds the assembled (pre-gzip) body at 64 MiB. It
	// mirrors TILE_BUNDLE_MAX_BYTES on the app side.
	MaxDecompressedBytes = 64 * 1024 * 1024
)

// Entry is one descendant's payload. Bytes is nil for a known-empty tile.
type Entry struct {
	Bytes []byte
}

// ErrTooLarge is returned when the assembled bundle would exceed
// MaxDecompressedBytes.
var ErrTooLarge = fmt.Errorf("scb1: bundle exceeds %d bytes", MaxDecompressedBytes)

// Size returns the exact encoded byte length for the given entries without
// allocating, so callers can reject oversized bundles before building them.
func Size(entries []Entry) (int, error) {
	total := HeaderBytes
	for _, e := range entries {
		total += 4
		if e.Bytes != nil {
			total += len(e.Bytes)
		}
		if total > MaxDecompressedBytes {
			return 0, ErrTooLarge
		}
	}
	return total, nil
}

// Encode serializes a complete bundle for req. len(entries) must equal the
// request's descendant count; entries are in the request's row-major order.
// A nil Entry.Bytes is written as the empty sentinel. The returned buffer is
// the uncompressed SCB1 body; callers gzip it for the wire.
func Encode(req privacy.BundleRequest, entries []Entry) ([]byte, error) {
	want := req.EntryCount()
	if len(entries) != want {
		return nil, fmt.Errorf("scb1: expected %d entries, got %d", want, len(entries))
	}
	total, err := Size(entries)
	if err != nil {
		return nil, err
	}

	buf := make([]byte, HeaderBytes, total)
	copy(buf[0:4], Magic)
	buf[4] = Version
	buf[5] = privacy.PrivacyAnchorZoom
	buf[6] = byte(req.TileZoom)
	buf[7] = 0 // flags
	binary.BigEndian.PutUint32(buf[8:12], uint32(req.AnchorX))
	binary.BigEndian.PutUint32(buf[12:16], uint32(req.AnchorY))
	binary.BigEndian.PutUint32(buf[16:20], uint32(want))

	var lenbuf [4]byte
	for _, e := range entries {
		if e.Bytes == nil {
			binary.BigEndian.PutUint32(lenbuf[:], EmptyTileLength)
			buf = append(buf, lenbuf[:]...)
			continue
		}
		binary.BigEndian.PutUint32(lenbuf[:], uint32(len(e.Bytes)))
		buf = append(buf, lenbuf[:]...)
		buf = append(buf, e.Bytes...)
	}
	return buf, nil
}

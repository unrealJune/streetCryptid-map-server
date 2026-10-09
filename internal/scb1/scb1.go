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
	// FlagGzipEntries marks a body whose every non-empty entry is one complete
	// gzip member (SCB3 payloads). Flags 0 means raw MVT entries.
	FlagGzipEntries = 0x01
	// minGzipMember is a gzip header plus trailer around an empty deflate body.
	minGzipMember = 18
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

// Encode serializes a complete raw-entry bundle (flags 0) for req.
func Encode(req privacy.BundleRequest, entries []Entry) ([]byte, error) {
	return EncodeFlags(req, 0, entries)
}

// EncodeFlags serializes a complete bundle for req. len(entries) must equal the
// request's descendant count; entries are in the request's row-major order.
// A nil Entry.Bytes is written as the empty sentinel. The returned buffer is
// the uncompressed SCB1 body; callers gzip it for the wire.
func EncodeFlags(req privacy.BundleRequest, flags byte, entries []Entry) ([]byte, error) {
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
	buf[7] = flags
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

// Validate checks a raw-entry (flags 0) body; see ValidateFlags.
func Validate(req privacy.BundleRequest, raw []byte) error {
	return ValidateFlags(req, raw, 0)
}

// ValidateFlags checks the exact requested header and flags, complete
// row-major descendant count, length bounds and absence of trailing bytes
// without copying payloads. With FlagGzipEntries every non-empty entry must
// look like a gzip member; entries are not inflated here.
func ValidateFlags(req privacy.BundleRequest, raw []byte, flags byte) error {
	if len(raw) < HeaderBytes || len(raw) > MaxDecompressedBytes {
		return fmt.Errorf("scb1: invalid size")
	}
	if string(raw[:4]) != Magic || raw[4] != Version ||
		raw[5] != privacy.PrivacyAnchorZoom || raw[6] != byte(req.TileZoom) || raw[7] != flags ||
		binary.BigEndian.Uint32(raw[8:]) != uint32(req.AnchorX) ||
		binary.BigEndian.Uint32(raw[12:]) != uint32(req.AnchorY) ||
		binary.BigEndian.Uint32(raw[16:]) != uint32(req.EntryCount()) {
		return fmt.Errorf("scb1: mismatched header")
	}
	offset := HeaderBytes
	for range req.EntryCount() {
		if len(raw)-offset < 4 {
			return fmt.Errorf("scb1: missing descendant")
		}
		n := binary.BigEndian.Uint32(raw[offset:])
		offset += 4
		if n == EmptyTileLength {
			continue
		}
		if uint64(n) > uint64(len(raw)-offset) {
			return fmt.Errorf("scb1: truncated descendant")
		}
		if flags&FlagGzipEntries != 0 {
			e := raw[offset : offset+int(n)]
			if len(e) < minGzipMember || e[0] != 0x1f || e[1] != 0x8b || e[2] != 0x08 {
				return fmt.Errorf("scb1: entry is not a gzip member")
			}
		}
		offset += int(n)
	}
	if offset != len(raw) {
		return fmt.Errorf("scb1: trailing bytes")
	}
	return nil
}

package scb1

import (
	"bytes"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junephilip/streetcryptid-map-server/internal/privacy"
)

// The golden fixture is the byte-for-byte contract with the app parser in
// streetCryptid/src/features/map/tiles/tile-bundle.ts. If the encoder changes,
// this test fails and the app parser would break in production.
func TestEncodeMatchesGolden(t *testing.T) {
	req, err := privacy.ValidateBundle(164, 357, 11)
	if err != nil {
		t.Fatal(err)
	}
	entries := []Entry{
		{Bytes: []byte{0x01, 0x02}},
		{Bytes: nil}, // known-empty sentinel
		{Bytes: []byte{0xAA}},
		{Bytes: []byte{0x0A, 0x0B, 0x0C}},
	}
	got, err := Encode(req, entries)
	if err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "scb1-z11.golden"))
	if err != nil {
		t.Fatal(err)
	}
	want, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("encoded bundle != golden\n got: %s\nwant: %s", hex.EncodeToString(got), hex.EncodeToString(want))
	}
}

func TestEncodeHeaderFields(t *testing.T) {
	req, _ := privacy.ValidateBundle(1, 2, 12)
	entries := make([]Entry, req.EntryCount())
	buf, err := Encode(req, entries)
	if err != nil {
		t.Fatal(err)
	}
	if string(buf[0:4]) != Magic {
		t.Fatal("magic mismatch")
	}
	if buf[4] != Version || buf[5] != privacy.PrivacyAnchorZoom || buf[6] != 12 || buf[7] != 0 {
		t.Fatalf("header bytes wrong: %v", buf[4:8])
	}
	// All-empty entries: 20 header + 16*4 = 84 bytes.
	if len(buf) != HeaderBytes+16*4 {
		t.Fatalf("all-empty z12 len = %d, want %d", len(buf), HeaderBytes+16*4)
	}
}

func TestEncodeRejectsWrongCount(t *testing.T) {
	req, _ := privacy.ValidateBundle(0, 0, 11) // wants 4
	if _, err := Encode(req, []Entry{{}, {}}); err == nil {
		t.Fatal("expected error for wrong entry count")
	}
}

func TestSizeRejectsOversized(t *testing.T) {
	entries := []Entry{{Bytes: make([]byte, MaxDecompressedBytes)}}
	if _, err := Size(entries); err != ErrTooLarge {
		t.Fatalf("want ErrTooLarge, got %v", err)
	}
}

func TestGzipEntriesFlag(t *testing.T) {
	req, _ := privacy.ValidateBundle(164, 357, 11)
	member := []byte{0x1f, 0x8b, 0x08, 0, 0, 0, 0, 0, 0, 0xff, 3, 0, 0, 0, 0, 0, 0, 0}
	entries := []Entry{{Bytes: member}, {}, {Bytes: member}, {}}
	raw, err := EncodeFlags(req, FlagGzipEntries, entries)
	if err != nil {
		t.Fatal(err)
	}
	if raw[7] != FlagGzipEntries {
		t.Fatalf("flags byte %d", raw[7])
	}
	if err := ValidateFlags(req, raw, FlagGzipEntries); err != nil {
		t.Fatal(err)
	}
	if Validate(req, raw) == nil {
		t.Fatal("flagged body accepted as flags 0")
	}
	plain, _ := Encode(req, entries)
	if ValidateFlags(req, plain, FlagGzipEntries) == nil {
		t.Fatal("flags 0 body accepted as gzip entries")
	}
	for name, bad := range map[string][]byte{
		"raw mvt":     {0x1a, 0x02, 0x0a, 0x00},
		"short":       member[:17],
		"not deflate": append([]byte{0x1f, 0x8b, 0x09}, member[3:]...),
	} {
		raw, err := EncodeFlags(req, FlagGzipEntries, []Entry{{}, {Bytes: bad}, {}, {}})
		if err != nil {
			t.Fatal(err)
		}
		if ValidateFlags(req, raw, FlagGzipEntries) == nil {
			t.Fatalf("%s entry accepted", name)
		}
	}
}

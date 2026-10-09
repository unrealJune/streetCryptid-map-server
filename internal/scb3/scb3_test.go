package scb3

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/junephilip/streetcryptid-map-server/internal/privacy"
	"github.com/junephilip/streetcryptid-map-server/internal/scb1"
)

// gzipMember is a complete gzip member of an empty body, as Go writes it.
var gzipMember = []byte{0x1f, 0x8b, 0x08, 0, 0, 0, 0, 0, 0, 0xff, 1, 0, 0, 0xff, 0xff, 0, 0, 0, 0, 0, 0, 0, 0}

type fixtureStage struct {
	Zoom       int    `json:"zoom"`
	Part       string `json:"part"`
	PayloadHex string `json:"payload_hex"`
	SHA256     string `json:"payload_sha256"`
}

// emptyStream builds the stream for an anchor whose descendants are all empty.
func emptyStream(t *testing.T, req privacy.BundleRequest) ([]byte, []fixtureStage) {
	t.Helper()
	stream := Header(req)
	var stages []fixtureStage
	for _, stage := range Stages(req) {
		payload, err := scb1.EncodeFlags(stage.Req, scb1.FlagGzipEntries, make([]scb1.Entry, stage.Req.EntryCount()))
		if err != nil {
			t.Fatal(err)
		}
		frame, err := Frame(stage, payload)
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(payload)
		stages = append(stages, fixtureStage{stage.Req.TileZoom, stage.Part.String(), hex.EncodeToString(payload), hex.EncodeToString(digest[:])})
		stream = append(stream, frame...)
	}
	return stream, stages
}

// The goldens are the cross-language contract with the app's SCB3 decoder.
func TestGoldenEmptyStreams(t *testing.T) {
	for _, z := range []int{11, 14} {
		t.Run(fmt.Sprint(z), func(t *testing.T) {
			req, _ := privacy.ValidateBundle(164, 357, z)
			stream, stages := emptyStream(t, req)
			fixture := struct {
				Request   string         `json:"request"`
				Stages    []fixtureStage `json:"stages"`
				StreamHex string         `json:"scb3_hex"`
			}{fmt.Sprintf("/planet/bundle/v3/164/357/%d", z), stages, hex.EncodeToString(stream)}
			want, err := json.MarshalIndent(fixture, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			want = append(want, '\n')
			base := filepath.Join("..", "..", "testdata", fmt.Sprintf("scb3-z%d-empty", z))
			if os.Getenv("UPDATE_SCB3_FIXTURE") == "1" {
				if err := os.WriteFile(base+".json", want, 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(base+".scb3", stream, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			got, err := os.ReadFile(base + ".json")
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("cross-language fixture changed:\n%s", want)
			}
			bin, err := os.ReadFile(base + ".scb3")
			if err != nil || !bytes.Equal(bin, stream) {
				t.Fatal("binary fixture mismatch", err)
			}
			count := byte(1)
			if z == 14 {
				count = 3
			}
			wantHeader := []byte{'S', 'C', 'B', '3', 3, 10, byte(z), 0, 0, 0, 0, 164, 0, 0, 1, 101, 0, 0, 0, count}
			if !bytes.Equal(stream[:20], wantHeader) {
				t.Fatalf("header: %x", stream[:20])
			}
		})
	}
}

func TestStages(t *testing.T) {
	for z := 11; z <= 14; z++ {
		req, _ := privacy.ValidateBundle(164, 357, z)
		type zp struct {
			z int
			p Part
		}
		want := []zp{{z, PartFull}}
		if z == 14 {
			want = []zp{{13, PartFull}, {14, PartStructure}, {14, PartLabels}}
		}
		stages := Stages(req)
		if len(stages) != len(want) {
			t.Fatal("stage count")
		}
		for i, s := range stages {
			if s.Req.TileZoom != want[i].z || s.Part != want[i].p || s.Req.AnchorX != 164 || s.Req.AnchorY != 357 {
				t.Fatal(s)
			}
		}
	}
}

func TestFrameLayoutAndValidation(t *testing.T) {
	req, _ := privacy.ValidateBundle(164, 357, 14)
	stage := Stage{req, PartLabels}
	entries := make([]scb1.Entry, 256)
	entries[3].Bytes = gzipMember
	payload, err := scb1.EncodeFlags(req, scb1.FlagGzipEntries, entries)
	if err != nil {
		t.Fatal(err)
	}
	frame, err := Frame(stage, payload)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(payload)
	if binary.BigEndian.Uint32(frame) != uint32(len(payload)) || frame[4] != 14 || frame[5] != 2 ||
		frame[6] != 0 || frame[7] != 0 || !bytes.Equal(frame[8:40], digest[:]) || !bytes.Equal(frame[40:], payload) {
		t.Fatalf("frame prefix %x", frame[:40])
	}
	raw, _ := scb1.Encode(req, entries)
	rawMVT := slices.Clone(entries)
	rawMVT[3].Bytes = []byte{0x1a, 0x00}
	notGzip, _ := scb1.EncodeFlags(req, scb1.FlagGzipEntries, rawMVT)
	wrongZoom, _ := scb1.EncodeFlags(privacy.BundleRequest{AnchorX: 164, AnchorY: 357, TileZoom: 13}, scb1.FlagGzipEntries, make([]scb1.Entry, 64))
	for name, bad := range map[string][]byte{
		"flags 0":       raw,
		"raw entry":     notGzip,
		"wrong zoom":    wrongZoom,
		"trailing":      append(bytes.Clone(payload), 0),
		"truncated":     payload[:len(payload)-1],
		"empty payload": nil,
	} {
		if _, err := Frame(stage, bad); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	if _, err := Frame(Stage{req, 3}, payload); err == nil {
		t.Fatal("invalid part accepted")
	}
	if _, err := Frame(stage, make([]byte, MaxPayloadBytes+1)); err == nil {
		t.Fatal("payload bound ignored")
	}
}

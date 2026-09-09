package scb2

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/junephilip/streetcryptid-map-server/internal/privacy"
	"github.com/junephilip/streetcryptid-map-server/internal/scb1"
)

func gzipTest(t *testing.T, raw []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	if _, err := w.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestGoldenEmptyStage(t *testing.T) {
	req, _ := privacy.ValidateBundle(164, 357, 11)
	raw, err := scb1.Encode(req, make([]scb1.Entry, 4))
	if err != nil {
		t.Fatal(err)
	}
	gz := gzipTest(t, raw)
	frame, err := Frame(req, gz)
	if err != nil {
		t.Fatal(err)
	}
	stream := append(Header(req), frame...)
	digest := sha256.Sum256(raw)
	fixture := struct {
		Request   string `json:"request"`
		RawHex    string `json:"raw_scb1_hex"`
		GzipHex   string `json:"gzip_hex"`
		SHA256    string `json:"raw_sha256"`
		StreamHex string `json:"scb2_hex"`
	}{"/planet/bundle/v2/164/357/11", hex.EncodeToString(raw), hex.EncodeToString(gz), hex.EncodeToString(digest[:]), hex.EncodeToString(stream)}
	want, err := json.MarshalIndent(fixture, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, '\n')
	path := filepath.Join("..", "..", "testdata", "scb2-z11-empty.json")
	if os.Getenv("UPDATE_SCB2_FIXTURE") == "1" {
		if err := os.WriteFile(path, want, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join("..", "..", "testdata", "scb2-z11-empty.scb2"), stream, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("cross-language fixture changed:\n%s", want)
	}
	bin, err := os.ReadFile(filepath.Join("..", "..", "testdata", "scb2-z11-empty.scb2"))
	if err != nil || !bytes.Equal(bin, stream) {
		t.Fatal("binary fixture mismatch", err)
	}
	wantHeader := []byte{'S', 'C', 'B', '2', 2, 10, 11, 0, 0, 0, 0, 164, 0, 0, 1, 101, 0, 0, 0, 1}
	if !bytes.Equal(stream[:20], wantHeader) {
		t.Fatalf("header: %x", stream[:20])
	}
	if binary.BigEndian.Uint32(frame[4:]) != 36 || !bytes.Equal(frame[8:40], digest[:]) {
		t.Fatal("length/hash contract mismatch")
	}
}

func TestStagesAndMalformedPayloads(t *testing.T) {
	for z := 11; z <= 14; z++ {
		req, _ := privacy.ValidateBundle(164, 357, z)
		stages := Stages(req)
		want := []int{z}
		if z == 14 {
			want = []int{13, 14}
		}
		if len(stages) != len(want) {
			t.Fatal("stage count")
		}
		for i, stage := range stages {
			if stage.TileZoom != want[i] || stage.AnchorX != 164 || stage.AnchorY != 357 {
				t.Fatal(stage)
			}
		}
	}
	req, _ := privacy.ValidateBundle(164, 357, 11)
	raw, _ := scb1.Encode(req, make([]scb1.Entry, 4))
	for name, corrupt := range map[string]func([]byte) []byte{
		"header":   func(b []byte) []byte { b[7] = 1; return b },
		"count":    func(b []byte) []byte { b[19] = 3; return b },
		"anchor":   func(b []byte) []byte { b[11] = 165; return b },
		"zoom":     func(b []byte) []byte { b[6] = 12; return b },
		"missing":  func(b []byte) []byte { return b[:len(b)-4] },
		"trailing": func(b []byte) []byte { return append(b, 0) },
		"length":   func(b []byte) []byte { binary.BigEndian.PutUint32(b[20:], 200); return b },
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Frame(req, gzipTest(t, corrupt(bytes.Clone(raw)))); err == nil {
				t.Fatal("malformed SCB1 accepted")
			}
		})
	}
	gz := gzipTest(t, raw)
	for _, bad := range [][]byte{nil, gz[:len(gz)-1], append(bytes.Clone(gz), 0), append(bytes.Clone(gz), gz...)} {
		if _, err := Frame(req, bad); err == nil {
			t.Fatal("bad gzip accepted")
		}
	}
	if _, err := Frame(req, make([]byte, MaxCompressedBytes+1)); err == nil {
		t.Fatal("compressed bound ignored")
	}
	if _, err := Frame(req, gzipTest(t, make([]byte, scb1.MaxDecompressedBytes+1))); err == nil {
		t.Fatal("raw bound ignored")
	}
}

// Package scb2 frames complete gzipped SCB1 stages for progressive transport.
package scb2

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/junephilip/streetcryptid-map-server/internal/privacy"
	"github.com/junephilip/streetcryptid-map-server/internal/scb1"
)

const (
	MediaType = "application/vnd.streetcryptid.tile-stream"
	// CodecVersion is part of the strong ETag and the persistent namespace.
	// Bump it whenever encoding/compressor changes can change representation bytes.
	CodecVersion       = "v2-scb1-gzip-go1.26-r1"
	HeaderBytes        = 20
	FrameBytes         = 40
	MaxCompressedBytes = scb1.MaxDecompressedBytes + 1<<20
	MaxStreamBytes     = 2*(MaxCompressedBytes+FrameBytes) + HeaderBytes
)

func Stages(req privacy.BundleRequest) []privacy.BundleRequest {
	if req.TileZoom == 14 {
		overview := req
		overview.TileZoom = 13
		return []privacy.BundleRequest{overview, req}
	}
	return []privacy.BundleRequest{req}
}

func Header(req privacy.BundleRequest) []byte {
	h := make([]byte, HeaderBytes)
	copy(h, "SCB2")
	h[4], h[5], h[6] = 2, privacy.PrivacyAnchorZoom, byte(req.TileZoom)
	binary.BigEndian.PutUint32(h[8:], uint32(req.AnchorX))
	binary.BigEndian.PutUint32(h[12:], uint32(req.AnchorY))
	binary.BigEndian.PutUint32(h[16:], uint32(len(Stages(req))))
	return h
}

// Frame validates the stage before framing it. Neither malformed cached stages
// nor decompression bombs may be promoted to a complete stream.
func Frame(req privacy.BundleRequest, compressed []byte) ([]byte, error) {
	if len(compressed) > MaxCompressedBytes {
		return nil, fmt.Errorf("scb2: compressed stage too large")
	}
	input := bytes.NewReader(compressed)
	zr, err := gzip.NewReader(input)
	if err != nil {
		return nil, err
	}
	zr.Multistream(false)
	raw, err := io.ReadAll(io.LimitReader(zr, scb1.MaxDecompressedBytes+1))
	closeErr := zr.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if input.Len() != 0 {
		return nil, fmt.Errorf("scb2: trailing gzip data")
	}
	if err := scb1.Validate(req, raw); err != nil {
		return nil, err
	}
	hash := sha256.Sum256(raw)
	frame := make([]byte, FrameBytes+len(compressed))
	binary.BigEndian.PutUint32(frame, uint32(len(compressed)))
	binary.BigEndian.PutUint32(frame[4:], uint32(len(raw)))
	copy(frame[8:], hash[:])
	copy(frame[FrameBytes:], compressed)
	return frame, nil
}

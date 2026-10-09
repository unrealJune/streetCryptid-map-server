// Package scb3 frames SCB1 stages whose entries are per-tile gzip members.
//
// Unlike SCB2 there is no outer gzip: entries are shipped as stored in the
// PMTiles archive, and the z14 stage is split by MVT layer into a structure
// part and a labels part (see package mvt). The stage list depends only on the
// requested zoom, so a stream reveals nothing beyond the z10 anchor.
package scb3

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"

	"github.com/junephilip/streetcryptid-map-server/internal/privacy"
	"github.com/junephilip/streetcryptid-map-server/internal/scb1"
)

const (
	MediaType = "application/vnd.streetcryptid.tile-stream3"
	// CodecVersion is part of the strong ETag and the persistent namespace.
	// Bump it whenever encoding/compressor changes or mvt.LabelLayers can
	// change representation bytes.
	CodecVersion    = "v3-scb1gz-go1.26-r1"
	HeaderBytes     = 20
	FrameBytes      = 40
	MaxPayloadBytes = scb1.MaxDecompressedBytes
	MaxStreamBytes  = 3*(MaxPayloadBytes+FrameBytes) + HeaderBytes
)

// Part selects which MVT layers a stage's entries carry.
type Part uint8

const (
	PartFull      Part = 0 // every layer
	PartStructure Part = 1 // every layer not in mvt.LabelLayers
	PartLabels    Part = 2 // only layers in mvt.LabelLayers
)

func (p Part) String() string {
	switch p {
	case PartFull:
		return "full"
	case PartStructure:
		return "structure"
	case PartLabels:
		return "labels"
	}
	return fmt.Sprintf("part(%d)", uint8(p))
}

// Stage is one framed SCB1 body: the descendants of Req at Req.TileZoom,
// restricted to Part's layers.
type Stage struct {
	Req  privacy.BundleRequest
	Part Part
}

// Stages returns the fixed stage list for a requested zoom: one full stage
// for z11-13; for z14 the z13 overview, then z14 structure, then z14 labels.
func Stages(req privacy.BundleRequest) []Stage {
	if req.TileZoom == privacy.MaxBundleZoom {
		overview := req
		overview.TileZoom = privacy.MaxBundleZoom - 1
		return []Stage{{overview, PartFull}, {req, PartStructure}, {req, PartLabels}}
	}
	return []Stage{{req, PartFull}}
}

func Header(req privacy.BundleRequest) []byte {
	h := make([]byte, HeaderBytes)
	copy(h, "SCB3")
	h[4], h[5], h[6] = 3, privacy.PrivacyAnchorZoom, byte(req.TileZoom)
	binary.BigEndian.PutUint32(h[8:], uint32(req.AnchorX))
	binary.BigEndian.PutUint32(h[12:], uint32(req.AnchorY))
	binary.BigEndian.PutUint32(h[16:], uint32(len(Stages(req))))
	return h
}

// Frame validates the stage payload before framing it, so a malformed cached
// stage can never be promoted into a complete stream.
func Frame(stage Stage, payload []byte) ([]byte, error) {
	if stage.Part > PartLabels {
		return nil, fmt.Errorf("scb3: invalid part %d", stage.Part)
	}
	if len(payload) > MaxPayloadBytes {
		return nil, fmt.Errorf("scb3: stage payload too large")
	}
	if err := scb1.ValidateFlags(stage.Req, payload, scb1.FlagGzipEntries); err != nil {
		return nil, err
	}
	hash := sha256.Sum256(payload)
	frame := make([]byte, FrameBytes+len(payload))
	binary.BigEndian.PutUint32(frame, uint32(len(payload)))
	frame[4] = byte(stage.Req.TileZoom)
	frame[5] = byte(stage.Part)
	copy(frame[8:], hash[:])
	copy(frame[FrameBytes:], payload)
	return frame, nil
}

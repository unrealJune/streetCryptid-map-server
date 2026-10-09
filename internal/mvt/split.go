// Package mvt splits Mapbox Vector Tiles by layer without decoding features.
//
// A tile is a protobuf message whose only defined field is
// `repeated Layer layers = 3`, so the serialized fields of a tile can be
// partitioned into two tiles and concatenated back into a valid one. Split
// walks only the top level of the tile and the top level of each layer; it
// never re-encodes anything, so every output byte is a slice of the input.
package mvt

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// MaxTileBytes bounds the tile Split and LayerNames accept.
const MaxTileBytes = 16 << 20

// LabelLayers is the fixed set that goes into part 2. Changing it changes
// representation bytes: bump scb3.CodecVersion.
var LabelLayers = map[string]bool{"housenumber": true, "poi": true}

var (
	ErrTooLarge  = fmt.Errorf("mvt: tile exceeds %d bytes", MaxTileBytes)
	ErrMalformed = errors.New("mvt: malformed tile")
)

const (
	tileLayersField = 3
	layerNameField  = 1
	wireVarint      = 0
	wireFixed64     = 1
	wireBytes       = 2
	wireFixed32     = 5
)

// field is one top-level protobuf field: raw spans the tag through the end of
// the value, payload is the value of a length-delimited field.
type field struct {
	number  uint64
	wire    byte
	raw     []byte
	payload []byte
}

// nextField reads one field from b and returns the remaining bytes.
func nextField(b []byte) (field, []byte, error) {
	tag, n := binary.Uvarint(b)
	if n <= 0 {
		return field{}, nil, ErrMalformed
	}
	f := field{number: tag >> 3, wire: byte(tag & 7)}
	if f.number == 0 {
		return field{}, nil, ErrMalformed
	}
	rest := b[n:]
	var size int
	switch f.wire {
	case wireVarint:
		_, m := binary.Uvarint(rest)
		if m <= 0 {
			return field{}, nil, ErrMalformed
		}
		size = m
	case wireFixed64:
		size = 8
	case wireFixed32:
		size = 4
	case wireBytes:
		l, m := binary.Uvarint(rest)
		if m <= 0 || l > uint64(len(rest)-m) {
			return field{}, nil, ErrMalformed
		}
		f.payload = rest[m : m+int(l)]
		size = m + int(l)
	default:
		// Groups (3, 4) and the undefined types 6 and 7.
		return field{}, nil, ErrMalformed
	}
	if size > len(rest) {
		return field{}, nil, ErrMalformed
	}
	f.raw = b[:n+size]
	return f, rest[size:], nil
}

// layerName walks a layer's top-level fields. The last name field wins, as in
// any protobuf decoder that reads repeated occurrences sequentially.
func layerName(layer []byte) (string, error) {
	var name []byte
	found := false
	for len(layer) > 0 {
		f, rest, err := nextField(layer)
		if err != nil {
			return "", err
		}
		if f.number == layerNameField {
			if f.wire != wireBytes {
				return "", ErrMalformed
			}
			name, found = f.payload, true
		}
		layer = rest
	}
	if !found {
		return "", ErrMalformed
	}
	return string(name), nil
}

// walk calls fn for every top-level field, with the layer name for layers.
func walk(tile []byte, fn func(f field, isLayer bool, name string)) error {
	if len(tile) > MaxTileBytes {
		return ErrTooLarge
	}
	for len(tile) > 0 {
		f, rest, err := nextField(tile)
		if err != nil {
			return err
		}
		isLayer := f.number == tileLayersField
		var name string
		if isLayer {
			if f.wire != wireBytes {
				return ErrMalformed
			}
			if name, err = layerName(f.payload); err != nil {
				return err
			}
		}
		fn(f, isLayer, name)
		tile = rest
	}
	return nil
}

// Split copies the top-level fields of an MVT tile into two tiles. Field 3
// (Layer) goes to labels when its name (Layer field 1) is in LabelLayers,
// else to structure. Any other top-level field is copied to structure only.
// A result with no layers is returned as nil. Malformed input is an error.
func Split(tile []byte) (structure, labels []byte, err error) {
	var structureLayers, labelLayers int
	err = walk(tile, func(f field, isLayer bool, name string) {
		switch {
		case isLayer && LabelLayers[name]:
			labels = append(labels, f.raw...)
			labelLayers++
		case isLayer:
			structure = append(structure, f.raw...)
			structureLayers++
		default:
			structure = append(structure, f.raw...)
		}
	})
	if err != nil {
		return nil, nil, err
	}
	if structureLayers == 0 {
		structure = nil
	}
	if labelLayers == 0 {
		labels = nil
	}
	return structure, labels, nil
}

// LayerNames lists layer names in order; used by tests and bundle-stat parity.
func LayerNames(tile []byte) ([]string, error) {
	var names []string
	err := walk(tile, func(_ field, isLayer bool, name string) {
		if isLayer {
			names = append(names, name)
		}
	})
	if err != nil {
		return nil, err
	}
	return names, nil
}

package mvt

import (
	"bytes"
	"encoding/binary"
	"errors"
	"slices"
	"testing"
)

func appendBytesField(b []byte, number uint64, payload []byte) []byte {
	b = binary.AppendUvarint(b, number<<3|wireBytes)
	b = binary.AppendUvarint(b, uint64(len(payload)))
	return append(b, payload...)
}

// layer builds a minimal layer: version, name, one dummy feature, extent.
func layer(name string) []byte {
	var l []byte
	l = binary.AppendUvarint(l, 15<<3|wireVarint)
	l = binary.AppendUvarint(l, 2)
	l = appendBytesField(l, layerNameField, []byte(name))
	feature := []byte{0x18, 0x01} // type = POINT
	l = appendBytesField(l, 2, feature)
	l = binary.AppendUvarint(l, 5<<3|wireVarint)
	l = binary.AppendUvarint(l, 4096)
	return appendBytesField(nil, tileLayersField, l)
}

func tile(names ...string) []byte {
	var t []byte
	for _, n := range names {
		t = append(t, layer(n)...)
	}
	return t
}

func TestSplitPartitionsLayers(t *testing.T) {
	in := tile("building", "transportation", "housenumber", "poi")
	structure, labels, err := Split(in)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(append(bytes.Clone(structure), labels...), in) {
		t.Fatal("parts do not re-concatenate to the input")
	}
	for _, tc := range []struct {
		part []byte
		want []string
	}{
		{structure, []string{"building", "transportation"}},
		{labels, []string{"housenumber", "poi"}},
		{in, []string{"building", "transportation", "housenumber", "poi"}},
	} {
		got, err := LayerNames(tc.part)
		if err != nil || !slices.Equal(got, tc.want) {
			t.Fatalf("layers %v %v, want %v", got, err, tc.want)
		}
	}
}

func TestSplitInterleavedKeepsOrderWithinPart(t *testing.T) {
	in := tile("poi", "building", "housenumber", "water")
	structure, labels, err := Split(in)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(structure, tile("building", "water")) || !bytes.Equal(labels, tile("poi", "housenumber")) {
		t.Fatal("parts are not the original layer bytes in order")
	}
	// Concatenating the parts is a valid tile with every layer.
	got, err := LayerNames(append(bytes.Clone(structure), labels...))
	if err != nil || !slices.Equal(got, []string{"building", "water", "poi", "housenumber"}) {
		t.Fatal(got, err)
	}
}

func TestSplitEmptyParts(t *testing.T) {
	structure, labels, err := Split(tile("poi"))
	if err != nil || structure != nil || labels == nil {
		t.Fatal("poi-only tile must have nil structure", err)
	}
	structure, labels, err = Split(tile("building"))
	if err != nil || structure == nil || labels != nil {
		t.Fatal("tile without labels must have nil labels", err)
	}
	for _, empty := range [][]byte{nil, {}} {
		structure, labels, err = Split(empty)
		if err != nil || structure != nil || labels != nil {
			t.Fatal("empty tile must split to nil/nil", err)
		}
	}
	// A non-layer top-level field alone does not make a structure tile.
	unknown := appendBytesField(nil, 9, []byte("x"))
	structure, labels, err = Split(append(bytes.Clone(unknown), tile("poi")...))
	if err != nil || structure != nil || !bytes.Equal(labels, tile("poi")) {
		t.Fatal("unknown field handling", err)
	}
	structure, _, err = Split(append(bytes.Clone(unknown), tile("building")...))
	if err != nil || !bytes.Equal(structure, append(bytes.Clone(unknown), tile("building")...)) {
		t.Fatal("unknown field must be copied to structure", err)
	}
}

func TestSplitLastNameWins(t *testing.T) {
	var l []byte
	l = appendBytesField(l, layerNameField, []byte("building"))
	l = appendBytesField(l, layerNameField, []byte("poi"))
	in := appendBytesField(nil, tileLayersField, l)
	structure, labels, err := Split(in)
	if err != nil || structure != nil || !bytes.Equal(labels, in) {
		t.Fatal("repeated name must resolve like a protobuf decoder", err)
	}
}

func TestSplitRejectsMalformed(t *testing.T) {
	good := tile("building", "poi")
	noName := appendBytesField(nil, tileLayersField, []byte{0x78, 0x02}) // version only
	for name, in := range map[string][]byte{
		"truncated length":   good[:len(good)-1],
		"truncated tag":      {0x80},
		"truncated varint":   {0x08, 0x80},
		"truncated fixed64":  {0x09, 1, 2, 3},
		"truncated fixed32":  {0x0d, 1, 2},
		"group start":        {0x0b},
		"group end":          {0x0c},
		"wire type 6":        {0x0e},
		"wire type 7":        {0x1f},
		"field zero":         {0x00, 0x00},
		"layer not bytes":    {0x18, 0x01},
		"layer without name": noName,
		"name not bytes":     appendBytesField(nil, tileLayersField, []byte{0x08, 0x01}),
		"bad layer inner":    appendBytesField(nil, tileLayersField, []byte{0x0b}),
		"varint overflow":    {0x08, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01},
	} {
		t.Run(name, func(t *testing.T) {
			structure, labels, err := Split(append(bytes.Clone(good), in...))
			if !errors.Is(err, ErrMalformed) || structure != nil || labels != nil {
				t.Fatalf("got %x %x %v", structure, labels, err)
			}
			if _, err := LayerNames(in); err == nil {
				t.Fatal("LayerNames accepted malformed tile")
			}
		})
	}
	if _, _, err := Split(make([]byte, MaxTileBytes+1)); !errors.Is(err, ErrTooLarge) {
		t.Fatal("size bound ignored", err)
	}
}

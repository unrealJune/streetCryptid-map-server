package privacy

import "testing"

func TestRawZoom0To10Accepted(t *testing.T) {
	for z := 0; z <= 10; z++ {
		if !AllowRawZoom(z) {
			t.Fatalf("raw zoom %d should be public", z)
		}
		if err := ValidateRawXYZ(z, 0, 0); err != nil {
			t.Fatalf("z%d/0/0 should validate: %v", z, err)
		}
	}
}

func TestRawZoom11To14Rejected(t *testing.T) {
	for z := 11; z <= 14; z++ {
		if AllowRawZoom(z) {
			t.Fatalf("raw zoom %d must not be public", z)
		}
		if err := ValidateRawXYZ(z, 0, 0); err == nil {
			t.Fatalf("z%d must be rejected", z)
		}
	}
}

func TestRawXYOutOfRange(t *testing.T) {
	// z1 has tiles 0..1 on each axis.
	cases := []struct{ z, x, y int }{
		{1, 2, 0}, {1, 0, 2}, {1, -1, 0}, {1, 0, -1}, {10, 1024, 0}, {10, 0, 1024},
	}
	for _, c := range cases {
		if err := ValidateRawXYZ(c.z, c.x, c.y); err == nil {
			t.Fatalf("z%d/%d/%d should be rejected", c.z, c.x, c.y)
		}
	}
}

func TestBundleZoomBoundaries(t *testing.T) {
	for _, z := range []int{9, 10, 15, 16} {
		if _, err := ValidateBundle(0, 0, z); err == nil {
			t.Fatalf("bundle tileZoom %d should be rejected", z)
		}
	}
	for _, z := range []int{11, 12, 13, 14} {
		if _, err := ValidateBundle(0, 0, z); err != nil {
			t.Fatalf("bundle tileZoom %d should be accepted: %v", z, err)
		}
	}
}

func TestBundleAnchorRange(t *testing.T) {
	for _, c := range []struct{ x, y int }{{-1, 0}, {0, -1}, {1024, 0}, {0, 1024}} {
		if _, err := ValidateBundle(c.x, c.y, 12); err == nil {
			t.Fatalf("anchor %d,%d should be rejected", c.x, c.y)
		}
	}
	if _, err := ValidateBundle(1023, 1023, 14); err != nil {
		t.Fatalf("max anchor should be accepted: %v", err)
	}
}

func TestDescendantCounts(t *testing.T) {
	cases := map[int]int{11: 4, 12: 16, 13: 64, 14: 256}
	for z, want := range cases {
		req, err := ValidateBundle(0, 0, z)
		if err != nil {
			t.Fatal(err)
		}
		if got := req.EntryCount(); got != want {
			t.Fatalf("z%d entry count = %d, want %d", z, got, want)
		}
		if got := len(req.Descendants()); got != want {
			t.Fatalf("z%d descendants len = %d, want %d", z, got, want)
		}
	}
}

// Seattle sits under z10 anchor 10/164/357. Its z14 descendants span
// x in [164<<4, 164<<4+15] = [2624, 2639] and y in [357<<4, ...] = [5712, 5727].
func TestSeattleAnchorRanges(t *testing.T) {
	req, err := ValidateBundle(164, 357, 14)
	if err != nil {
		t.Fatal(err)
	}
	tiles := req.Descendants()
	if len(tiles) != 256 {
		t.Fatalf("want 256 tiles, got %d", len(tiles))
	}
	first, last := tiles[0], tiles[len(tiles)-1]
	if first.X != 2624 || first.Y != 5712 {
		t.Fatalf("first tile = %d/%d, want 2624/5712", first.X, first.Y)
	}
	if last.X != 2639 || last.Y != 5727 {
		t.Fatalf("last tile = %d/%d, want 2639/5727", last.X, last.Y)
	}
	// Row-major: y outer, x inner. Second tile shares the first row.
	if tiles[1].Y != first.Y || tiles[1].X != first.X+1 {
		t.Fatalf("row-major order broken: %+v", tiles[1])
	}
	// Tile 16 begins the next row.
	if tiles[16].Y != first.Y+1 || tiles[16].X != first.X {
		t.Fatalf("row wrap broken: %+v", tiles[16])
	}
}

func TestNoConfigCanMoveBoundary(t *testing.T) {
	// The boundary is a compile-time constant; there is no setter to exercise.
	// This test documents the invariant and fails if someone lowers the cutoff.
	if MaxPublicRawZoom != 10 || PrivacyAnchorZoom != 10 || MaxBundleZoom != 14 {
		t.Fatal("privacy constants changed; the app protocol assumes 10/10/14")
	}
}

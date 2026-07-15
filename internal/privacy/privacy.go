// Package privacy holds the non-configurable privacy boundary for the map API.
//
// These constants are compiled into the binary. They are intentionally NOT
// wired to environment variables, flags, Helm values, ConfigMap fields, or any
// runtime feature flag. The only supported way to change them is to change this
// source file and rebuild the image. The test suite proves the boundary cannot
// be moved through configuration.
package privacy

import "fmt"

const (
	// PrivacyAnchorZoom is the fixed z at which fine detail is quantized. The
	// server may learn a request's z10 ancestor but never which child caused it.
	PrivacyAnchorZoom = 10
	// MaxPublicRawZoom is the deepest raw XYZ tile the public API will proxy.
	// z11..z14 raw requests are rejected before Martin is contacted.
	MaxPublicRawZoom = 10
	// MaxBundleZoom is the deepest data zoom obtainable through the bundle
	// endpoint.
	MaxBundleZoom = 14
)

// TileCoord is an XYZ tile address.
type TileCoord struct {
	Z int
	X int
	Y int
}

// AllowRawZoom reports whether a raw XYZ request at zoom z may be proxied to
// Martin. It is the single gate for the coarse path.
func AllowRawZoom(z int) bool {
	return z >= 0 && z <= MaxPublicRawZoom
}

// ValidateRawXYZ validates a coarse public request. It returns an error for any
// zoom above the public raw cutoff, and for x/y outside [0, 2^z-1]. The zoom
// check is performed first so a rejected fine request never reveals whether its
// x/y were in range.
func ValidateRawXYZ(z, x, y int) error {
	if !AllowRawZoom(z) {
		return fmt.Errorf("raw zoom %d is not public", z)
	}
	if err := validateXY(z, x, y); err != nil {
		return err
	}
	return nil
}

func validateXY(z, x, y int) error {
	// z is already known to be within [0, MaxPublicRawZoom] so 1<<z is safe.
	max := (1 << uint(z)) - 1
	if x < 0 || y < 0 || x > max || y > max {
		return fmt.Errorf("tile x/y out of range for zoom %d", z)
	}
	return nil
}

// BundleRequest is a validated fine-detail bundle request. It carries only the
// fixed z10 anchor and the requested data zoom — never a child, viewport, or
// bounding box.
type BundleRequest struct {
	AnchorX  int
	AnchorY  int
	TileZoom int
}

// ValidateBundle parses and validates the three bundle path fields. AnchorX and
// AnchorY must be valid z10 coordinates ([0,1023]); tileZoom must be one of
// 11..14. Any other input is rejected.
func ValidateBundle(x10, y10, tileZoom int) (BundleRequest, error) {
	if tileZoom <= PrivacyAnchorZoom || tileZoom > MaxBundleZoom {
		return BundleRequest{}, fmt.Errorf("bundle tile zoom %d out of range", tileZoom)
	}
	n := 1 << uint(PrivacyAnchorZoom) // 1024
	if x10 < 0 || y10 < 0 || x10 >= n || y10 >= n {
		return BundleRequest{}, fmt.Errorf("bundle anchor out of range")
	}
	return BundleRequest{AnchorX: x10, AnchorY: y10, TileZoom: tileZoom}, nil
}

// Descendants returns every tile at the request's data zoom beneath the fixed
// z10 anchor, in deterministic row-major order (y outer, x inner). This matches
// the app-side bundleTiles() ordering exactly.
func (r BundleRequest) Descendants() []TileCoord {
	d := uint(r.TileZoom - PrivacyAnchorZoom)
	side := 1 << d
	x0 := r.AnchorX << d
	y0 := r.AnchorY << d
	tiles := make([]TileCoord, 0, side*side)
	for y := y0; y < y0+side; y++ {
		for x := x0; x < x0+side; x++ {
			tiles = append(tiles, TileCoord{Z: r.TileZoom, X: x, Y: y})
		}
	}
	return tiles
}

// EntryCount is the number of descendants for the request's data zoom.
func (r BundleRequest) EntryCount() int {
	d := uint(r.TileZoom - PrivacyAnchorZoom)
	side := 1 << d
	return side * side
}

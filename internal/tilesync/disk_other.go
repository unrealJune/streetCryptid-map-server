//go:build !unix

package tilesync

// ensureFreeSpace is a no-op on non-unix platforms (used only for local dev and
// tests). Production runs on Linux where the unix implementation enforces the
// free-space check before downloading.
func ensureFreeSpace(dir string, need int64) error { return nil }

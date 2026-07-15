//go:build unix

package tilesync

import (
	"fmt"
	"syscall"
)

// ensureFreeSpace fails if fewer than need bytes (plus a safety margin) are
// available on the filesystem backing dir. Prevents partially filling a volume.
func ensureFreeSpace(dir string, need int64) error {
	if need <= 0 {
		return nil
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		// If we cannot stat the filesystem, do not block the download.
		return nil
	}
	avail := int64(st.Bavail) * int64(st.Bsize)
	margin := need / 20 // ~5% headroom
	if avail < need+margin {
		return fmt.Errorf("insufficient disk: need %d, have %d", need+margin, avail)
	}
	return nil
}

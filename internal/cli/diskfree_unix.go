//go:build unix

package cli

import (
	"errors"
	"math"
	"syscall"
)

// freeBytes reports free space for the filesystem holding dir.
//
// The hub shares a disk with the user's live memory database, so it refuses
// writes before filling it rather than turning its own growth into claude-mem's
// outage.
func freeBytes(dir string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	if st.Bsize <= 0 {
		return 0, errors.New("cmemlan: filesystem reported a non-positive block size")
	}

	// Both operands are non-negative; the product is clamped rather than allowed
	// to wrap, which on a very large filesystem would otherwise report a negative
	// amount of free space and refuse every write.
	free := uint64(st.Bavail) * uint64(st.Bsize)
	if free > uint64(math.MaxInt64) {
		return math.MaxInt64, nil
	}
	return int64(free), nil //nolint:gosec // clamped immediately above
}

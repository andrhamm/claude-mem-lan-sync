//go:build unix

package cli

import "syscall"

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
	return int64(st.Bavail) * int64(st.Bsize), nil
}

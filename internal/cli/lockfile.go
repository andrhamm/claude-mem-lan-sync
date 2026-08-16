package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// LockFile is the filename used to keep two serve processes apart.
const LockFile = "serve.lock"

// Lockfile takes an exclusive run lock on a data directory.
//
// SQLite's own locking keeps concurrent writers correct, but two hubs sharing a
// directory is never what anyone intended, and the resulting symptoms — pushes
// blocking on each other, two mDNS advertisements for one hub — are confusing
// enough to be worth failing loudly instead.
func Lockfile(dir string) (release func() error, err error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, LockFile)

	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err == nil {
		if _, werr := fmt.Fprintf(f, "%d\n", os.Getpid()); werr != nil {
			_ = f.Close()
			_ = os.Remove(path)
			return nil, werr
		}
		_ = f.Close()
		return func() error { return os.Remove(path) }, nil
	}
	if !errors.Is(err, os.ErrExist) {
		return nil, err
	}

	// A lock file exists. If the process is gone, it is stale.
	owner, readErr := os.ReadFile(path)
	if readErr != nil {
		return nil, fmt.Errorf("cmemlan: reading the existing lock: %w", readErr)
	}
	pid, convErr := strconv.Atoi(strings.TrimSpace(string(owner)))
	if convErr == nil && processAlive(pid) {
		return nil, fmt.Errorf(
			"cmemlan: another serve process (pid %d) is already using %s", pid, dir)
	}

	if err := os.Remove(path); err != nil {
		return nil, fmt.Errorf("cmemlan: clearing a stale lock: %w", err)
	}
	return Lockfile(dir)
}

// processAlive reports whether a pid is running. Signal 0 performs the
// permission and existence checks without delivering anything.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	// EPERM means it exists but belongs to someone else.
	return errors.Is(err, os.ErrPermission)
}

//go:build !unix

package cli

import "errors"

var errNoDiskCheck = errors.New("cmemlan: free space checks are unsupported on this platform")

// freeBytes has no portable implementation here; returning an error makes the
// caller skip the check rather than refuse writes.
func freeBytes(string) (int64, error) {
	return 0, errNoDiskCheck
}

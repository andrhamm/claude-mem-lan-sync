// Package buildinfo carries the version stamped at release time.
package buildinfo

import "fmt"

// Version is set at build time with
// -ldflags "-X github.com/andrhamm/claude-mem-lan-sync/internal/buildinfo.Version=v1.2.3".
// doctor reports it and bug reports need it, so an unstamped build says so plainly.
var Version = "dev"

// TestedClaudeMem is the claude-mem release this build's protocol behaviour was
// verified against. doctor warns when the installed version differs.
const TestedClaudeMem = "13.15.0"

// String renders the version for the `version` command.
func String() string {
	return fmt.Sprintf("cmemlan %s (verified against claude-mem %s)", Version, TestedClaudeMem)
}

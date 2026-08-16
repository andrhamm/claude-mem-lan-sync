// Package cli dispatches cmemlan's subcommands.
//
// Every dependency a command touches arrives through Env so that doctor,
// connect, and backfill are testable without a real filesystem, a real hub, or a
// real clock.
package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/andrhamm/claude-mem-lan-sync/internal/buildinfo"
)

// StatusResult is what a hub reports from GET /v1/sync/status.
type StatusResult struct {
	Epoch        string
	HeadSeq      string
	ProjectedSeq string
	SyncMode     string
}

// HubClient talks to a hub. Injected so doctor can be tested against a fake.
type HubClient interface {
	Status(ctx context.Context, url, userID, token string) (StatusResult, error)
}

// Env carries everything a command needs from the outside world.
type Env struct {
	Stdout, Stderr io.Writer
	// HomeDir roots filesystem lookups in tests; empty means the real home.
	HomeDir string
	// DataDir overrides cmemlan's data directory; empty means resolve normally.
	DataDir string
	Hub     HubClient
	Now     func() time.Time
}

const usage = `cmemlan — self-hosted LAN sync for claude-mem

Usage:
  cmemlan <command> [flags]

Hub commands:
  serve          run the hub
  pair           open a pairing window and print a code
  devices        list devices that have contacted this hub
  revoke <id>    deny a device further access
  rotate-token   replace the pre-shared key

Client commands:
  connect [url]  point this machine's claude-mem at a hub
  backfill       queue existing memories for upload
  status         show sync state
  doctor         diagnose sync problems

  version        print the version

Run "cmemlan <command> -h" for command flags.
`

// Run dispatches args (without the program name) and returns an exit code.
func Run(args []string, env Env) int {
	if len(args) == 0 {
		fmt.Fprint(env.Stderr, usage)
		return 2
	}

	cmd, rest := args[0], args[1:]
	switch cmd {
	case "version", "-v", "--version":
		fmt.Fprintln(env.Stdout, buildinfo.String())
		return 0
	case "help", "-h", "--help":
		fmt.Fprint(env.Stdout, usage)
		return 0
	case "serve":
		return runServe(rest, env)
	case "pair":
		return runPair(rest, env)
	case "devices":
		return runDevices(rest, env)
	case "revoke":
		return runRevoke(rest, env)
	case "rotate-token":
		return runRotateToken(rest, env)
	case "connect":
		return runConnect(rest, env)
	case "backfill":
		return runBackfill(rest, env)
	case "status":
		return runStatus(rest, env)
	case "doctor":
		return runDoctor(rest, env)
	case "fixtures":
		return runFixtures(rest, env)
	default:
		fmt.Fprintf(env.Stderr, "cmemlan: unknown command %q\n\n%s", cmd, usage)
		return 2
	}
}

// parseFlags parses args allowing flags and positional arguments to interleave.
//
// Go's flag package stops at the first non-flag argument, so
// `cmemlan connect http://host --code 431-982` would silently drop --code and
// leave the user with a hub that was never paired. Parsing in a loop, peeling one
// positional off at a time, accepts either order.
func parseFlags(fs *flag.FlagSet, args []string) ([]string, error) {
	var positionals []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			return positionals, nil
		}
		positionals = append(positionals, rest[0])
		args = rest[1:]
	}
}

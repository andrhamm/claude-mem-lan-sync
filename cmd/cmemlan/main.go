// Command cmemlan is a self-hosted LAN sync hub for claude-mem, plus the CLI
// that points machines at it.
package main

import (
	"os"

	"github.com/andrhamm/claude-mem-lan-sync/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], cli.Env{
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	}))
}

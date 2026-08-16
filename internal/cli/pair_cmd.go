package cli

import (
	"context"
	"flag"
	"fmt"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/andrhamm/claude-mem-lan-sync/internal/pair"
	"github.com/andrhamm/claude-mem-lan-sync/internal/paths"
	"github.com/andrhamm/claude-mem-lan-sync/internal/store"
)

func dataDirFor(dataDirFlag string, env Env) (string, error) {
	return paths.DataDir(cmp(dataDirFlag, env.DataDir))
}

func runPair(args []string, env Env) int {
	fs := flag.NewFlagSet("pair", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	dataDir := fs.String("data-dir", "", "hub data directory")
	ttl := fs.Duration("ttl", pair.DefaultWindow, "how long the pairing window stays open")
	if _, err := parseFlags(fs, args); err != nil {
		return 2
	}

	dir, err := dataDirFor(*dataDir, env)
	if err != nil {
		fmt.Fprintf(env.Stderr, "cmemlan: %v\n", err)
		return 1
	}

	keys, err := pair.LoadOrCreate(dir)
	if err != nil {
		fmt.Fprintf(env.Stderr, "cmemlan: %v\n", err)
		return 1
	}

	w := pair.FileWindow{Dir: dir, Now: env.Now}
	code, expires, err := w.Open(*ttl)
	if err != nil {
		fmt.Fprintf(env.Stderr, "cmemlan: %v\n", err)
		return 1
	}

	fmt.Fprintf(env.Stdout, "pairing open for %s\n\n", time.Until(expires).Round(time.Second))
	fmt.Fprintf(env.Stdout, "  code         %s\n", code)
	fmt.Fprintf(env.Stdout, "  fingerprint  %s\n\n", keys.Fingerprint())
	fmt.Fprintf(env.Stdout, "On the other machine:\n  cmemlan connect http://<this-host>:8787 --code %s\n\n", code)
	// The key authenticates a device to the hub but never the hub to a device,
	// so comparing this fingerprint is the only thing that detects a relay.
	fmt.Fprintf(env.Stdout, "Check that it shows the same fingerprint before confirming.\n")
	return 0
}

func runDevices(args []string, env Env) int {
	fs := flag.NewFlagSet("devices", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	dataDir := fs.String("data-dir", "", "hub data directory")
	if _, err := parseFlags(fs, args); err != nil {
		return 2
	}

	dir, err := dataDirFor(*dataDir, env)
	if err != nil {
		fmt.Fprintf(env.Stderr, "cmemlan: %v\n", err)
		return 1
	}
	st, err := store.Open(filepath.Join(dir, "hub.db"), nil)
	if err != nil {
		fmt.Fprintf(env.Stderr, "cmemlan: %v\n", err)
		return 1
	}
	defer func() { _ = st.Close() }()

	devices, err := st.ListDevices(context.Background())
	if err != nil {
		fmt.Fprintf(env.Stderr, "cmemlan: %v\n", err)
		return 1
	}
	if len(devices) == 0 {
		fmt.Fprintln(env.Stdout, "no devices have contacted this hub yet")
		return 0
	}

	tw := tabwriter.NewWriter(env.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "DEVICE ID\tNAME\tLAST SEEN\tSTATUS")
	for _, d := range devices {
		status := "active"
		if d.Revoked {
			status = "revoked"
		}
		// The name is attacker-supplied, so it is sanitised before display.
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", d.ID, sanitise(d.Name), d.LastSeen.Format(time.RFC3339), status)
	}
	_ = tw.Flush()
	return 0
}

func runRevoke(args []string, env Env) int {
	fs := flag.NewFlagSet("revoke", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	dataDir := fs.String("data-dir", "", "hub data directory")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return 2
	}
	if len(pos) != 1 {
		fmt.Fprintln(env.Stderr, "usage: cmemlan revoke <device-id>")
		return 2
	}

	dir, err := dataDirFor(*dataDir, env)
	if err != nil {
		fmt.Fprintf(env.Stderr, "cmemlan: %v\n", err)
		return 1
	}
	st, err := store.Open(filepath.Join(dir, "hub.db"), nil)
	if err != nil {
		fmt.Fprintf(env.Stderr, "cmemlan: %v\n", err)
		return 1
	}
	defer func() { _ = st.Close() }()

	if err := st.RevokeDevice(context.Background(), pos[0]); err != nil {
		fmt.Fprintf(env.Stderr, "cmemlan: %v\n", err)
		return 1
	}
	fmt.Fprintf(env.Stdout, "revoked %s\n", pos[0])
	fmt.Fprintln(env.Stdout, "Restart the hub if it is running, so the change takes effect immediately.")
	return 0
}

func runRotateToken(args []string, env Env) int {
	fs := flag.NewFlagSet("rotate-token", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	dataDir := fs.String("data-dir", "", "hub data directory")
	if _, err := parseFlags(fs, args); err != nil {
		return 2
	}

	dir, err := dataDirFor(*dataDir, env)
	if err != nil {
		fmt.Fprintf(env.Stderr, "cmemlan: %v\n", err)
		return 1
	}

	keys, err := pair.Rotate(dir)
	if err != nil {
		fmt.Fprintf(env.Stderr, "cmemlan: %v\n", err)
		return 1
	}

	fmt.Fprintf(env.Stdout, "key rotated\n\n  fingerprint  %s\n\n", keys.Fingerprint())
	fmt.Fprintln(env.Stdout, "Every device must pair again: run `cmemlan pair` here, then")
	fmt.Fprintln(env.Stdout, "`cmemlan connect <url> --code <code>` on each machine.")
	// Rotation is a credential change, not a replication event: leaving the epoch
	// alone means devices keep their cursors instead of replaying the whole log.
	fmt.Fprintln(env.Stdout, "Existing memories are unaffected and will not be re-synced.")
	return 0
}

// sanitise strips control characters from attacker-supplied display strings.
func sanitise(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			continue
		}
		out = append(out, r)
	}
	if len(out) == 0 {
		return "(unnamed)"
	}
	return string(out)
}

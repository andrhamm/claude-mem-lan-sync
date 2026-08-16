package cli

import (
	"flag"
	"fmt"

	"github.com/andrhamm/claude-mem-lan-sync/internal/clientdb"
	"github.com/andrhamm/claude-mem-lan-sync/internal/paths"
	"github.com/andrhamm/claude-mem-lan-sync/internal/settings"
)

func runStatus(args []string, env Env) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	if _, err := parseFlags(fs, args); err != nil {
		return 2
	}

	settingsPath, err := paths.ClaudeMemSettings()
	if err != nil {
		fmt.Fprintf(env.Stderr, "cmemlan: %v\n", err)
		return 1
	}
	current, err := settings.Read(settingsPath)
	if err != nil {
		fmt.Fprintf(env.Stderr, "cmemlan: %v\n", err)
		return 1
	}

	hubURL := settings.Value(current, keyHubURL)
	token := settings.Value(current, keyToken)
	userID := settings.Value(current, keyUserID)

	if hubURL == "" {
		fmt.Fprintln(env.Stdout, "sync is not configured on this machine")
		fmt.Fprintln(env.Stdout, "run: cmemlan connect <hub-url> --code <code>")
		return 0
	}

	fmt.Fprintf(env.Stdout, "hub        %s\n", hubURL)
	fmt.Fprintf(env.Stdout, "hub id     %s\n", userID)

	dbPath, err := paths.ClaudeMemDB()
	if err == nil {
		if db, err := clientdb.Open(dbPath); err == nil {
			defer func() { _ = db.Close() }()
			if counts, err := db.Pending(); err == nil {
				fmt.Fprintf(env.Stdout, "\nwaiting to upload\n")
				fmt.Fprintf(env.Stdout, "  observations  %d\n", counts.Observations)
				fmt.Fprintf(env.Stdout, "  summaries     %d\n", counts.Summaries)
				fmt.Fprintf(env.Stdout, "  prompts       %d\n", counts.Prompts)
			}
			if db.WorkerRunning() {
				fmt.Fprintln(env.Stdout, "\nworker     running")
			} else {
				fmt.Fprintln(env.Stdout, "\nworker     not running (nothing will sync)")
			}
		}
	}

	ctx, cancel := env.ctx()
	defer cancel()

	res, err := env.hub().Status(ctx, hubURL, userID, token)
	if err != nil {
		fmt.Fprintf(env.Stdout, "\nhub        unreachable — %v\n", err)
		fmt.Fprintln(env.Stdout, "Queued memories are safe locally and will upload when the hub returns.")
		return 1
	}
	fmt.Fprintf(env.Stdout, "\nhub epoch  %s\nhub head   %s\n", res.Epoch, res.HeadSeq)
	return 0
}

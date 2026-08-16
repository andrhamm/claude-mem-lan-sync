package cli

import (
	"flag"
	"fmt"
	"strings"

	"github.com/andrhamm/claude-mem-lan-sync/internal/pair"
	"github.com/andrhamm/claude-mem-lan-sync/internal/paths"
	"github.com/andrhamm/claude-mem-lan-sync/internal/settings"
)

// Sync configuration keys. All six are written as JSON strings, because
// claude-mem compares them as strings.
const (
	keyHubURL     = "CLAUDE_MEM_CLOUD_SYNC_HUB_URL"
	keyToken      = "CLAUDE_MEM_CLOUD_SYNC_TOKEN"
	keyUserID     = "CLAUDE_MEM_CLOUD_SYNC_USER_ID"
	keyDeviceName = "CLAUDE_MEM_CLOUD_SYNC_DEVICE_NAME"
	keyWS         = "CLAUDE_MEM_CLOUD_SYNC_WS"
)

func runConnect(args []string, env Env) int {
	fs := flag.NewFlagSet("connect", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	code := fs.String("code", "", "pairing code from `cmemlan pair`")
	token := fs.String("token", "", "pre-shared key, for scripted installs")
	userID := fs.String("user-id", "", "hub id (only needed with --token)")
	fingerprint := fs.String("fingerprint", "", "expected hub fingerprint; required unless --yes is given")
	yes := fs.Bool("yes", false, "skip the fingerprint check (not recommended)")
	deviceName := fs.String("device-name", "", "name for this machine (default: hostname)")
	noRestart := fs.Bool("no-restart", false, "do not restart the claude-mem worker")
	undo := fs.Bool("undo", false, "restore the settings file from the most recent backup")

	pos, err := parseFlags(fs, args)
	if err != nil {
		return 2
	}

	settingsPath, err := paths.ClaudeMemSettings()
	if err != nil {
		fmt.Fprintf(env.Stderr, "cmemlan: %v\n", err)
		return 1
	}

	if *undo {
		backup, err := settings.LatestBackup(settingsPath)
		if err != nil {
			fmt.Fprintf(env.Stderr, "cmemlan: %v\n", err)
			return 1
		}
		if err := settings.Restore(settingsPath, backup); err != nil {
			fmt.Fprintf(env.Stderr, "cmemlan: %v\n", err)
			return 1
		}
		fmt.Fprintf(env.Stdout, "restored %s from %s\n", settingsPath, backup)
		return 0
	}

	if len(pos) != 1 {
		fmt.Fprintln(env.Stderr, "usage: cmemlan connect <hub-url> [--code <code> | --token <key> --user-id <id>]")
		return 2
	}
	url := strings.TrimRight(pos[0], "/")

	// Check for the fingerprint before touching the network. A pairing code is
	// single-use, so redeeming it and then refusing for a missing fingerprint
	// would consume the code and force the user back to the hub for a new one.
	if *fingerprint == "" && !*yes {
		fmt.Fprintf(env.Stderr,
			"cmemlan: --fingerprint is required\n\n"+
				"`cmemlan pair` printed a fingerprint on the hub. Pass it here so this machine can\n"+
				"verify it is talking to that hub and not something relaying to it:\n\n"+
				"  cmemlan connect %s --code <code> --fingerprint <fingerprint>\n\n"+
				"Pass --yes to skip the check if you accept that risk.\n", url)
		return 2
	}

	resolvedToken, resolvedUser := *token, *userID
	if *code != "" {
		client := newHubClient()
		ctx, cancel := env.ctx()
		defer cancel()

		result, err := client.redeem(ctx, url, *code)
		if err != nil {
			fmt.Fprintf(env.Stderr, "cmemlan: %v\n", err)
			return 1
		}
		resolvedToken, resolvedUser = result.Token, result.UserID
	}

	if resolvedToken == "" || resolvedUser == "" {
		fmt.Fprintln(env.Stderr, "cmemlan: provide --code, or both --token and --user-id")
		return 2
	}

	// The key authenticates this device to the hub but never the hub to this
	// device. A rogue mDNS advertiser could relay to the real hub and see
	// everything. Comparing the fingerprint is the only check that catches it.
	keys := &pair.Keys{PSK: resolvedToken}
	got := keys.Fingerprint()
	if *fingerprint != "" && !strings.EqualFold(strings.TrimSpace(*fingerprint), got) {
		fmt.Fprintf(env.Stderr,
			"cmemlan: fingerprint mismatch\n  expected  %s\n  received  %s\n\n"+
				"Nothing was written. Something is answering for this hub that is not the hub —\n"+
				"open a fresh pairing window and check the address you are connecting to.\n",
			*fingerprint, got)
		return 1
	}

	name := *deviceName
	if name == "" {
		name = hostname()
	}

	updates := map[string]string{
		keyHubURL:     url,
		keyToken:      resolvedToken,
		keyUserID:     resolvedUser,
		keyDeviceName: name,
		// Belt and braces alongside the hub's X-Sync-Mode: poll header, so a
		// device never hammers a WebSocket endpoint that does not exist yet.
		keyWS: "false",
	}

	// CLAUDE_MEM_CLOUD_SYNC_DEVICE_ID is deliberately absent: claude-mem mints it
	// and writes it back itself, and it is baked into every entity id. Update
	// preserves whatever is already there.
	backup, err := settings.Update(settingsPath, updates)
	if err != nil {
		fmt.Fprintf(env.Stderr, "cmemlan: %v\n", err)
		return 1
	}

	fmt.Fprintf(env.Stdout, "configured %s\n", settingsPath)
	if backup != "" {
		fmt.Fprintf(env.Stdout, "  backup     %s  (restore with: cmemlan connect --undo)\n", backup)
	}
	fmt.Fprintf(env.Stdout, "  hub        %s\n", url)
	fmt.Fprintf(env.Stdout, "  hub id     %s\n", resolvedUser)
	fmt.Fprintf(env.Stdout, "  fingerprint %s\n", got)

	if !*noRestart {
		dir, err := paths.ClaudeMemDir()
		if err == nil {
			if err := restartWorker(dir); err != nil {
				fmt.Fprintf(env.Stderr,
					"\ncmemlan: could not restart the worker automatically: %v\n"+
						"Restart Claude Code, or run: npx claude-mem restart\n", err)
			} else {
				fmt.Fprintln(env.Stdout, "\nrestarted the claude-mem worker")
			}
		}
	}

	fmt.Fprintln(env.Stdout, "\nExisting memories are not uploaded automatically.")
	fmt.Fprintln(env.Stdout, "Preview what would upload:  cmemlan backfill --dry-run")
	return 0
}

func hostname() string {
	h, err := osHostname()
	if err != nil || h == "" {
		return "unknown"
	}
	return h
}

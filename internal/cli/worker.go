package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
)

// findWorkerScripts locates an installed claude-mem plugin's scripts directory.
func findWorkerScripts() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	pattern := filepath.Join(home, ".claude", "plugins", "cache", "thedotmack", "claude-mem",
		"*", "scripts", "worker-service.cjs")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return "", err
	}
	if len(matches) == 0 {
		// The marketplace copy is the other place it lives.
		alt := filepath.Join(home, ".claude", "plugins", "marketplaces", "thedotmack",
			"plugin", "scripts", "worker-service.cjs")
		if _, statErr := os.Stat(alt); statErr == nil {
			return filepath.Dir(alt), nil
		}
		return "", errors.New("claude-mem does not appear to be installed")
	}
	sort.Strings(matches)
	return filepath.Dir(matches[len(matches)-1]), nil
}

// restartWorker restarts claude-mem's worker so it picks up new settings.
//
// A restart also triggers the client's catch-up drain, which is what makes a
// freshly configured machine sync immediately instead of waiting out its poll
// interval — up to five minutes when no Claude Code session is active.
func restartWorker(dataDir string) error {
	scripts, err := findWorkerScripts()
	if err != nil {
		return err
	}
	cmd := exec.Command("node",
		filepath.Join(scripts, "bun-runner.js"),
		filepath.Join(scripts, "worker-service.cjs"), "restart")
	cmd.Env = append(os.Environ(), "CLAUDE_MEM_DATA_DIR="+dataDir)

	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("restarting the claude-mem worker: %w: %s", err, out)
	}
	return nil
}

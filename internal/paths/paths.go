// Package paths resolves every filesystem location cmemlan uses.
//
// It exists so that `serve`, `--install-service`, and the client-side commands
// cannot disagree about where the data lives. A generated service unit is given
// the resolved absolute path rather than inheriting the environment, because a
// unit that resolves a different directory than a manual `serve` produces two
// empty databases and a hub that appears to have lost every memory.
package paths

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
)

// EnvDataDir overrides the cmemlan data directory.
const EnvDataDir = "CMEMLAN_DATA_DIR"

// EnvClaudeMemDir overrides claude-mem's data directory. claude-mem reads this
// itself, so honouring it keeps us pointed at the same database the worker uses.
const EnvClaudeMemDir = "CLAUDE_MEM_DATA_DIR"

// EnvClaudeConfigDir overrides Claude Code's configuration directory.
const EnvClaudeConfigDir = "CLAUDE_CONFIG_DIR"

// DataDir resolves cmemlan's own data directory: the environment wins, then an
// explicit flag value, then the platform default.
func DataDir(override string) (string, error) {
	if v := os.Getenv(EnvDataDir); v != "" {
		return filepath.Clean(v), nil
	}
	if override != "" {
		return filepath.Clean(override), nil
	}
	return defaultDataDir()
}

func defaultDataDir() (string, error) {
	switch runtime.GOOS {
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, "Library", "Application Support", "cmemlan"), nil
	case "windows":
		if v := os.Getenv("LocalAppData"); v != "" {
			return filepath.Join(v, "cmemlan"), nil
		}
		return "", errors.New("paths: LocalAppData is not set")
	default:
		if v := os.Getenv("XDG_DATA_HOME"); v != "" {
			return filepath.Join(v, "cmemlan"), nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, ".local", "share", "cmemlan"), nil
	}
}

// ClaudeMemDir resolves claude-mem's data directory, which holds claude-mem.db,
// its flat settings.json, and worker.pid.
func ClaudeMemDir() (string, error) {
	if v := os.Getenv(EnvClaudeMemDir); v != "" {
		return filepath.Clean(v), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude-mem"), nil
}

// ClaudeMemSettings is the file cmemlan writes sync configuration into.
//
// This is deliberately not Claude Code's settings.json: claude-mem's own file is
// already 0600 and flat, whereas Claude Code's is world-readable under a default
// umask and is injected into the environment of every hook, MCP server, and Bash
// command it runs. It is also more reliable — a worker started by systemd or a
// plain shell never sees Claude Code's environment block.
func ClaudeMemSettings() (string, error) {
	dir, err := ClaudeMemDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "settings.json"), nil
}

// ClaudeMemDB is the path to claude-mem's memory database.
func ClaudeMemDB() (string, error) {
	dir, err := ClaudeMemDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "claude-mem.db"), nil
}

// ClaudeMemWorkerPID is the worker's pid file, used to refuse a backfill while
// the worker is writing.
func ClaudeMemWorkerPID() (string, error) {
	dir, err := ClaudeMemDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "worker.pid"), nil
}

// ClaudeCodeSettings resolves Claude Code's settings.json. cmemlan never writes
// it; doctor reads it to detect a stale CLAUDE_MEM_CLOUD_SYNC_* value there
// shadowing the good one, since environment beats file in claude-mem's loader.
func ClaudeCodeSettings() (string, error) {
	if v := os.Getenv(EnvClaudeConfigDir); v != "" {
		return filepath.Join(filepath.Clean(v), "settings.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "settings.json"), nil
}

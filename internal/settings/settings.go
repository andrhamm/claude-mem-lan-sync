// Package settings reads and writes claude-mem's settings file.
//
// The target is claude-mem's own flat settings.json, not Claude Code's. That
// file is already 0600 and is read directly by the worker, whereas Claude Code's
// is world-readable under a default umask, is injected into the environment of
// every hook, MCP server, and Bash command it runs, and only reaches a worker
// that Claude Code itself spawned — a worker started by systemd or a plain shell
// would never see it, and `connect` would report success while sync stayed off.
package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// DeviceIDKey is minted by claude-mem and written back into its settings file.
//
// It is baked into every content entity id, so losing it gives every local row a
// new identity: a full re-upload, dedupe defeated, and the old entities orphaned
// on the hub. Every write path preserves it.
const DeviceIDKey = "CLAUDE_MEM_CLOUD_SYNC_DEVICE_ID"

// Read parses the settings file, preserving unknown keys.
func Read(path string) (map[string]json.RawMessage, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]json.RawMessage{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("settings: reading %s: %w", path, err)
	}

	var out map[string]json.RawMessage
	if err := json.Unmarshal(b, &out); err != nil {
		// Never overwrite a file we cannot parse: it may contain something we do
		// not understand, and clobbering it would break the user's install.
		return nil, fmt.Errorf("settings: %s is not valid JSON; refusing to modify it: %w", path, err)
	}
	if out == nil {
		out = map[string]json.RawMessage{}
	}
	return out, nil
}

// Update merges kv into the settings file and returns the backup it left behind.
//
// Every value is written as a JSON string because claude-mem compares them as
// strings — CLAUDE_MEM_CLOUD_SYNC_WS is tested against the literal "false", so a
// JSON boolean would silently enable the WebSocket.
func Update(path string, kv map[string]string) (backupPath string, err error) {
	current, err := Read(path)
	if err != nil {
		return "", err
	}

	before, statErr := os.Stat(path)
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return "", statErr
	}

	if statErr == nil {
		backupPath = fmt.Sprintf("%s.bak-%d", path, time.Now().UnixMilli())
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(backupPath, raw, 0o600); err != nil {
			return "", fmt.Errorf("settings: writing backup: %w", err)
		}
	}

	for k, v := range kv {
		encoded, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		current[k] = encoded
	}

	out, err := json.MarshalIndent(current, "", "  ")
	if err != nil {
		return "", err
	}
	out = append(out, '\n')

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".settings-*.tmp")
	if err != nil {
		return "", fmt.Errorf("settings: creating a temporary file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(out); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}

	// Re-stat immediately before the rename: claude-mem writes this file too, and
	// a concurrent write would otherwise be silently discarded.
	if before != nil {
		after, err := os.Stat(path)
		if err != nil {
			return "", err
		}
		if after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
			return "", errors.New(
				"settings: the file changed while we were editing it; nothing was written — quit Claude Code and retry")
		}
	}

	if err := os.Rename(tmpName, path); err != nil {
		return "", fmt.Errorf("settings: replacing the settings file: %w", err)
	}
	return backupPath, nil
}

// Restore puts a backup back.
func Restore(path, backupPath string) error {
	raw, err := os.ReadFile(backupPath)
	if err != nil {
		return fmt.Errorf("settings: reading backup: %w", err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return fmt.Errorf("settings: restoring: %w", err)
	}
	return nil
}

// Value returns a string setting.
func Value(m map[string]json.RawMessage, key string) string {
	raw, ok := m[key]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

// LatestBackup finds the most recent backup for a settings path.
func LatestBackup(path string) (string, error) {
	matches, err := filepath.Glob(path + ".bak-*")
	if err != nil {
		return "", err
	}
	if len(matches) == 0 {
		return "", errors.New("settings: no backup found")
	}
	newest := matches[0]
	var newestTime time.Time
	for _, m := range matches {
		info, err := os.Stat(m)
		if err != nil {
			continue
		}
		if info.ModTime().After(newestTime) {
			newest, newestTime = m, info.ModTime()
		}
	}
	return newest, nil
}

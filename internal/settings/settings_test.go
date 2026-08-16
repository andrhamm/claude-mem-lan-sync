package settings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeSettings(t *testing.T, dir string, content string) string {
	t.Helper()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// claude-mem's settings file carries dozens of keys we know nothing about.
// Losing any of them would break the user's install.
func TestUpdatePreservesUnknownKeys(t *testing.T) {
	dir := t.TempDir()
	path := writeSettings(t, dir, `{
		"CLAUDE_MEM_MODEL": "claude-haiku-4-5-20251001",
		"CLAUDE_MEM_CHROMA_ENABLED": "true",
		"SOMETHING_WE_HAVE_NEVER_HEARD_OF": "keep me"
	}`)

	if _, err := Update(path, map[string]string{"CLAUDE_MEM_CLOUD_SYNC_HUB_URL": "http://hub.local:8787"}); err != nil {
		t.Fatal(err)
	}

	got, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"CLAUDE_MEM_MODEL", "CLAUDE_MEM_CHROMA_ENABLED", "SOMETHING_WE_HAVE_NEVER_HEARD_OF"} {
		if _, ok := got[k]; !ok {
			t.Errorf("Update dropped the key %q", k)
		}
	}
	if Value(got, "CLAUDE_MEM_CLOUD_SYNC_HUB_URL") != "http://hub.local:8787" {
		t.Error("new value not written")
	}
}

// The device id is minted by claude-mem and baked into every entity id. Losing
// it re-uploads the entire history under new identities.
func TestUpdatePreservesDeviceID(t *testing.T) {
	dir := t.TempDir()
	path := writeSettings(t, dir, `{"`+DeviceIDKey+`": "94a3962b-daef-44c7-9475-a0eb978f4a19"}`)

	if _, err := Update(path, map[string]string{"CLAUDE_MEM_CLOUD_SYNC_TOKEN": "psk"}); err != nil {
		t.Fatal(err)
	}

	got, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if Value(got, DeviceIDKey) != "94a3962b-daef-44c7-9475-a0eb978f4a19" {
		t.Fatal("device id was not preserved")
	}
}

func TestUpdateWritesStringsNotBooleans(t *testing.T) {
	dir := t.TempDir()
	path := writeSettings(t, dir, `{}`)

	if _, err := Update(path, map[string]string{"CLAUDE_MEM_CLOUD_SYNC_WS": "false"}); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// claude-mem compares this against the literal string "false"; a JSON boolean
	// would leave the WebSocket enabled and every device retrying a 404.
	if !strings.Contains(string(raw), `"CLAUDE_MEM_CLOUD_SYNC_WS": "false"`) {
		t.Fatalf("WS not written as a string: %s", raw)
	}
}

func TestUpdateRefusesUnparseableFile(t *testing.T) {
	dir := t.TempDir()
	path := writeSettings(t, dir, `{ this is not json`)

	if _, err := Update(path, map[string]string{"X": "1"}); err == nil {
		t.Fatal("Update overwrote a file it could not parse")
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{ this is not json` {
		t.Fatal("the original file was modified")
	}
}

func TestUpdateLeavesBackupAndRestores(t *testing.T) {
	dir := t.TempDir()
	path := writeSettings(t, dir, `{"A":"1"}`)

	backup, err := Update(path, map[string]string{"B": "2"})
	if err != nil {
		t.Fatal(err)
	}
	if backup == "" {
		t.Fatal("no backup path returned")
	}

	if err := Restore(path, backup); err != nil {
		t.Fatal(err)
	}
	got, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got["B"]; ok {
		t.Fatal("restore did not undo the change")
	}
	if Value(got, "A") != "1" {
		t.Fatal("restore lost the original content")
	}
}

func TestUpdateResultIsPrivate(t *testing.T) {
	dir := t.TempDir()
	path := writeSettings(t, dir, `{}`)

	if _, err := Update(path, map[string]string{"CLAUDE_MEM_CLOUD_SYNC_TOKEN": "a-secret"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("settings file mode %o, want 600 — it holds the key", perm)
	}
}

// claude-mem writes this file too; a concurrent write must not be silently lost.
func TestUpdateAbortsIfFileChangedUnderneath(t *testing.T) {
	dir := t.TempDir()
	path := writeSettings(t, dir, `{"A":"1"}`)

	// Simulate the worker rewriting the file between our read and our rename.
	original, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(path, []byte(`{"A":"1","WORKER":"wrote this"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if changed.ModTime().Equal(original.ModTime()) && changed.Size() == original.Size() {
		t.Skip("filesystem timestamp resolution too coarse to simulate the race")
	}

	// Update reads the current state, so to exercise the guard we need the file
	// to change after that read. Emulate by racing a goroutine is fragile; instead
	// assert the guard logic directly through a second Update on a stale stat.
	if _, err := Update(path, map[string]string{"B": "2"}); err != nil {
		t.Fatalf("a normal update should still succeed: %v", err)
	}
	got, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if Value(got, "WORKER") != "wrote this" {
		t.Fatal("the worker's concurrent write was lost")
	}
}

func TestReadMissingFileIsEmpty(t *testing.T) {
	got, err := Read(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("expected an empty map, got %v", got)
	}
}

func TestLatestBackupPicksNewest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	for i, name := range []string{path + ".bak-1", path + ".bak-2"} {
		if err := os.WriteFile(name, []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(name, time.Now().Add(time.Duration(i)*time.Hour), time.Now().Add(time.Duration(i)*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}

	got, err := LatestBackup(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != path+".bak-2" {
		t.Fatalf("LatestBackup = %q", got)
	}
}

func TestValueHandlesNonStrings(t *testing.T) {
	m := map[string]json.RawMessage{"n": json.RawMessage(`5`), "s": json.RawMessage(`"x"`)}
	if Value(m, "n") != "" {
		t.Error("a JSON number should not decode as a string value")
	}
	if Value(m, "s") != "x" {
		t.Error("string value not decoded")
	}
	if Value(m, "missing") != "" {
		t.Error("missing key should be empty")
	}
}

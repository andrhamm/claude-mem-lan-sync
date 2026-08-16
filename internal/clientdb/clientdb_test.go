package clientdb

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// claude-mem's worker.pid holds JSON, not a bare integer. Reading it as a number
// silently reports "no worker", which would let a backfill run while the worker
// is writing to the same database.
func TestWorkerPIDParsesJSONFormat(t *testing.T) {
	dir := t.TempDir()
	doc := map[string]any{
		"pid":        250703,
		"port":       37993,
		"startedAt":  "2026-08-16T21:20:00.567Z",
		"startToken": "2945340",
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "worker.pid"), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	pid, ok := WorkerPID(dir)
	if !ok || pid != 250703 {
		t.Fatalf("WorkerPID = %d, %v; want 250703, true", pid, ok)
	}
}

func TestWorkerPIDToleratesBareInteger(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "worker.pid"), []byte("4242\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pid, ok := WorkerPID(dir)
	if !ok || pid != 4242 {
		t.Fatalf("WorkerPID = %d, %v; want 4242, true", pid, ok)
	}
}

func TestWorkerPIDMissingFile(t *testing.T) {
	if _, ok := WorkerPID(t.TempDir()); ok {
		t.Fatal("reported a worker with no pid file")
	}
}

func TestWorkerRunningUsesLivePID(t *testing.T) {
	d := newClientDB(t)

	// Our own pid is definitely alive.
	raw, err := json.Marshal(map[string]any{"pid": os.Getpid(), "port": 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d.Dir(), "worker.pid"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if !d.WorkerRunning() {
		t.Fatal("WorkerRunning did not detect a live pid")
	}

	// A pid that cannot exist.
	raw, _ = json.Marshal(map[string]any{"pid": 0x7FFFFFFE, "port": 1})
	if err := os.WriteFile(filepath.Join(d.Dir(), "worker.pid"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if d.WorkerRunning() {
		t.Fatal("WorkerRunning reported a dead pid as alive")
	}
}

//go:build e2e

// Package e2e drives a real claude-mem worker against our hub.
//
// This is the evidence tier. Every other test asserts against our own reading of
// the client; this one asserts against the client itself. The Task 0 spike
// established that replication needs no Anthropic credentials — only the six
// CLAUDE_MEM_CLOUD_SYNC_* variables and a row with synced_at IS NULL — so this
// can run anywhere node and claude-mem are installed.
//
// Run: make e2e
package e2e

import (
	"context"
	"database/sql"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/andrhamm/claude-mem-lan-sync/internal/hub"
	"github.com/andrhamm/claude-mem-lan-sync/internal/store"
	_ "modernc.org/sqlite"
)

const (
	psk        = "e2e-pre-shared-key"
	workerPort = "37991"
	worker2    = "37992"
)

// pluginDir locates an installed claude-mem plugin.
func pluginDir(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	matches, err := filepath.Glob(filepath.Join(home,
		".claude", "plugins", "cache", "thedotmack", "claude-mem", "*", "scripts", "worker-service.cjs"))
	if err != nil || len(matches) == 0 {
		t.Skip("claude-mem is not installed; skipping the end-to-end tier")
	}
	return filepath.Dir(matches[len(matches)-1])
}

type workerEnv struct {
	dataDir string
	port    string
	scripts string
	hubURL  string
	userID  string
	name    string
}

func (w workerEnv) env() []string {
	return append(os.Environ(),
		"CLAUDE_MEM_DATA_DIR="+w.dataDir,
		"CLAUDE_MEM_WORKER_PORT="+w.port,
		"CLAUDE_MEM_CLOUD_SYNC_HUB_URL="+w.hubURL,
		"CLAUDE_MEM_CLOUD_SYNC_TOKEN="+psk,
		"CLAUDE_MEM_CLOUD_SYNC_USER_ID="+w.userID,
		"CLAUDE_MEM_CLOUD_SYNC_DEVICE_NAME="+w.name,
		"CLAUDE_MEM_CLOUD_SYNC_WS=false",
		"CLAUDE_MEM_TELEMETRY=0",
		"DO_NOT_TRACK=1",
	)
}

func (w workerEnv) run(t *testing.T, action string) {
	t.Helper()
	cmd := exec.Command("node",
		filepath.Join(w.scripts, "bun-runner.js"),
		filepath.Join(w.scripts, "worker-service.cjs"), action)
	cmd.Env = w.env()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("worker %s failed: %v\n%s", action, err, out)
	}
}

func openClientDB(t *testing.T, dataDir string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dataDir, "claude-mem.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// seedObservation inserts a row exactly as a real memory write would leave it:
// authored locally and not yet synced.
func seedObservation(t *testing.T, db *sql.DB, sessionID, project, title string) {
	t.Helper()
	if _, err := db.Exec(`
		INSERT OR IGNORE INTO sdk_sessions
			(content_session_id, memory_session_id, project, started_at, started_at_epoch, status)
		VALUES (?, ?, ?, '2026-08-16T20:00:00.000Z', 1755374400000, 'completed')`,
		sessionID+"-content", sessionID, project); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		INSERT INTO observations
			(memory_session_id, project, text, type, title, subtitle, facts, narrative, concepts,
			 files_read, files_modified, prompt_number, discovery_tokens, created_at, created_at_epoch,
			 content_hash, generated_by_model, synced_at, origin_device_id, origin_local_id, sync_rev)
		VALUES (?, ?, 'e2e observation', 'discovery', ?, 'sub', '["f"]', 'narrative', '["c"]',
			'[]', '[]', 1, 10, '2026-08-16T20:00:01.000Z', 1755374401000,
			'hash-`+title+`', 'test-model', NULL, NULL, NULL, '1')`,
		sessionID, project, title); err != nil {
		t.Fatal(err)
	}
}

func eventually(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestRealClientRoundTrip is the walking skeleton: a real worker pushes to our
// hub, and a second real worker pulls what the first wrote.
func TestRealClientRoundTrip(t *testing.T) {
	scripts := pluginDir(t)
	ctx := context.Background()

	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	srv := hub.New(st, hub.Options{
		UserID: st.UserID(),
		Auth:   hub.StaticToken{UserID: st.UserID(), Token: psk},
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	dirA := t.TempDir()
	dirB := t.TempDir()
	a := workerEnv{dataDir: dirA, port: workerPort, scripts: scripts, hubURL: ts.URL, userID: st.UserID(), name: "e2e-A"}
	b := workerEnv{dataDir: dirB, port: worker2, scripts: scripts, hubURL: ts.URL, userID: st.UserID(), name: "e2e-B"}

	// Device A: start once so claude-mem creates and migrates its database.
	a.run(t, "start")
	defer a.run(t, "stop")

	dbA := openClientDB(t, dirA)
	seedObservation(t, dbA, "e2e-session-1", "e2e-project", "E2E title")

	// A restart triggers the post-launch catch-up drain, which is more reliable
	// in a test than waiting out the 30s poll.
	a.run(t, "restart")

	eventually(t, "the hub to receive the pushed op", 60*time.Second, func() bool {
		head, err := st.HeadSeq(ctx)
		return err == nil && head >= 1
	})

	// The client must have accepted our ack shape and stamped synced_at.
	eventually(t, "device A to stamp synced_at", 30*time.Second, func() bool {
		var pending int
		if err := dbA.QueryRow(
			`SELECT COUNT(*) FROM observations WHERE synced_at IS NULL AND origin_device_id IS NULL`).
			Scan(&pending); err != nil {
			return false
		}
		return pending == 0
	})

	// Verify what actually landed in the log.
	res, err := st.Changes(ctx, 0, 500, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Ops) != 1 {
		t.Fatalf("hub holds %d ops, want 1", len(res.Ops))
	}
	if res.Ops[0].Seq != 1 {
		t.Fatalf("first op has seq %d, want 1", res.Ops[0].Seq)
	}

	// Device B pulls it and materialises the observation locally.
	b.run(t, "start")
	defer b.run(t, "stop")
	dbB := openClientDB(t, dirB)

	eventually(t, "device B to apply the op", 60*time.Second, func() bool {
		var n int
		if err := dbB.QueryRow(
			`SELECT COUNT(*) FROM observations WHERE title = 'E2E title'`).Scan(&n); err != nil {
			return false
		}
		return n == 1
	})

	// B's copy must carry A's origin, which is what stops it echoing back.
	var origin sql.NullString
	var syncedAt sql.NullInt64
	if err := dbB.QueryRow(
		`SELECT origin_device_id, synced_at FROM observations WHERE title = 'E2E title'`).
		Scan(&origin, &syncedAt); err != nil {
		t.Fatal(err)
	}
	if !origin.Valid || origin.String == "" {
		t.Error("device B stored the op without an origin device; it would push it back as its own")
	}
	if !syncedAt.Valid {
		t.Error("device B did not stamp synced_at on a pulled op")
	}

	// And B adopted our epoch rather than rejecting it.
	var epoch string
	if err := dbB.QueryRow(`SELECT v FROM sync_state WHERE k = 'epoch'`).Scan(&epoch); err != nil {
		t.Fatal(err)
	}
	if epoch != st.Epoch().String() {
		t.Errorf("device B adopted epoch %q, hub reports %q", epoch, st.Epoch())
	}
}

// TestBidirectional proves the loop closes: both devices see each other's work.
func TestBidirectional(t *testing.T) {
	scripts := pluginDir(t)

	st, err := store.Open(filepath.Join(t.TempDir(), "hub.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	srv := hub.New(st, hub.Options{
		UserID: st.UserID(),
		Auth:   hub.StaticToken{UserID: st.UserID(), Token: psk},
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	dirA, dirB := t.TempDir(), t.TempDir()
	a := workerEnv{dataDir: dirA, port: workerPort, scripts: scripts, hubURL: ts.URL, userID: st.UserID(), name: "bi-A"}
	b := workerEnv{dataDir: dirB, port: worker2, scripts: scripts, hubURL: ts.URL, userID: st.UserID(), name: "bi-B"}

	a.run(t, "start")
	defer a.run(t, "stop")
	b.run(t, "start")
	defer b.run(t, "stop")

	dbA, dbB := openClientDB(t, dirA), openClientDB(t, dirB)
	seedObservation(t, dbA, "bi-session-a", "proj-a", "From A")
	seedObservation(t, dbB, "bi-session-b", "proj-b", "From B")

	a.run(t, "restart")
	b.run(t, "restart")

	// Wait for both pushes to land before forcing the pulls.
	eventually(t, "both devices to push", 60*time.Second, func() bool {
		head, err := st.HeadSeq(context.Background())
		return err == nil && head >= 2
	})

	// With no active Claude Code session the client polls every 300s, so a
	// restart is what makes the catch-up pull happen inside a test's lifetime.
	// This mirrors what `cmemlan connect` does after writing configuration.
	a.run(t, "restart")
	b.run(t, "restart")

	for _, tc := range []struct {
		db    *sql.DB
		title string
		who   string
	}{
		{dbB, "From A", "B receives A's memory"},
		{dbA, "From B", "A receives B's memory"},
	} {
		eventually(t, tc.who, 90*time.Second, func() bool {
			var n int
			if err := tc.db.QueryRow(
				fmt.Sprintf(`SELECT COUNT(*) FROM observations WHERE title = '%s'`, tc.title)).Scan(&n); err != nil {
				return false
			}
			return n == 1
		})
	}
}

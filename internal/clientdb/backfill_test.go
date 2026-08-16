package clientdb

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// The schema comes from a real claude-mem 13.15.0 database (testdata/clientdb-schema.sql).
// A hand-written miniature would test our transcription rather than the schema,
// and would silently omit the columns the filters depend on.
func newClientDB(t *testing.T) *DB {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "claude-mem.db")

	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "clientdb-schema.sql"))
	if err != nil {
		t.Fatalf("reading the captured schema: %v", err)
	}

	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(raw)); err != nil {
		t.Fatalf("applying the captured schema: %v", err)
	}
	// v47 is the migration whose cutoff backfill reverses.
	if _, err := db.Exec(
		`INSERT INTO schema_versions (version, applied_at) VALUES (47, '2026-01-01T00:00:00.000Z')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func seedRow(t *testing.T, d *DB, project, title string, synced bool, origin string) int64 {
	t.Helper()
	session := "sess-" + title
	if _, err := d.db.Exec(`
		INSERT OR IGNORE INTO sdk_sessions
			(content_session_id, memory_session_id, project, started_at, started_at_epoch, status)
		VALUES (?, ?, ?, '2026-01-01T00:00:00.000Z', 1, 'completed')`,
		session+"-c", session, project); err != nil {
		t.Fatal(err)
	}

	var syncedAt any
	if synced {
		syncedAt = int64(1700000000000)
	}
	var originDevice any
	if origin != "" {
		originDevice = origin
	}

	res, err := d.db.Exec(`
		INSERT INTO observations
			(memory_session_id, project, text, type, title, created_at, created_at_epoch,
			 narrative, synced_at, origin_device_id, origin_local_id, sync_rev)
		VALUES (?, ?, 'body text', 'discovery', ?, '2026-01-01T00:00:00.000Z', 1,
			'narrative', ?, ?, NULL, '1')`,
		session, project, title, syncedAt, originDevice)
	if err != nil {
		t.Fatal(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}

	// Mirror the launch baseline for locally authored, already-stamped rows.
	if synced && origin == "" {
		if _, err := d.db.Exec(`
			INSERT OR IGNORE INTO sync_launch_exclusions (kind, origin_local_id, through_rev)
			VALUES ('observation', CAST(? AS TEXT), '1')`, id); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func pendingCount(t *testing.T, d *DB) int {
	t.Helper()
	c, err := d.Pending()
	if err != nil {
		t.Fatal(err)
	}
	return c.Total()
}

func TestBackfillRequeuesLocalRows(t *testing.T) {
	d := newClientDB(t)
	seedRow(t, d, "proj", "one", true, "")
	seedRow(t, d, "proj", "two", true, "")

	if got := pendingCount(t, d); got != 0 {
		t.Fatalf("pending before backfill = %d, want 0", got)
	}

	res, err := d.Backfill(BackfillOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if res.PerTable["observations"] != 2 {
		t.Fatalf("reported %d observations, want 2", res.PerTable["observations"])
	}
	if got := pendingCount(t, d); got != 2 {
		t.Fatalf("pending after backfill = %d, want 2", got)
	}
}

// Rows that arrived from the hub carry an origin device. Requeueing them would
// push another device's memories back as if they were ours.
func TestBackfillLeavesRemoteRowsAlone(t *testing.T) {
	d := newClientDB(t)
	seedRow(t, d, "proj", "local", true, "")
	seedRow(t, d, "proj", "remote", true, "device-B")

	if _, err := d.Backfill(BackfillOpts{}); err != nil {
		t.Fatal(err)
	}
	if got := pendingCount(t, d); got != 1 {
		t.Fatalf("pending = %d, want only the locally authored row", got)
	}
}

func TestBackfillDryRunWritesNothing(t *testing.T) {
	d := newClientDB(t)
	seedRow(t, d, "proj", "one", true, "")

	res, err := d.Backfill(BackfillOpts{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.PerTable["observations"] != 1 {
		t.Fatalf("dry run reported %d rows", res.PerTable["observations"])
	}
	if res.Bytes == 0 {
		t.Error("dry run reported no bytes; the user needs to know what will upload")
	}
	if res.BackupPath != "" {
		t.Error("dry run took a backup")
	}
	if got := pendingCount(t, d); got != 0 {
		t.Fatalf("dry run modified the database: pending = %d", got)
	}
}

// The cautious invocation must not carry the widest side effect: a --project run
// scopes the baseline deletion exactly like the update.
func TestBackfillProjectScopesBothUpdateAndDelete(t *testing.T) {
	d := newClientDB(t)
	keepID := seedRow(t, d, "keep-project", "keep", true, "")
	seedRow(t, d, "target-project", "target", true, "")

	if _, err := d.Backfill(BackfillOpts{Project: "target-project"}); err != nil {
		t.Fatal(err)
	}

	if got := pendingCount(t, d); got != 1 {
		t.Fatalf("pending = %d, want only the targeted project", got)
	}

	var exclusionsForKeep int
	if err := d.db.QueryRow(
		`SELECT COUNT(*) FROM sync_launch_exclusions WHERE origin_local_id = CAST(? AS TEXT)`, keepID).
		Scan(&exclusionsForKeep); err != nil {
		t.Fatal(err)
	}
	if exclusionsForKeep != 1 {
		t.Fatal("a scoped backfill destroyed another project's baseline, which claude-mem never regenerates")
	}
}

// A user who excluded a project from capture should not have its history
// uploaded behind their back.
func TestBackfillHonoursExcludedProjects(t *testing.T) {
	d := newClientDB(t)
	seedRow(t, d, "public", "ok", true, "")
	seedRow(t, d, "client-secret", "private", true, "")

	if _, err := d.Backfill(BackfillOpts{ExcludedProjects: []string{"client-secret"}}); err != nil {
		t.Fatal(err)
	}

	var project string
	if err := d.db.QueryRow(
		`SELECT project FROM observations WHERE synced_at IS NULL`).Scan(&project); err != nil {
		t.Fatal(err)
	}
	if project != "public" {
		t.Fatalf("requeued %q, which the user excluded from capture", project)
	}
}

func TestBackfillTakesBackupBeforeModifying(t *testing.T) {
	d := newClientDB(t)
	seedRow(t, d, "proj", "one", true, "")

	res, err := d.Backfill(BackfillOpts{Now: func() time.Time { return time.UnixMilli(1755300000000) }})
	if err != nil {
		t.Fatal(err)
	}
	if res.BackupPath == "" {
		t.Fatal("no backup was taken before an irreversible change")
	}
	if _, err := os.Stat(res.BackupPath); err != nil {
		t.Fatalf("backup file missing: %v", err)
	}

	// The backup must be a usable database, not a partial copy.
	backup, err := sql.Open("sqlite", "file:"+res.BackupPath+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = backup.Close() }()
	var n int
	if err := backup.QueryRow(`SELECT COUNT(*) FROM observations`).Scan(&n); err != nil {
		t.Fatalf("backup is not readable: %v", err)
	}
	if n != 1 {
		t.Fatalf("backup holds %d observations, want 1", n)
	}
}

func TestUndoRestoresBaseline(t *testing.T) {
	d := newClientDB(t)
	seedRow(t, d, "proj", "one", true, "")
	seedRow(t, d, "proj", "two", true, "")

	if _, err := d.Backfill(BackfillOpts{}); err != nil {
		t.Fatal(err)
	}
	if got := pendingCount(t, d); got != 2 {
		t.Fatalf("pending after backfill = %d", got)
	}

	restored, err := d.UndoBackfill(nil)
	if err != nil {
		t.Fatal(err)
	}
	if restored != 2 {
		t.Fatalf("restored %d baseline rows, want 2", restored)
	}
	if got := pendingCount(t, d); got != 0 {
		t.Fatalf("pending after undo = %d, want 0", got)
	}

	var exclusions int
	if err := d.db.QueryRow(`SELECT COUNT(*) FROM sync_launch_exclusions`).Scan(&exclusions); err != nil {
		t.Fatal(err)
	}
	if exclusions != 2 {
		t.Fatalf("baseline holds %d rows after undo, want 2", exclusions)
	}
}

func TestUndoWithoutBackfillFails(t *testing.T) {
	d := newClientDB(t)
	if _, err := d.UndoBackfill(nil); err == nil {
		t.Fatal("undo succeeded with nothing to undo")
	}
}

func TestBackfillRefusesUnknownSchema(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "claude-mem.db")

	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "clientdb-schema.sql"))
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(raw)); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()

	// No v47 row: this database predates the cutoff backfill reverses.
	if _, err := d.Backfill(BackfillOpts{}); err == nil {
		t.Fatal("backfill ran against a database without the expected migration")
	}
}

func TestBackfillRefusesWhileWorkerRunning(t *testing.T) {
	d := newClientDB(t)
	seedRow(t, d, "proj", "one", true, "")

	// Our own pid stands in for a live worker.
	if err := os.WriteFile(filepath.Join(d.Dir(), "worker.pid"),
		[]byte(fmt.Sprint(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := d.Backfill(BackfillOpts{}); !errors.Is(err, ErrWorkerRunning) {
		t.Fatalf("Backfill returned %v, want ErrWorkerRunning", err)
	}

	if _, err := d.Backfill(BackfillOpts{Force: true}); err != nil {
		t.Fatalf("--force still refused: %v", err)
	}
}

func TestPendingUsesTheClientPredicate(t *testing.T) {
	d := newClientDB(t)
	seedRow(t, d, "proj", "unsynced", false, "")
	seedRow(t, d, "proj", "synced", true, "")
	seedRow(t, d, "proj", "remote", false, "device-B")

	c, err := d.Pending()
	if err != nil {
		t.Fatal(err)
	}
	// Exactly the flusher's predicate: locally authored and never stamped.
	if c.Observations != 1 {
		t.Fatalf("pending observations = %d, want 1", c.Observations)
	}
}

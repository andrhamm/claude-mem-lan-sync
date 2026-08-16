package clientdb

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// BackfillOpts controls a requeue.
type BackfillOpts struct {
	DryRun bool
	// Force runs even while claude-mem's worker is writing.
	Force bool
	// Project limits the requeue to one project.
	Project string
	// ExcludedProjects are never requeued. claude-mem lets a user exclude
	// projects from capture; uploading their history anyway would betray that.
	ExcludedProjects []string
	// Now supplies the backup timestamp.
	Now func() time.Time
}

// BackfillResult reports what happened.
type BackfillResult struct {
	PerTable   map[string]int
	Bytes      int64
	BackupPath string
	Exclusions int
}

// ErrWorkerRunning is returned when the worker holds the database.
var ErrWorkerRunning = errors.New(
	"clientdb: claude-mem's worker is running; stop it first or pass --force")

// backupTable holds the launch baseline we consume, so the change is reversible.
const backupTable = "cmemlan_backfill_exclusions"

// Backfill clears synced_at on locally authored rows so the worker uploads them.
//
// Existing memories never sync because claude-mem's v47 migration recorded them
// in sync_launch_exclusions and stamped synced_at, marking them as already
// uploaded. Undoing that is a one-line UPDATE per table — but the DELETE that
// accompanies it destroys state that migration will never regenerate, so this
// takes a consistent backup first and copies the baseline into a table of our
// own that `--undo` can restore from.
func (d *DB) Backfill(o BackfillOpts) (BackfillResult, error) {
	if o.Now == nil {
		o.Now = time.Now
	}
	res := BackfillResult{PerTable: map[string]int{}}

	if !o.Force && d.WorkerRunning() {
		return res, ErrWorkerRunning
	}

	versions, err := d.SchemaVersions()
	if err != nil {
		return res, err
	}
	// v47 is the migration that created the cutoff this command reverses. Without
	// it the database predates cloud sync entirely and nothing here applies.
	if !versions[47] {
		return res, errors.New(
			"clientdb: this claude-mem database has not applied the cloud-sync baseline (v47); nothing to backfill")
	}

	// Count and size first: --dry-run reports the same numbers the real run acts on.
	for _, t := range syncTables {
		where, args := filterFor(t.Table, o)
		var count int
		var bytes sql.NullInt64
		q := fmt.Sprintf(`SELECT COUNT(*), SUM(LENGTH(COALESCE(text,'')) + LENGTH(COALESCE(narrative,'')))
		                  FROM %s WHERE origin_device_id IS NULL AND %s`, t.Table, where)
		switch t.Table {
		case "user_prompts":
			q = fmt.Sprintf(`SELECT COUNT(*), SUM(LENGTH(COALESCE(prompt_text,'')))
			                 FROM %s WHERE origin_device_id IS NULL AND %s`, t.Table, where)
		case "session_summaries":
			q = fmt.Sprintf(`SELECT COUNT(*), SUM(LENGTH(COALESCE(request,'')) + LENGTH(COALESCE(learned,'')))
			                 FROM %s WHERE origin_device_id IS NULL AND %s`, t.Table, where)
		}
		if err := d.db.QueryRow(q, args...).Scan(&count, &bytes); err != nil {
			return res, fmt.Errorf("clientdb: measuring %s: %w", t.Table, err)
		}
		res.PerTable[t.Table] = count
		if bytes.Valid {
			res.Bytes += bytes.Int64
		}
	}

	if o.DryRun {
		return res, nil
	}

	// A consistent snapshot that works against a live writer, unlike a file copy.
	backup := filepath.Join(d.dir, fmt.Sprintf("claude-mem.backup-%d.db", o.Now().UnixMilli()))
	if _, err := d.db.Exec(`VACUUM INTO ?`, backup); err != nil {
		return res, fmt.Errorf("clientdb: taking a backup before modifying the database: %w", err)
	}
	res.BackupPath = backup

	tx, err := d.db.Begin()
	if err != nil {
		return res, err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s (
			kind TEXT NOT NULL,
			origin_local_id TEXT NOT NULL,
			through_rev TEXT NOT NULL,
			PRIMARY KEY (kind, origin_local_id)
		)`, backupTable)); err != nil {
		return res, fmt.Errorf("clientdb: preparing the undo table: %w", err)
	}

	for _, t := range syncTables {
		where, args := filterFor(t.Table, o)
		// Save the baseline rows this run consumes, scoped exactly like the update
		// so a --project run cannot destroy another project's baseline.
		saved, err := tx.Exec(fmt.Sprintf(`
			INSERT OR IGNORE INTO %s (kind, origin_local_id, through_rev)
			SELECT kind, origin_local_id, through_rev FROM sync_launch_exclusions
			WHERE kind = ? AND origin_local_id IN (
				SELECT CAST(id AS TEXT) FROM %s WHERE origin_device_id IS NULL AND %s
			)`, backupTable, t.Table, where), append([]any{t.Kind}, args...)...)
		if err != nil {
			return res, fmt.Errorf("clientdb: saving the baseline for %s: %w", t.Table, err)
		}
		if n, err := saved.RowsAffected(); err == nil {
			res.Exclusions += int(n)
		}

		if _, err := tx.Exec(fmt.Sprintf(`
			DELETE FROM sync_launch_exclusions
			WHERE kind = ? AND origin_local_id IN (
				SELECT CAST(id AS TEXT) FROM %s WHERE origin_device_id IS NULL AND %s
			)`, t.Table, where), append([]any{t.Kind}, args...)...); err != nil {
			return res, fmt.Errorf("clientdb: clearing the baseline for %s: %w", t.Table, err)
		}

		if _, err := tx.Exec(fmt.Sprintf(
			`UPDATE %s SET synced_at = NULL WHERE origin_device_id IS NULL AND %s`, t.Table, where),
			args...); err != nil {
			return res, fmt.Errorf("clientdb: requeueing %s: %w", t.Table, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return res, fmt.Errorf("clientdb: committing the backfill: %w", err)
	}
	return res, nil
}

// filterFor builds a table's project predicate, so the UPDATE and the DELETE
// can never disagree about scope.
//
// user_prompts has no project column — it reaches one only through
// sdk_sessions — so project filters there have to go through a subquery. This is
// the kind of difference a hand-built fixture would have hidden until the first
// real backfill.
func filterFor(table string, o BackfillOpts) (string, []any) {
	clauses := []string{"1 = 1"}
	var args []any

	projectExpr := "project"
	if table == "user_prompts" {
		projectExpr = "(SELECT s.project FROM sdk_sessions s WHERE s.id = session_db_id)"
	}

	if o.Project != "" {
		clauses = append(clauses, projectExpr+" = ?")
		args = append(args, o.Project)
	}
	for _, ex := range o.ExcludedProjects {
		ex = strings.TrimSpace(ex)
		if ex == "" {
			continue
		}
		// COALESCE so a prompt with no resolvable session is kept rather than
		// silently dropped by a NULL comparison.
		clauses = append(clauses, "COALESCE("+projectExpr+", '') <> ?")
		args = append(args, ex)
	}
	return strings.Join(clauses, " AND "), args
}

// UndoBackfill restores the launch baseline and re-stamps the rows it covers.
func (d *DB) UndoBackfill(now func() time.Time) (int, error) {
	if now == nil {
		now = time.Now
	}
	var exists string
	err := d.db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, backupTable).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, errors.New("clientdb: no backfill to undo")
	}
	if err != nil {
		return 0, err
	}

	tx, err := d.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	restored, err := tx.Exec(fmt.Sprintf(`
		INSERT OR IGNORE INTO sync_launch_exclusions (kind, origin_local_id, through_rev)
		SELECT kind, origin_local_id, through_rev FROM %s`, backupTable))
	if err != nil {
		return 0, fmt.Errorf("clientdb: restoring the baseline: %w", err)
	}

	stamp := now().UnixMilli()
	for _, t := range syncTables {
		if _, err := tx.Exec(fmt.Sprintf(`
			UPDATE %s SET synced_at = ?
			WHERE synced_at IS NULL AND origin_device_id IS NULL
			  AND CAST(id AS TEXT) IN (SELECT origin_local_id FROM %s WHERE kind = ?)`,
			t.Table, backupTable), stamp, t.Kind); err != nil {
			return 0, fmt.Errorf("clientdb: re-stamping %s: %w", t.Table, err)
		}
	}

	if _, err := tx.Exec(fmt.Sprintf(`DROP TABLE %s`, backupTable)); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}

	n, _ := restored.RowsAffected()
	return int(n), nil
}

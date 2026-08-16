// Package clientdb reads and carefully modifies claude-mem's own database.
//
// Everything here touches another application's live data, so the rules are
// strict: take a consistent backup first, do the work in one immediate
// transaction, refuse to run while the worker is writing unless forced, and
// keep enough state to undo it.
package clientdb

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	_ "modernc.org/sqlite"
)

// Tables that replicate, with the kind each maps to.
var syncTables = []struct {
	Table string
	Kind  string
}{
	{"observations", "observation"},
	{"session_summaries", "summary"},
	{"user_prompts", "prompt"},
}

// DB is an open claude-mem database.
type DB struct {
	db   *sql.DB
	path string
	dir  string
}

// Counts is the local push queue.
type Counts struct {
	Observations int
	Summaries    int
	Prompts      int
}

// Total is the number of rows waiting to upload.
func (c Counts) Total() int { return c.Observations + c.Summaries + c.Prompts }

// Open opens claude-mem's database.
func Open(path string) (*DB, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("clientdb: %s not found; is claude-mem installed?", path)
	}
	db, err := sql.Open("sqlite",
		"file:"+path+"?_pragma=busy_timeout(10000)&_txlock=immediate")
	if err != nil {
		return nil, fmt.Errorf("clientdb: opening %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	return &DB{db: db, path: path, dir: filepath.Dir(path)}, nil
}

// Close releases the handle.
func (d *DB) Close() error { return d.db.Close() }

// Pending counts rows the worker has not yet pushed.
//
// This is the client's own queue predicate: locally authored rows that have
// never been stamped.
func (d *DB) Pending() (Counts, error) {
	var c Counts
	targets := []*int{&c.Observations, &c.Summaries, &c.Prompts}
	for i, t := range syncTables {
		q := fmt.Sprintf(
			`SELECT COUNT(*) FROM %s WHERE synced_at IS NULL AND origin_device_id IS NULL`, t.Table)
		if err := d.db.QueryRow(q).Scan(targets[i]); err != nil {
			return Counts{}, fmt.Errorf("clientdb: counting %s: %w", t.Table, err)
		}
	}
	return c, nil
}

// SchemaVersions returns the applied claude-mem migration numbers.
func (d *DB) SchemaVersions() (map[int]bool, error) {
	rows, err := d.db.Query(`SELECT version FROM schema_versions`)
	if err != nil {
		return nil, fmt.Errorf("clientdb: reading schema_versions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := map[int]bool{}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out[v] = true
	}
	return out, rows.Err()
}

// WorkerPID reads claude-mem's pid file.
//
// Despite the name, the file holds JSON — {"pid":123,"port":37700,...} — not a
// bare integer. Parsing it as a number silently reports "no worker", which would
// let a backfill modify the database underneath a running worker.
func WorkerPID(dir string) (int, bool) {
	raw, err := os.ReadFile(filepath.Join(dir, "worker.pid"))
	if err != nil {
		return 0, false
	}

	var doc struct {
		PID int `json:"pid"`
	}
	if err := json.Unmarshal(raw, &doc); err == nil && doc.PID > 0 {
		return doc.PID, true
	}

	// Tolerate a bare integer in case the format changes back.
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return 0, false
	}
	return pid, true
}

// ProcessAlive reports whether a pid is running.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, os.ErrPermission)
}

// WorkerRunning reports whether claude-mem's worker holds this data directory.
func (d *DB) WorkerRunning() bool {
	pid, ok := WorkerPID(d.dir)
	return ok && ProcessAlive(pid)
}

// Dir is the claude-mem data directory.
func (d *DB) Dir() string { return d.dir }

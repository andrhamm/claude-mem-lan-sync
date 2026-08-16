package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/andrhamm/claude-mem-lan-sync/internal/proto"
)

func tempStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hub.db")
	s, err := Open(path, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

func TestOpenCreatesSchema(t *testing.T) {
	s, _ := tempStore(t)

	var version int
	if err := s.w.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion {
		t.Fatalf("user_version = %d, want %d", version, schemaVersion)
	}

	for _, table := range []string{"ops", "meta", "devices"} {
		var name string
		err := s.w.QueryRow(
			`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&name)
		if err != nil {
			t.Errorf("table %s missing: %v", table, err)
		}
	}
}

// Unknown DSN keys are ignored silently by modernc, so a wrong pragma syntax
// would leave a rollback-journal database that looks fine until it deadlocks.
func TestOpenVerifiesWAL(t *testing.T) {
	s, _ := tempStore(t)
	var mode string
	if err := s.w.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode = %q, want wal", mode)
	}
}

func TestReopenIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")

	first, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	userID, epoch := first.UserID(), first.Epoch()
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := Open(path, nil)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = second.Close() }()

	if second.UserID() != userID {
		t.Errorf("user id changed across reopen: %q -> %q", userID, second.UserID())
	}
	// Epoch stability matters: a changed epoch tells every device to replay.
	if second.Epoch() != epoch {
		t.Errorf("epoch changed across a clean reopen: %s -> %s", epoch, second.Epoch())
	}
}

func TestRefusesNewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	s, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.w.Exec(`PRAGMA user_version = 99`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := Open(path, nil); err == nil {
		t.Fatal("opened a database written by a newer build")
	}
}

func TestFilePermissions(t *testing.T) {
	s, path := tempStore(t)

	// -wal and -shm only exist once something has been written.
	if _, err := s.w.Exec(`INSERT INTO devices (user_id, device_id, first_seen, last_seen)
	                       VALUES (?, 'd', 1, 1)`, s.UserID()); err != nil {
		t.Fatal(err)
	}
	if err := s.restrictPermissions(); err != nil {
		t.Fatal(err)
	}

	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		info, err := os.Stat(p)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("%s has mode %o, want 600 — the hub holds every device's memory",
				filepath.Base(p), perm)
		}
	}

	dir, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if perm := dir.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("data directory mode %o is group/world accessible", perm)
	}
}

func TestEpochIsPositiveCanonicalDecimal(t *testing.T) {
	s, _ := tempStore(t)

	// The client validates the epoch with the same rule it uses for sequences,
	// so a UUID or hex value would be rejected outright.
	got, err := proto.ParseDecPositive(s.Epoch().String())
	if err != nil {
		t.Fatalf("epoch %q is not a positive canonical decimal: %v", s.Epoch(), err)
	}
	if got != s.Epoch() {
		t.Fatal("epoch did not round-trip")
	}
}

func TestBumpEpochChangesIt(t *testing.T) {
	s, _ := tempStore(t)
	before := s.Epoch()
	if err := s.BumpEpoch("test"); err != nil {
		t.Fatal(err)
	}
	if s.Epoch() == before {
		t.Fatal("BumpEpoch did not change the epoch")
	}

	var stored string
	if err := s.w.QueryRow(`SELECT epoch FROM meta WHERE user_id = ?`, s.UserID()).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != s.Epoch().String() {
		t.Fatalf("stored epoch %q does not match in-memory %q", stored, s.Epoch())
	}
}

// A database restored from an older backup leaves clients holding a cursor
// above head_seq: they poll forever, get empty pages, and never see an error.
// Rotating the epoch is the protocol's only way to say "replay from zero".
func TestHeadDriftRotatesEpoch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	s, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	userID := s.UserID()
	epochBefore := s.Epoch()

	// Simulate a restore: the counter claims more ops than the log holds.
	if _, err := s.w.Exec(`UPDATE meta SET head_seq = 42 WHERE user_id = ?`, userID); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path, nil)
	if err != nil {
		t.Fatalf("reopen after drift: %v", err)
	}
	defer func() { _ = reopened.Close() }()

	if reopened.Epoch() == epochBefore {
		t.Fatal("head_seq drift did not rotate the epoch — devices would poll an empty page forever")
	}

	var head int64
	if err := reopened.w.QueryRow(`SELECT head_seq FROM meta WHERE user_id = ?`, userID).Scan(&head); err != nil {
		t.Fatal(err)
	}
	if head != 0 {
		t.Fatalf("head_seq = %d, want it repaired to the log's actual maximum (0)", head)
	}
}

func TestSetTxHookIsAvailable(t *testing.T) {
	s, _ := tempStore(t)
	called := false
	s.SetTxHook(func(*sql.Tx) error {
		called = true
		return nil
	})
	s.mu.Lock()
	hook := s.txHook
	s.mu.Unlock()
	if hook == nil {
		t.Fatal("hook not installed")
	}
	if err := hook(nil); err != nil || !called {
		t.Fatal("hook not invocable")
	}
}

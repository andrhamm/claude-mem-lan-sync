// Package store holds the append-only operation log.
//
// The log's one hard guarantee is that sequence numbers are gapless and start at
// 1. claude-mem applies pulled pages with requireContiguous anchored to its
// stored cursor: a single missing sequence makes it throw, roll the page back,
// leave the cursor unmoved, and retry that page forever. Everything here —
// immediate transactions, a single writer, allocation from a counter row rather
// than AUTOINCREMENT — exists to protect that property.
package store

import (
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/andrhamm/claude-mem-lan-sync/internal/proto"
	_ "modernc.org/sqlite"
)

// Store is the hub's database.
type Store struct {
	w    *sql.DB // single connection, immediate transactions
	r    *sql.DB // concurrent readers
	path string
	log  *slog.Logger

	mu sync.Mutex // serialises writes

	userID string
	epoch  proto.Dec

	// clock is overridable so tests get deterministic server_ts values.
	clock func() time.Time

	// txHook runs inside a write transaction so tests can inject a rollback and
	// prove that a failed push consumes no sequence number.
	txHook func(*sql.Tx) error
}

// Open prepares the database at path, creating it if needed.
func Open(path string, log *slog.Logger) (*Store, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("store: creating data directory: %w", err)
		}
		// MkdirAll leaves an existing directory's mode alone, and a fresh one is
		// still subject to umask. The hub's directory holds every paired device's
		// memory, so tighten it explicitly rather than trusting the environment.
		if err := os.Chmod(dir, 0o700); err != nil {
			log.Warn("could not restrict data directory permissions; it may be readable by other local users",
				"dir", dir, "error", err)
		}
	}

	w, err := sql.Open("sqlite", dsn(path, "immediate"))
	if err != nil {
		return nil, fmt.Errorf("store: opening write handle: %w", err)
	}
	// One connection: every write is serialised, and the sequencer never races
	// itself for the head counter.
	w.SetMaxOpenConns(1)

	if err := verifyWAL(w); err != nil {
		_ = w.Close()
		return nil, err
	}
	if err := migrate(w); err != nil {
		_ = w.Close()
		return nil, err
	}

	r, err := sql.Open("sqlite", dsn(path, "deferred"))
	if err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("store: opening read handle: %w", err)
	}

	s := &Store{w: w, r: r, path: path, log: log}

	if err := s.loadOrCreateMeta(); err != nil {
		_ = s.Close()
		return nil, err
	}
	if err := s.restrictPermissions(); err != nil {
		_ = s.Close()
		return nil, err
	}
	if err := s.verifyHeadConsistency(); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

// dsn builds a modernc.org/sqlite connection string.
//
// modernc's pragma syntax differs from mattn's, and unknown DSN keys are
// silently ignored — a copy-pasted mattn DSN yields a rollback-journal database
// that looks healthy until it deadlocks. verifyWAL exists because of that.
func dsn(path, txlock string) string {
	return "file:" + path +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_txlock=" + txlock
}

func verifyWAL(db *sql.DB) error {
	var mode string
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		return fmt.Errorf("store: reading journal_mode: %w", err)
	}
	if mode != "wal" {
		return fmt.Errorf("store: journal_mode is %q, want wal — the DSN pragmas did not apply", mode)
	}
	return nil
}

// restrictPermissions tightens the database files.
//
// SQLite creates them with 0666 & ~umask, which is 0664 on a default Ubuntu
// setup. This hub's file is every paired device's memory in one place, so it
// must not be world-readable. The -wal and -shm files only exist after a write,
// so missing ones are not an error.
func (s *Store) restrictPermissions() error {
	for _, p := range []string{s.path, s.path + "-wal", s.path + "-shm"} {
		if err := os.Chmod(p, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("store: tightening permissions on %s: %w", filepath.Base(p), err)
		}
	}
	return nil
}

func (s *Store) loadOrCreateMeta() error {
	row := s.w.QueryRow(`SELECT user_id, epoch, head_seq FROM meta LIMIT 1`)

	var userID, epoch string
	var head int64
	switch err := row.Scan(&userID, &epoch, &head); {
	case err == nil:
		e, perr := proto.ParseDecPositive(epoch)
		if perr != nil {
			return fmt.Errorf("store: stored epoch %q is not a positive decimal: %w", epoch, perr)
		}
		s.userID, s.epoch = userID, e
		return nil

	case errors.Is(err, sql.ErrNoRows):
		userID, err := randomHubID()
		if err != nil {
			return err
		}
		epoch, err := randomEpoch()
		if err != nil {
			return err
		}
		if _, err := s.w.Exec(
			`INSERT INTO meta (user_id, epoch, head_seq) VALUES (?, ?, 0)`,
			userID, epoch.String()); err != nil {
			return fmt.Errorf("store: creating meta row: %w", err)
		}
		s.userID, s.epoch = userID, epoch
		s.log.Info("initialised hub", "user_id", userID, "epoch", epoch.String())
		return nil

	default:
		return fmt.Errorf("store: reading meta: %w", err)
	}
}

// verifyHeadConsistency detects a sequence space that moved backwards.
//
// Restoring the database from an older backup leaves clients holding a cursor
// above head_seq. They would poll forever, receive empty pages, and never
// report an error — the failure is completely silent. A changed epoch is the
// protocol's way of saying "your cursor is meaningless, replay from zero", so
// any drift between the counter and the log rotates it.
func (s *Store) verifyHeadConsistency() error {
	var head int64
	if err := s.w.QueryRow(`SELECT head_seq FROM meta WHERE user_id = ?`, s.userID).Scan(&head); err != nil {
		return fmt.Errorf("store: reading head_seq: %w", err)
	}

	var maxSeq sql.NullInt64
	if err := s.w.QueryRow(`SELECT MAX(seq) FROM ops WHERE user_id = ?`, s.userID).Scan(&maxSeq); err != nil {
		return fmt.Errorf("store: reading max sequence: %w", err)
	}

	var actual int64
	if maxSeq.Valid {
		actual = maxSeq.Int64
	}
	if actual == head {
		return nil
	}

	s.log.Warn("operation log and head counter disagree — rotating the epoch so devices replay",
		"head_seq", head, "max_seq", actual)

	if _, err := s.w.Exec(`UPDATE meta SET head_seq = ? WHERE user_id = ?`, actual, s.userID); err != nil {
		return fmt.Errorf("store: repairing head_seq: %w", err)
	}
	return s.BumpEpoch("head_seq drift")
}

// BumpEpoch rotates the epoch, telling every device its cursor is void and a
// full replay is required. Dedupe absorbs the resulting duplicate pushes.
//
// Rotating the pre-shared key must NOT call this: the two are independent, and
// conflating them would force a needless full replay across every device.
func (s *Store) BumpEpoch(reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	epoch, err := randomEpoch()
	if err != nil {
		return err
	}
	if _, err := s.w.Exec(`UPDATE meta SET epoch = ? WHERE user_id = ?`, epoch.String(), s.userID); err != nil {
		return fmt.Errorf("store: rotating epoch: %w", err)
	}
	s.epoch = epoch
	s.log.Warn("epoch rotated; connected devices will replay from zero", "reason", reason, "epoch", epoch.String())
	return nil
}

// UserID is this hub's identity, and the only value accepted in X-User-Id.
func (s *Store) UserID() string { return s.userID }

// Epoch is the current epoch.
func (s *Store) Epoch() proto.Dec {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.epoch
}

// SetTxHook installs a hook that runs inside each write transaction. Tests use
// it to force a rollback; production never sets one.
func (s *Store) SetTxHook(f func(*sql.Tx) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.txHook = f
}

// Close releases both handles and truncates the write-ahead log.
func (s *Store) Close() error {
	var errs []error
	if s.w != nil {
		if _, err := s.w.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
			errs = append(errs, err)
		}
		if err := s.w.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if s.r != nil {
		if err := s.r.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func randomHubID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("store: generating hub id: %w", err)
	}
	return fmt.Sprintf("%x", b), nil
}

// randomEpoch returns a positive 63-bit decimal.
//
// The client validates the epoch with the same canonical-decimal rule it uses
// for sequences, so a UUID or hex string would be rejected outright.
func randomEpoch() (proto.Dec, error) {
	max := new(big.Int).Lsh(big.NewInt(1), 63)
	n, err := rand.Int(rand.Reader, max)
	if err != nil {
		return 0, fmt.Errorf("store: generating epoch: %w", err)
	}
	v := n.Uint64()
	if v == 0 {
		v = 1 // must be positive
	}
	return proto.Dec(v), nil
}

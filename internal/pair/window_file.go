package pair

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// WindowFile is the on-disk pairing window.
const WindowFile = "pairing.json"

// FileWindow shares a pairing window between the `pair` command and the running
// `serve` process, which are separate processes.
//
// Only a hash of the code is stored, so the file is not a second copy of the
// secret, and the attempt counter is persisted so a restart cannot be used to
// reset a brute-force budget.
type FileWindow struct {
	Dir string
	Now func() time.Time
}

type windowState struct {
	CodeHash  string `json:"code_hash"`
	ExpiresAt int64  `json:"expires_at_ms"`
	Attempts  int    `json:"attempts"`
}

func (f FileWindow) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

func (f FileWindow) path() string { return filepath.Join(f.Dir, WindowFile) }

func hashCode(code string) string {
	sum := sha256.Sum256([]byte("cmemlan-pairing-v1:" + normalise(code)))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// Open starts a window and returns the code to show the user.
func (f FileWindow) Open(ttl time.Duration) (code string, expires time.Time, err error) {
	if ttl <= 0 {
		ttl = DefaultWindow
	}
	code, err = newCode()
	if err != nil {
		return "", time.Time{}, err
	}
	expires = f.now().Add(ttl)

	state := windowState{CodeHash: hashCode(code), ExpiresAt: expires.UnixMilli()}
	b, err := json.Marshal(state)
	if err != nil {
		return "", time.Time{}, err
	}
	if err := os.MkdirAll(f.Dir, 0o700); err != nil {
		return "", time.Time{}, err
	}

	unlock, err := f.lock()
	if err != nil {
		return "", time.Time{}, err
	}
	defer unlock()

	if err := f.writeAtomic(b); err != nil {
		return "", time.Time{}, fmt.Errorf("pair: writing the pairing window: %w", err)
	}
	return code, expires, nil
}

// Close removes any open window.
func (f FileWindow) Close() error {
	err := os.Remove(f.path())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Redeem exchanges a code for the pre-shared key, consuming the window.
//
// The whole read-modify-write runs under an exclusive lock. Without one,
// concurrent requests each read the same attempt count and each write back
// count+1, so a documented budget of five guesses becomes however many a caller
// can issue in parallel — and the hub serves 64 requests at once.
func (f FileWindow) Redeem(code string) (string, error) {
	unlock, err := f.lock()
	if err != nil {
		return "", err
	}
	defer unlock()

	b, err := os.ReadFile(f.path())
	if errors.Is(err, os.ErrNotExist) {
		return "", ErrNoWindow
	}
	if err != nil {
		return "", fmt.Errorf("pair: reading the pairing window: %w", err)
	}

	var state windowState
	if err := json.Unmarshal(b, &state); err != nil {
		// An unreadable window is not grounds for deleting it: a torn read would
		// otherwise let any caller destroy a legitimate pairing window.
		return "", ErrNoWindow
	}

	if f.now().UnixMilli() > state.ExpiresAt {
		_ = f.Close()
		return "", ErrNoWindow
	}
	if state.Attempts >= MaxAttempts {
		_ = f.Close()
		return "", ErrTooManyAttempts
	}

	if subtle.ConstantTimeCompare([]byte(hashCode(code)), []byte(state.CodeHash)) != 1 {
		state.Attempts++
		// Persist the attempt before returning, so restarting the hub cannot be
		// used to reset the budget. Written via a temp file and renamed, so a
		// concurrent reader never sees a truncated window.
		if updated, mErr := json.Marshal(state); mErr == nil {
			_ = f.writeAtomic(updated)
		}
		if state.Attempts >= MaxAttempts {
			_ = f.Close()
			return "", ErrTooManyAttempts
		}
		return "", ErrBadCode
	}

	keys, err := LoadOrCreate(f.Dir)
	if err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return keys.PSK, nil
}

// lock takes an exclusive lock for the pairing window.
//
// A lock file rather than flock keeps this working the same way across
// platforms, which matters because the hub and the `pair` command are separate
// processes. A lock older than the window itself is treated as abandoned.
func (f FileWindow) lock() (func(), error) {
	path := f.path() + ".lock"
	deadline := f.now().Add(5 * time.Second)

	for {
		fh, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_ = fh.Close()
			return func() { _ = os.Remove(path) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("pair: locking the pairing window: %w", err)
		}

		// Clear a lock left behind by a process that died mid-redemption.
		if info, statErr := os.Stat(path); statErr == nil {
			if f.now().Sub(info.ModTime()) > time.Minute {
				_ = os.Remove(path)
				continue
			}
		}
		if f.now().After(deadline) {
			return nil, errors.New("pair: timed out waiting for the pairing window lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// writeAtomic replaces the window file without ever truncating it in place.
func (f FileWindow) writeAtomic(b []byte) error {
	tmp, err := os.CreateTemp(f.Dir, ".pairing-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()

	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, f.path())
}

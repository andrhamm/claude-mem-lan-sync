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
	if err := os.WriteFile(f.path(), b, 0o600); err != nil {
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
func (f FileWindow) Redeem(code string) (string, error) {
	b, err := os.ReadFile(f.path())
	if errors.Is(err, os.ErrNotExist) {
		return "", ErrNoWindow
	}
	if err != nil {
		return "", fmt.Errorf("pair: reading the pairing window: %w", err)
	}

	var state windowState
	if err := json.Unmarshal(b, &state); err != nil {
		_ = f.Close()
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
		// used to reset the budget.
		if updated, mErr := json.Marshal(state); mErr == nil {
			_ = os.WriteFile(f.path(), updated, 0o600)
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

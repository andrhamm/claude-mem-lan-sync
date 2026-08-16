package pair

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFileWindowRoundTrip(t *testing.T) {
	dir := t.TempDir()
	keys, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	w := FileWindow{Dir: dir}

	code, expires, err := w.Open(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if expires.Before(time.Now()) {
		t.Fatal("window already expired")
	}

	psk, err := w.Redeem(code)
	if err != nil {
		t.Fatal(err)
	}
	if psk != keys.PSK {
		t.Fatal("redeemed the wrong key")
	}

	// Single use.
	if _, err := w.Redeem(code); !errors.Is(err, ErrNoWindow) {
		t.Fatalf("second redemption returned %v", err)
	}
}

// The file must not be a second copy of the code.
func TestFileWindowStoresOnlyAHash(t *testing.T) {
	dir := t.TempDir()
	w := FileWindow{Dir: dir}
	code, _, err := w.Open(time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, WindowFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), normalise(code)) {
		t.Fatalf("pairing file contains the code itself: %s", raw)
	}

	var state windowState
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	if state.CodeHash == "" {
		t.Fatal("no code hash stored")
	}

	info, err := os.Stat(filepath.Join(dir, WindowFile))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("pairing file mode %o, want 600", perm)
	}
}

func TestFileWindowExpires(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1000, 0)
	w := FileWindow{Dir: dir, Now: func() time.Time { return now }}

	code, _, err := w.Open(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)

	if _, err := w.Redeem(code); !errors.Is(err, ErrNoWindow) {
		t.Fatalf("expired window returned %v", err)
	}
}

// Restarting the hub must not reset the brute-force budget, so attempts are
// persisted rather than kept in memory.
func TestFileWindowAttemptsSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadOrCreate(dir); err != nil {
		t.Fatal(err)
	}

	w := FileWindow{Dir: dir}
	code, _, err := w.Open(time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < MaxAttempts-1; i++ {
		// A fresh value each time, as a separate process would see it.
		fresh := FileWindow{Dir: dir}
		if _, err := fresh.Redeem("000-000-000"); !errors.Is(err, ErrBadCode) {
			t.Fatalf("attempt %d returned %v", i, err)
		}
	}

	fresh := FileWindow{Dir: dir}
	if _, err := fresh.Redeem("000-000-000"); !errors.Is(err, ErrTooManyAttempts) {
		t.Fatalf("final attempt returned %v, want the window destroyed", err)
	}
	if _, err := fresh.Redeem(code); err == nil {
		t.Fatal("the correct code still worked after the window was destroyed")
	}
}

func TestFileWindowNoWindowOpen(t *testing.T) {
	w := FileWindow{Dir: t.TempDir()}
	if _, err := w.Redeem("123-456-789"); !errors.Is(err, ErrNoWindow) {
		t.Fatalf("Redeem with no window returned %v", err)
	}
}

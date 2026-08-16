package pair

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadOrCreateGeneratesAndPersists(t *testing.T) {
	dir := t.TempDir()

	first, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.PSK) < 40 { // 32 raw bytes base64url-encoded
		t.Fatalf("key looks too short: %d chars", len(first.PSK))
	}

	second, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if second.PSK != first.PSK {
		t.Fatal("key changed across reloads; every device would be locked out")
	}

	info, err := os.Stat(filepath.Join(dir, KeyFile))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("key file mode %o, want 600", perm)
	}
}

func TestLoadOrCreateRepairsPermissions(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadOrCreate(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, KeyFile)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadOrCreate(dir); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("mode %o after reload, want it repaired to 600", perm)
	}
}

func TestVerifyIsExact(t *testing.T) {
	k := &Keys{PSK: "correct-horse-battery-staple"}
	if !k.Verify("correct-horse-battery-staple") {
		t.Error("Verify rejected the correct key")
	}
	for _, wrong := range []string{"", "correct-horse-battery-stapl", "Correct-Horse-Battery-Staple", "x"} {
		if k.Verify(wrong) {
			t.Errorf("Verify accepted %q", wrong)
		}
	}
}

// The fingerprint is what a user compares between two screens, so different
// keys must never look alike, and it must not be the key itself.
func TestFingerprintDistinguishesKeys(t *testing.T) {
	a := (&Keys{PSK: "key-one"}).Fingerprint()
	b := (&Keys{PSK: "key-two"}).Fingerprint()

	if a == b {
		t.Fatal("different keys produced the same fingerprint")
	}
	if a == "key-one" || len(a) > 20 {
		t.Fatalf("fingerprint %q should be short and derived, not the key", a)
	}
	if (&Keys{PSK: "key-one"}).Fingerprint() != a {
		t.Fatal("fingerprint is not stable")
	}
}

// Rotation is a credential change, not a replication event: callers must not
// bump the epoch, which would force every device to replay the whole log.
func TestRotateReplacesKey(t *testing.T) {
	dir := t.TempDir()
	before, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}

	after, err := Rotate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if after.PSK == before.PSK {
		t.Fatal("Rotate did not change the key")
	}
	if after.Fingerprint() == before.Fingerprint() {
		t.Fatal("fingerprint did not change with the key")
	}

	reloaded, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.PSK != after.PSK {
		t.Fatal("rotated key did not persist")
	}
}

func TestWindowRedeemsOnce(t *testing.T) {
	w, err := NewWindow("the-key", time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}

	got, err := w.Redeem(w.Code())
	if err != nil {
		t.Fatal(err)
	}
	if got != "the-key" {
		t.Fatalf("redeemed %q", got)
	}
	if _, err := w.Redeem(w.Code()); !errors.Is(err, ErrNoWindow) {
		t.Fatalf("second redemption returned %v, want ErrNoWindow", err)
	}
}

func TestWindowExpires(t *testing.T) {
	now := time.Unix(1000, 0)
	clock := func() time.Time { return now }

	w, err := NewWindow("the-key", time.Minute, clock)
	if err != nil {
		t.Fatal(err)
	}

	now = now.Add(2 * time.Minute)
	if _, err := w.Redeem(w.Code()); !errors.Is(err, ErrNoWindow) {
		t.Fatalf("expired window returned %v", err)
	}
}

// A short code is only safe with a hard cap: a LAN attacker can try thousands
// per second, so failures must destroy the window rather than merely slow it.
func TestWindowDiesAfterFailedAttempts(t *testing.T) {
	w, err := NewWindow("the-key", time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	code := w.Code()

	for i := 0; i < MaxAttempts-1; i++ {
		if _, err := w.Redeem("000-000-000"); !errors.Is(err, ErrBadCode) {
			t.Fatalf("attempt %d returned %v, want ErrBadCode", i, err)
		}
	}
	if _, err := w.Redeem("000-000-000"); !errors.Is(err, ErrTooManyAttempts) {
		t.Fatalf("final attempt returned %v, want ErrTooManyAttempts", err)
	}

	// Even the correct code is now useless.
	if _, err := w.Redeem(code); err == nil {
		t.Fatal("the correct code still worked after the window was destroyed")
	}
}

func TestCodeFormatAndSeparatorTolerance(t *testing.T) {
	w, err := NewWindow("the-key", time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	code := w.Code()

	if len(code) != 11 || code[3] != '-' || code[7] != '-' {
		t.Fatalf("code %q is not in the ddd-ddd-ddd form", code)
	}
	// A user retyping without separators must still succeed.
	if _, err := w.Redeem(normalise(code)); err != nil {
		t.Fatalf("redeeming without separators failed: %v", err)
	}
}

func TestCodesAreNotPredictable(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		w, err := NewWindow("k", time.Minute, nil)
		if err != nil {
			t.Fatal(err)
		}
		if seen[w.Code()] {
			t.Fatalf("duplicate code %q in 50 windows", w.Code())
		}
		seen[w.Code()] = true
	}
}

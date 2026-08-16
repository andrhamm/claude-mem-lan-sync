package pair

import (
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"math/big"
	"sync"
	"time"
)

// Pairing window defaults.
const (
	DefaultWindow = 5 * time.Minute
	MaxAttempts   = 5
	codeDigits    = 9 // three groups of three
)

var (
	// ErrNoWindow means no pairing window is open.
	ErrNoWindow = errors.New("pair: no pairing window is open")
	// ErrBadCode means the code did not match.
	ErrBadCode = errors.New("pair: incorrect pairing code")
	// ErrTooManyAttempts means the window was destroyed by failed guesses.
	ErrTooManyAttempts = errors.New("pair: too many incorrect attempts; the window was closed")
)

// Window is a time-boxed opportunity to exchange a code for the key.
//
// A short code is only safe with hard limits: a LAN attacker can attempt
// thousands of guesses per second, so five failures destroy the window outright
// rather than merely rate-limiting it. Codes are single-use and expire.
type Window struct {
	mu       sync.Mutex
	code     string
	psk      string
	expires  time.Time
	attempts int
	redeemed bool
	now      func() time.Time
}

// NewWindow opens a pairing window for the given key.
func NewWindow(psk string, ttl time.Duration, now func() time.Time) (*Window, error) {
	if now == nil {
		now = time.Now
	}
	if ttl <= 0 {
		ttl = DefaultWindow
	}
	code, err := newCode()
	if err != nil {
		return nil, err
	}
	return &Window{
		code:    code,
		psk:     psk,
		expires: now().Add(ttl),
		now:     now,
	}, nil
}

// Code is the value the user types on the joining device.
func (w *Window) Code() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.code
}

// Expires reports when the window closes.
func (w *Window) Expires() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.expires
}

// Redeem exchanges a code for the key.
func (w *Window) Redeem(code string) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.redeemed || w.code == "" {
		return "", ErrNoWindow
	}
	if w.now().After(w.expires) {
		w.code = ""
		return "", ErrNoWindow
	}
	if w.attempts >= MaxAttempts {
		w.code = ""
		return "", ErrTooManyAttempts
	}

	// Constant time: the code is a secret for the length of the window.
	if subtle.ConstantTimeCompare([]byte(normalise(code)), []byte(normalise(w.code))) != 1 {
		w.attempts++
		if w.attempts >= MaxAttempts {
			w.code = ""
			return "", ErrTooManyAttempts
		}
		return "", ErrBadCode
	}

	w.redeemed = true
	return w.psk, nil
}

// Redeemed reports whether the window has been used.
func (w *Window) Redeemed() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.redeemed
}

func normalise(code string) string {
	out := make([]byte, 0, codeDigits)
	for i := 0; i < len(code); i++ {
		if code[i] >= '0' && code[i] <= '9' {
			out = append(out, code[i])
		}
	}
	return string(out)
}

// newCode returns a nine-digit code formatted in three groups.
func newCode() (string, error) {
	const numerals = "0123456789"
	digits := make([]byte, codeDigits)
	for i := range digits {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(numerals))))
		if err != nil {
			return "", err
		}
		digits[i] = numerals[n.Int64()]
	}
	return string(digits[0:3]) + "-" + string(digits[3:6]) + "-" + string(digits[6:9]), nil
}

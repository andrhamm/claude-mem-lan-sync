// Package pair owns the hub's pre-shared key and the pairing handshake.
//
// The key authenticates a device to the hub. It does not authenticate the hub
// to a device — nothing in the protocol does — so a rogue mDNS advertiser could
// impersonate a hub and relay to the real one. The fingerprint printed by `pair`
// and confirmed by `connect` is the only defence against that, which is why it
// is not optional in the connect flow.
package pair

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// KeyFile is the filename inside the data directory.
const KeyFile = "psk"

// Keys holds the hub's pre-shared key.
type Keys struct {
	PSK string
}

// LoadOrCreate reads the key, generating one on first run.
func LoadOrCreate(dir string) (*Keys, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("pair: creating data directory: %w", err)
	}
	path := filepath.Join(dir, KeyFile)

	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		psk := strings.TrimSpace(string(b))
		if psk == "" {
			return nil, errors.New("pair: stored key is empty")
		}
		// Repair permissions in case the file was created by an older version or
		// copied in by hand.
		if err := os.Chmod(path, 0o600); err != nil {
			return nil, fmt.Errorf("pair: securing key file: %w", err)
		}
		return &Keys{PSK: psk}, nil

	case errors.Is(err, os.ErrNotExist):
		psk, err := newSecret(32)
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, []byte(psk+"\n"), 0o600); err != nil {
			return nil, fmt.Errorf("pair: writing key file: %w", err)
		}
		return &Keys{PSK: psk}, nil

	default:
		return nil, fmt.Errorf("pair: reading key file: %w", err)
	}
}

// Rotate replaces the key.
//
// Rotation deliberately does not touch the epoch: they are independent, and
// bumping the epoch would force every device to replay the entire log for what
// is only a credential change.
func Rotate(dir string) (*Keys, error) {
	psk, err := newSecret(32)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, KeyFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(psk+"\n"), 0o600); err != nil {
		return nil, fmt.Errorf("pair: writing new key: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return nil, fmt.Errorf("pair: replacing key file: %w", err)
	}
	return &Keys{PSK: psk}, nil
}

// Fingerprint is a short, non-secret identifier for the key.
//
// The user compares it between the hub's screen and the joining device. It is
// derived through SHA-256 and truncated, so it identifies the key without
// revealing material that would help recover it.
func (k *Keys) Fingerprint() string {
	sum := sha256.Sum256([]byte("cmemlan-fingerprint-v1:" + k.PSK))
	enc := base64.RawURLEncoding.EncodeToString(sum[:])
	return fmt.Sprintf("%s-%s-%s", enc[0:4], enc[4:8], enc[8:12])
}

// Verify compares a presented key in constant time.
func (k *Keys) Verify(token string) bool {
	return subtle.ConstantTimeCompare([]byte(token), []byte(k.PSK)) == 1
}

func newSecret(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("pair: generating random material: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

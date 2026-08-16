package proto

import (
	"crypto/sha256"
	"encoding/base64"
)

// DigestLen is the length of a base64url SHA-256 digest with no padding.
const DigestLen = 43

// NullPayloadDigest is the digest of the canonical JSON literal `null`.
// Mutation ops carry it as payload_sha256; having the constant here lets tests
// assert our digest function agrees with the client's without a live client.
const NullPayloadDigest = "dCNOmK_nSY-12vHzasLXiswzlGT5UHA7jAGYkvmCuQs"

// Digest returns the base64url, unpadded SHA-256 of b.
//
// The client uses base64url (`-` and `_`), not standard base64, and not hex.
// Emitting the wrong alphabet makes every ack tuple miss and wedges the push
// pipeline, so the encoding is pinned here and asserted in tests.
func Digest(b []byte) string {
	sum := sha256.Sum256(b)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// ValidDigest reports whether s is shaped like a base64url SHA-256 digest.
func ValidDigest(s string) bool {
	if len(s) != DigestLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z',
			c >= 'a' && c <= 'z',
			c >= '0' && c <= '9',
			c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

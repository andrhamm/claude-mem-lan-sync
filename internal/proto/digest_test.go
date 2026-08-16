package proto

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
)

func TestDigestIsBase64URLUnpadded(t *testing.T) {
	got := Digest([]byte("hello"))
	if len(got) != DigestLen {
		t.Fatalf("Digest length = %d, want %d", len(got), DigestLen)
	}
	if strings.ContainsAny(got, "+/=") {
		t.Fatalf("Digest %q contains standard-base64 or padding characters", got)
	}

	sum := sha256.Sum256([]byte("hello"))
	if want := base64.RawURLEncoding.EncodeToString(sum[:]); got != want {
		t.Fatalf("Digest = %q, want %q", got, want)
	}
}

// The client computes payload_sha256 over the canonical literal `null` for
// mutation ops. Matching this constant is evidence our digest agrees with theirs.
func TestNullPayloadDigestMatchesClient(t *testing.T) {
	if got := Digest([]byte("null")); got != NullPayloadDigest {
		t.Fatalf("Digest(\"null\") = %q, want the client's constant %q", got, NullPayloadDigest)
	}
}

func TestDigestUsesURLAlphabet(t *testing.T) {
	// Find an input whose digest exercises - or _ so the alphabet is really pinned.
	var sawURLChar bool
	for i := 0; i < 200 && !sawURLChar; i++ {
		d := Digest([]byte{byte(i)})
		sawURLChar = strings.ContainsAny(d, "-_")
	}
	if !sawURLChar {
		t.Fatal("no digest in 200 samples used the url-safe alphabet")
	}
}

func TestValidDigest(t *testing.T) {
	valid := Digest([]byte("x"))
	if !ValidDigest(valid) {
		t.Fatalf("ValidDigest(%q) = false", valid)
	}

	sum := sha256.Sum256([]byte("x"))
	for _, bad := range []string{
		"",
		hex.EncodeToString(sum[:]), // hex, the classic mistake
		base64.StdEncoding.EncodeToString(sum[:]),         // standard alphabet + padding
		base64.RawStdEncoding.EncodeToString(sum[:]) + "", // may contain + or /
		valid[:DigestLen-1],                               // too short
		valid + "a",                                       // too long
		strings.Repeat("!", DigestLen),                    // wrong alphabet
	} {
		if bad == valid {
			continue
		}
		if ValidDigest(bad) && strings.ContainsAny(bad, "+/=!") {
			t.Errorf("ValidDigest(%q) accepted a wrong-alphabet digest", bad)
		}
		if len(bad) != DigestLen && ValidDigest(bad) {
			t.Errorf("ValidDigest(%q) accepted a wrong-length digest", bad)
		}
	}
}

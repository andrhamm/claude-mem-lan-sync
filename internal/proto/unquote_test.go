package proto

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestUnquoteBasicEscapes(t *testing.T) {
	for _, tc := range []struct {
		literal string
		want    string
	}{
		{`""`, ""},
		{`"plain"`, "plain"},
		{`"quote:\""`, `quote:"`},
		{`"backslash:\\"`, `backslash:\`},
		{`"solidus:\/"`, "solidus:/"},
		{`"controls:\b\f\n\r\t"`, "controls:\b\f\n\r\t"},
		{`"unicode:é"`, "unicode:é"},
		{`"snowman:☃"`, "snowman:☃"},
		{`"raw snowman:☃"`, "raw snowman:☃"},
		{`"pair:😀"`, "pair:😀"},
		{`"low first is invalid:\udc00"`, "low first is invalid:�"},
	} {
		got, err := UnquoteJSONString([]byte(tc.literal))
		if err != nil {
			t.Errorf("UnquoteJSONString(%s): %v", tc.literal, err)
			continue
		}
		if string(got) != tc.want {
			t.Errorf("UnquoteJSONString(%s) = %q, want %q", tc.literal, got, tc.want)
		}
	}
}

// Node replaces an unpaired surrogate with U+FFFD when encoding to UTF-8, and
// the client hashes those replaced bytes. Verified against node:
//
//	Buffer.from("a\ud800b", "utf8") -> 61 ef bf bd 62
//
// Rejecting instead of substituting would 400 a body the client considers valid,
// and a 400 blocks that device's outbox permanently.
func TestUnquoteLoneSurrogateMatchesNode(t *testing.T) {
	got, err := UnquoteJSONString([]byte(`"a\ud800b"`))
	if err != nil {
		t.Fatalf("lone surrogate rejected: %v", err)
	}
	want := []byte{0x61, 0xef, 0xbf, 0xbd, 0x62}
	if !bytes.Equal(got, want) {
		t.Fatalf("bytes = % x, want % x", got, want)
	}
	if Digest(got) != "BQh4Ezku_Bb-j_RIkgxjKOU6-GXfOUGUNmWdn_2pD3s" {
		t.Fatalf("digest = %q, want the value node produced for the same string", Digest(got))
	}
}

func TestUnquoteAgreesWithEncodingJSON(t *testing.T) {
	// For inputs without lone surrogates, our unquoter must agree with the
	// standard library exactly.
	inputs := []string{
		"", "hello", "tab\there", "nested \"quotes\"", "emoji 😀 and ☃",
		"</script>", "a\\b", "line\nbreak", "null-ish  escaped",
	}
	for _, s := range inputs {
		lit, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		got, err := UnquoteJSONString(lit)
		if err != nil {
			t.Fatalf("UnquoteJSONString(%s): %v", lit, err)
		}
		if string(got) != s {
			t.Errorf("round trip of %q produced %q", s, got)
		}
	}
}

func TestUnquoteRejectsMalformed(t *testing.T) {
	for _, bad := range []string{
		``,          // empty
		`"`,         // one quote
		`unquoted`,  // no quotes
		`"trailing`, // unterminated
		`"bad escape \x"`,
		`"truncated escape \u12"`,
		`"raw control ` + "\x01" + `"`,
		`"unescaped " quote"`,
	} {
		if _, err := UnquoteJSONString([]byte(bad)); err == nil {
			t.Errorf("UnquoteJSONString(%q) accepted malformed input", bad)
		}
	}
}

func TestUnquoteReplacesInvalidUTF8Bytes(t *testing.T) {
	// A stray continuation byte inside the literal.
	lit := []byte{'"', 'a', 0xff, 'b', '"'}
	got, err := UnquoteJSONString(lit)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []byte{0x61, 0xef, 0xbf, 0xbd, 0x62}
	if !bytes.Equal(got, want) {
		t.Fatalf("bytes = % x, want % x", got, want)
	}
}

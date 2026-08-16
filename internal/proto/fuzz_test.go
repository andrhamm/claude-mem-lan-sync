package proto

import (
	"bytes"
	"encoding/json"
	"testing"
)

// The property that guards the byte-verbatim invariant: anything ParseOp
// accepts must emit back exactly the literal it was given.
func FuzzOpRoundTrip(f *testing.F) {
	f.Add(validContentBody())
	f.Add(buildBody("mutation", "mutation:0b7f3e2a-1c4d-4a6b-8f2e-9d0c1a2b3c4d", "2", "d", "", "null"))
	f.Add(buildBody("prompt", "prompt:x", "9", "d", "3", `{"text":"< & > \" ☃ 😀"}`))
	f.Add(`{"body_schema_version":1}`)
	f.Add(``)

	f.Fuzz(func(t *testing.T, body string) {
		raw, err := json.Marshal(map[string]string{
			"body":             body,
			"operation_sha256": Digest([]byte(body)),
		})
		if err != nil {
			t.Skip()
		}

		op, err := ParseOp(raw)
		if err != nil {
			return // rejection is a valid outcome
		}

		var buf bytes.Buffer
		if err := EmitChangeOp(&buf, 1, 1, op); err != nil {
			t.Fatalf("emit failed for an accepted op: %v", err)
		}

		var decoded struct {
			Body   string `json:"body"`
			Digest string `json:"operation_sha256"`
		}
		if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
			t.Fatalf("emitted invalid JSON: %v", err)
		}
		if decoded.Body != body {
			t.Fatalf("body did not survive the round trip\n got: %q\nwant: %q", decoded.Body, body)
		}
		if decoded.Digest != Digest([]byte(body)) {
			t.Fatal("digest did not survive the round trip")
		}
	})
}

// Whatever the unquoter accepts must hash the same as the standard library's
// decoding, except where lone surrogates make them legitimately differ.
func FuzzUnquoteMatchesStdlib(f *testing.F) {
	f.Add(`"hello"`)
	f.Add(`"é"`)
	f.Add(`"😀"`)
	f.Add(`"\ud800"`)

	f.Fuzz(func(t *testing.T, literal string) {
		got, err := UnquoteJSONString([]byte(literal))
		if err != nil {
			return
		}
		var want string
		if err := json.Unmarshal([]byte(literal), &want); err != nil {
			return // stdlib rejects it; ours is free to be more permissive
		}
		if string(got) != want {
			t.Fatalf("unquote mismatch for %q\n got: % x\nwant: % x", literal, got, want)
		}
	})
}

func FuzzParseDec(f *testing.F) {
	f.Add("0")
	f.Add("1")
	f.Add("18446744073709551615")
	f.Add("01")

	f.Fuzz(func(t *testing.T, s string) {
		d, err := ParseDec(s)
		if err != nil {
			return
		}
		// Anything accepted must render back to exactly the input, which is what
		// makes the TEXT unique index on entity_rev safe.
		if d.String() != s {
			t.Fatalf("ParseDec(%q).String() = %q — not canonical", s, d.String())
		}
	})
}

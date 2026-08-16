package proto

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// The whole point of emitting by hand: json.Marshal would rewrite <, > and &
// even inside a json.RawMessage, and the client re-canonicalises every body it
// receives and compares it to the raw string. One rewritten byte wedges it.
func TestEmitChangeOpPreservesBytesExactly(t *testing.T) {
	body := buildBody("observation", "observation:abc", "1", "device-1", "7",
		`{"text":"a < b && c > d"}`)
	raw, err := json.Marshal(map[string]string{"body": body, "operation_sha256": Digest([]byte(body))})
	if err != nil {
		t.Fatal(err)
	}
	op, err := ParseOp(raw)
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := EmitChangeOp(&buf, 403, 1755300000000, op); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	if strings.Contains(out, `<`) || strings.Contains(out, `&`) {
		t.Fatalf("emitted body was HTML-escaped: %s", out)
	}
	if !bytes.Contains(buf.Bytes(), op.RawLiteral) {
		t.Fatal("emitted output does not contain the literal byte-for-byte")
	}

	// Sequence and timestamp must be quoted decimals; a number wedges the client.
	var decoded struct {
		Seq      json.RawMessage `json:"seq"`
		ServerTS json.RawMessage `json:"server_ts"`
		Body     string          `json:"body"`
		Digest   string          `json:"operation_sha256"`
	}
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("emitted invalid JSON: %v (%s)", err, out)
	}
	if string(decoded.Seq) != `"403"` {
		t.Errorf("seq = %s, want a quoted decimal", decoded.Seq)
	}
	if string(decoded.ServerTS) != `"1755300000000"` {
		t.Errorf("server_ts = %s, want a quoted decimal", decoded.ServerTS)
	}
	if decoded.Body != body {
		t.Error("body did not survive emission")
	}
	if decoded.Digest != Digest([]byte(body)) {
		t.Error("digest did not survive emission")
	}
}

// origin_local_id must be present even when null: the client type-checks it as
// string|null, and an omitted key is undefined, which throws.
func TestEmitAckAlwaysIncludesOriginLocalID(t *testing.T) {
	var buf bytes.Buffer
	if err := EmitAck(&buf, Ack{
		ID: "mutation:0b7f3e2a", Kind: "mutation", EntityRev: 3,
		Digest: Digest([]byte("x")), Seq: 9, OriginLocalID: nil,
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"origin_local_id":null`) {
		t.Fatalf("ack omitted a null origin_local_id: %s", buf.String())
	}

	local := Dec(42)
	buf.Reset()
	if err := EmitAck(&buf, Ack{
		ID: "observation:abc", Kind: "observation", EntityRev: 1,
		Digest: Digest([]byte("x")), Seq: 10, OriginLocalID: &local,
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"origin_local_id":"42"`) {
		t.Fatalf("ack emitted a non-string origin_local_id: %s", buf.String())
	}
}

func TestEmitAckFieldsAreAllPresent(t *testing.T) {
	var buf bytes.Buffer
	if err := EmitAck(&buf, Ack{
		ID: "observation:abc", Kind: "observation", EntityRev: 1,
		Digest: Digest([]byte("x")), Seq: 5,
	}); err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("invalid ack JSON: %v", err)
	}
	for _, k := range []string{"id", "kind", "entity_rev", "operation_sha256", "seq", "origin_local_id"} {
		if _, ok := got[k]; !ok {
			t.Errorf("ack is missing the mandatory field %q", k)
		}
	}
	if string(got["seq"]) != `"5"` || string(got["entity_rev"]) != `"1"` {
		t.Errorf("seq/entity_rev must be quoted decimals: %s %s", got["seq"], got["entity_rev"])
	}
}

func TestEmitAckEscapesStringFields(t *testing.T) {
	var buf bytes.Buffer
	if err := EmitAck(&buf, Ack{
		ID: `weird"id`, Kind: "observation", EntityRev: 1, Digest: Digest([]byte("x")), Seq: 1,
	}); err != nil {
		t.Fatal(err)
	}
	var got struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("quoting produced invalid JSON: %v", err)
	}
	if got.ID != `weird"id` {
		t.Fatalf("id = %q", got.ID)
	}
}

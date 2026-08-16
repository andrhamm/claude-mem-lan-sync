package proto

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// buildBody produces a canonical operation body: twelve keys in sorted order,
// exactly as the client emits them. Verified against real captured traffic from
// the Task 0 spike.
func buildBody(kind, id, rev, deviceID, localID, payload string) string {
	local := "null"
	if localID != "" {
		local = `"` + localID + `"`
	}
	mutation := "null"
	if kind == "mutation" {
		mutation = `{"op":"set_title","title":"x"}`
	}
	return fmt.Sprintf(
		`{"body_schema_version":1,"deleted":false,"deleted_at":null,"entity_rev":"%s",`+
			`"id":"%s","kind":"%s","mutation":%s,"origin_device_id":"%s",`+
			`"origin_local_id":%s,"payload":%s,"payload_schema_version":2,"payload_sha256":"%s"}`,
		rev, id, kind, mutation, deviceID, local, payload, Digest([]byte(payload)))
}

func wrap(t *testing.T, body string) json.RawMessage {
	t.Helper()
	w, err := json.Marshal(map[string]string{
		"body":             body,
		"operation_sha256": Digest([]byte(body)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func validContentBody() string {
	return buildBody("observation", "observation:abc", "1", "device-1", "7", `{"project":"p"}`)
}

func TestParseOpAcceptsContentOp(t *testing.T) {
	op, err := ParseOp(wrap(t, validContentBody()))
	if err != nil {
		t.Fatalf("ParseOp: %v", err)
	}
	if op.Kind != "observation" || op.ID != "observation:abc" {
		t.Fatalf("routing fields wrong: %+v", op)
	}
	if op.EntityRev != 1 {
		t.Fatalf("EntityRev = %d", op.EntityRev)
	}
	if op.OriginLocalID == nil || *op.OriginLocalID != 7 {
		t.Fatalf("OriginLocalID = %v, want 7", op.OriginLocalID)
	}
	if op.OriginDeviceID != "device-1" {
		t.Fatalf("OriginDeviceID = %q", op.OriginDeviceID)
	}
}

// mutation is a fourth kind with its own branch in the client. Rejecting it
// would 400 every set_title / set_prompt_session / remap_project, and a 400
// parks that op at the head of the outbox forever, blocking everything behind it.
func TestParseOpAcceptsMutationKind(t *testing.T) {
	body := buildBody("mutation", "mutation:0b7f3e2a-1c4d-4a6b-8f2e-9d0c1a2b3c4d", "3", "device-1", "", "null")
	op, err := ParseOp(wrap(t, body))
	if err != nil {
		t.Fatalf("ParseOp rejected a mutation op: %v", err)
	}
	if op.Kind != "mutation" {
		t.Fatalf("Kind = %q", op.Kind)
	}
	if op.OriginLocalID != nil {
		t.Fatal("mutation ops carry a null origin_local_id")
	}
}

func TestParseOpRejectsUnknownKind(t *testing.T) {
	body := buildBody("telemetry", "telemetry:x", "1", "device-1", "1", `{}`)
	_, err := ParseOp(wrap(t, body))
	if ReasonOf(err) != ReasonUnknownKind {
		t.Fatalf("reason = %q, want unknown_kind", ReasonOf(err))
	}
}

func TestParseOpRejectsWrapperShape(t *testing.T) {
	body := validContentBody()
	cases := map[string]string{
		"extra key":       fmt.Sprintf(`{"body":%q,"operation_sha256":%q,"extra":1}`, body, Digest([]byte(body))),
		"missing body":    fmt.Sprintf(`{"operation_sha256":%q}`, Digest([]byte(body))),
		"missing digest":  fmt.Sprintf(`{"body":%q}`, body),
		"body not string": fmt.Sprintf(`{"body":{"a":1},"operation_sha256":%q}`, Digest([]byte(body))),
		"hex digest":      fmt.Sprintf(`{"body":%q,"operation_sha256":"%s"}`, body, strings.Repeat("a", 64)),
		"duplicate key":   fmt.Sprintf(`{"body":%q,"body":%q,"operation_sha256":%q}`, body, body, Digest([]byte(body))),
		"trailing data":   fmt.Sprintf(`{"body":%q,"operation_sha256":%q}{"evil":1}`, body, Digest([]byte(body))),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseOp(json.RawMessage(raw)); err == nil {
				t.Fatal("accepted a malformed wrapper")
			}
		})
	}
}

func TestParseOpRejectsDigestMismatch(t *testing.T) {
	body := validContentBody()
	raw := fmt.Sprintf(`{"body":%q,"operation_sha256":%q}`, body, Digest([]byte("something else")))
	_, err := ParseOp(json.RawMessage(raw))
	if ReasonOf(err) != ReasonDigestMismatch {
		t.Fatalf("reason = %q, want digest_mismatch", ReasonOf(err))
	}
}

// Duplicate keys inside the body defeat a plain map-length check: last-wins
// would leave twelve entries while meaning something different to the client.
func TestParseOpRejectsDuplicateBodyKeys(t *testing.T) {
	body := validContentBody()
	dup := strings.Replace(body, `"kind":"observation"`, `"kind":"observation","kind":"summary"`, 1)
	raw := fmt.Sprintf(`{"body":%q,"operation_sha256":%q}`, dup, Digest([]byte(dup)))
	_, err := ParseOp(json.RawMessage(raw))
	if ReasonOf(err) != ReasonBodyShape {
		t.Fatalf("reason = %q, want body_shape", ReasonOf(err))
	}
}

func TestParseOpRejectsWrongKeyCount(t *testing.T) {
	body := strings.TrimSuffix(validContentBody(), "}") + `,"thirteenth":1}`
	raw := fmt.Sprintf(`{"body":%q,"operation_sha256":%q}`, body, Digest([]byte(body)))
	if _, err := ParseOp(json.RawMessage(raw)); err == nil {
		t.Fatal("accepted a body with thirteen keys")
	}
}

// "01" and "1" must not both be storable: the unique index is a TEXT
// comparison, so two forms of one revision would break first-write-wins dedupe.
func TestParseOpRejectsNonCanonicalEntityRev(t *testing.T) {
	for _, rev := range []string{"01", "0", "-1", "1.0", ""} {
		body := buildBody("observation", "observation:abc", rev, "device-1", "7", `{}`)
		raw := fmt.Sprintf(`{"body":%q,"operation_sha256":%q}`, body, Digest([]byte(body)))
		if _, err := ParseOp(json.RawMessage(raw)); err == nil {
			t.Errorf("accepted entity_rev %q", rev)
		}
	}
}

func TestParseOpRejectsOversizeBody(t *testing.T) {
	big := strings.Repeat("x", MaxBodyBytes+1)
	body := buildBody("observation", "observation:abc", "1", "device-1", "7", fmt.Sprintf(`{"t":%q}`, big))
	raw, err := json.Marshal(map[string]string{"body": body, "operation_sha256": Digest([]byte(body))})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseOp(raw); ReasonOf(err) != ReasonTooLarge {
		t.Fatalf("reason = %q, want too_large", ReasonOf(err))
	}
}

func TestParseOpRejectsOversizeDeviceID(t *testing.T) {
	body := buildBody("observation", "observation:abc", "1", strings.Repeat("d", MaxDeviceIDLen+1), "7", `{}`)
	raw := fmt.Sprintf(`{"body":%q,"operation_sha256":%q}`, body, Digest([]byte(body)))
	if _, err := ParseOp(json.RawMessage(raw)); err == nil {
		t.Fatal("accepted an oversized origin_device_id")
	}
}

// The literal must survive parsing untouched — it is what gets stored and
// re-emitted, and the client re-canonicalises it on receipt.
func TestParseOpPreservesLiteralBytes(t *testing.T) {
	body := buildBody("observation", "observation:abc", "1", "device-1", "7",
		`{"text":"angle < brackets > and & ampersands ☃"}`)
	raw, err := json.Marshal(map[string]string{"body": body, "operation_sha256": Digest([]byte(body))})
	if err != nil {
		t.Fatal(err)
	}

	op, err := ParseOp(raw)
	if err != nil {
		t.Fatalf("ParseOp: %v", err)
	}

	var literal string
	if err := json.Unmarshal(op.RawLiteral, &literal); err != nil {
		t.Fatalf("RawLiteral is not a JSON string: %v", err)
	}
	if literal != body {
		t.Fatal("RawLiteral does not decode back to the original body")
	}
	if op.RawLiteral[0] != '"' || op.RawLiteral[len(op.RawLiteral)-1] != '"' {
		t.Fatal("RawLiteral must include its surrounding quotes")
	}
}

package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/andrhamm/claude-mem-lan-sync/internal/proto"
)

func captureWithSecrets(t *testing.T) []byte {
	t.Helper()
	payload := `{"project":"client-acme","text":"the API key is sk-live-9f3a and the db is at db.internal"}`
	body := fmt.Sprintf(
		`{"body_schema_version":1,"deleted":false,"deleted_at":null,"entity_rev":"1",`+
			`"id":"observation:abc","kind":"observation","mutation":null,"origin_device_id":"device-1",`+
			`"origin_local_id":"7","payload":%s,"payload_schema_version":2,"payload_sha256":"%s"}`,
		payload, proto.Digest([]byte(payload)))

	push := map[string]any{
		"protocol_version": 2,
		"ops": []map[string]string{{
			"body":             body,
			"operation_sha256": proto.Digest([]byte(body)),
		}},
	}
	pushRaw, err := json.Marshal(push)
	if err != nil {
		t.Fatal(err)
	}

	exchange := map[string]any{
		"method": "POST",
		"path":   "/v1/sync/ops",
		"headers": map[string]string{
			"Authorization": "Bearer super-secret-pre-shared-key",
			"X-User-Id":     "d0b66c9ef725e115cf458c365b3fa4c0",
			"X-Device-Id":   "94a3962b-daef-44c7-9475-a0eb978f4a19",
			"X-Device-Name": "example-laptop",
		},
		"request": json.RawMessage(pushRaw),
		"status":  200,
	}
	raw, err := json.Marshal(exchange)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// A captured exchange contains real prompt text and a bearer token. Committing
// one to a public repository is a one-way mistake, so scrubbing must remove both.
func TestScrubRemovesSecretsAndContent(t *testing.T) {
	scrubbed, err := ScrubExchange(captureWithSecrets(t))
	if err != nil {
		t.Fatal(err)
	}
	out := string(scrubbed)

	for _, secret := range []string{
		"super-secret-pre-shared-key",
		"sk-live-9f3a",
		"db.internal",
		"example-laptop",
		"94a3962b-daef-44c7-9475-a0eb978f4a19",
		"client-acme",
	} {
		if strings.Contains(out, secret) {
			t.Errorf("scrubbed fixture still contains %q", secret)
		}
	}
}

// The fixture is only useful if it still validates, which means the digests must
// be recomputed over the synthetic content rather than carried over.
func TestScrubbedFixtureStillParses(t *testing.T) {
	scrubbed, err := ScrubExchange(captureWithSecrets(t))
	if err != nil {
		t.Fatal(err)
	}

	var doc struct {
		Request struct {
			Ops []json.RawMessage `json:"ops"`
		} `json:"request"`
	}
	if err := json.Unmarshal(scrubbed, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Request.Ops) != 1 {
		t.Fatalf("scrubbed capture has %d ops", len(doc.Request.Ops))
	}

	op, err := proto.ParseOp(doc.Request.Ops[0])
	if err != nil {
		t.Fatalf("scrubbed op no longer validates: %v", err)
	}
	if op.Kind != "observation" || op.EntityRev != 1 {
		t.Fatalf("scrubbing changed routing fields: %+v", op)
	}
}

// Sizes are what several behaviours key on — page byte caps, body limits — so
// the replacement keeps them.
func TestScrubPreservesShapeAndLength(t *testing.T) {
	scrubbed, err := ScrubExchange(captureWithSecrets(t))
	if err != nil {
		t.Fatal(err)
	}

	var doc struct {
		Request struct {
			Ops []map[string]json.RawMessage `json:"ops"`
		} `json:"request"`
	}
	if err := json.Unmarshal(scrubbed, &doc); err != nil {
		t.Fatal(err)
	}

	var body string
	if err := json.Unmarshal(doc.Request.Ops[0]["body"], &body); err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 12 {
		t.Fatalf("scrubbed body has %d keys, want the canonical twelve", len(fields))
	}

	var payload map[string]string
	if err := json.Unmarshal(fields["payload"], &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload["text"]) != len("the API key is sk-live-9f3a and the db is at db.internal") {
		t.Errorf("replacement text length %d does not match the original", len(payload["text"]))
	}
}

func TestLoremMatchesLength(t *testing.T) {
	for _, in := range []string{"", "a", "hello world", strings.Repeat("x", 500)} {
		if got := lorem(in); len(got) != len(in) {
			t.Errorf("lorem(%d chars) produced %d", len(in), len(got))
		}
	}
}

package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"

	"github.com/andrhamm/claude-mem-lan-sync/internal/proto"
)

func runFixtures(args []string, env Env) int {
	if len(args) == 0 || args[0] != "scrub" {
		fmt.Fprintln(env.Stderr, "usage: cmemlan fixtures scrub --in <dir> --out <dir>")
		return 2
	}

	fs := flag.NewFlagSet("fixtures scrub", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	in := fs.String("in", "", "directory of raw captures")
	out := fs.String("out", "testdata/fixtures", "directory for scrubbed fixtures")
	if _, err := parseFlags(fs, args[1:]); err != nil {
		return 2
	}
	if *in == "" {
		fmt.Fprintln(env.Stderr, "cmemlan: --in is required")
		return 2
	}

	entries, err := filepath.Glob(filepath.Join(*in, "*.json"))
	if err != nil || len(entries) == 0 {
		fmt.Fprintf(env.Stderr, "cmemlan: no captures found in %s\n", *in)
		return 1
	}
	if err := os.MkdirAll(*out, 0o750); err != nil {
		fmt.Fprintf(env.Stderr, "cmemlan: %v\n", err)
		return 1
	}

	for _, path := range entries {
		raw, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(env.Stderr, "cmemlan: %v\n", err)
			return 1
		}
		scrubbed, err := ScrubExchange(raw)
		if err != nil {
			fmt.Fprintf(env.Stderr, "cmemlan: %s: %v\n", filepath.Base(path), err)
			return 1
		}
		dst := filepath.Join(*out, filepath.Base(path))
		if err := os.WriteFile(dst, scrubbed, 0o644); err != nil {
			fmt.Fprintf(env.Stderr, "cmemlan: %v\n", err)
			return 1
		}
		fmt.Fprintf(env.Stdout, "scrubbed %s\n", dst)
	}

	fmt.Fprintf(env.Stdout, "\n%d fixtures written to %s\n", len(entries), *out)
	fmt.Fprintln(env.Stdout, "Review them before committing: only scrubbed fixtures belong in the repository.")
	return 0
}

// ScrubExchange replaces captured content with synthetic text of the same shape
// and removes credentials, so a fixture can be committed.
//
// Structure, key order, escaping and length are preserved because those are what
// the tests exercise; the words themselves are not. Digests are recomputed so
// the result still validates.
func ScrubExchange(raw []byte) ([]byte, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("not a capture file: %w", err)
	}

	// Credentials and device identity never survive.
	if h, ok := doc["headers"]; ok {
		var headers map[string]string
		if err := json.Unmarshal(h, &headers); err == nil {
			for _, k := range []string{"Authorization", "X-User-Id", "X-Device-Id", "X-Device-Name"} {
				if _, present := headers[k]; present {
					headers[k] = "REDACTED"
				}
			}
			if b, err := json.Marshal(headers); err == nil {
				doc["headers"] = b
			}
		}
	}

	// A /pair exchange carries the pairing code in the request and the hub's
	// pre-shared key in the response. Neither belongs in a fixture, and neither
	// sits under an "ops" envelope, so the op-scrubbing path below never sees
	// them. Replace both payloads outright.
	if stringField(doc, "path") == "/pair" {
		doc["request"] = json.RawMessage(`{"code":"REDACTED"}`)
		doc["response"] = json.RawMessage(`{"token":"REDACTED","user_id":"REDACTED"}`)
		return json.MarshalIndent(doc, "", "  ")
	}

	for _, field := range []string{"request", "response"} {
		body, ok := doc[field]
		if !ok {
			continue
		}
		scrubbed, err := scrubBodies(body)
		if err != nil {
			return nil, err
		}
		doc[field] = scrubbed
	}

	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}

	// Last line of defence. A capture shape this code does not understand must
	// fail loudly rather than quietly emit a fixture with a secret in it.
	if leak := findSecretShapes(out); leak != "" {
		return nil, fmt.Errorf(
			"refusing to emit a fixture containing %s; this capture has a shape the scrubber "+
				"does not understand, so it must not be committed", leak)
	}
	return out, nil
}

func stringField(doc map[string]json.RawMessage, key string) string {
	raw, ok := doc[key]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

var (
	// Any credential-ish field holding a long opaque value. Digests live in
	// dedicated fields (operation_sha256, payload_sha256) and are legitimate.
	secretLike = regexp.MustCompile(`"(token|psk|key|secret|password)"\s*:\s*"[A-Za-z0-9_+/=-]{16,}"`)
	// The pairing code format.
	codeLike = regexp.MustCompile(`\b\d{3}-\d{3}-\d{3}\b`)
)

// findSecretShapes describes the first secret-shaped value found, or "".
func findSecretShapes(b []byte) string {
	if secretLike.Find(b) != nil {
		return "a credential-shaped field"
	}
	if codeLike.Find(b) != nil {
		return "a pairing code"
	}
	return ""
}

// scrubBodies rewrites every op body inside a push or changes payload.
func scrubBodies(payload json.RawMessage) (json.RawMessage, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return payload, nil // not an envelope we understand; leave it alone
	}

	opsRaw, ok := envelope["ops"]
	if !ok {
		return payload, nil
	}
	var ops []map[string]json.RawMessage
	if err := json.Unmarshal(opsRaw, &ops); err != nil {
		return payload, nil
	}

	for i, op := range ops {
		bodyRaw, ok := op["body"]
		if !ok {
			continue
		}
		var body string
		if err := json.Unmarshal(bodyRaw, &body); err != nil {
			continue
		}

		synthetic, err := synthesiseBody(body)
		if err != nil {
			return nil, err
		}
		encoded, err := json.Marshal(synthetic)
		if err != nil {
			return nil, err
		}
		ops[i]["body"] = encoded

		// The digest must match the new bytes or the fixture is unusable.
		digest, err := json.Marshal(proto.Digest([]byte(synthetic)))
		if err != nil {
			return nil, err
		}
		ops[i]["operation_sha256"] = digest
	}

	newOps, err := json.Marshal(ops)
	if err != nil {
		return nil, err
	}
	envelope["ops"] = newOps
	return json.Marshal(envelope)
}

// synthesiseBody replaces content inside an op body while keeping its shape.
func synthesiseBody(body string) (string, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		return "", fmt.Errorf("op body is not JSON: %w", err)
	}

	if payload, ok := doc["payload"]; ok && string(payload) != "null" {
		// Every string leaf, not only the top-level ones. In a real observation
		// the extracted memory lives in arrays — facts, concepts, files_read — so
		// replacing only top-level strings would leave the actual content behind.
		var decoded any
		if err := json.Unmarshal(payload, &decoded); err == nil {
			if b, err := json.Marshal(replaceStrings(decoded)); err == nil {
				doc["payload"] = b
				if d, err := json.Marshal(proto.Digest(b)); err == nil {
					doc["payload_sha256"] = d
				}
			}
		}
	}

	// Identifiers outside the payload are personal too: a device uuid is stable
	// and correlates every op a machine ever pushed.
	for _, k := range []string{"origin_device_id", "id"} {
		raw, ok := doc[k]
		if !ok {
			continue
		}
		var s string
		if err := json.Unmarshal(raw, &s); err != nil || s == "" {
			continue
		}
		if b, err := json.Marshal(anonymiseIdentifier(k, s)); err == nil {
			doc[k] = b
		}
	}

	// Re-encode with sorted keys, which is the canonical form the client expects.
	out, err := json.Marshal(doc)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// replaceStrings walks a decoded JSON value and replaces every string leaf,
// preserving structure and length.
func replaceStrings(v any) any {
	switch t := v.(type) {
	case string:
		return lorem(t)
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = replaceStrings(e)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = replaceStrings(e)
		}
		return out
	default:
		// Numbers, booleans and nulls carry no content and keep the fixture
		// structurally faithful.
		return v
	}
}

// anonymiseIdentifier replaces an identifier while keeping its shape, so
// validation that depends on the format still gets exercised.
func anonymiseIdentifier(key, value string) string {
	if key == "id" {
		// Entity ids are "<kind>:<digest>"; keep the kind so routing still works.
		if i := strings.Index(value, ":"); i >= 0 {
			return value[:i+1] + proto.Digest([]byte("fixture:"+value))
		}
		return proto.Digest([]byte("fixture:" + value))
	}

	// Device ids are uuids; emit a stable fake of the same shape.
	digest := proto.Digest([]byte("fixture-device:" + value))
	hex := make([]rune, 0, 32)
	for _, r := range strings.ToLower(digest) {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') {
			hex = append(hex, r)
		}
		if len(hex) == 32 {
			break
		}
	}
	for len(hex) < 32 {
		hex = append(hex, '0')
	}
	s := string(hex)
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32]
}

// lorem returns filler of the same length as the input, so size-sensitive
// behaviour still gets exercised.
func lorem(s string) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	words := []string{"lorem", "ipsum", "dolor", "sit", "amet", "consectetur"}
	i := 0
	for b.Len() < len(s) {
		w := words[i%len(words)]
		i++
		for _, r := range w {
			if b.Len() >= len(s) {
				break
			}
			b.WriteRune(unicode.ToLower(r))
		}
		if b.Len() < len(s) {
			b.WriteByte(' ')
		}
	}
	return b.String()[:len(s)]
}

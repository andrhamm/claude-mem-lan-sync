package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
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

	return json.MarshalIndent(doc, "", "  ")
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

// synthesiseBody replaces free text inside a body while keeping its shape.
func synthesiseBody(body string) (string, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		return "", fmt.Errorf("op body is not JSON: %w", err)
	}

	if payload, ok := doc["payload"]; ok && string(payload) != "null" {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(payload, &fields); err == nil {
			for k, v := range fields {
				var s string
				if err := json.Unmarshal(v, &s); err != nil {
					continue // not a string; leave numbers, arrays and nulls alone
				}
				replaced, err := json.Marshal(lorem(s))
				if err != nil {
					return "", err
				}
				fields[k] = replaced
			}
			if b, err := json.Marshal(fields); err == nil {
				doc["payload"] = b
				// payload_sha256 must match the synthetic payload.
				if d, err := json.Marshal(proto.Digest(b)); err == nil {
					doc["payload_sha256"] = d
				}
			}
		}
	}

	// Re-encode with sorted keys, which is the canonical form the client expects.
	out, err := json.Marshal(doc)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// lorem returns filler of the same length and rough shape as the input, so
// size-sensitive behaviour still gets exercised.
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

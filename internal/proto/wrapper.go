package proto

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// Limits taken from the client. MaxBodyBytes applies to the decoded body; a
// literal can be several times longer once escapes are counted, so it gets its
// own looser guard rather than a shared one.
const (
	MaxBodyBytes    = 256_000
	MaxBodyLiteral  = 6*MaxBodyBytes + 2
	MaxRequestBytes = 4_000_000
	MaxOpsPerPush   = 1_000
	MaxPageLimit    = 500
	MaxDeviceIDLen  = 128
)

// Op is a validated operation.
//
// RawLiteral is the JSON string literal exactly as received, quotes included.
// It is what gets stored and what gets written back out; nothing re-encodes it.
// The decoded form exists only long enough to verify the digest and read the
// routing fields, then is discarded.
type Op struct {
	RawLiteral     []byte
	Digest         string
	ID             string
	Kind           string
	EntityRev      Dec
	OriginDeviceID string
	OriginLocalID  *Dec
}

// Ack is one entry in a push response.
//
// Every field is mandatory on the wire. OriginLocalID is nullable but must be
// present: the client type-checks it as `string | null` and an omitted key is
// undefined, which fails that check and wedges the push pipeline.
type Ack struct {
	ID            string
	Kind          string
	EntityRev     Dec
	Digest        string
	OriginLocalID *Dec
	Seq           Dec
}

// Valid content kinds, plus the mutation kind.
//
// mutation is a real fourth kind with its own validation branch in the client
// (set_title, set_prompt_session, remap_project). Omitting it from this set
// would 400 every project rename, and a 400 blocks that device's outbox forever.
var validKinds = map[string]bool{
	"observation": true,
	"summary":     true,
	"prompt":      true,
	"mutation":    true,
}

// bodyKeys is the exact key set of an operation body.
var bodyKeys = []string{
	"body_schema_version", "deleted", "deleted_at", "entity_rev", "id", "kind",
	"mutation", "origin_device_id", "origin_local_id", "payload",
	"payload_schema_version", "payload_sha256",
}

// ParseOp validates a single operation wrapper and extracts what the hub needs.
//
// Validation here is deliberately minimal. A rejected push is not dropped by
// the client — it stays at the head of the outbox and is retried forever,
// blocking every op behind it. So this rejects only what the hub cannot store
// or route, never anything a conforming client could legitimately produce.
func ParseOp(wrapper json.RawMessage) (Op, error) {
	if len(wrapper) > MaxBodyLiteral+1024 {
		return Op{}, Reject(ReasonTooLarge)
	}

	fields, err := objectFields(wrapper)
	if err != nil {
		return Op{}, Reject(ReasonWrapperShape)
	}
	if len(fields) != 2 {
		return Op{}, Reject(ReasonWrapperShape)
	}
	rawBody, okBody := fields["body"]
	rawDigest, okDigest := fields["operation_sha256"]
	if !okBody || !okDigest {
		return Op{}, Reject(ReasonWrapperShape)
	}

	if len(rawBody) == 0 || rawBody[0] != '"' {
		return Op{}, Reject(ReasonWrapperShape)
	}
	if len(rawBody) > MaxBodyLiteral {
		return Op{}, Reject(ReasonTooLarge)
	}

	var digest string
	if err := json.Unmarshal(rawDigest, &digest); err != nil || !ValidDigest(digest) {
		return Op{}, Reject(ReasonWrapperShape)
	}

	decoded, err := UnquoteJSONString(rawBody)
	if err != nil {
		return Op{}, err
	}
	if len(decoded) > MaxBodyBytes {
		return Op{}, Reject(ReasonTooLarge)
	}
	if Digest(decoded) != digest {
		return Op{}, Reject(ReasonDigestMismatch)
	}

	op, err := parseBody(decoded)
	if err != nil {
		return Op{}, err
	}
	op.Digest = digest

	// Copy the literal: the caller's buffer may be reused.
	op.RawLiteral = bytes.Clone(rawBody)
	return op, nil
}

func parseBody(decoded []byte) (Op, error) {
	fields, err := objectFields(decoded)
	if err != nil {
		return Op{}, Reject(ReasonBodyShape)
	}
	if len(fields) != len(bodyKeys) {
		return Op{}, Reject(ReasonBodyShape)
	}
	for _, k := range bodyKeys {
		if _, ok := fields[k]; !ok {
			return Op{}, Reject(ReasonBodyShape)
		}
	}

	var op Op

	if err := json.Unmarshal(fields["kind"], &op.Kind); err != nil {
		return Op{}, Reject(ReasonBodyShape)
	}
	if !validKinds[op.Kind] {
		return Op{}, Reject(ReasonUnknownKind)
	}

	if err := json.Unmarshal(fields["id"], &op.ID); err != nil || op.ID == "" {
		return Op{}, Reject(ReasonBodyShape)
	}

	var rev string
	if err := json.Unmarshal(fields["entity_rev"], &rev); err != nil {
		return Op{}, Reject(ReasonEntityRev)
	}
	op.EntityRev, err = ParseDecPositive(rev)
	if err != nil {
		return Op{}, Reject(ReasonEntityRev)
	}

	if err := json.Unmarshal(fields["origin_device_id"], &op.OriginDeviceID); err != nil {
		return Op{}, Reject(ReasonBodyShape)
	}
	if op.OriginDeviceID == "" || len(op.OriginDeviceID) > MaxDeviceIDLen {
		return Op{}, Reject(ReasonBodyShape)
	}

	// origin_local_id is a decimal string for content ops and null for mutations.
	if !bytes.Equal(bytes.TrimSpace(fields["origin_local_id"]), []byte("null")) {
		var local string
		if err := json.Unmarshal(fields["origin_local_id"], &local); err != nil {
			return Op{}, Reject(ReasonBodyShape)
		}
		v, err := ParseDec(local)
		if err != nil {
			return Op{}, Reject(ReasonBodyShape)
		}
		op.OriginLocalID = &v
	}

	return op, nil
}

// objectFields decodes a JSON object one token at a time so that duplicate keys
// are rejected.
//
// Unmarshalling into a map would silently apply last-wins, so a body carrying
// "id" twice plus eleven other keys would pass a twelve-key count while meaning
// something different to us than to the client. Trailing data after the object
// is rejected for the same reason.
func objectFields(data []byte) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()

	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errors.New("proto: expected a JSON object")
	}

	fields := make(map[string]json.RawMessage)
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, errors.New("proto: non-string object key")
		}
		if _, dup := fields[key]; dup {
			return nil, errors.New("proto: duplicate object key")
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		fields[key] = raw
	}

	// Consume the closing brace, then require end of input.
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("proto: trailing data after object")
	}
	return fields, nil
}

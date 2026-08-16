package proto

import (
	"io"
	"strconv"
)

// Emission writes bytes directly rather than marshalling structs.
//
// json.Marshal hardcodes HTML escaping and applies it even to json.RawMessage,
// rewriting <, > and & and stripping insignificant whitespace. json.RawMessage
// is verbatim on decode but not on encode, which makes the obvious
// implementation look correct while quietly breaking the byte-for-byte
// guarantee the client checks by re-canonicalising every body it receives.
//
// So the literal is written straight through, and nothing here ever hands a
// body to encoding/json.

// EmitChangeOp writes one entry of a /v1/sync/changes page.
func EmitChangeOp(w io.Writer, seq, serverTS Dec, op Op) error {
	if _, err := io.WriteString(w, `{"seq":"`); err != nil {
		return err
	}
	if _, err := io.WriteString(w, seq.String()); err != nil {
		return err
	}
	if _, err := io.WriteString(w, `","server_ts":"`); err != nil {
		return err
	}
	if _, err := io.WriteString(w, serverTS.String()); err != nil {
		return err
	}
	if _, err := io.WriteString(w, `","body":`); err != nil {
		return err
	}
	// The literal already carries its own quotes.
	if _, err := w.Write(op.RawLiteral); err != nil {
		return err
	}
	if _, err := io.WriteString(w, `,"operation_sha256":`); err != nil {
		return err
	}
	if _, err := io.WriteString(w, strconv.Quote(op.Digest)); err != nil {
		return err
	}
	_, err := io.WriteString(w, `}`)
	return err
}

// EmitAck writes one acknowledgement.
//
// origin_local_id is emitted as null when absent rather than omitted: the
// client requires the key to be present and type-checks it as string|null, so
// an omitted key is undefined and throws.
func EmitAck(w io.Writer, a Ack) error {
	if _, err := io.WriteString(w, `{"id":`); err != nil {
		return err
	}
	if _, err := io.WriteString(w, strconv.Quote(a.ID)); err != nil {
		return err
	}
	if _, err := io.WriteString(w, `,"kind":`); err != nil {
		return err
	}
	if _, err := io.WriteString(w, strconv.Quote(a.Kind)); err != nil {
		return err
	}
	if _, err := io.WriteString(w, `,"entity_rev":"`); err != nil {
		return err
	}
	if _, err := io.WriteString(w, a.EntityRev.String()); err != nil {
		return err
	}
	if _, err := io.WriteString(w, `","operation_sha256":`); err != nil {
		return err
	}
	if _, err := io.WriteString(w, strconv.Quote(a.Digest)); err != nil {
		return err
	}
	if _, err := io.WriteString(w, `,"seq":"`); err != nil {
		return err
	}
	if _, err := io.WriteString(w, a.Seq.String()); err != nil {
		return err
	}
	if _, err := io.WriteString(w, `","origin_local_id":`); err != nil {
		return err
	}
	if a.OriginLocalID == nil {
		if _, err := io.WriteString(w, `null`); err != nil {
			return err
		}
	} else {
		if _, err := io.WriteString(w, `"`+a.OriginLocalID.String()+`"`); err != nil {
			return err
		}
	}
	_, err := io.WriteString(w, `}`)
	return err
}

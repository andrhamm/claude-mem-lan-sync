package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/andrhamm/claude-mem-lan-sync/internal/proto"
)

// ChangeOp is one entry of a /v1/sync/changes page.
type ChangeOp struct {
	Seq        proto.Dec
	ServerTS   proto.Dec
	RawLiteral []byte
	Digest     string
}

// ChangesResult is a page plus the state the client validates it against.
type ChangesResult struct {
	Ops     []ChangeOp
	More    bool
	HeadSeq proto.Dec
	Epoch   proto.Dec
}

// DefaultMaxPageBytes bounds a response body. The client asks for 500 ops and an
// op body may be 256,000 bytes, so an unbounded page could reach 128 MB.
const DefaultMaxPageBytes = 8 << 20

// Changes returns the ops after since, in sequence order.
//
// Two rules the hub must never break, both of which permanently wedge a client:
//
//   - Ops are never filtered by device. Not echoing a device its own writes
//     looks like an obvious optimisation and produces sequence gaps; the client
//     discards its own ops itself, after checking contiguity.
//   - The page, head_seq and epoch are read in one transaction. Reading head
//     separately lets a concurrent push make it inconsistent with the page.
func (s *Store) Changes(ctx context.Context, since proto.Dec, limit, maxBytes int) (ChangesResult, error) {
	if limit <= 0 || limit > proto.MaxPageLimit {
		limit = proto.MaxPageLimit
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxPageBytes
	}

	sinceInt, err := since.Int64()
	if err != nil {
		return ChangesResult{}, proto.Reject(proto.ReasonBadCursor)
	}

	tx, err := s.r.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ChangesResult{}, fmt.Errorf("store: beginning read transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var head int64
	var epochStr string
	if err := tx.QueryRowContext(ctx,
		`SELECT head_seq, epoch FROM meta WHERE user_id = ?`, s.userID).Scan(&head, &epochStr); err != nil {
		return ChangesResult{}, fmt.Errorf("store: reading hub state: %w", err)
	}
	epoch, err := proto.ParseDecPositive(epochStr)
	if err != nil {
		return ChangesResult{}, fmt.Errorf("store: stored epoch is invalid: %w", err)
	}

	// One extra row tells us whether another page follows.
	rows, err := tx.QueryContext(ctx, `
		SELECT seq, server_ts, body, operation_sha256
		FROM ops
		WHERE user_id = ? AND seq > ?
		ORDER BY seq
		LIMIT ?`, s.userID, sinceInt, limit+1)
	if err != nil {
		return ChangesResult{}, fmt.Errorf("store: reading changes: %w", err)
	}
	defer func() { _ = rows.Close() }()

	res := ChangesResult{HeadSeq: proto.Dec(head), Epoch: epoch}
	var bytesSoFar int

	for rows.Next() {
		var seq, serverTS int64
		var body, digest string
		if err := rows.Scan(&seq, &serverTS, &body, &digest); err != nil {
			return ChangesResult{}, err
		}

		if len(res.Ops) == limit {
			res.More = true
			break
		}
		// Truncate only at an op boundary, and never to an empty page: a partial
		// op would break the digest, and an empty page would stall the cursor.
		if len(res.Ops) > 0 && bytesSoFar+len(body) > maxBytes {
			res.More = true
			break
		}

		seqDec, err := proto.DecFromInt64(seq)
		if err != nil {
			return ChangesResult{}, err
		}
		tsDec, err := proto.DecFromInt64(serverTS)
		if err != nil {
			return ChangesResult{}, err
		}

		res.Ops = append(res.Ops, ChangeOp{
			Seq:        seqDec,
			ServerTS:   tsDec,
			RawLiteral: []byte(body),
			Digest:     digest,
		})
		bytesSoFar += len(body)
	}
	if err := rows.Err(); err != nil {
		return ChangesResult{}, err
	}
	return res, nil
}

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/andrhamm/claude-mem-lan-sync/internal/proto"
)

// PushResult is the response to POST /v1/sync/ops.
//
// ProjectedSeq always equals HeadSeq. The client enforces opposite inequalities
// on the two routes — /status rejects projected > head, /ops rejects head >
// projected — so equality is the only value that satisfies both.
type PushResult struct {
	Acks         []proto.Ack
	HeadSeq      proto.Dec
	ProjectedSeq proto.Dec
}

// Push appends ops to the log and acknowledges every one of them.
//
// The acknowledgement rules are unforgiving, and each one wedges the client if
// broken:
//   - exactly one ack per received op, counted per op rather than per entity
//   - the ack echoes the digest the client sent, never one read from storage
//   - a duplicate appearing twice in one push is acked twice with the same seq
//   - a duplicate of an already-stored op consumes no sequence number
func (s *Store) Push(ctx context.Context, ops []proto.Op, deviceID, deviceName string) (PushResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// A client hanging up must not abort the sequencer: modernc implements
	// neither driver.Validator nor driver.SessionResetter, so a cancelled
	// transaction discards the connection. Both outcomes would be safe for the
	// log, but the churn is pointless.
	ctx = context.WithoutCancel(ctx)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	revoked, err := s.deviceRevoked(ctx, deviceID)
	if err != nil {
		return PushResult{}, err
	}
	if revoked {
		return PushResult{}, proto.Reject(proto.ReasonUnauthorized)
	}

	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return PushResult{}, fmt.Errorf("store: beginning push transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var head int64
	if err := tx.QueryRowContext(ctx,
		`SELECT head_seq FROM meta WHERE user_id = ?`, s.userID).Scan(&head); err != nil {
		return PushResult{}, fmt.Errorf("store: reading head_seq: %w", err)
	}

	acks := make([]proto.Ack, 0, len(ops))
	// Tracks entities seen in this batch so a repeat within one push is acked
	// with the same sequence rather than consuming another.
	batch := make(map[string]int64, len(ops))
	now := s.now().UnixMilli()

	for _, op := range ops {
		key := op.ID + "\x00" + op.EntityRev.String()

		seq, seen := batch[key]
		if !seen {
			stored, found, err := existingSeq(ctx, tx, s.userID, op.ID, op.EntityRev.String())
			if err != nil {
				return PushResult{}, err
			}
			switch {
			case found:
				seq = stored
			default:
				if head == math.MaxInt64 {
					return PushResult{}, errors.New("store: sequence space exhausted")
				}
				head++
				seq = head
				if _, err := tx.ExecContext(ctx, `
					INSERT INTO ops (user_id, seq, entity_id, entity_rev, kind,
					                 origin_device_id, operation_sha256, body, server_ts)
					VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
					s.userID, seq, op.ID, op.EntityRev.String(), op.Kind,
					op.OriginDeviceID, op.Digest, string(op.RawLiteral), now,
				); err != nil {
					return PushResult{}, fmt.Errorf("store: inserting op: %w", err)
				}
			}
			batch[key] = seq
		}

		// First-write-wins: if an entity revision already exists with a different
		// body, the stored one stands. There is no conforming response to this —
		// acking both with the stored sequence trips the client's
		// distinct-tuples-share-a-sequence check, and rejecting the push would
		// park it at the head of the outbox forever — so it is logged loudly.
		if err := s.warnOnDigestConflict(ctx, tx, op, seq); err != nil {
			return PushResult{}, err
		}

		seqDec, err := proto.DecFromInt64(seq)
		if err != nil {
			return PushResult{}, err
		}
		acks = append(acks, proto.Ack{
			ID:            op.ID,
			Kind:          op.Kind,
			EntityRev:     op.EntityRev,
			Digest:        op.Digest, // the client's digest, never the stored one
			OriginLocalID: op.OriginLocalID,
			Seq:           seqDec,
		})
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE meta SET head_seq = ? WHERE user_id = ?`, head, s.userID); err != nil {
		return PushResult{}, fmt.Errorf("store: updating head_seq: %w", err)
	}

	if err := s.upsertDevice(ctx, tx, deviceID, deviceName, now); err != nil {
		return PushResult{}, err
	}

	if s.txHook != nil {
		if err := s.txHook(tx); err != nil {
			return PushResult{}, err
		}
	}

	if err := tx.Commit(); err != nil {
		return PushResult{}, fmt.Errorf("store: committing push: %w", err)
	}

	headDec, err := proto.DecFromInt64(head)
	if err != nil {
		return PushResult{}, err
	}
	return PushResult{Acks: acks, HeadSeq: headDec, ProjectedSeq: headDec}, nil
}

func existingSeq(ctx context.Context, tx *sql.Tx, userID, entityID, rev string) (int64, bool, error) {
	var seq int64
	err := tx.QueryRowContext(ctx, `
		SELECT seq FROM ops
		WHERE user_id = ? AND entity_id = ? AND entity_rev = ?`,
		userID, entityID, rev).Scan(&seq)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, false, nil
	case err != nil:
		return 0, false, fmt.Errorf("store: looking up an existing revision: %w", err)
	}
	return seq, true, nil
}

func (s *Store) warnOnDigestConflict(ctx context.Context, tx *sql.Tx, op proto.Op, seq int64) error {
	var stored string
	err := tx.QueryRowContext(ctx,
		`SELECT operation_sha256 FROM ops WHERE user_id = ? AND seq = ?`, s.userID, seq).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("store: reading stored digest: %w", err)
	}
	if stored != op.Digest {
		s.log.Warn("two different bodies claim the same entity revision; keeping the stored one",
			"entity_id", op.ID, "entity_rev", op.EntityRev.String(), "seq", seq)
	}
	return nil
}

// HeadSeq reports the highest assigned sequence number.
func (s *Store) HeadSeq(ctx context.Context) (proto.Dec, error) {
	var head int64
	if err := s.r.QueryRowContext(ctx,
		`SELECT head_seq FROM meta WHERE user_id = ?`, s.userID).Scan(&head); err != nil {
		return 0, fmt.Errorf("store: reading head_seq: %w", err)
	}
	return proto.DecFromInt64(head)
}

func (s *Store) now() time.Time {
	if s.clock != nil {
		return s.clock()
	}
	return time.Now()
}

// SetClock overrides the timestamp source. Tests use it so server_ts is
// deterministic in golden comparisons.
func (s *Store) SetClock(f func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clock = f
}

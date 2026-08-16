package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/andrhamm/claude-mem-lan-sync/internal/proto"
	"pgregory.net/rapid"
)

// The log's contract in one place: sequences run 1..head with no gaps, and the
// head counter always equals the log's maximum. If either breaks, every client
// applying with requireContiguous stalls forever on the same page.
func assertLogInvariants(t *rapid.T, s *Store) {
	t.Helper()
	ctx := context.Background()

	head, err := s.HeadSeq(ctx)
	if err != nil {
		t.Fatalf("HeadSeq: %v", err)
	}

	var maxSeq, count int64
	if err := s.r.QueryRow(
		`SELECT COALESCE(MAX(seq), 0), COUNT(*) FROM ops WHERE user_id = ?`, s.UserID()).
		Scan(&maxSeq, &count); err != nil {
		t.Fatalf("querying log: %v", err)
	}

	if int64(head) != maxSeq {
		t.Fatalf("head_seq %d != MAX(seq) %d", head, maxSeq)
	}
	if count != maxSeq {
		t.Fatalf("log holds %d rows but the highest sequence is %d — there is a gap", count, maxSeq)
	}

	// Walk the log and confirm each sequence is exactly the previous plus one.
	rows, err := s.r.Query(`SELECT seq FROM ops WHERE user_id = ? ORDER BY seq`, s.UserID())
	if err != nil {
		t.Fatalf("scanning log: %v", err)
	}
	defer func() { _ = rows.Close() }()

	var expected int64 = 1
	for rows.Next() {
		var seq int64
		if err := rows.Scan(&seq); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if seq != expected {
			t.Fatalf("sequence %d where %d was expected", seq, expected)
		}
		expected++
	}
}

// Deterministic model test: generated push sequences with duplicates, repeats,
// and injected rollbacks, applied serially so rapid can shrink a failure.
// Concurrency is exercised separately in the race stress test, because rapid
// shrinks by deterministic replay and a scheduling-dependent failure would not
// reproduce.
func TestSequencerModel(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		dir, err := os.MkdirTemp("", "cmemlan-model-*")
		if err != nil {
			rt.Fatalf("temp dir: %v", err)
		}
		defer func() { _ = os.RemoveAll(dir) }()

		s, err := Open(dir+"/hub.db", nil)
		if err != nil {
			rt.Fatalf("Open: %v", err)
		}
		defer func() { _ = s.Close() }()

		ctx := context.Background()
		injected := errors.New("injected rollback")

		steps := rapid.IntRange(1, 12).Draw(rt, "steps")
		for i := 0; i < steps; i++ {
			opCount := rapid.IntRange(1, 3).Draw(rt, "op_count")
			fail := rapid.Bool().Draw(rt, "fail")

			ops := make([]proto.Op, 0, opCount)
			for j := 0; j < opCount; j++ {
				// A small id space guarantees duplicates within and across pushes.
				id := rapid.IntRange(1, 4).Draw(rt, "entity")
				rev := rapid.IntRange(1, 2).Draw(rt, "rev")
				ops = append(ops, makeOpForModel(rt, id, rev))
			}

			if fail {
				s.SetTxHook(func(*sql.Tx) error { return injected })
			}
			res, err := s.Push(ctx, ops, "device-A", "A")
			s.SetTxHook(nil)

			switch {
			case fail:
				if !errors.Is(err, injected) {
					rt.Fatalf("expected the injected rollback, got %v", err)
				}
			case err != nil:
				rt.Fatalf("Push: %v", err)
			default:
				if len(res.Acks) != len(ops) {
					rt.Fatalf("acked %d of %d ops — every received op must be acked",
						len(res.Acks), len(ops))
				}
				if res.HeadSeq != res.ProjectedSeq {
					rt.Fatalf("head %d != projected %d", res.HeadSeq, res.ProjectedSeq)
				}
				// Identical tuples in one response must carry identical sequences.
				seqByTuple := map[string]proto.Dec{}
				for _, a := range res.Acks {
					key := a.ID + "|" + a.EntityRev.String() + "|" + a.Digest
					if prev, ok := seqByTuple[key]; ok && prev != a.Seq {
						rt.Fatalf("tuple %s acked with sequences %d and %d", key, prev, a.Seq)
					}
					seqByTuple[key] = a.Seq
					if a.Seq > res.HeadSeq {
						rt.Fatalf("acked seq %d exceeds head %d", a.Seq, res.HeadSeq)
					}
				}
			}

			assertLogInvariants(rt, s)
		}
	})
}

func makeOpForModel(t *rapid.T, id, rev int) proto.Op {
	t.Helper()
	payload := `{"p":1}`
	body := fmt.Sprintf(
		`{"body_schema_version":1,"deleted":false,"deleted_at":null,"entity_rev":"%d",`+
			`"id":"observation:%d","kind":"observation","mutation":null,"origin_device_id":"device-A",`+
			`"origin_local_id":"%d","payload":%s,"payload_schema_version":2,"payload_sha256":"%s"}`,
		rev, id, id, payload, proto.Digest([]byte(payload)))

	wrapper := fmt.Sprintf(`{"body":%s,"operation_sha256":%q}`, quoteJSON(body), proto.Digest([]byte(body)))
	op, err := proto.ParseOp([]byte(wrapper))
	if err != nil {
		t.Fatalf("building op: %v", err)
	}
	return op
}

func quoteJSON(s string) string {
	b, err := jsonMarshalString(s)
	if err != nil {
		panic(err)
	}
	return b
}

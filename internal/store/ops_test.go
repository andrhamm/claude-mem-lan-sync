package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/andrhamm/claude-mem-lan-sync/internal/proto"
)

// makeOp builds a valid operation with the canonical twelve-key body.
func makeOp(t *testing.T, kind, id, rev, deviceID, localID, payload string) proto.Op {
	t.Helper()
	local := "null"
	if localID != "" {
		local = `"` + localID + `"`
	}
	mutation := "null"
	if kind == "mutation" {
		mutation = `{"op":"set_title","title":"x"}`
	}
	body := fmt.Sprintf(
		`{"body_schema_version":1,"deleted":false,"deleted_at":null,"entity_rev":"%s",`+
			`"id":"%s","kind":"%s","mutation":%s,"origin_device_id":"%s",`+
			`"origin_local_id":%s,"payload":%s,"payload_schema_version":2,"payload_sha256":"%s"}`,
		rev, id, kind, mutation, deviceID, local, payload, proto.Digest([]byte(payload)))

	wrapper, err := json.Marshal(map[string]string{
		"body":             body,
		"operation_sha256": proto.Digest([]byte(body)),
	})
	if err != nil {
		t.Fatal(err)
	}
	op, err := proto.ParseOp(wrapper)
	if err != nil {
		t.Fatalf("building test op: %v", err)
	}
	return op
}

func obs(t *testing.T, localID, rev string) proto.Op {
	t.Helper()
	return makeOp(t, "observation", "observation:"+localID, rev, "device-A", localID, `{"p":1}`)
}

func TestPushAssignsSequencesFromOne(t *testing.T) {
	s, _ := tempStore(t)
	ctx := context.Background()

	res, err := s.Push(ctx, []proto.Op{obs(t, "1", "1"), obs(t, "2", "1")}, "device-A", "A")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Acks) != 2 {
		t.Fatalf("got %d acks, want 2", len(res.Acks))
	}
	if res.Acks[0].Seq != 1 || res.Acks[1].Seq != 2 {
		t.Fatalf("sequences = %d, %d; the first op must be seq 1", res.Acks[0].Seq, res.Acks[1].Seq)
	}
	if res.HeadSeq != 2 {
		t.Fatalf("HeadSeq = %d", res.HeadSeq)
	}
	// /status rejects projected > head and /ops rejects head > projected, so
	// equality is the only value that satisfies both routes.
	if res.ProjectedSeq != res.HeadSeq {
		t.Fatalf("ProjectedSeq %d != HeadSeq %d", res.ProjectedSeq, res.HeadSeq)
	}
}

func TestPushIsContiguousAcrossCalls(t *testing.T) {
	s, _ := tempStore(t)
	ctx := context.Background()

	var seqs []proto.Dec
	for i := 1; i <= 5; i++ {
		res, err := s.Push(ctx, []proto.Op{obs(t, fmt.Sprint(i), "1")}, "device-A", "A")
		if err != nil {
			t.Fatal(err)
		}
		seqs = append(seqs, res.Acks[0].Seq)
	}
	for i, got := range seqs {
		if want := proto.Dec(i + 1); got != want {
			t.Fatalf("sequence %d = %d, want %d — a gap wedges every client", i, got, want)
		}
	}
}

// A retry after a dropped response must be free: the duplicate consumes no
// sequence and is acked with the original one.
func TestDuplicateAcrossPushesReusesSequence(t *testing.T) {
	s, _ := tempStore(t)
	ctx := context.Background()
	op := obs(t, "1", "1")

	first, err := s.Push(ctx, []proto.Op{op}, "device-A", "A")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Push(ctx, []proto.Op{op}, "device-A", "A")
	if err != nil {
		t.Fatal(err)
	}

	if second.Acks[0].Seq != first.Acks[0].Seq {
		t.Fatalf("re-ack seq = %d, want the original %d", second.Acks[0].Seq, first.Acks[0].Seq)
	}
	if second.HeadSeq != first.HeadSeq {
		t.Fatalf("head advanced on a duplicate: %d -> %d", first.HeadSeq, second.HeadSeq)
	}

	var count int
	if err := s.r.QueryRow(`SELECT COUNT(*) FROM ops WHERE user_id = ?`, s.UserID()).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("stored %d rows for one entity revision", count)
	}
}

// The client counts acknowledgements per op received, not per unique entity.
// Collapsing these two into one ack is a multiplicity mismatch that throws.
func TestSameOpTwiceInOnePushIsAckedTwiceWithOneSequence(t *testing.T) {
	s, _ := tempStore(t)
	op := obs(t, "1", "1")

	res, err := s.Push(context.Background(), []proto.Op{op, op}, "device-A", "A")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Acks) != 2 {
		t.Fatalf("got %d acks for 2 ops — every received op must be acked", len(res.Acks))
	}
	if res.Acks[0].Seq != res.Acks[1].Seq {
		t.Fatalf("same op acked with different sequences: %d and %d", res.Acks[0].Seq, res.Acks[1].Seq)
	}
	if res.HeadSeq != 1 {
		t.Fatalf("HeadSeq = %d, want 1", res.HeadSeq)
	}
}

// The ack must echo the digest the client sent. Echoing a stored digest makes
// the tuple miss the client's map and reads as an extra acknowledgement.
func TestAckEchoesClientDigestAndAllFields(t *testing.T) {
	s, _ := tempStore(t)
	op := obs(t, "42", "3")

	res, err := s.Push(context.Background(), []proto.Op{op}, "device-A", "A")
	if err != nil {
		t.Fatal(err)
	}
	a := res.Acks[0]
	if a.Digest != op.Digest {
		t.Errorf("ack digest = %q, want the client's %q", a.Digest, op.Digest)
	}
	if a.ID != op.ID || a.Kind != op.Kind || a.EntityRev != op.EntityRev {
		t.Errorf("ack tuple does not match the pushed op: %+v", a)
	}
	if a.OriginLocalID == nil || *a.OriginLocalID != 42 {
		t.Errorf("ack origin_local_id = %v, want 42", a.OriginLocalID)
	}
	if a.Seq == 0 {
		t.Error("ack seq must be positive")
	}
}

// Within one response, no two distinct tuples may share a sequence and no tuple
// may carry two sequences.
func TestAckSetIsInternallyConsistent(t *testing.T) {
	s, _ := tempStore(t)
	ops := []proto.Op{obs(t, "1", "1"), obs(t, "2", "1"), obs(t, "1", "2"), obs(t, "2", "1")}

	res, err := s.Push(context.Background(), ops, "device-A", "A")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Acks) != len(ops) {
		t.Fatalf("got %d acks for %d ops", len(res.Acks), len(ops))
	}

	seqByTuple := map[string]proto.Dec{}
	tupleBySeq := map[proto.Dec]string{}
	for _, a := range res.Acks {
		tuple := fmt.Sprintf("%s|%s|%s|%s", a.ID, a.Kind, a.EntityRev, a.Digest)
		if prev, ok := seqByTuple[tuple]; ok && prev != a.Seq {
			t.Fatalf("tuple %s acked with two sequences: %d and %d", tuple, prev, a.Seq)
		}
		seqByTuple[tuple] = a.Seq
		if prev, ok := tupleBySeq[a.Seq]; ok && prev != tuple {
			t.Fatalf("sequence %d claimed by two tuples: %s and %s", a.Seq, prev, tuple)
		}
		tupleBySeq[a.Seq] = tuple
	}
}

// Mutation ops carry a null origin_local_id and must round-trip through storage.
func TestPushAcceptsMutationOps(t *testing.T) {
	s, _ := tempStore(t)
	op := makeOp(t, "mutation", "mutation:0b7f3e2a-1c4d-4a6b-8f2e-9d0c1a2b3c4d", "1", "device-A", "", "null")

	res, err := s.Push(context.Background(), []proto.Op{op}, "device-A", "A")
	if err != nil {
		t.Fatalf("mutation op rejected: %v", err)
	}
	if res.Acks[0].OriginLocalID != nil {
		t.Error("mutation ack must carry a null origin_local_id")
	}
}

// A failed transaction must consume no sequence number, or the log develops a
// gap and every client stalls permanently.
func TestRollbackConsumesNoSequence(t *testing.T) {
	s, _ := tempStore(t)
	ctx := context.Background()

	if _, err := s.Push(ctx, []proto.Op{obs(t, "1", "1")}, "device-A", "A"); err != nil {
		t.Fatal(err)
	}

	boom := errors.New("injected failure")
	s.SetTxHook(func(*sql.Tx) error { return boom })
	if _, err := s.Push(ctx, []proto.Op{obs(t, "2", "1")}, "device-A", "A"); !errors.Is(err, boom) {
		t.Fatalf("expected the injected failure, got %v", err)
	}
	s.SetTxHook(nil)

	res, err := s.Push(ctx, []proto.Op{obs(t, "3", "1")}, "device-A", "A")
	if err != nil {
		t.Fatal(err)
	}
	if res.Acks[0].Seq != 2 {
		t.Fatalf("after a rollback the next sequence is %d, want 2 — the log has a gap", res.Acks[0].Seq)
	}

	head, err := s.HeadSeq(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var maxSeq int64
	if err := s.r.QueryRow(`SELECT COALESCE(MAX(seq),0) FROM ops WHERE user_id = ?`, s.UserID()).Scan(&maxSeq); err != nil {
		t.Fatal(err)
	}
	if int64(head) != maxSeq {
		t.Fatalf("head_seq %d disagrees with MAX(seq) %d", head, maxSeq)
	}
}

func TestPushRecordsDevice(t *testing.T) {
	s, _ := tempStore(t)
	ctx := context.Background()

	if _, err := s.Push(ctx, []proto.Op{obs(t, "1", "1")}, "device-A", "laptop"); err != nil {
		t.Fatal(err)
	}
	devices, err := s.ListDevices(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 1 || devices[0].ID != "device-A" || devices[0].Name != "laptop" {
		t.Fatalf("devices = %+v", devices)
	}
}

// Revocation must actually deny access, not merely hide the device from a list.
func TestRevokedDeviceCannotPush(t *testing.T) {
	s, _ := tempStore(t)
	ctx := context.Background()

	if _, err := s.Push(ctx, []proto.Op{obs(t, "1", "1")}, "device-A", "A"); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeDevice(ctx, "device-A"); err != nil {
		t.Fatal(err)
	}

	_, err := s.Push(ctx, []proto.Op{obs(t, "2", "1")}, "device-A", "A")
	if proto.ReasonOf(err) != proto.ReasonUnauthorized {
		t.Fatalf("revoked device push returned %v, want unauthorized", err)
	}
}

func TestDeviceNameIsTruncated(t *testing.T) {
	s, _ := tempStore(t)
	ctx := context.Background()

	long := make([]byte, MaxDeviceNameLen+50)
	for i := range long {
		long[i] = 'x'
	}
	if _, err := s.Push(ctx, []proto.Op{obs(t, "1", "1")}, "device-A", string(long)); err != nil {
		t.Fatal(err)
	}
	devices, err := s.ListDevices(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(devices[0].Name) != MaxDeviceNameLen {
		t.Fatalf("device name length = %d, want %d", len(devices[0].Name), MaxDeviceNameLen)
	}
}

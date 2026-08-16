package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/andrhamm/claude-mem-lan-sync/internal/proto"
)

func jsonMarshalString(s string) (string, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// Concurrency lives here rather than in the rapid model: rapid shrinks by
// deterministic replay, so a scheduling-dependent failure would not reproduce
// and a flake would be indistinguishable from a real bug.
//
// Run with -race.
func TestConcurrentPushesKeepLogContiguous(t *testing.T) {
	s, _ := tempStore(t)
	ctx := context.Background()

	const (
		devices        = 6
		pushesPerDev   = 15
		opsPerPush     = 2
		expectedUnique = devices * pushesPerDev * opsPerPush
	)

	var wg sync.WaitGroup
	for d := 0; d < devices; d++ {
		wg.Add(1)
		go func(device int) {
			defer wg.Done()
			deviceID := fmt.Sprintf("device-%d", device)
			for p := 0; p < pushesPerDev; p++ {
				ops := make([]proto.Op, 0, opsPerPush)
				for o := 0; o < opsPerPush; o++ {
					ops = append(ops, makeOp(t,
						"observation",
						fmt.Sprintf("observation:%d-%d-%d", device, p, o),
						"1", deviceID, fmt.Sprint(device*1000+p*10+o), `{"p":1}`))
				}
				if _, err := s.Push(ctx, ops, deviceID, deviceID); err != nil {
					t.Errorf("push from %s: %v", deviceID, err)
					return
				}
			}
		}(d)
	}
	wg.Wait()

	head, err := s.HeadSeq(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if head != expectedUnique {
		t.Fatalf("head_seq = %d, want %d", head, expectedUnique)
	}

	rows, err := s.r.Query(`SELECT seq FROM ops WHERE user_id = ? ORDER BY seq`, s.UserID())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()

	var expected int64 = 1
	for rows.Next() {
		var seq int64
		if err := rows.Scan(&seq); err != nil {
			t.Fatal(err)
		}
		if seq != expected {
			t.Fatalf("gap in the log: found %d where %d was expected", seq, expected)
		}
		expected++
	}
	if expected-1 != int64(head) {
		t.Fatalf("walked %d sequences but head_seq is %d", expected-1, head)
	}
}

// Concurrent duplicate pushes of the same entity must converge on one sequence.
func TestConcurrentDuplicatesConsumeOneSequence(t *testing.T) {
	s, _ := tempStore(t)
	ctx := context.Background()
	op := obs(t, "1", "1")

	var wg sync.WaitGroup
	seqs := make([]proto.Dec, 8)
	for i := range seqs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := s.Push(ctx, []proto.Op{op}, "device-A", "A")
			if err != nil {
				t.Errorf("push: %v", err)
				return
			}
			seqs[i] = res.Acks[0].Seq
		}(i)
	}
	wg.Wait()

	for i, got := range seqs {
		if got != 1 {
			t.Fatalf("push %d got sequence %d, want 1 for the same entity revision", i, got)
		}
	}
	head, err := s.HeadSeq(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if head != 1 {
		t.Fatalf("head_seq = %d after concurrent duplicates, want 1", head)
	}
}

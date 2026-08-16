package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/andrhamm/claude-mem-lan-sync/internal/proto"
)

func seedOps(t *testing.T, s *Store, n int) {
	t.Helper()
	ctx := context.Background()
	for i := 1; i <= n; i++ {
		if _, err := s.Push(ctx, []proto.Op{obs(t, fmt.Sprint(i), "1")}, "device-A", "A"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestChangesReturnsOpsAfterCursor(t *testing.T) {
	s, _ := tempStore(t)
	seedOps(t, s, 5)

	res, err := s.Changes(context.Background(), 2, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Ops) != 3 {
		t.Fatalf("got %d ops after cursor 2, want 3", len(res.Ops))
	}
	for i, op := range res.Ops {
		if want := proto.Dec(i + 3); op.Seq != want {
			t.Fatalf("op %d has seq %d, want %d", i, op.Seq, want)
		}
	}
	if res.More {
		t.Error("More set when the page covered everything")
	}
}

// The client applies with requireContiguous anchored to its cursor, so the first
// op of every page must be exactly cursor+1.
func TestChangesIsContiguousFromCursor(t *testing.T) {
	s, _ := tempStore(t)
	seedOps(t, s, 10)

	cursor := proto.Dec(0)
	var seen []proto.Dec
	for {
		res, err := s.Changes(context.Background(), cursor, 3, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Ops) == 0 {
			break
		}
		if res.Ops[0].Seq != cursor+1 {
			t.Fatalf("page starts at %d, want cursor+1 = %d", res.Ops[0].Seq, cursor+1)
		}
		for _, op := range res.Ops {
			if len(seen) > 0 && op.Seq != seen[len(seen)-1]+1 {
				t.Fatalf("gap: %d follows %d", op.Seq, seen[len(seen)-1])
			}
			seen = append(seen, op.Seq)
		}
		cursor = res.Ops[len(res.Ops)-1].Seq
		if !res.More {
			break
		}
	}
	if len(seen) != 10 {
		t.Fatalf("paged %d ops, want 10", len(seen))
	}
}

// Pagination while another device is pushing is the exact scenario that wedges
// a client forever, and it is the least-tested constraint in the design.
func TestChangesContiguousDuringConcurrentPush(t *testing.T) {
	s, _ := tempStore(t)
	ctx := context.Background()
	seedOps(t, s, 6)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 100; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := s.Push(ctx, []proto.Op{obs(t, fmt.Sprint(i), "1")}, "device-B", "B"); err != nil {
				return
			}
		}
	}()

	cursor := proto.Dec(0)
	var last proto.Dec
	for page := 0; page < 6; page++ {
		res, err := s.Changes(ctx, cursor, 3, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, op := range res.Ops {
			if last != 0 && op.Seq != last+1 {
				t.Fatalf("sequence gap across pages during concurrent pushes: %d then %d", last, op.Seq)
			}
			last = op.Seq
		}
		if len(res.Ops) > 0 {
			cursor = res.Ops[len(res.Ops)-1].Seq
		}
		// head_seq must never be behind the page it accompanied.
		if res.HeadSeq < last {
			t.Fatalf("head_seq %d is behind the last returned seq %d", res.HeadSeq, last)
		}
	}
	close(stop)
	wg.Wait()
}

// Not echoing a device its own ops looks like an optimisation and creates gaps
// that wedge that device permanently. The client filters them itself.
func TestChangesDoesNotFilterByDevice(t *testing.T) {
	s, _ := tempStore(t)
	ctx := context.Background()

	if _, err := s.Push(ctx, []proto.Op{obs(t, "1", "1")}, "device-A", "A"); err != nil {
		t.Fatal(err)
	}
	res, err := s.Changes(ctx, 0, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Ops) != 1 {
		t.Fatal("the pushing device's own op must still come back to it")
	}
}

func TestChangesMoreFlagAndLimit(t *testing.T) {
	s, _ := tempStore(t)
	seedOps(t, s, 5)

	res, err := s.Changes(context.Background(), 0, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Ops) != 2 || !res.More {
		t.Fatalf("got %d ops, more=%v; want 2 and more=true", len(res.Ops), res.More)
	}

	// A limit above the protocol maximum is clamped, not rejected.
	res, err = s.Changes(context.Background(), 0, 100000, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Ops) != 5 {
		t.Fatalf("got %d ops with an oversized limit", len(res.Ops))
	}
}

// A byte cap must never split an op: a partial body fails its digest, and an
// empty page would stall the cursor forever.
func TestChangesByteCapTruncatesAtOpBoundary(t *testing.T) {
	s, _ := tempStore(t)
	seedOps(t, s, 4)

	res, err := s.Changes(context.Background(), 0, 100, 10) // absurdly small cap
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Ops) != 1 {
		t.Fatalf("got %d ops under a tiny byte cap, want exactly 1", len(res.Ops))
	}
	if !res.More {
		t.Error("More must be set when the cap truncated the page")
	}
	// The single op must be whole.
	var literal string
	if err := json.Unmarshal(res.Ops[0].RawLiteral, &literal); err != nil {
		t.Fatalf("truncated op body is not a valid JSON string: %v", err)
	}
	if proto.Digest([]byte(literal)) != res.Ops[0].Digest {
		t.Fatal("op body does not match its digest — the page was split mid-op")
	}
}

func TestChangesReportsEpochAndHead(t *testing.T) {
	s, _ := tempStore(t)
	seedOps(t, s, 3)

	res, err := s.Changes(context.Background(), 0, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Epoch != s.Epoch() {
		t.Errorf("Epoch = %d, want %d", res.Epoch, s.Epoch())
	}
	if res.HeadSeq != 3 {
		t.Errorf("HeadSeq = %d, want 3", res.HeadSeq)
	}
}

func TestChangesEmptyPastHead(t *testing.T) {
	s, _ := tempStore(t)
	seedOps(t, s, 2)

	res, err := s.Changes(context.Background(), 99, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Ops) != 0 || res.More {
		t.Fatalf("cursor beyond head returned %d ops, more=%v", len(res.Ops), res.More)
	}
}

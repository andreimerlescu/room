package room

import (
	"sync"
	"testing"
	"time"
)

// TestSetTicketRank_OrdersLineByRank ranks clients as they would arrive:
// higher ranks ahead, arrival order within a rank, guests behind, and
// every position exact (no ties).
func TestSetTicketRank_OrdersLineByRank(t *testing.T) {
	wr := newTestWR(t, 1)
	events := collectEvents(wr, EventRank)
	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	r := newTestRouter(wr, serving, release)
	defer close(release)

	fillOneSlot(t, r, serving)
	toks := queueTokens(t, r, 5)
	g1, g2, p1, m, p2 := toks[0], toks[1], toks[2], toks[3], toks[4]
	depth := wr.QueueDepth()

	for _, step := range []struct {
		tok  string
		rank int
	}{{p1, 2}, {m, 1}, {p2, 2}} {
		moved, err := wr.SetTicketRank(step.tok, step.rank)
		if err != nil || !moved {
			t.Fatalf("rank %d: moved=%v err=%v", step.rank, moved, err)
		}
		if s := nextSnapshot(t, events); s.Token != step.tok {
			t.Errorf("EventRank carried the wrong token")
		}
	}

	want := []struct {
		tok  string
		rank int
	}{{p1, 2}, {p2, 2}, {m, 1}, {g1, 0}, {g2, 0}}
	q := wr.Queue(10)
	if len(q) != len(want) {
		t.Fatalf("queue has %d tickets, want %d", len(q), len(want))
	}
	for i, w := range want {
		if q[i].Token != w.tok || q[i].Rank != w.rank || q[i].Position != int64(i+1) {
			t.Errorf("slot %d: got rank %d at position %d, want rank %d at position %d",
				i, q[i].Rank, q[i].Position, w.rank, i+1)
		}
	}
	if d := wr.QueueDepth(); d != depth {
		t.Errorf("ranking must not change QueueDepth: %d -> %d", depth, d)
	}
}

func TestSetTicketRank_Errors(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 10)
	wr.tokens.set("ready", ticketEntry{ticket: 3, issuedAt: time.Now()})

	if _, err := wr.SetTicketRank("ready", -1); err == nil {
		t.Error("expected ErrInvalidRank")
	} else if _, ok := err.(ErrInvalidRank); !ok {
		t.Errorf("expected ErrInvalidRank, got %T", err)
	}
	if _, err := wr.SetTicketRank("missing", 1); err == nil {
		t.Error("expected ErrTokenNotFound")
	} else if _, ok := err.(ErrTokenNotFound); !ok {
		t.Errorf("expected ErrTokenNotFound, got %T", err)
	}
	if moved, err := wr.SetTicketRank("ready", 2); moved {
		t.Error("an in-window ticket must not move")
	} else if _, ok := err.(ErrAlreadyAdmitted); !ok {
		t.Errorf("expected ErrAlreadyAdmitted, got %T", err)
	}
	if ti, _ := wr.Ticket("ready"); ti.Rank != 2 {
		t.Errorf("the rank of an in-window ticket should still be recorded, got %d", ti.Rank)
	}

	var zero WaitingRoom
	if _, err := zero.SetTicketRank("x", 1); err == nil {
		t.Error("expected ErrNotInitialised")
	} else if _, ok := err.(ErrNotInitialised); !ok {
		t.Errorf("expected ErrNotInitialised, got %T", err)
	}
}

func TestSetTicketRank_LoweringRecordsButDoesNotMoveBack(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 1)
	now := time.Now()
	wr.nextTicket.Store(3)
	wr.tokens.set("a", ticketEntry{ticket: 2, createdAt: now, issuedAt: now})
	wr.tokens.set("b", ticketEntry{ticket: 3, createdAt: now, issuedAt: now})

	if moved, err := wr.SetTicketRank("b", 3); err != nil || !moved {
		t.Fatalf("raise: moved=%v err=%v", moved, err)
	}
	if moved, err := wr.SetTicketRank("b", 0); err != nil || moved {
		t.Fatalf("lower: moved=%v err=%v", moved, err)
	}
	ti, _ := wr.Ticket("b")
	if ti.Rank != 0 || ti.Position != 1 {
		t.Errorf("lowering should keep the place and record the rank: %+v", ti)
	}

	// With b back at rank 0, ranking a moves it ahead of b.
	if moved, err := wr.SetTicketRank("a", 1); err != nil || !moved {
		t.Fatalf("rank a: moved=%v err=%v", moved, err)
	}
	if q := wr.Queue(2); q[0].Token != "a" || q[1].Token != "b" {
		t.Errorf("order: %s, %s", q[0].Token, q[1].Token)
	}
}

// TestSetTicketRank_GhostsMoveWithTickets checks that a retired ticket
// inside the moved range keeps its place relative to its neighbours, so
// positions stay exact and nothing is skipped or admitted twice.
func TestSetTicketRank_GhostsMoveWithTickets(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 1)
	now := time.Now()
	wr.nextTicket.Store(6)
	for tok, n := range map[string]int64{"a": 2, "b": 3, "c": 5, "d": 6} {
		wr.tokens.set(tok, ticketEntry{ticket: n, createdAt: now, issuedAt: now})
	}
	wr.retire(4) // behind the window edge (1): waits in the ledger

	if moved, err := wr.SetTicketRank("d", 1); err != nil || !moved {
		t.Fatalf("moved=%v err=%v", moved, err)
	}

	want := []string{"d", "a", "b", "c"}
	q := wr.Queue(10)
	for i, tok := range want {
		if q[i].Token != tok || q[i].Position != int64(i+1) {
			t.Errorf("slot %d: %s at position %d, want %s at %d", i, q[i].Token, q[i].Position, tok, i+1)
		}
	}
	if l := wr.ledger.len(); l != 1 {
		t.Errorf("the ghost must survive the move, ledger=%d", l)
	}
	if d := wr.QueueDepth(); d != 4 {
		t.Errorf("QueueDepth: got %d, want 4", d)
	}
}

func TestSetTicketRank_SurvivesExportImport(t *testing.T) {
	a := newTestWR(t, 1)
	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	ra := newTestRouter(a, serving, release)
	defer close(release)
	fillOneSlot(t, ra, serving)
	toks := queueTokens(t, ra, 3)
	if _, err := a.SetTicketRank(toks[2], 2); err != nil {
		t.Fatal(err)
	}

	b := newTestWR(t, 1)
	if _, err := b.Import(exportRoom(t, a)); err != nil {
		t.Fatal(err)
	}
	q := b.Queue(10)
	if len(q) != 3 || q[0].Token != toks[2] || q[0].Rank != 2 || q[1].Rank != 0 {
		t.Errorf("restored line lost its ranks: %+v", q)
	}
}

// TestSetTicketRank_ConcurrentKeepsNumbersExact ranks a line while its
// clients poll. Afterwards every waiting number is held exactly once, and
// the line is ordered by rank.
func TestSetTicketRank_ConcurrentKeepsNumbersExact(t *testing.T) {
	wr := newTestWR(t, 1)
	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	r := newTestRouter(wr, serving, release)
	defer close(release)
	fillOneSlot(t, r, serving)
	toks := queueTokens(t, r, 60)

	var wg sync.WaitGroup
	for i, tok := range toks {
		wg.Add(2)
		go func(i int, tok string) {
			defer wg.Done()
			_, _ = wr.SetTicketRank(tok, i%4)
		}(i, tok)
		go func(tok string) {
			defer wg.Done()
			pollStatus(r, tok)
		}(tok)
	}
	wg.Wait()

	held := map[int64]int{}
	q := wr.Queue(1000)
	for _, ti := range q {
		held[ti.Ticket]++
	}
	wr.ledger.mu.Lock()
	for _, g := range wr.ledger.tickets {
		held[g]++
	}
	wr.ledger.mu.Unlock()
	edge := wr.nowServing.Load() + int64(wr.Cap())
	for n := edge + 1; n <= wr.nextTicket.Load(); n++ {
		if held[n] != 1 {
			t.Errorf("number %d is held %d times, want exactly once", n, held[n])
		}
	}
	for i := 1; i < len(q); i++ {
		if q[i].Rank > q[i-1].Rank {
			t.Errorf("slot %d (rank %d) waits behind slot %d (rank %d)", i, q[i].Rank, i-1, q[i-1].Rank)
		}
	}
}

package room

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// waitForNowServing spins until nowServing reaches want or deadline passes.
func waitForNowServing(t *testing.T, wr *WaitingRoom, want int64, deadline time.Duration) {
	t.Helper()
	timeout := time.After(deadline)
	for wr.nowServing.Load() < want {
		select {
		case <-timeout:
			t.Fatalf("timed out: nowServing=%d, want %d", wr.nowServing.Load(), want)
		default:
			time.Sleep(2 * time.Millisecond)
		}
	}
}

// ── ghostLedger unit tests ───────────────────────────────────────────────────

func TestGhostLedger_AddCountConsume(t *testing.T) {
	t.Parallel()
	l := newGhostLedger()

	if l.len() != 0 || l.minTicket() != math.MaxInt64 {
		t.Fatalf("empty ledger: len=%d min=%d", l.len(), l.minTicket())
	}

	for _, v := range []int64{5, 3, 9, 3} {
		l.add(v)
	}
	if l.len() != 4 {
		t.Fatalf("expected len 4, got %d", l.len())
	}
	if l.minTicket() != 3 {
		t.Fatalf("expected min 3, got %d", l.minTicket())
	}
	if got := l.countBelow(4); got != 2 {
		t.Errorf("countBelow(4) = %d, want 2", got)
	}
	if got := l.countBelow(3); got != 0 {
		t.Errorf("countBelow(3) = %d, want 0", got)
	}
	if got := l.countBelow(100); got != 4 {
		t.Errorf("countBelow(100) = %d, want 4", got)
	}
	if got := l.consumeUpTo(3); got != 2 {
		t.Errorf("consumeUpTo(3) = %d, want 2 (duplicates both consumed)", got)
	}
	if l.minTicket() != 5 {
		t.Errorf("expected min 5 after consume, got %d", l.minTicket())
	}
	if got := l.consumeUpTo(4); got != 0 {
		t.Errorf("consumeUpTo(4) = %d, want 0", got)
	}
	if got := l.consumeUpTo(100); got != 2 {
		t.Errorf("consumeUpTo(100) = %d, want 2", got)
	}
	if l.len() != 0 || l.minTicket() != math.MaxInt64 {
		t.Errorf("drained ledger: len=%d min=%d", l.len(), l.minTicket())
	}
}

func TestGhostLedger_AddManyMergesSorted(t *testing.T) {
	t.Parallel()
	l := newGhostLedger()
	l.add(10)
	l.add(30)
	l.addMany([]int64{40, 5, 20, 30})
	l.addMany(nil) // no-op

	want := []int64{5, 10, 20, 30, 30, 40}
	l.mu.Lock()
	got := append([]int64(nil), l.tickets...)
	l.mu.Unlock()

	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	if l.minTicket() != 5 {
		t.Errorf("expected min 5, got %d", l.minTicket())
	}
}

func TestGhostLedger_ShrinksAfterBulkConsume(t *testing.T) {
	t.Parallel()
	l := newGhostLedger()
	batch := make([]int64, 5000)
	for i := range batch {
		batch[i] = int64(1000 + i)
	}
	l.addMany(batch)
	if got := l.consumeUpTo(math.MaxInt64); got != 5000 {
		t.Fatalf("consumed %d, want 5000", got)
	}
	l.mu.Lock()
	c := cap(l.tickets)
	l.mu.Unlock()
	if c > ledgerShrinkFloor {
		t.Errorf("backing array not released after bulk consume: cap=%d", c)
	}
}

// ── Regression: the queue must never freeze ──────────────────────────────────

// TestStall_AbandonAtReadyDoesNotFreezeQueue is the regression test for the
// capacity leak. With cap=1: ticket 1 is served, ticket 2 is told ready and
// abandons before reloading, and the reaper removes it. Previously the
// in-window "window guard" skipped the nowServing advance, so the window
// stayed at ticket 2 forever: nothing was active, nothing would ever
// release, and every new arrival was queued behind a ghost.
func TestStall_AbandonAtReadyDoesNotFreezeQueue(t *testing.T) {
	wr := newTestWR(t, 1)

	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	r := newTestRouter(wr, serving, release)

	fillOneSlot(t, r, serving) // ticket 1 active

	_, tokA := serveWithCookie(r, "") // ticket 2 queued
	if tokA == "" {
		t.Fatal("expected a queue token for the second arrival")
	}

	close(release)
	waitForNowServing(t, wr, 1, 2*time.Second)

	if !pollStatus(r, tokA).Ready {
		t.Fatal("setup: ticket 2 should be ready after ticket 1 released")
	}

	// Client A abandons at the ready moment; the reaper collects it.
	ageAllTokens(wr.tokens, wr.TokenTTL()+time.Minute)
	wr.reap()
	if _, ok := wr.tokens.get(tokA); ok {
		t.Fatal("expected the abandoned token to be reaped")
	}

	// A new arrival must be admitted on the fast path (no token issued).
	w, tokB := serveWithCookie(r, "")
	if tokB != "" {
		t.Fatalf("queue frozen: new arrival was queued with nothing active "+
			"(nowServing=%d nextTicket=%d cap=%d)",
			wr.nowServing.Load(), wr.nextTicket.Load(), wr.Cap())
	}
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 from fast path, got %d", w.Code)
	}
}

// TestStall_ExpiredPollRetiresTicket covers the same leak on the
// /queue/status path: a token found expired on poll used to be deleted
// with no accounting at all.
func TestStall_ExpiredPollRetiresTicket(t *testing.T) {
	wr := newTestWR(t, 1)

	var evicts atomic.Int32
	wr.On(EventEvict, func(Snapshot) { evicts.Add(1) })

	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	r := newTestRouter(wr, serving, release)

	fillOneSlot(t, r, serving)

	_, tokA := serveWithCookie(r, "")
	if tokA == "" {
		t.Fatal("no token issued")
	}

	close(release)
	waitForNowServing(t, wr, 1, 2*time.Second)

	ageAllTokens(wr.tokens, wr.TokenTTL()+time.Minute)
	if !pollStatus(r, tokA).Ready {
		t.Fatal("expected ready=true for an expired token")
	}
	if ns := wr.nowServing.Load(); ns != 2 {
		t.Errorf("expected nowServing=2 after the expired ticket was retired, got %d", ns)
	}
	waitForCount(t, &evicts, 1, 200*time.Millisecond)

	if _, tokB := serveWithCookie(r, ""); tokB != "" {
		t.Error("queue frozen after expiry-on-poll: new arrival was queued")
	}
}

// TestAcquireFailure_RetiresClaimedTicket covers the resume path when the
// slot acquire fails. It relies on sema's AcquireWith honouring context
// cancellation, as the existing EventTimeout path already does.
func TestAcquireFailure_RetiresClaimedTicket(t *testing.T) {
	wr := newTestWR(t, 1)

	var timeouts atomic.Int32
	wr.On(EventTimeout, func(Snapshot) { timeouts.Add(1) })

	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	r := newTestRouter(wr, serving, release)
	defer close(release)

	fillOneSlot(t, r, serving) // ticket 1 holds the only slot

	// A ready ticket whose owner cannot get a slot: share ticket 1, the
	// way a promotion collision does, so it is in the window while the
	// semaphore is full.
	wr.tokens.set("stuck", ticketEntry{ticket: 1, issuedAt: time.Now()})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: "stuck"})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 on acquire failure, got %d", w.Code)
	}
	if _, ok := wr.tokens.get("stuck"); ok {
		t.Error("expected the claimed token to be gone")
	}
	if ns := wr.nowServing.Load(); ns != 1 {
		t.Errorf("expected nowServing=1 after retiring the in-window ticket, got %d", ns)
	}
	waitForCount(t, &timeouts, 1, 200*time.Millisecond)
}

// ── Exactly-once accounting under concurrency ────────────────────────────────

// TestAdmitAndReapRace_EachTicketCountedOnce races resume admissions
// against the reaper for the same expired, in-window tokens. Every ticket
// number must end up counted exactly once: in nowServing (released or
// retired-and-consumed), in the ledger (retired, not yet reached), or
// still live in the token store. Before claim-before-acquire, a token
// could be both admitted and reaped, advancing nowServing twice.
func TestAdmitAndReapRace_EachTicketCountedOnce(t *testing.T) {
	const n = 50
	wr := newTestWR(t, n)

	r := gin.New()
	wr.RegisterRoutes(r)
	r.GET("/", func(c *gin.Context) { c.Status(http.StatusOK) })

	stale := time.Now().Add(-(wr.TokenTTL() + time.Minute))
	for i := 1; i <= n; i++ {
		wr.tokens.set(fmt.Sprintf("tok-%d", i), ticketEntry{ticket: int64(i), issuedAt: stale})
	}
	wr.nextTicket.Store(n)

	var wg sync.WaitGroup
	for i := 1; i <= n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.AddCookie(&http.Cookie{Name: cookieName, Value: fmt.Sprintf("tok-%d", i)})
			r.ServeHTTP(httptest.NewRecorder(), req)
		}(i)
	}
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wr.reap()
		}()
	}
	wg.Wait()

	if l := wr.Len(); l != 0 {
		t.Fatalf("expected no active requests, got %d", l)
	}
	accounted := wr.nowServing.Load() + wr.ledger.len() + int64(wr.tokens.len())
	if issued := wr.nextTicket.Load(); accounted != issued {
		t.Errorf("ticket accounting broken: nowServing(%d)+ledger(%d)+live(%d)=%d, issued=%d",
			wr.nowServing.Load(), wr.ledger.len(), wr.tokens.len(), accounted, issued)
	}
}

// TestRetireAndAdvance_ConcurrentExactlyOnce interleaves out-of-window
// retirements with releases in arbitrary order. Ghosts 2..501 are
// contiguous, so once everything settles every one of them must have been
// consumed: nowServing = releases + ghosts and the ledger is empty. A ghost
// stranded below the window edge would leave the ledger non-empty.
func TestRetireAndAdvance_ConcurrentExactlyOnce(t *testing.T) {
	wr := newTestWR(t, 1)

	const ghosts = 500
	const releases = 100
	wr.nextTicket.Store(ghosts + releases + 1)

	var wg sync.WaitGroup
	for i := 0; i < ghosts; i++ {
		wg.Add(1)
		go func(ticket int64) {
			defer wg.Done()
			wr.retire(ticket)
		}(int64(i + 2))
	}
	for range releases {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wr.advance(1)
		}()
	}
	wg.Wait()

	if got, want := wr.nowServing.Load(), int64(ghosts+releases); got != want {
		t.Errorf("nowServing = %d, want %d", got, want)
	}
	if l := wr.ledger.len(); l != 0 {
		t.Errorf("expected empty ledger, %d ghosts stranded", l)
	}
}

// ── Positions, depth and SetCap with pending ghosts ──────────────────────────

func TestSetCap_GrowDrainsLedger(t *testing.T) {
	wr := newTestWR(t, 1)

	wr.nextTicket.Store(3)
	wr.tokens.set("live", ticketEntry{ticket: 3, issuedAt: time.Now()})
	wr.retire(2) // edge is 1, so ticket 2 waits in the ledger

	if l := wr.ledger.len(); l != 1 {
		t.Fatalf("expected 1 pending ghost, got %d", l)
	}
	if pos := wr.positionOf(3); pos != 1 {
		t.Errorf("expected position 1 (ghost ahead subtracted), got %d", pos)
	}
	if d := wr.QueueDepth(); d != 1 {
		t.Errorf("expected QueueDepth 1 (ghost subtracted), got %d", d)
	}
	if wr.ticketReady(3) {
		t.Fatal("setup: ticket 3 must not be ready at cap=1")
	}

	if err := wr.SetCap(2); err != nil {
		t.Fatal(err)
	}

	// Edge 2 reaches the ghost → skipped → nowServing 1 → edge 3.
	if ns := wr.nowServing.Load(); ns != 1 {
		t.Errorf("expected nowServing=1 after SetCap drained the ghost, got %d", ns)
	}
	if l := wr.ledger.len(); l != 0 {
		t.Errorf("expected empty ledger, got %d", l)
	}
	if !wr.ticketReady(3) {
		t.Error("expected ticket 3 ready after growth reached past the ghost")
	}
}

func TestPositionOf_NeverZeroForWaitingTicket(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 1)

	// Duplicate ghosts ahead (possible with promotion collisions) must
	// not push a waiting ticket to position <= 0, or /queue/status would
	// report ready while the middleware still refuses admission.
	wr.ledger.add(3)
	wr.ledger.add(3)
	wr.ledger.add(3)
	if pos := wr.positionOf(4); pos != 1 {
		t.Errorf("expected clamped position 1, got %d", pos)
	}
	if wr.ticketReady(4) {
		t.Error("ticket 4 must not be ready")
	}
}

// ── Benchmarks ────────────────────────────────────────────────────────────────

func BenchmarkAdvance_EmptyLedger(b *testing.B) {
	wr := &WaitingRoom{}
	if err := wr.Init(10); err != nil {
		b.Fatal(err)
	}
	defer wr.Stop()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		wr.advance(1)
	}
}

func BenchmarkPositionOf_Ledger1k(b *testing.B) {
	wr := &WaitingRoom{}
	if err := wr.Init(1); err != nil {
		b.Fatal(err)
	}
	defer wr.Stop()
	for i := 0; i < 1000; i++ {
		wr.ledger.add(int64(1_000_000 + i*2))
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = wr.positionOf(2_000_000)
	}
}

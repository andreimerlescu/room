package room

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// queueTokens issues n waiting-room tokens on a router whose slots are
// already full and returns them in arrival order.
func queueTokens(t *testing.T, r *gin.Engine, n int) []string {
	t.Helper()
	out := make([]string, n)
	for i := range out {
		_, tok := serveWithCookie(r, "")
		if tok == "" {
			t.Fatalf("arrival %d was not queued", i)
		}
		out[i] = tok
	}
	return out
}

// positionOfToken returns the current position of a live token.
func positionOfToken(t *testing.T, wr *WaitingRoom, token string) int64 {
	t.Helper()
	e, ok := wr.tokens.get(token)
	if !ok {
		t.Fatalf("token %q not in store", token)
	}
	return wr.positionOf(e.ticket)
}

// ── RemoveToken: errors ──────────────────────────────────────────────────────

func TestRemoveToken_UnknownReturnsNotFound(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 5)

	err := wr.RemoveToken("does-not-exist")
	if _, ok := err.(ErrTokenNotFound); !ok {
		t.Errorf("expected ErrTokenNotFound, got %T: %v", err, err)
	}
}

func TestRemoveToken_NotInitialised(t *testing.T) {
	t.Parallel()
	wr := &WaitingRoom{}

	err := wr.RemoveToken("anything")
	if _, ok := err.(ErrNotInitialised); !ok {
		t.Errorf("expected ErrNotInitialised, got %T: %v", err, err)
	}
	if n := wr.RemoveTokensFunc(func(TicketInfo) bool { return true }); n != 0 {
		t.Errorf("expected 0 removals on uninitialised room, got %d", n)
	}
}

func TestRemoveToken_Twice_CountsOnce(t *testing.T) {
	wr := newTestWR(t, 1)
	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	r := newTestRouter(wr, serving, release)
	defer close(release)

	fillOneSlot(t, r, serving)
	toks := queueTokens(t, r, 1)

	if err := wr.RemoveToken(toks[0]); err != nil {
		t.Fatalf("first removal: %v", err)
	}
	if err := wr.RemoveToken(toks[0]); err == nil {
		t.Fatal("second removal should fail")
	} else if _, ok := err.(ErrTokenNotFound); !ok {
		t.Errorf("expected ErrTokenNotFound on second removal, got %T", err)
	}
	if l := wr.ledger.len(); l != 1 {
		t.Errorf("expected exactly one retired ticket, ledger has %d", l)
	}
}

// ── RemoveToken: queue effects ───────────────────────────────────────────────

// TestRemoveToken_MidQueue verifies the documented semantics: positions
// behind the removed ticket improve at once, positions ahead are
// unchanged, QueueDepth drops immediately, and nobody is admitted early.
func TestRemoveToken_MidQueue(t *testing.T) {
	wr := newTestWR(t, 1)
	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	r := newTestRouter(wr, serving, release)
	defer close(release)

	fillOneSlot(t, r, serving)
	toks := queueTokens(t, r, 3) // tickets 2, 3, 4 → positions 1, 2, 3
	a, b, c := toks[0], toks[1], toks[2]

	if d := wr.QueueDepth(); d != 3 {
		t.Fatalf("setup: expected QueueDepth 3, got %d", d)
	}

	if err := wr.RemoveToken(b); err != nil {
		t.Fatalf("RemoveToken: %v", err)
	}

	if pos := positionOfToken(t, wr, a); pos != 1 {
		t.Errorf("ahead of removal: expected position 1, got %d", pos)
	}
	if pos := positionOfToken(t, wr, c); pos != 2 {
		t.Errorf("behind removal: expected position 2, got %d", pos)
	}
	if d := wr.QueueDepth(); d != 2 {
		t.Errorf("expected QueueDepth 2, got %d", d)
	}
	if d := wr.LiveQueueDepth(); d != 2 {
		t.Errorf("expected LiveQueueDepth 2, got %d", d)
	}
	if ns := wr.nowServing.Load(); ns != 0 {
		t.Errorf("mid-queue removal must not admit anyone early: nowServing=%d", ns)
	}

	// The removed client's next poll sends it back to the main handler.
	resp := pollStatus(r, b)
	if !resp.Ready || resp.CookiesRequired {
		t.Errorf("removed token poll: expected ready=true cookies_required=false, got %+v", resp)
	}
}

// TestRemoveToken_InWindowFreesSlotNow covers removing a client that was
// already told ready but has not reloaded: its slot passes on at once.
func TestRemoveToken_InWindowFreesSlotNow(t *testing.T) {
	wr := newTestWR(t, 1)
	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	r := newTestRouter(wr, serving, release)

	fillOneSlot(t, r, serving)
	toks := queueTokens(t, r, 1)

	close(release)
	waitForNowServing(t, wr, 1, 2*time.Second)

	if err := wr.RemoveToken(toks[0]); err != nil {
		t.Fatalf("RemoveToken: %v", err)
	}
	if ns := wr.nowServing.Load(); ns != 2 {
		t.Errorf("expected nowServing=2 after removing an in-window ticket, got %d", ns)
	}
	if _, tok := serveWithCookie(r, ""); tok != "" {
		t.Error("expected the next arrival on the fast path, but it was queued")
	}
}

func TestRemoveToken_FiresEventRemoveNotEvict(t *testing.T) {
	wr := newTestWR(t, 1)

	var removes, evicts atomic.Int32
	wr.On(EventRemove, func(Snapshot) { removes.Add(1) })
	wr.On(EventEvict, func(Snapshot) { evicts.Add(1) })

	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	r := newTestRouter(wr, serving, release)
	defer close(release)

	fillOneSlot(t, r, serving)
	toks := queueTokens(t, r, 1)

	if err := wr.RemoveToken(toks[0]); err != nil {
		t.Fatal(err)
	}
	waitForCount(t, &removes, 1, 200*time.Millisecond)
	time.Sleep(30 * time.Millisecond)
	if evicts.Load() != 0 {
		t.Errorf("RemoveToken must not fire EventEvict, got %d", evicts.Load())
	}
}

// TestRemoveToken_BreakerRecovers verifies removal lowers both breaker
// measures immediately, so new arrivals are queued again without waiting
// for the reaper.
func TestRemoveToken_BreakerRecovers(t *testing.T) {
	wr := newTestWR(t, 1)
	if err := wr.SetMaxQueueDepth(2); err != nil {
		t.Fatal(err)
	}
	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	r := newTestRouter(wr, serving, release)
	defer close(release)

	fillOneSlot(t, r, serving)
	toks := queueTokens(t, r, 2)

	if w, _ := serveWithCookie(r, ""); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("setup: expected 503 at max depth, got %d", w.Code)
	}

	if err := wr.RemoveToken(toks[1]); err != nil {
		t.Fatal(err)
	}

	w, tok := serveWithCookie(r, "")
	if w.Code != http.StatusOK || tok == "" {
		t.Errorf("expected the next arrival to be queued after removal, got code=%d token=%q", w.Code, tok)
	}
}

// TestRemoveToken_RacesAdmission_AccountingHolds races removals against
// resume admissions for the same in-window tokens. Whichever claims the
// token first accounts for it; the ticket accounting invariant must hold.
func TestRemoveToken_RacesAdmission_AccountingHolds(t *testing.T) {
	const n = 40
	wr := newTestWR(t, n)

	r := gin.New()
	wr.RegisterRoutes(r)
	r.GET("/", func(c *gin.Context) { c.Status(http.StatusOK) })

	for i := 1; i <= n; i++ {
		wr.tokens.set(fmt.Sprintf("tok-%d", i), ticketEntry{ticket: int64(i), issuedAt: time.Now()})
	}
	wr.nextTicket.Store(n)

	var wg sync.WaitGroup
	for i := 1; i <= n; i++ {
		tok := fmt.Sprintf("tok-%d", i)
		wg.Add(2)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.AddCookie(&http.Cookie{Name: cookieName, Value: tok})
			r.ServeHTTP(httptest.NewRecorder(), req)
		}()
		go func() {
			defer wg.Done()
			_ = wr.RemoveToken(tok)
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

// ── RemoveTokensFunc ─────────────────────────────────────────────────────────

func TestRemoveTokensFunc_RemovesMatchingAndFiresOnce(t *testing.T) {
	wr := newTestWR(t, 1)

	var removes atomic.Int32
	wr.On(EventRemove, func(Snapshot) { removes.Add(1) })

	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	r := newTestRouter(wr, serving, release)
	defer close(release)

	fillOneSlot(t, r, serving)
	toks := queueTokens(t, r, 10) // tickets 2..11

	n := wr.RemoveTokensFunc(func(ti TicketInfo) bool { return ti.Ticket%2 == 0 })
	if n != 5 {
		t.Fatalf("expected 5 removals, got %d", n)
	}
	if live := wr.LiveQueueDepth(); live != 5 {
		t.Errorf("expected 5 live tokens, got %d", live)
	}
	if d := wr.QueueDepth(); d != 5 {
		t.Errorf("expected QueueDepth 5, got %d", d)
	}

	// Survivors hold odd tickets 3,5,7,9,11 → positions 1..5 in order.
	want := int64(1)
	for _, tok := range toks {
		e, ok := wr.tokens.get(tok)
		if !ok {
			continue
		}
		if pos := wr.positionOf(e.ticket); pos != want {
			t.Errorf("ticket %d: expected position %d, got %d", e.ticket, want, pos)
		}
		want++
	}

	waitForCount(t, &removes, 1, 200*time.Millisecond)
	time.Sleep(30 * time.Millisecond)
	if got := removes.Load(); got != 1 {
		t.Errorf("expected EventRemove once per call, got %d", got)
	}
}

func TestRemoveTokensFunc_PredicateSeesInfo(t *testing.T) {
	wr := newTestWR(t, 1)
	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	r := newTestRouter(wr, serving, release)
	defer close(release)

	fillOneSlot(t, r, serving)
	toks := queueTokens(t, r, 3)

	seen := make(map[string]TicketInfo)
	wr.RemoveTokensFunc(func(ti TicketInfo) bool {
		seen[ti.Token] = ti
		return false
	})

	if len(seen) != 3 {
		t.Fatalf("predicate saw %d tokens, want 3", len(seen))
	}
	for i, tok := range toks {
		ti, ok := seen[tok]
		if !ok {
			t.Fatalf("predicate never saw token %d", i)
		}
		if ti.Position != int64(i+1) {
			t.Errorf("token %d: Position = %d, want %d", i, ti.Position, i+1)
		}
		if ti.LastSeen.IsZero() {
			t.Errorf("token %d: LastSeen is zero", i)
		}
		if ti.Promoted {
			t.Errorf("token %d: unexpectedly Promoted", i)
		}
	}
	if live := wr.LiveQueueDepth(); live != 3 {
		t.Errorf("a predicate returning false must remove nothing, live=%d", live)
	}
}

func TestRemoveTokensFunc_NilAndNoMatch(t *testing.T) {
	wr := newTestWR(t, 1)

	var removes atomic.Int32
	wr.On(EventRemove, func(Snapshot) { removes.Add(1) })

	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	r := newTestRouter(wr, serving, release)
	defer close(release)

	fillOneSlot(t, r, serving)
	queueTokens(t, r, 3)

	if n := wr.RemoveTokensFunc(nil); n != 0 {
		t.Errorf("nil predicate removed %d", n)
	}
	if n := wr.RemoveTokensFunc(func(TicketInfo) bool { return false }); n != 0 {
		t.Errorf("no-match predicate removed %d", n)
	}
	time.Sleep(30 * time.Millisecond)
	if removes.Load() != 0 {
		t.Errorf("EventRemove fired without any removal")
	}
}

// TestRemoveTokensFunc_PredicateMayCallBack verifies no locks are held
// while the predicate runs, so it can call back into the WaitingRoom, and
// that a token removed by the callback is skipped rather than double-counted.
func TestRemoveTokensFunc_PredicateMayCallBack(t *testing.T) {
	wr := newTestWR(t, 1)
	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	r := newTestRouter(wr, serving, release)
	defer close(release)

	fillOneSlot(t, r, serving)
	toks := queueTokens(t, r, 5) // tickets 2..6

	var once sync.Once
	done := make(chan int, 1)
	go func() {
		done <- wr.RemoveTokensFunc(func(ti TicketInfo) bool {
			once.Do(func() {
				_ = wr.LiveQueueDepth()
				_ = wr.RemoveToken(toks[0])
			})
			return true
		})
	}()

	var n int
	select {
	case n = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RemoveTokensFunc deadlocked when the predicate called back into the room")
	}

	if n != 4 {
		t.Errorf("expected 4 removals (one already taken by the callback), got %d", n)
	}
	if live := wr.LiveQueueDepth(); live != 0 {
		t.Errorf("expected 0 live tokens, got %d", live)
	}
	if l := wr.ledger.len(); l != 5 {
		t.Errorf("expected 5 retired tickets, ledger has %d", l)
	}
	if d := wr.QueueDepth(); d != 0 {
		t.Errorf("expected QueueDepth 0, got %d", d)
	}
}

// ── Benchmarks ────────────────────────────────────────────────────────────────

func BenchmarkRemoveToken(b *testing.B) {
	wr := &WaitingRoom{}
	if err := wr.Init(1); err != nil {
		b.Fatal(err)
	}
	defer wr.Stop()

	tokens := make([]string, b.N)
	for i := range tokens {
		tokens[i] = fmt.Sprintf("tok-%d", i)
		wr.tokens.set(tokens[i], ticketEntry{ticket: int64(10 + i), issuedAt: time.Now()})
	}
	wr.nextTicket.Store(int64(10 + b.N))

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = wr.RemoveToken(tokens[i])
	}
}

func BenchmarkRemoveTokensFunc_10k(b *testing.B) {
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		wr := &WaitingRoom{}
		if err := wr.Init(1); err != nil {
			b.Fatal(err)
		}
		for j := 0; j < 10_000; j++ {
			wr.tokens.set(fmt.Sprintf("tok-%d", j), ticketEntry{ticket: int64(10 + j), issuedAt: time.Now()})
		}
		wr.nextTicket.Store(10_010)
		b.StartTimer()

		wr.RemoveTokensFunc(func(ti TicketInfo) bool { return ti.Ticket%10 == 0 })

		b.StopTimer()
		wr.Stop()
		b.StartTimer()
	}
}

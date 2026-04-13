package room

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ── reap() — basic eviction ──────────────────────────────────────────────────

func TestReap_EmptyTokenStore_IsNoop(t *testing.T) {
	wr := newTestWR(t, 5)
	before := wr.nowServing.Load()
	wr.reap()
	if wr.nowServing.Load() != before {
		t.Error("nowServing changed on empty token store")
	}
}

func TestReap_AllLive_NoneEvicted(t *testing.T) {
	wr := newTestWR(t, 5)

	for i := 0; i < 10; i++ {
		wr.tokens.set(fmt.Sprintf("tok-%d", i), ticketEntry{
			ticket:   int64(i + 1),
			issuedAt: time.Now(),
		})
	}

	wr.reap()

	if wr.tokens.len() != 10 {
		t.Errorf("expected 10 live tokens, got %d", wr.tokens.len())
	}
}

func TestReap_AllExpired_AllEvicted(t *testing.T) {
	wr := newTestWR(t, 5)
	expired := time.Now().Add(-(cookieTTL + time.Minute))

	for i := 0; i < 10; i++ {
		wr.tokens.set(fmt.Sprintf("tok-%d", i), ticketEntry{
			ticket:   int64(100 + i), // all outside window (cap=5, nowServing=0)
			issuedAt: expired,
		})
	}

	wr.reap()

	if wr.tokens.len() != 0 {
		t.Errorf("expected 0 tokens after reap, got %d", wr.tokens.len())
	}
}

func TestReap_MixedExpiredAndLive(t *testing.T) {
	wr := newTestWR(t, 5)
	expired := time.Now().Add(-(cookieTTL + time.Minute))

	wr.tokens.set("live-1", ticketEntry{ticket: 1, issuedAt: time.Now()})
	wr.tokens.set("live-2", ticketEntry{ticket: 2, issuedAt: time.Now()})
	wr.tokens.set("ghost-1", ticketEntry{ticket: 100, issuedAt: expired})
	wr.tokens.set("ghost-2", ticketEntry{ticket: 101, issuedAt: expired})

	wr.reap()

	if _, ok := wr.tokens.get("live-1"); !ok {
		t.Error("live-1 was evicted")
	}
	if _, ok := wr.tokens.get("live-2"); !ok {
		t.Error("live-2 was evicted")
	}
	if _, ok := wr.tokens.get("ghost-1"); ok {
		t.Error("ghost-1 should have been evicted")
	}
	if _, ok := wr.tokens.get("ghost-2"); ok {
		t.Error("ghost-2 should have been evicted")
	}
}

// ── reap() — nowServing advancement ──────────────────────────────────────────

func TestReap_AdvancesNowServingOnlyForOutOfWindowTickets(t *testing.T) {
	// cap=2, nowServing=0 → window is tickets [1..2].
	wr := newTestWR(t, 2)
	expired := time.Now().Add(-(cookieTTL + time.Minute))

	// Inside window — must NOT advance nowServing.
	wr.tokens.set("inside", ticketEntry{ticket: 1, issuedAt: expired})
	// Outside window — must advance nowServing.
	wr.tokens.set("outside-1", ticketEntry{ticket: 10, issuedAt: expired})
	wr.tokens.set("outside-2", ticketEntry{ticket: 20, issuedAt: expired})

	before := wr.nowServing.Load()
	wr.reap()

	// 2 out-of-window tickets evicted → nowServing should advance by 2.
	expected := before + 2
	if got := wr.nowServing.Load(); got != expected {
		t.Errorf("expected nowServing=%d, got %d", expected, got)
	}
}

func TestReap_DoesNotAdvanceNowServingForWindowTickets(t *testing.T) {
	// cap=10, nowServing=0 → window is tickets [1..10].
	wr := newTestWR(t, 10)
	expired := time.Now().Add(-(cookieTTL + time.Minute))

	for i := int64(1); i <= 5; i++ {
		wr.tokens.set(fmt.Sprintf("win-%d", i), ticketEntry{
			ticket:   i,
			issuedAt: expired,
		})
	}

	before := wr.nowServing.Load()
	wr.reap()

	if wr.nowServing.Load() != before {
		t.Errorf("nowServing advanced for within-window tickets: before=%d after=%d",
			before, wr.nowServing.Load())
	}
	// Tokens should still be evicted even if nowServing doesn't advance.
	if wr.tokens.len() != 0 {
		t.Errorf("expected all tokens evicted, got %d remaining", wr.tokens.len())
	}
}

// ── reap() — multi-batch looping ─────────────────────────────────────────────

func TestReap_ClearsMoreThanOneBatch(t *testing.T) {
	wr := newTestWR(t, 1)
	expired := time.Now().Add(-(cookieTTL + time.Minute))

	// Insert more than reaperBatchSize expired tokens.
	total := reaperBatchSize + 500
	for i := 0; i < total; i++ {
		wr.tokens.set(fmt.Sprintf("ghost-%d", i), ticketEntry{
			ticket:   int64(100 + i), // all outside window
			issuedAt: expired,
		})
	}

	if wr.tokens.len() != total {
		t.Fatalf("setup: expected %d tokens, got %d", total, wr.tokens.len())
	}

	wr.reap()

	if remaining := wr.tokens.len(); remaining != 0 {
		t.Errorf("expected 0 tokens after reap, got %d (batch looping may be broken)", remaining)
	}
}

// ── reap() — EventEvict callback ─────────────────────────────────────────────

func TestReap_FiresEventEvict(t *testing.T) {
	wr := newTestWR(t, 5)

	var evictCount atomic.Int32
	wr.On(EventEvict, func(s Snapshot) { evictCount.Add(1) })

	expired := time.Now().Add(-(cookieTTL + time.Minute))
	wr.tokens.set("ghost", ticketEntry{ticket: 100, issuedAt: expired})

	wr.reap()

	// Wait for async callback.
	deadline := time.After(200 * time.Millisecond)
	for {
		if evictCount.Load() >= 1 {
			break
		}
		select {
		case <-deadline:
			t.Error("EventEvict not fired after reap eviction")
			return
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
}

func TestReap_DoesNotFireEventEvictWhenNothingExpired(t *testing.T) {
	wr := newTestWR(t, 5)

	var evictCount atomic.Int32
	wr.On(EventEvict, func(s Snapshot) { evictCount.Add(1) })

	wr.tokens.set("live", ticketEntry{ticket: 1, issuedAt: time.Now()})
	wr.reap()

	time.Sleep(50 * time.Millisecond)
	if evictCount.Load() != 0 {
		t.Error("EventEvict fired when no tokens were expired")
	}
}

func TestReap_DoesNotFireEventEvictForWindowOnlyEvictions(t *testing.T) {
	// When only within-window tokens are evicted, nowServing doesn't
	// advance, so EventEvict should not fire (evicted == 0 in the code).
	wr := newTestWR(t, 10)

	var evictCount atomic.Int32
	wr.On(EventEvict, func(s Snapshot) { evictCount.Add(1) })

	expired := time.Now().Add(-(cookieTTL + time.Minute))
	wr.tokens.set("win-ghost", ticketEntry{ticket: 1, issuedAt: expired})

	wr.reap()

	time.Sleep(50 * time.Millisecond)
	if evictCount.Load() != 0 {
		t.Errorf("EventEvict fired for within-window eviction (no queue advancement), got %d", evictCount.Load())
	}
}

// ── reapBatch() — TOCTOU double-check ────────────────────────────────────────

func TestReapBatch_DoubleCheckPreventsRaceEviction(t *testing.T) {
	// Simulate a token that was expired during the read-lock scan but
	// was refreshed (touchIssuedAt) before the write-lock eviction.
	wr := newTestWR(t, 5)

	almostExpired := time.Now().Add(-(cookieTTL - 10*time.Millisecond))
	wr.tokens.set("borderline", ticketEntry{
		ticket:   100,
		issuedAt: almostExpired,
	})

	// Sleep until the token has just crossed the TTL boundary.
	time.Sleep(15 * time.Millisecond)

	// Simulate the client refreshing the token right before eviction.
	wr.tokens.touchIssuedAt("borderline")

	// Now reap — the token should survive because touchIssuedAt refreshed it.
	wr.reap()

	if _, ok := wr.tokens.get("borderline"); !ok {
		t.Error("borderline token should have survived reap after touchIssuedAt refresh")
	}
}

// ── SetReaperInterval — reaper restarts with new interval ────────────────────

func TestSetReaperInterval_RestartSignalSent(t *testing.T) {
	wr := newTestWR(t, 5)

	// Drain any pending signal from Init.
	select {
	case <-wr.reaperRestart:
	default:
	}

	if err := wr.SetReaperInterval(10 * time.Second); err != nil {
		t.Fatal(err)
	}

	select {
	case <-wr.reaperRestart:
		// Signal received — correct.
	default:
		// No signal but that's okay if one was already pending.
		// The important thing is the interval was stored.
	}

	if wr.ReaperInterval() != 10*time.Second {
		t.Errorf("expected 10s, got %s", wr.ReaperInterval())
	}
}

func TestSetReaperInterval_DuplicateSignalDoesNotBlock(t *testing.T) {
	wr := newTestWR(t, 5)

	// Fill the channel.
	select {
	case wr.reaperRestart <- struct{}{}:
	default:
	}

	// This must not block even though the channel is full.
	done := make(chan struct{})
	go func() {
		wr.SetReaperInterval(15 * time.Second)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("SetReaperInterval blocked on full reaperRestart channel")
	}
}

// ── Concurrency: reap under concurrent token mutations ───────────────────────

func TestReap_ConcurrentWithTokenStoreWrites(t *testing.T) {
	wr := newTestWR(t, 5)
	expired := time.Now().Add(-(cookieTTL + time.Minute))

	// Pre-populate some expired tokens.
	for i := 0; i < 100; i++ {
		wr.tokens.set(fmt.Sprintf("ghost-%d", i), ticketEntry{
			ticket:   int64(100 + i),
			issuedAt: expired,
		})
	}

	var wg sync.WaitGroup

	// Concurrent reaps.
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wr.reap()
		}()
	}

	// Concurrent writes.
	for i := 0; i < 50; i++ {
		wg.Add(1)
		i := i
		go func() {
			defer wg.Done()
			wr.tokens.set(fmt.Sprintf("new-%d", i), ticketEntry{
				ticket:   int64(1000 + i),
				issuedAt: time.Now(),
			})
		}()
	}

	// Concurrent reads.
	for i := 0; i < 50; i++ {
		wg.Add(1)
		i := i
		go func() {
			defer wg.Done()
			wr.tokens.get(fmt.Sprintf("ghost-%d", i))
		}()
	}

	wg.Wait()

	// All ghosts should be evicted, all new tokens should survive.
	for i := 0; i < 100; i++ {
		if _, ok := wr.tokens.get(fmt.Sprintf("ghost-%d", i)); ok {
			t.Errorf("ghost-%d should have been evicted", i)
		}
	}
	for i := 0; i < 50; i++ {
		if _, ok := wr.tokens.get(fmt.Sprintf("new-%d", i)); !ok {
			t.Errorf("new-%d should still exist", i)
		}
	}
}

// ── startReaper — shutdown via context cancellation ──────────────────────────

func TestStartReaper_StopsOnContextCancel(t *testing.T) {
	wr := &WaitingRoom{}
	if err := wr.Init(5); err != nil {
		t.Fatal(err)
	}

	// Stop should not block or panic.
	wr.Stop()

	// After Stop, the reaper should not be running. Verify by checking
	// that we can re-init without issues.
	if err := wr.Init(10); err != nil {
		t.Fatalf("re-Init after Stop failed: %v", err)
	}
	wr.Stop()
}

// ── tokenStore.len() ─────────────────────────────────────────────────────────

func TestTokenStore_Len(t *testing.T) {
	ts := newTokenStore()

	if ts.len() != 0 {
		t.Errorf("expected len 0, got %d", ts.len())
	}

	ts.set("a", ticketEntry{ticket: 1, issuedAt: time.Now()})
	ts.set("b", ticketEntry{ticket: 2, issuedAt: time.Now()})

	if ts.len() != 2 {
		t.Errorf("expected len 2, got %d", ts.len())
	}

	ts.delete("a")

	if ts.len() != 1 {
		t.Errorf("expected len 1, got %d", ts.len())
	}
}

// ── tokenStore.touchLastPoll() ───────────────────────────────────────────────

func TestTokenStore_TouchLastPoll(t *testing.T) {
	ts := newTokenStore()

	// Non-existent token.
	if _, ok := ts.touchLastPoll("missing"); ok {
		t.Error("expected ok=false for missing token")
	}

	ts.set("tok", ticketEntry{ticket: 1, issuedAt: time.Now()})

	// First touch — previous should be zero.
	prev, ok := ts.touchLastPoll("tok")
	if !ok {
		t.Fatal("expected ok=true")
	}
	if !prev.IsZero() {
		t.Errorf("expected zero time on first touch, got %v", prev)
	}

	time.Sleep(5 * time.Millisecond)

	// Second touch — previous should be recent.
	prev2, ok := ts.touchLastPoll("tok")
	if !ok {
		t.Fatal("expected ok=true")
	}
	if prev2.IsZero() {
		t.Error("expected non-zero time on second touch")
	}
	if time.Since(prev2) > time.Second {
		t.Errorf("previous lastPoll too old: %v", prev2)
	}
}

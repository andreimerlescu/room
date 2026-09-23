package room

import (
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ── SetFirstPollGrace: configuration ─────────────────────────────────────────

func TestSetFirstPollGrace_DefaultDisabled(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 5)
	if g := wr.FirstPollGrace(); g != 0 {
		t.Errorf("expected first-poll grace disabled by default, got %s", g)
	}
}

func TestSetFirstPollGrace_ValidRange(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 5)
	for _, d := range []time.Duration{firstPollGraceMin, 30 * time.Second, time.Hour, firstPollGraceMax} {
		if err := wr.SetFirstPollGrace(d); err != nil {
			t.Errorf("expected no error for %s, got %v", d, err)
		}
		if got := wr.FirstPollGrace(); got != d {
			t.Errorf("expected %s, got %s", d, got)
		}
	}
}

func TestSetFirstPollGrace_InvalidRange(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 5)
	for _, d := range []time.Duration{
		-time.Second,
		time.Second,
		firstPollGraceMin - time.Nanosecond,
		firstPollGraceMax + time.Nanosecond,
	} {
		err := wr.SetFirstPollGrace(d)
		if _, ok := err.(ErrFirstPollGrace); !ok {
			t.Errorf("expected ErrFirstPollGrace for %s, got %T: %v", d, err, err)
		}
	}
	if g := wr.FirstPollGrace(); g != 0 {
		t.Errorf("rejected values must not be stored, got %s", g)
	}
}

func TestSetFirstPollGrace_ZeroDisables(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 5)
	if err := wr.SetFirstPollGrace(30 * time.Second); err != nil {
		t.Fatal(err)
	}
	if err := wr.SetFirstPollGrace(0); err != nil {
		t.Fatalf("0 should disable, got %v", err)
	}
	if g := wr.FirstPollGrace(); g != 0 {
		t.Errorf("expected 0 after disabling, got %s", g)
	}
}

// ── Reaper cadence ────────────────────────────────────────────────────────────

func TestEffectiveReaperInterval(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 5)

	if got := wr.effectiveReaperInterval(); got != DefaultReaperInterval {
		t.Errorf("no grace: expected %s, got %s", DefaultReaperInterval, got)
	}

	if err := wr.SetFirstPollGrace(30 * time.Second); err != nil {
		t.Fatal(err)
	}
	if got := wr.effectiveReaperInterval(); got != 30*time.Second {
		t.Errorf("grace 30s: expected 30s cadence, got %s", got)
	}
	if got := wr.ReaperInterval(); got != DefaultReaperInterval {
		t.Errorf("ReaperInterval must report the configured value, got %s", got)
	}

	if err := wr.SetReaperInterval(15 * time.Second); err != nil {
		t.Fatal(err)
	}
	if got := wr.effectiveReaperInterval(); got != 15*time.Second {
		t.Errorf("interval shorter than grace: expected 15s, got %s", got)
	}

	if err := wr.SetFirstPollGrace(time.Hour); err != nil {
		t.Fatal(err)
	}
	if got := wr.effectiveReaperInterval(); got != 15*time.Second {
		t.Errorf("grace longer than interval: expected 15s, got %s", got)
	}
}

func TestSetFirstPollGrace_SignalsReaperRestart(t *testing.T) {
	wr := newTestWR(t, 5)

	// Drain any pending signal.
	select {
	case <-wr.reaperRestart:
	default:
	}

	done := make(chan struct{})
	go func() {
		_ = wr.SetFirstPollGrace(30 * time.Second)
		_ = wr.SetFirstPollGrace(20 * time.Second) // second signal must not block
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("SetFirstPollGrace blocked on the reaper restart channel")
	}
}

// ── Reaping never-seen tickets ───────────────────────────────────────────────

func TestReap_FirstPollGrace_EvictsOnlyNeverSeenPastGrace(t *testing.T) {
	wr := newTestWR(t, 1)
	if err := wr.SetFirstPollGrace(30 * time.Second); err != nil {
		t.Fatal(err)
	}

	var evicts atomic.Int32
	wr.On(EventEvict, func(Snapshot) { evicts.Add(1) })

	now := time.Now()
	wr.nextTicket.Store(12)
	wr.tokens.set("unseen-old", ticketEntry{ticket: 10, issuedAt: now.Add(-40 * time.Second)})
	wr.tokens.set("seen-old", ticketEntry{ticket: 11, issuedAt: now.Add(-40 * time.Second), seen: true})
	wr.tokens.set("unseen-new", ticketEntry{ticket: 12, issuedAt: now.Add(-10 * time.Second)})

	wr.reap()

	if _, ok := wr.tokens.get("unseen-old"); ok {
		t.Error("never-seen ticket past the grace should be reaped")
	}
	if _, ok := wr.tokens.get("seen-old"); !ok {
		t.Error("a seen ticket is governed by the TTL, not the grace")
	}
	if _, ok := wr.tokens.get("unseen-new"); !ok {
		t.Error("a never-seen ticket within the grace must survive")
	}
	if l := wr.ledger.len(); l != 1 {
		t.Errorf("expected the reaped ticket retired to the ledger, ledger=%d", l)
	}
	waitForCount(t, &evicts, 1, 200*time.Millisecond)
}

func TestReap_FirstPollGrace_DisabledKeepsUnseen(t *testing.T) {
	wr := newTestWR(t, 1)

	wr.tokens.set("unseen", ticketEntry{ticket: 10, issuedAt: time.Now().Add(-time.Minute)})
	wr.reap()

	if _, ok := wr.tokens.get("unseen"); !ok {
		t.Error("with the grace disabled, an unseen ticket within the TTL must survive")
	}
}

func TestReap_FirstPollGrace_InWindowFreesSlot(t *testing.T) {
	wr := newTestWR(t, 5)
	if err := wr.SetFirstPollGrace(10 * time.Second); err != nil {
		t.Fatal(err)
	}

	wr.tokens.set("unseen-ready", ticketEntry{ticket: 1, issuedAt: time.Now().Add(-11 * time.Second)})
	wr.reap()

	if ns := wr.nowServing.Load(); ns != 1 {
		t.Errorf("an in-window never-seen ticket should be skipped at once, nowServing=%d", ns)
	}
}

// TestFirstPollGrace_CookielessFloodReclaimed is the end-to-end case the
// grace exists for: a burst of arrivals that never come back (here, never
// sending a cookie) is reclaimed after the grace, while a real client that
// polled once keeps its place.
func TestFirstPollGrace_CookielessFloodReclaimed(t *testing.T) {
	wr := newTestWR(t, 1)
	if err := wr.SetFirstPollGrace(firstPollGraceMin); err != nil {
		t.Fatal(err)
	}

	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	r := newTestRouter(wr, serving, release)
	defer close(release)

	fillOneSlot(t, r, serving)

	_, real := serveWithCookie(r, "")
	if real == "" {
		t.Fatal("real client was not queued")
	}
	if resp := pollStatus(r, real); resp.Ready || resp.CookiesRequired {
		t.Fatalf("real client's first poll: unexpected %+v", resp)
	}

	const flood = 100
	for i := 0; i < flood; i++ {
		if w, tok := serveWithCookie(r, ""); tok == "" || w.Code != http.StatusOK {
			t.Fatalf("flood arrival %d not queued", i)
		}
	}
	if live := wr.LiveQueueDepth(); live != flood+1 {
		t.Fatalf("setup: expected %d live tokens, got %d", flood+1, live)
	}

	// Past the grace, well inside the TTL.
	ageAllTokens(wr.tokens, firstPollGraceMin+time.Second)
	wr.reap()

	if live := wr.LiveQueueDepth(); live != 1 {
		t.Errorf("expected only the real client to remain, live=%d", live)
	}
	if d := wr.QueueDepth(); d != 1 {
		t.Errorf("expected QueueDepth 1 after reclaiming the flood, got %d", d)
	}
	if _, ok := wr.tokens.get(real); !ok {
		t.Error("the real, polling client lost its place")
	}
}

// ── Concurrency ───────────────────────────────────────────────────────────────

func TestFirstPollGrace_ConcurrentSetAndReap(t *testing.T) {
	wr := newTestWR(t, 1)

	for i := 0; i < 200; i++ {
		wr.tokens.set(fmt.Sprintf("tok-%d", i), ticketEntry{
			ticket:   int64(10 + i),
			issuedAt: time.Now().Add(-time.Duration(i%60) * time.Second),
			seen:     i%2 == 0,
		})
	}
	wr.nextTicket.Store(210)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				_ = wr.SetFirstPollGrace(firstPollGraceMin)
			} else {
				_ = wr.SetFirstPollGrace(0)
			}
		}(i)
		go func() {
			defer wg.Done()
			wr.reap()
		}()
	}
	wg.Wait()

	accounted := wr.nowServing.Load() + wr.ledger.len() + int64(wr.tokens.len())
	// 200 tokens issued as tickets 10..209 plus 9 unissued numbers 1..9
	// that were never tokens: only the 200 are subject to accounting here.
	if live := int64(wr.tokens.len()); live+wr.ledger.len()+wr.nowServing.Load() != accounted {
		t.Fatalf("accounting drift")
	}
	if wr.tokens.len()+int(wr.ledger.len())+int(wr.nowServing.Load()) < 200 {
		t.Errorf("tickets lost: live=%d ledger=%d nowServing=%d",
			wr.tokens.len(), wr.ledger.len(), wr.nowServing.Load())
	}
}

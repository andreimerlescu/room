package room

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// fakeClock is a mutex-protected deterministic clock injected via WaitingRoom.nowFn
// before Init so background reaper ticks and test calls are race-free under -race.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(start time.Time) *fakeClock {
	return &fakeClock{now: start}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	return c.now
}

func newTestWRWithClock(t *testing.T, cap int32, clock *fakeClock, opts ...Option) *WaitingRoom {
	t.Helper()
	wr := &WaitingRoom{nowFn: clock.Now}
	if err := wr.Init(cap, opts...); err != nil {
		t.Fatalf("Init(%d): %v", cap, err)
	}
	t.Cleanup(wr.Stop)
	return wr
}

func newAdmitRouter(wr *WaitingRoom, serving chan struct{}, release chan struct{}, admitted *atomic.Int32) *gin.Engine {
	r := gin.New()
	wr.RegisterRoutes(r)
	r.GET("/", func(c *gin.Context) {
		if serving != nil {
			serving <- struct{}{}
		}
		if release != nil {
			<-release
		}
		if admitted != nil {
			admitted.Add(1)
		}
		c.String(http.StatusOK, "admitted")
	})
	return r
}

func assertTicketAccounting(t *testing.T, wr *WaitingRoom, context string) {
	t.Helper()
	ns := wr.nowServing.Load()
	led := wr.ledger.len()
	live := int64(wr.tokens.len())
	occ := int64(wr.occupancy.Load())
	next := wr.nextTicket.Load()
	if ns+led+live+occ != next {
		t.Errorf("%s: ticket accounting drift: nowServing(%d) + ledger(%d) + live(%d) + occupancy(%d) = %d, want nextTicket(%d)",
			context, ns, led, live, occ, ns+led+live+occ, next)
	}
}

func assertNodeHeapValid(t *testing.T, ts *tokenStore) {
	t.Helper()
	ts.mu.RLock()
	defer ts.mu.RUnlock()

	if ts.ttlHeap.len() != len(ts.entries) {
		t.Errorf("ttlHeap len=%d != entries len=%d", ts.ttlHeap.len(), len(ts.entries))
	}
	checkHeap := func(name string, h *nodeHeap) {
		for i, n := range h.nodes {
			if n == nil {
				t.Errorf("%s[%d] is nil", name, i)
				continue
			}
			if got := h.idx(n); got != i {
				t.Errorf("%s[%d]: node index=%d, want %d", name, i, got, i)
			}
			if _, ok := ts.entries[n.token]; !ok {
				t.Errorf("%s[%d]: orphan node %q not in entries", name, i, n.token)
			}
			if i > 0 {
				parent := (i - 1) / 2
				if h.less(i, parent) {
					t.Errorf("%s heap order violated: child[%d]=%v < parent[%d]=%v",
						name, i, h.key(n), parent, h.key(h.nodes[parent]))
				}
			}
		}
	}
	checkHeap("ttlHeap", &ts.ttlHeap)
	checkHeap("unseenHeap", &ts.unseenHeap)
}

func runWithTimeout(t *testing.T, limit time.Duration, label string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(limit):
		t.Fatalf("%s did not complete within %s", label, limit)
	}
}

// 1. Config validation & options

func TestSetReadyGrace_RangeAndOptions(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 5)

	if got := wr.ReadyGrace(); got != 0 {
		t.Fatalf("expected default ReadyGrace 0, got %s", got)
	}

	// Valid range [5s, 10m].
	for _, d := range []time.Duration{readyGraceMin, 15 * time.Second, time.Minute, readyGraceMax} {
		if err := wr.SetReadyGrace(d); err != nil {
			t.Errorf("SetReadyGrace(%s): unexpected error %v", d, err)
		}
		if got := wr.ReadyGrace(); got != d {
			t.Errorf("ReadyGrace() = %s, want %s", got, d)
		}
	}

	// Store a known valid value, then verify rejected values return ErrReadyGrace
	// without mutating the stored value.
	const kept = 30 * time.Second
	if err := wr.SetReadyGrace(kept); err != nil {
		t.Fatal(err)
	}
	for _, d := range []time.Duration{
		-time.Second,
		-readyGraceMin,
		time.Nanosecond,
		time.Second,
		readyGraceMin - time.Nanosecond,
		readyGraceMax + time.Nanosecond,
		time.Hour,
	} {
		err := wr.SetReadyGrace(d)
		e, ok := err.(ErrReadyGrace)
		if !ok {
			t.Errorf("SetReadyGrace(%s): expected ErrReadyGrace, got %T (%v)", d, err, err)
			continue
		}
		if e.Given != d || e.Min != readyGraceMin || e.Max != readyGraceMax {
			t.Errorf("ErrReadyGrace fields = %+v, want Given=%s Min=%s Max=%s", e, d, readyGraceMin, readyGraceMax)
		}
		if !strings.Contains(e.Error(), "ready grace") || !strings.Contains(e.Error(), "0 disables") {
			t.Errorf("unexpected ErrReadyGrace.Error(): %q", e.Error())
		}
		if got := wr.ReadyGrace(); got != kept {
			t.Errorf("SetReadyGrace(%s) mutated stored value to %s, want %s", d, got, kept)
		}
	}

	// Zero disables.
	if err := wr.SetReadyGrace(0); err != nil {
		t.Fatalf("SetReadyGrace(0): %v", err)
	}
	if got := wr.ReadyGrace(); got != 0 {
		t.Errorf("ReadyGrace() after disable = %s, want 0", got)
	}

	// Non-blocking reaper restart signal even on repeated calls.
	runWithTimeout(t, time.Second, "repeated SetReadyGrace signals", func() {
		_ = wr.SetReadyGrace(10 * time.Second)
		_ = wr.SetReadyGrace(20 * time.Second)
	})

	// Reset to 0 by Init.
	if err := wr.SetReadyGrace(45 * time.Second); err != nil {
		t.Fatal(err)
	}
	if err := wr.Init(5); err != nil {
		t.Fatalf("re-Init: %v", err)
	}
	if got := wr.ReadyGrace(); got != 0 {
		t.Errorf("Init did not reset ReadyGrace to 0: got %s", got)
	}

	// WithReadyGrace option: valid values.
	for _, d := range []time.Duration{0, readyGraceMin, 25 * time.Second, readyGraceMax} {
		w := &WaitingRoom{}
		if err := w.Init(5, WithReadyGrace(d)); err != nil {
			t.Errorf("Init with WithReadyGrace(%s): unexpected error %v", d, err)
			continue
		}
		if got := w.ReadyGrace(); got != d {
			t.Errorf("Init with WithReadyGrace(%s): ReadyGrace() = %s", d, got)
		}
		w.Stop()
	}

	// WithReadyGrace option: invalid values fail Init with ErrReadyGrace.
	for _, d := range []time.Duration{-time.Second, 4 * time.Second, 11 * time.Minute} {
		w := &WaitingRoom{}
		err := w.Init(5, WithReadyGrace(d))
		if e, ok := err.(ErrReadyGrace); !ok || e.Given != d {
			t.Errorf("Init with WithReadyGrace(%s): expected ErrReadyGrace{Given:%s}, got %T (%v)", d, d, err, err)
		}
		if w.initialised.Load() {
			t.Errorf("Init with invalid WithReadyGrace(%s) must not mark WaitingRoom initialised", d)
		}
		w.Stop()
	}
}

func TestSetIdlePollTTL_RangeAndOptions(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 5)

	if got := wr.IdlePollTTL(); got != 0 {
		t.Fatalf("expected default IdlePollTTL 0, got %s", got)
	}

	// Valid range [10s, 24h].
	for _, d := range []time.Duration{idlePollTTLMin, 30 * time.Second, time.Hour, idlePollTTLMax} {
		if err := wr.SetIdlePollTTL(d); err != nil {
			t.Errorf("SetIdlePollTTL(%s): unexpected error %v", d, err)
		}
		if got := wr.IdlePollTTL(); got != d {
			t.Errorf("IdlePollTTL() = %s, want %s", got, d)
		}
	}

	// Store a known valid value, then verify rejected values return ErrIdlePollTTL
	// without mutating the stored value.
	const kept = 45 * time.Second
	if err := wr.SetIdlePollTTL(kept); err != nil {
		t.Fatal(err)
	}
	for _, d := range []time.Duration{
		-time.Second,
		-idlePollTTLMin,
		time.Nanosecond,
		5 * time.Second,
		idlePollTTLMin - time.Nanosecond,
		idlePollTTLMax + time.Nanosecond,
		48 * time.Hour,
	} {
		err := wr.SetIdlePollTTL(d)
		e, ok := err.(ErrIdlePollTTL)
		if !ok {
			t.Errorf("SetIdlePollTTL(%s): expected ErrIdlePollTTL, got %T (%v)", d, err, err)
			continue
		}
		if e.Given != d || e.Min != idlePollTTLMin || e.Max != idlePollTTLMax {
			t.Errorf("ErrIdlePollTTL fields = %+v, want Given=%s Min=%s Max=%s", e, d, idlePollTTLMin, idlePollTTLMax)
		}
		if !strings.Contains(e.Error(), "idle-poll TTL") || !strings.Contains(e.Error(), "0 disables") {
			t.Errorf("unexpected ErrIdlePollTTL.Error(): %q", e.Error())
		}
		if got := wr.IdlePollTTL(); got != kept {
			t.Errorf("SetIdlePollTTL(%s) mutated stored value to %s, want %s", d, got, kept)
		}
	}

	// Zero disables.
	if err := wr.SetIdlePollTTL(0); err != nil {
		t.Fatalf("SetIdlePollTTL(0): %v", err)
	}
	if got := wr.IdlePollTTL(); got != 0 {
		t.Errorf("IdlePollTTL() after disable = %s, want 0", got)
	}

	// Non-blocking reaper restart signal even on repeated calls.
	runWithTimeout(t, time.Second, "repeated SetIdlePollTTL signals", func() {
		_ = wr.SetIdlePollTTL(20 * time.Second)
		_ = wr.SetIdlePollTTL(30 * time.Second)
	})

	// Reset to 0 by Init.
	if err := wr.SetIdlePollTTL(time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := wr.Init(5); err != nil {
		t.Fatalf("re-Init: %v", err)
	}
	if got := wr.IdlePollTTL(); got != 0 {
		t.Errorf("Init did not reset IdlePollTTL to 0: got %s", got)
	}

	// WithIdlePollTTL option: valid values.
	for _, d := range []time.Duration{0, idlePollTTLMin, 45 * time.Second, idlePollTTLMax} {
		w := &WaitingRoom{}
		if err := w.Init(5, WithIdlePollTTL(d)); err != nil {
			t.Errorf("Init with WithIdlePollTTL(%s): unexpected error %v", d, err)
			continue
		}
		if got := w.IdlePollTTL(); got != d {
			t.Errorf("Init with WithIdlePollTTL(%s): IdlePollTTL() = %s", d, got)
		}
		w.Stop()
	}

	// WithIdlePollTTL option: invalid values fail Init with ErrIdlePollTTL.
	for _, d := range []time.Duration{-time.Second, 9 * time.Second, 25 * time.Hour} {
		w := &WaitingRoom{}
		err := w.Init(5, WithIdlePollTTL(d))
		if e, ok := err.(ErrIdlePollTTL); !ok || e.Given != d {
			t.Errorf("Init with WithIdlePollTTL(%s): expected ErrIdlePollTTL{Given:%s}, got %T (%v)", d, d, err, err)
		}
		if w.initialised.Load() {
			t.Errorf("Init with invalid WithIdlePollTTL(%s) must not mark WaitingRoom initialised", d)
		}
		w.Stop()
	}
}

func TestEffectiveReaperInterval_ReadyGraceAndIdlePollTTL(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 5)

	assertCadence := func(label string, wantEffective, wantConfigured time.Duration) {
		t.Helper()
		if got := wr.effectiveReaperInterval(); got != wantEffective {
			t.Errorf("%s: effectiveReaperInterval() = %s, want %s", label, got, wantEffective)
		}
		if got := wr.ReaperInterval(); got != wantConfigured {
			t.Errorf("%s: ReaperInterval() = %s, want %s", label, got, wantConfigured)
		}
	}

	assertCadence("defaults", DefaultReaperInterval, DefaultReaperInterval)

	// IdlePollTTL clamping.
	if err := wr.SetIdlePollTTL(30 * time.Second); err != nil {
		t.Fatal(err)
	}
	assertCadence("IdlePollTTL 30s < DefaultReaperInterval", 30*time.Second, DefaultReaperInterval)

	if err := wr.SetIdlePollTTL(10 * time.Minute); err != nil {
		t.Fatal(err)
	}
	assertCadence("IdlePollTTL 10m > DefaultReaperInterval", DefaultReaperInterval, DefaultReaperInterval)

	if err := wr.SetIdlePollTTL(0); err != nil {
		t.Fatal(err)
	}

	// ReadyGrace clamping: max(1s, readyGrace/2).
	if err := wr.SetReadyGrace(10 * time.Second); err != nil {
		t.Fatal(err)
	}
	assertCadence("ReadyGrace 10s -> 5s", 5*time.Second, DefaultReaperInterval)

	if err := wr.SetReadyGrace(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	assertCadence("ReadyGrace 5s -> 2.5s", 2500*time.Millisecond, DefaultReaperInterval)

	if err := wr.SetReadyGrace(2 * time.Minute); err != nil {
		t.Fatal(err)
	}
	assertCadence("ReadyGrace 2m -> 1m", time.Minute, DefaultReaperInterval)

	if err := wr.SetReadyGrace(10 * time.Minute); err != nil {
		t.Fatal(err)
	}
	assertCadence("ReadyGrace 10m -> 5m (equals default)", DefaultReaperInterval, DefaultReaperInterval)

	// Verify the 1s floor inside max(readyGraceReaperFloor, rg/2) directly.
	wr.readyGrace.Store(int64(time.Second))
	assertCadence("ReadyGrace floor 1s", readyGraceReaperFloor, DefaultReaperInterval)
	if err := wr.SetReadyGrace(0); err != nil {
		t.Fatal(err)
	}

	// Combined interactions: ReaperInterval, FirstPollGrace, IdlePollTTL, ReadyGrace.
	if err := wr.SetReaperInterval(time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := wr.SetFirstPollGrace(20 * time.Second); err != nil {
		t.Fatal(err)
	}
	if err := wr.SetIdlePollTTL(30 * time.Second); err != nil {
		t.Fatal(err)
	}
	if err := wr.SetReadyGrace(time.Minute); err != nil { // half = 30s
		t.Fatal(err)
	}
	assertCadence("FirstPollGrace 20s is shortest", 20*time.Second, time.Minute)

	if err := wr.SetIdlePollTTL(15 * time.Second); err != nil {
		t.Fatal(err)
	}
	assertCadence("IdlePollTTL 15s is shortest", 15*time.Second, time.Minute)

	if err := wr.SetReadyGrace(10 * time.Second); err != nil { // half = 5s
		t.Fatal(err)
	}
	assertCadence("ReadyGrace/2 5s is shortest", 5*time.Second, time.Minute)

	if err := wr.SetReaperInterval(8 * time.Second); err != nil {
		t.Fatal(err)
	}
	if err := wr.SetReadyGrace(5 * time.Second); err != nil { // half = 2.5s
		t.Fatal(err)
	}
	assertCadence("ReadyGrace/2 2.5s beats ReaperInterval 8s", 2500*time.Millisecond, 8*time.Second)

	if err := wr.SetReadyGrace(0); err != nil {
		t.Fatal(err)
	}
	assertCadence("ReadyGrace disabled -> ReaperInterval 8s wins", 8*time.Second, 8*time.Second)

	if err := wr.SetReaperInterval(time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := wr.SetIdlePollTTL(0); err != nil {
		t.Fatal(err)
	}
	assertCadence("IdlePollTTL disabled -> FirstPollGrace 20s wins", 20*time.Second, time.Minute)

	if err := wr.SetFirstPollGrace(0); err != nil {
		t.Fatal(err)
	}
	assertCadence("all graces disabled -> ReaperInterval 1m", time.Minute, time.Minute)
}

// 2. Core ReadyGrace regressions

func TestReadyGrace_UnclaimedReadyTicketsEvictedAndWindowAdvances(t *testing.T) {
	t.Run("polled_once_while_ready_then_disconnected", func(t *testing.T) {
		const (
			capSize    = 2
			queuedN    = 4
			readyGrace = 10 * time.Second
		)

		clock := newFakeClock(time.Now())
		wr := newTestWRWithClock(t, capSize, clock, WithReadyGrace(readyGrace))

		var evicts atomic.Int32
		wr.On(EventEvict, func(Snapshot) { evicts.Add(1) })

		serving := make(chan struct{}, capSize+1)
		release := make(chan struct{})
		var admitted atomic.Int32
		r := newAdmitRouter(wr, serving, release, &admitted)

		var activeWG sync.WaitGroup
		for i := 0; i < capSize; i++ {
			activeWG.Add(1)
			go func() {
				defer activeWG.Done()
				w, _ := serveWithCookie(r, "")
				if w.Code != http.StatusOK || w.Body.String() != "admitted" {
					t.Errorf("initial holder: code=%d body=%q", w.Code, w.Body.String())
				}
			}()
			select {
			case <-serving:
			case <-time.After(2 * time.Second):
				t.Fatal("timed out filling active slot")
			}
		}

		// Queue N tickets behind the full room and mark all seen via /queue/status.
		toks := make([]string, queuedN)
		for i := 0; i < queuedN; i++ {
			_, tok := serveWithCookie(r, "")
			if tok == "" {
				t.Fatalf("queued arrival %d received no token", i)
			}
			toks[i] = tok
		}
		clock.Advance(2 * time.Second)
		for i, tok := range toks {
			resp := pollStatus(r, tok)
			if resp.Ready {
				t.Fatalf("queued ticket %d unexpectedly ready while room full", i)
			}
		}

		// Release all active handlers so occupancy drops to 0.
		close(release)
		activeWG.Wait()
		if occ := wr.occupancy.Load(); occ != 0 {
			t.Fatalf("expected occupancy 0 after releasing active handlers, got %d", occ)
		}
		if ns := wr.nowServing.Load(); ns != capSize {
			t.Fatalf("expected nowServing=%d, got %d", capSize, ns)
		}

		// Front cap tickets poll once (becoming ready and stamping ReadyAt) then disconnect.
		readyPollTime := clock.Advance(2 * time.Second)
		for i := 0; i < capSize; i++ {
			resp := pollStatus(r, toks[i])
			if !resp.Ready {
				t.Fatalf("front ticket %d: expected ready=true, got %+v", i, resp)
			}
			ti, ok := wr.Ticket(toks[i])
			if !ok {
				t.Fatalf("front ticket %d missing from store after ready poll", i)
			}
			if !ti.ReadyAt.Equal(readyPollTime) {
				t.Errorf("front ticket %d: TicketInfo.ReadyAt = %v, want %v", i, ti.ReadyAt, readyPollTime)
			}
		}
		// Waiting ticket #cap+1 is still outside the window.
		if resp := pollStatus(r, toks[capSize]); resp.Ready || resp.Position != 1 {
			t.Fatalf("waiting ticket #%d: expected ready=false pos=1, got %+v", capSize+1, resp)
		}

		// Before readyGrace elapses, reap must keep the front tickets.
		clock.Advance(readyGrace - time.Second)
		wr.reap()
		for i := 0; i < capSize; i++ {
			if _, ok := wr.tokens.get(toks[i]); !ok {
				t.Fatalf("front ticket %d evicted before readyGrace elapsed", i)
			}
		}

		// Advance past readyGrace and reap.
		clock.Advance(2 * time.Second)
		wr.reap()

		for i := 0; i < capSize; i++ {
			if _, ok := wr.tokens.get(toks[i]); ok {
				t.Errorf("front ticket %d should have been evicted past readyGrace", i)
			}
		}
		if ns := wr.nowServing.Load(); ns != 2*capSize {
			t.Errorf("nowServing = %d, want %d after evicting front %d ready tickets", ns, 2*capSize, capSize)
		}
		waitForCount(t, &evicts, 1, 500*time.Millisecond)
		if occ := wr.occupancy.Load(); occ != 0 {
			t.Errorf("occupancy = %d, want 0 before admitting ticket #%d", occ, capSize+1)
		}
		assertTicketAccounting(t, wr, "after evicting front ready tickets")

		// Waiting ticket #cap+1 is now ready and admitted while occupancy == 0.
		if resp := pollStatus(r, toks[capSize]); !resp.Ready {
			t.Fatalf("ticket #%d: expected ready=true after window advanced, got %+v", capSize+1, resp)
		}
		w, newTok := serveWithCookie(r, toks[capSize])
		if w.Code != http.StatusOK || w.Body.String() != "admitted" || newTok != "" {
			t.Fatalf("ticket #%d admission failed: code=%d body=%q newTok=%q", capSize+1, w.Code, w.Body.String(), newTok)
		}
		if got := admitted.Load(); got != capSize+1 {
			t.Errorf("admitted count = %d, want %d", got, capSize+1)
		}
		if _, ok := wr.tokens.get(toks[capSize]); ok {
			t.Errorf("ticket #%d still in store after admission", capSize+1)
		}
		if ns := wr.nowServing.Load(); ns != 2*capSize+1 {
			t.Errorf("nowServing = %d, want %d after admitting ticket #%d", ns, 2*capSize+1, capSize+1)
		}
		assertTicketAccounting(t, wr, "after admitting ticket #cap+1")
	})

	t.Run("disconnected_before_entering_window_never_polled_again", func(t *testing.T) {
		const (
			capSize    = 2
			queuedN    = 4
			readyGrace = 10 * time.Second
		)

		clock := newFakeClock(time.Now())
		wr := newTestWRWithClock(t, capSize, clock, WithReadyGrace(readyGrace))

		var evicts atomic.Int32
		wr.On(EventEvict, func(Snapshot) { evicts.Add(1) })

		serving := make(chan struct{}, capSize+1)
		release := make(chan struct{})
		var admitted atomic.Int32
		r := newAdmitRouter(wr, serving, release, &admitted)

		var activeWG sync.WaitGroup
		for i := 0; i < capSize; i++ {
			activeWG.Add(1)
			go func() {
				defer activeWG.Done()
				serveWithCookie(r, "")
			}()
			select {
			case <-serving:
			case <-time.After(2 * time.Second):
				t.Fatal("timed out filling active slot")
			}
		}

		toks := make([]string, queuedN)
		for i := 0; i < queuedN; i++ {
			_, tok := serveWithCookie(r, "")
			if tok == "" {
				t.Fatalf("queued arrival %d received no token", i)
			}
			toks[i] = tok
		}
		clock.Advance(2 * time.Second)
		for i, tok := range toks {
			if resp := pollStatus(r, tok); resp.Ready {
				t.Fatalf("queued ticket %d unexpectedly ready while room full", i)
			}
		}

		// Release active slots; front cap tickets disconnect and NEVER poll again.
		close(release)
		activeWG.Wait()
		if occ := wr.occupancy.Load(); occ != 0 {
			t.Fatalf("expected occupancy 0, got %d", occ)
		}

		// Tick 1: reap stamps readyAt on front cap tickets without evicting.
		stampTime := clock.Advance(2 * time.Second)
		wr.reap()

		for i := 0; i < capSize; i++ {
			ti, ok := wr.Ticket(toks[i])
			if !ok {
				t.Fatalf("front ticket %d should not be evicted on stamp tick", i)
			}
			if !ti.ReadyAt.Equal(stampTime) {
				t.Errorf("front ticket %d: ReadyAt = %v, want %v", i, ti.ReadyAt, stampTime)
			}
		}
		for i := capSize; i < queuedN; i++ {
			ti, ok := wr.Ticket(toks[i])
			if !ok {
				t.Fatalf("out-of-window ticket %d missing after stamp tick", i)
			}
			if !ti.ReadyAt.IsZero() {
				t.Errorf("out-of-window ticket %d must not have ReadyAt stamped, got %v", i, ti.ReadyAt)
			}
		}
		if ns := wr.nowServing.Load(); ns != capSize {
			t.Fatalf("nowServing advanced prematurely on stamp tick: got %d, want %d", ns, capSize)
		}

		// At exact readyGrace boundary (now.Sub(readyAt) == readyGrace), still not evicted.
		clock.Advance(readyGrace)
		wr.reap()
		for i := 0; i < capSize; i++ {
			if _, ok := wr.tokens.get(toks[i]); !ok {
				t.Fatalf("front ticket %d evicted at exact readyGrace boundary", i)
			}
		}

		// Tick 2: strictly past readyGrace — front cap tickets are evicted,
		// nowServing advances, and ticket #cap+1 is admitted.
		clock.Advance(time.Second)
		wr.reap()

		for i := 0; i < capSize; i++ {
			if _, ok := wr.tokens.get(toks[i]); ok {
				t.Errorf("front ticket %d should have been evicted on tick 2", i)
			}
		}
		if ns := wr.nowServing.Load(); ns != 2*capSize {
			t.Errorf("nowServing = %d, want %d", ns, 2*capSize)
		}
		waitForCount(t, &evicts, 1, 500*time.Millisecond)
		assertTicketAccounting(t, wr, "after tick 2 eviction")

		if resp := pollStatus(r, toks[capSize]); !resp.Ready {
			t.Fatalf("ticket #%d expected ready=true, got %+v", capSize+1, resp)
		}
		w, newTok := serveWithCookie(r, toks[capSize])
		if w.Code != http.StatusOK || w.Body.String() != "admitted" || newTok != "" {
			t.Fatalf("ticket #%d admission failed: code=%d body=%q newTok=%q", capSize+1, w.Code, w.Body.String(), newTok)
		}
		if got := admitted.Load(); got != capSize+1 {
			t.Errorf("admitted count = %d, want %d", got, capSize+1)
		}
		assertTicketAccounting(t, wr, "after tick 2 admission")
	})
}

func TestReadyGrace_ContinuousPollingCannotPinSlotForever(t *testing.T) {
	t.Run("ready_grace_enabled", func(t *testing.T) {
		const readyGrace = 10 * time.Second
		clock := newFakeClock(time.Now())
		wr := newTestWRWithClock(t, 1, clock, WithReadyGrace(readyGrace))

		var evicts atomic.Int32
		wr.On(EventEvict, func(Snapshot) { evicts.Add(1) })

		serving := make(chan struct{}, 1)
		release := make(chan struct{})
		r := newTestRouter(wr, serving, release)

		finished := make(chan struct{}, 1)
		go func() {
			serveWithCookie(r, "")
			finished <- struct{}{}
		}()
		select {
		case <-serving:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out filling slot")
		}

		_, tok1 := serveWithCookie(r, "")
		_, tok2 := serveWithCookie(r, "")
		if tok1 == "" || tok2 == "" {
			t.Fatalf("expected tokens for queued arrivals, got %q and %q", tok1, tok2)
		}

		// Initial waiting poll anchors issuedAt (LastSeen).
		preReadyTime := clock.Advance(2 * time.Second)
		if resp := pollStatus(r, tok1); resp.Ready {
			t.Fatal("tok1 unexpectedly ready while room full")
		}
		if resp := pollStatus(r, tok2); resp.Ready {
			t.Fatal("tok2 unexpectedly ready while room full")
		}

		// Free the slot so tok1 enters the serving window (nowServing=1, cap=1, edge=2).
		close(release)
		<-finished

		firstReadyTime := clock.Advance(2 * time.Second)
		if resp := pollStatus(r, tok1); !resp.Ready {
			t.Fatalf("tok1 first ready poll: expected ready=true, got %+v", resp)
		}
		ti, ok := wr.Ticket(tok1)
		if !ok {
			t.Fatal("tok1 missing immediately after first ready poll")
		}
		if !ti.LastSeen.Equal(preReadyTime) {
			t.Errorf("ready poll advanced issuedAt: got %v, want %v", ti.LastSeen, preReadyTime)
		}
		if !ti.ReadyAt.Equal(firstReadyTime) {
			t.Errorf("first ready poll ReadyAt = %v, want %v", ti.ReadyAt, firstReadyTime)
		}

		// Continuously poll every 1s up to readyGrace without reloading.
		for sec := 1; sec <= int(readyGrace/time.Second); sec++ {
			now := clock.Advance(time.Second)
			w := pollStatusRaw(r, tok1)
			if w.Code != http.StatusOK {
				t.Fatalf("sec %d: expected 200, got %d", sec, w.Code)
			}
			var resp statusResponse
			if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
				t.Fatalf("sec %d: decode error: %v", sec, err)
			}
			if !resp.Ready {
				t.Fatalf("sec %d: expected ready=true, got %+v", sec, resp)
			}
			cur, ok := wr.Ticket(tok1)
			if !ok {
				t.Fatalf("sec %d: tok1 evicted prematurely (elapsed=%ds <= %s)", sec, sec, readyGrace)
			}
			if !cur.LastSeen.Equal(preReadyTime) {
				t.Errorf("sec %d: issuedAt advanced to %v, want pinned %v", sec, cur.LastSeen, preReadyTime)
			}
			if !cur.ReadyAt.Equal(firstReadyTime) {
				t.Errorf("sec %d: ReadyAt drifted to %v, want pinned %v", sec, cur.ReadyAt, firstReadyTime)
			}
			if !cur.LastPoll.Equal(now) {
				t.Errorf("sec %d: LastPoll = %v, want %v", sec, cur.LastPoll, now)
			}
		}

		// First poll strictly after readyGrace elapses evicts tok1 without needing reap().
		clock.Advance(time.Second)
		w := pollStatusRaw(r, tok1)
		if w.Code != http.StatusOK {
			t.Fatalf("evicting poll: expected 200, got %d", w.Code)
		}
		if _, ok := wr.tokens.get(tok1); ok {
			t.Fatal("tok1 must be removed from store on first poll after readyGrace")
		}
		waitForCount(t, &evicts, 1, 500*time.Millisecond)
		if ns := wr.nowServing.Load(); ns != 2 {
			t.Errorf("nowServing = %d, want 2 after tok1 eviction", ns)
		}
		if resp := pollStatus(r, tok2); !resp.Ready {
			t.Errorf("tok2 expected ready=true after tok1 eviction, got %+v", resp)
		}
		assertTicketAccounting(t, wr, "ready_grace_enabled end")
	})

	t.Run("ready_grace_disabled_expires_at_token_ttl", func(t *testing.T) {
		const ttl = tokenTTLMin // 30s
		clock := newFakeClock(time.Now())
		wr := newTestWRWithClock(t, 1, clock)
		if err := wr.SetTokenTTL(ttl); err != nil {
			t.Fatal(err)
		}
		if wr.ReadyGrace() != 0 {
			t.Fatalf("expected ReadyGrace disabled, got %s", wr.ReadyGrace())
		}

		var evicts atomic.Int32
		wr.On(EventEvict, func(Snapshot) { evicts.Add(1) })

		serving := make(chan struct{}, 1)
		release := make(chan struct{})
		r := newTestRouter(wr, serving, release)

		finished := make(chan struct{}, 1)
		go func() {
			serveWithCookie(r, "")
			finished <- struct{}{}
		}()
		select {
		case <-serving:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out filling slot")
		}

		_, tok1 := serveWithCookie(r, "")
		_, tok2 := serveWithCookie(r, "")

		// Waiting poll anchors tok1's issuedAt before it becomes ready.
		anchor := clock.Advance(2 * time.Second)
		if resp := pollStatus(r, tok1); resp.Ready {
			t.Fatal("tok1 unexpectedly ready while room full")
		}
		if resp := pollStatus(r, tok2); resp.Ready {
			t.Fatal("tok2 unexpectedly ready while room full")
		}

		close(release)
		<-finished

		// Continuously poll tok1 (inside window) and tok2 (outside window) every 2s
		// up to TokenTTL. tok1's issuedAt must stay pinned at anchor while tok2 refreshes.
		for elapsed := 2 * time.Second; elapsed <= ttl; elapsed += 2 * time.Second {
			now := clock.Advance(2 * time.Second)
			if resp := pollStatus(r, tok1); !resp.Ready {
				t.Fatalf("at +%s: tok1 expected ready=true, got %+v", elapsed, resp)
			}
			cur1, ok := wr.Ticket(tok1)
			if !ok {
				t.Fatalf("at +%s: tok1 evicted before TokenTTL (%s)", elapsed, ttl)
			}
			if !cur1.LastSeen.Equal(anchor) {
				t.Fatalf("at +%s: tok1 issuedAt advanced to %v while ready with ReadyGrace=0 (want %v)",
					elapsed, cur1.LastSeen, anchor)
			}

			// Keep waiting tok2 alive outside the window.
			if resp := pollStatus(r, tok2); resp.Ready {
				t.Fatalf("at +%s: tok2 unexpectedly ready before tok1 expired", elapsed)
			}
			if cur2, ok := wr.Ticket(tok2); !ok || !cur2.LastSeen.Equal(now) {
				t.Fatalf("at +%s: waiting tok2 failed to refresh issuedAt: ok=%v lastSeen=%v want=%v",
					elapsed, ok, cur2.LastSeen, now)
			}
		}

		// Cross TokenTTL: tok1 expires on poll rather than pinning the slot forever.
		clock.Advance(2 * time.Second)
		w := pollStatusRaw(r, tok1)
		if w.Code != http.StatusOK {
			t.Fatalf("expiring poll: expected 200, got %d", w.Code)
		}
		if _, ok := wr.tokens.get(tok1); ok {
			t.Fatal("tok1 pinned slot past TokenTTL even with ReadyGrace=0")
		}
		waitForCount(t, &evicts, 1, 500*time.Millisecond)
		if ns := wr.nowServing.Load(); ns != 2 {
			t.Errorf("nowServing = %d, want 2 after tok1 TokenTTL eviction", ns)
		}
		if resp := pollStatus(r, tok2); !resp.Ready {
			t.Errorf("tok2 expected ready=true after tok1 expired at TokenTTL, got %+v", resp)
		}
		assertTicketAccounting(t, wr, "ready_grace_disabled end")
	})
}

func TestReadyGrace_SetCapShrinkClearsReadyAt(t *testing.T) {
	const readyGrace = 10 * time.Second
	clock := newFakeClock(time.Now())
	wr := newTestWRWithClock(t, 2, clock, WithReadyGrace(readyGrace))

	gates := []chan struct{}{make(chan struct{}), make(chan struct{})}
	serving := make(chan struct{}, 2)
	finished := make(chan struct{}, 2)
	r := newTestRouterWithGates(wr, serving, gates)

	for i := 0; i < 2; i++ {
		go func() {
			serveWithCookie(r, "")
			finished <- struct{}{}
		}()
		select {
		case <-serving:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out filling slot %d", i)
		}
	}

	// Ticket 3 queues behind the two active slots.
	_, tok := serveWithCookie(r, "")
	if tok == "" {
		t.Fatal("expected queued token")
	}
	t0 := clock.Advance(2 * time.Second)
	if resp := pollStatus(r, tok); resp.Ready || resp.Position != 1 {
		t.Fatalf("initial waiting poll: expected ready=false pos=1, got %+v", resp)
	}

	// Release one slot: nowServing=1, cap=2, edge=3 -> ticket 3 enters window.
	close(gates[0])
	<-finished

	t1 := clock.Advance(2 * time.Second)
	if resp := pollStatus(r, tok); !resp.Ready {
		t.Fatalf("expected ticket 3 ready after slot freed, got %+v", resp)
	}
	ti, ok := wr.Ticket(tok)
	if !ok {
		t.Fatal("ticket 3 missing after ready poll")
	}
	if !ti.ReadyAt.Equal(t1) {
		t.Fatalf("ReadyAt = %v, want %v", ti.ReadyAt, t1)
	}
	if !ti.LastSeen.Equal(t0) {
		t.Fatalf("LastSeen = %v, want %v (not refreshed while ready)", ti.LastSeen, t0)
	}

	// Consume 6s of the 10s ReadyGrace, then shrink cap 2 -> 1 so edge drops to 2
	// and ticket 3 is pushed back outside the serving window.
	t2 := clock.Advance(6 * time.Second)
	if err := wr.SetCap(1); err != nil {
		t.Fatalf("SetCap(1): %v", err)
	}

	if resp := pollStatus(r, tok); resp.Ready || resp.Position != 1 {
		t.Fatalf("post-shrink poll: expected ready=false pos=1, got %+v", resp)
	}
	ti, ok = wr.Ticket(tok)
	if !ok {
		t.Fatal("ticket 3 missing after post-shrink poll")
	}
	if !ti.ReadyAt.IsZero() {
		t.Errorf("post-shrink poll did not clear ReadyAt: got %v", ti.ReadyAt)
	}
	if !ti.LastSeen.Equal(t2) {
		t.Errorf("post-shrink poll did not resume refreshing issuedAt: got %v, want %v", ti.LastSeen, t2)
	}

	// Advance another 6s (now 12s past t1, exceeding the old readyGrace) and reap:
	// ticket 3 is outside the window and must survive.
	clock.Advance(6 * time.Second)
	wr.reap()
	if _, ok := wr.tokens.get(tok); !ok {
		t.Fatal("ticket 3 evicted after shrink despite being pushed out of window")
	}

	// Expand cap 1 -> 2: ticket 3 re-enters the window and gets a fresh readyGrace.
	if err := wr.SetCap(2); err != nil {
		t.Fatalf("SetCap(2): %v", err)
	}
	t3 := clock.Advance(time.Second)
	if resp := pollStatus(r, tok); !resp.Ready {
		t.Fatalf("post-expand poll: expected ready=true, got %+v", resp)
	}
	ti, ok = wr.Ticket(tok)
	if !ok || !ti.ReadyAt.Equal(t3) {
		t.Fatalf("post-expand ReadyAt = %v (ok=%v), want fresh %v", ti.ReadyAt, ok, t3)
	}

	// 9s into the fresh 10s window (22s past t1): still alive.
	clock.Advance(readyGrace - time.Second)
	wr.reap()
	if ti, ok = wr.Ticket(tok); !ok || !ti.ReadyAt.Equal(t3) {
		t.Fatalf("ticket 3 did not get full fresh readyGrace window: ok=%v readyAt=%v", ok, ti.ReadyAt)
	}

	// Past the fresh 10s window: now evicted.
	clock.Advance(2 * time.Second)
	wr.reap()
	if _, ok := wr.tokens.get(tok); ok {
		t.Fatal("ticket 3 should be evicted once fresh readyGrace elapses")
	}

	// Clean up remaining active handler and verify exact accounting.
	close(gates[1])
	<-finished
	if occ := wr.occupancy.Load(); occ != 0 {
		t.Errorf("occupancy = %d, want 0", occ)
	}
	assertTicketAccounting(t, wr, "SetCap shrink/expand end")
}

func TestReadyGrace_LargeCapLiveWindowDoesNotLoopReap(t *testing.T) {
	const (
		largeCap   = reaperBatchSize + 200 // 1200
		readyGrace = 10 * time.Second
	)

	clock := newFakeClock(time.Now())
	wr := newTestWRWithClock(t, largeCap, clock, WithReadyGrace(readyGrace))

	t0 := clock.Now()
	wr.nextTicket.Store(largeCap)
	for i := 1; i <= largeCap; i++ {
		wr.tokens.set(fmt.Sprintf("live-%d", i), ticketEntry{
			ticket:    int64(i),
			createdAt: t0,
			issuedAt:  t0,
			seen:      true,
		})
	}

	// First reap stamps readyAt on all 1200 in-window tickets and must exit promptly
	// without looping infinitely (stamping does not count toward reaperBatchSize).
	runWithTimeout(t, 2*time.Second, "reap stamping 1200 live in-window tickets", wr.reap)

	if got := wr.tokens.len(); got != largeCap {
		t.Fatalf("expected 0 evicted (live=%d), got live=%d", largeCap, got)
	}
	if ns := wr.nowServing.Load(); ns != 0 {
		t.Errorf("expected nowServing=0, got %d", ns)
	}
	for i := 1; i <= largeCap; i++ {
		tok := fmt.Sprintf("live-%d", i)
		ti, ok := wr.Ticket(tok)
		if !ok {
			t.Fatalf("missing ticket %s after stamp pass", tok)
		}
		if !ti.ReadyAt.Equal(t0) {
			t.Fatalf("ticket %s: ReadyAt = %v, want %v", tok, ti.ReadyAt, t0)
		}
	}

	// Advance past readyGrace and verify multi-batch eviction drains all 1200 cleanly.
	clock.Advance(readyGrace + time.Second)
	runWithTimeout(t, 2*time.Second, "reap evicting 1200 expired ready tickets", wr.reap)

	if got := wr.tokens.len(); got != 0 {
		t.Errorf("expected all 1200 expired ready tickets evicted, got %d remaining", got)
	}
	if ns := wr.nowServing.Load(); ns != largeCap {
		t.Errorf("nowServing = %d, want %d", ns, largeCap)
	}
	assertTicketAccounting(t, wr, "large cap end")
}

func TestReadyGrace_SkipReadyGraceNotEvicted(t *testing.T) {
	t.Parallel()
	const readyGrace = 10 * time.Second
	clock := newFakeClock(time.Now())
	wr := newTestWRWithClock(t, 2, clock, WithReadyGrace(readyGrace))

	t0 := clock.Now()
	wr.nextTicket.Store(2)
	wr.tokens.set("skip-tok", ticketEntry{
		ticket:         1,
		createdAt:      t0,
		issuedAt:       t0,
		seen:           true,
		skipReadyGrace: true,
	})
	wr.tokens.set("normal-tok", ticketEntry{
		ticket:    2,
		createdAt: t0,
		issuedAt:  t0,
		seen:      true,
	})

	wr.reap()
	clock.Advance(readyGrace + time.Second)
	wr.reap()

	if ti, ok := wr.Ticket("skip-tok"); !ok {
		t.Error("skipReadyGrace ticket must not be evicted by ReadyGrace")
	} else if !ti.ReadyAt.IsZero() {
		t.Errorf("skipReadyGrace ticket must not have ReadyAt stamped by reap, got %v", ti.ReadyAt)
	}
	if _, ok := wr.tokens.get("normal-tok"); ok {
		t.Error("normal in-window ticket should have been evicted past ReadyGrace")
	}
	assertTicketAccounting(t, wr, "skipReadyGrace end")
}

// 3. Core IdlePollTTL regressions

func TestIdlePollTTL_ReclaimsSilentSeenTicketsBeforeTokenTTL(t *testing.T) {
	const (
		tokenTTL    = 5 * time.Minute
		idlePollTTL = 30 * time.Second
	)

	t.Run("active_poller_survives_past_idle_and_token_ttl", func(t *testing.T) {
		clock := newFakeClock(time.Now())
		wr := newTestWRWithClock(t, 1, clock, WithIdlePollTTL(idlePollTTL))
		if err := wr.SetTokenTTL(tokenTTL); err != nil {
			t.Fatal(err)
		}

		serving := make(chan struct{}, 1)
		release := make(chan struct{})
		r := newTestRouter(wr, serving, release)
		defer close(release)
		fillOneSlot(t, r, serving)

		_, activeTok := serveWithCookie(r, "")
		if activeTok == "" {
			t.Fatal("expected token for queued client")
		}

		// Poll every 15s for 6m (12x IdlePollTTL and past 5m TokenTTL).
		for step := 1; step <= 24; step++ {
			now := clock.Advance(15 * time.Second)
			w := pollStatusRaw(r, activeTok)
			if w.Code != http.StatusOK {
				t.Fatalf("step %d: expected 200, got %d", step, w.Code)
			}
			var resp statusResponse
			if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
				t.Fatalf("step %d: decode: %v", step, err)
			}
			if resp.Ready || resp.Position != 1 {
				t.Fatalf("step %d: expected ready=false pos=1, got %+v", step, resp)
			}
			wr.reap()
			ti, ok := wr.Ticket(activeTok)
			if !ok {
				t.Fatalf("step %d (%s): active poller evicted", step, time.Duration(step)*15*time.Second)
			}
			if !ti.LastSeen.Equal(now) {
				t.Errorf("step %d: LastSeen = %v, want %v", step, ti.LastSeen, now)
			}
		}
	})

	t.Run("silent_seen_evicted_by_reap_and_late_poll", func(t *testing.T) {
		clock := newFakeClock(time.Now())
		wr := newTestWRWithClock(t, 1, clock, WithIdlePollTTL(idlePollTTL))
		if err := wr.SetTokenTTL(tokenTTL); err != nil {
			t.Fatal(err)
		}

		var evicts atomic.Int32
		wr.On(EventEvict, func(Snapshot) { evicts.Add(1) })

		serving := make(chan struct{}, 1)
		release := make(chan struct{})
		r := newTestRouter(wr, serving, release)
		finished := make(chan struct{}, 1)
		go func() {
			serveWithCookie(r, "")
			finished <- struct{}{}
		}()
		select {
		case <-serving:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out filling slot")
		}

		_, silentReapTok := serveWithCookie(r, "")
		_, silentPollTok := serveWithCookie(r, "")
		_, activeTok := serveWithCookie(r, "")

		// Mark all three seen while room is full.
		clock.Advance(2 * time.Second)
		for name, tok := range map[string]string{
			"silentReap": silentReapTok,
			"silentPoll": silentPollTok,
			"active":     activeTok,
		} {
			if resp := pollStatus(r, tok); resp.Ready {
				t.Fatalf("%s unexpectedly ready while room full", name)
			}
			if ti, ok := wr.Ticket(tok); !ok || !ti.Seen {
				t.Fatalf("%s expected seen=true, got ok=%v seen=%v", name, ok, ti.Seen)
			}
		}

		// After 15s, keep activeTok and silentReapTok fresh while silentPollTok stays silent.
		clock.Advance(15 * time.Second)
		pollStatus(r, activeTok)
		pollStatus(r, silentReapTok)

		// Advance 16s: silentPollTok is silent for 31s (> 30s IdlePollTTL, << 5m TokenTTL);
		// silentReapTok and activeTok have been silent for only 16s.
		clock.Advance(16 * time.Second)
		if resp := pollStatus(r, activeTok); resp.Ready {
			t.Fatal("activeTok unexpectedly ready")
		}

		// Late /queue/status poll on silentPollTok evicts it immediately without reap().
		w := pollStatusRaw(r, silentPollTok)
		if w.Code != http.StatusOK {
			t.Fatalf("late poll: expected 200, got %d", w.Code)
		}
		if _, ok := wr.tokens.get(silentPollTok); ok {
			t.Fatal("silentPollTok should be evicted on late /queue/status poll past IdlePollTTL")
		}
		waitForCount(t, &evicts, 1, 500*time.Millisecond)
		if _, ok := wr.tokens.get(silentReapTok); !ok {
			t.Fatal("silentReapTok (16s idle) should still be alive")
		}

		// Advance another 15s: silentReapTok is now silent for 31s (> 30s IdlePollTTL).
		clock.Advance(15 * time.Second)
		if resp := pollStatus(r, activeTok); resp.Ready || resp.Position != 2 {
			t.Errorf("activeTok before reap: expected ready=false pos=2, got %+v", resp)
		}
		wr.reap()

		if _, ok := wr.tokens.get(silentReapTok); ok {
			t.Fatal("silentReapTok should be evicted by reap() past IdlePollTTL")
		}
		clock.Advance(time.Second)
		if resp := pollStatus(r, activeTok); resp.Ready || resp.Position != 1 {
			t.Errorf("activeTok after silent evictions: expected ready=false pos=1, got %+v", resp)
		}
		waitForCount(t, &evicts, 2, 500*time.Millisecond)

		close(release)
		<-finished
		assertTicketAccounting(t, wr, "silent seen end")
	})

	t.Run("unseen_capped_by_idle_poll_ttl_and_earlier_first_poll_grace", func(t *testing.T) {
		clock := newFakeClock(time.Now())
		wr := newTestWRWithClock(t, 1, clock, WithIdlePollTTL(idlePollTTL))
		if err := wr.SetTokenTTL(tokenTTL); err != nil {
			t.Fatal(err)
		}

		t0 := clock.Now()
		wr.nextTicket.Store(10)
		// FirstPollGrace is 0: unseen ticket is still capped by IdlePollTTL (30s < 5m TokenTTL).
		wr.tokens.set("unseen-idle", ticketEntry{
			ticket:    10,
			createdAt: t0,
			issuedAt:  t0,
			seen:      false,
		})

		clock.Advance(idlePollTTL - time.Second)
		wr.reap()
		if _, ok := wr.tokens.get("unseen-idle"); !ok {
			t.Fatal("unseen ticket evicted before IdlePollTTL")
		}

		clock.Advance(2 * time.Second)
		wr.reap()
		if _, ok := wr.tokens.get("unseen-idle"); ok {
			t.Fatal("unseen ticket silent past IdlePollTTL must be evicted even with FirstPollGrace=0")
		}

		// With FirstPollGrace = 10s (< 30s IdlePollTTL), unseen tickets are reclaimed
		// earlier at 10s while seen tickets survive until 30s IdlePollTTL.
		if err := wr.SetFirstPollGrace(10 * time.Second); err != nil {
			t.Fatal(err)
		}
		t1 := clock.Now()
		wr.nextTicket.Store(12)
		wr.tokens.set("unseen-grace", ticketEntry{
			ticket:    11,
			createdAt: t1,
			issuedAt:  t1,
			seen:      false,
		})
		wr.tokens.set("seen-idle", ticketEntry{
			ticket:    12,
			createdAt: t1,
			issuedAt:  t1,
			seen:      true,
		})

		clock.Advance(11 * time.Second)
		wr.reap()
		if _, ok := wr.tokens.get("unseen-grace"); ok {
			t.Error("unseen ticket should be reclaimed early at 10s by FirstPollGrace")
		}
		if _, ok := wr.tokens.get("seen-idle"); !ok {
			t.Error("seen ticket must survive FirstPollGrace until IdlePollTTL")
		}

		clock.Advance(20 * time.Second) // 31s total > 30s IdlePollTTL
		wr.reap()
		if _, ok := wr.tokens.get("seen-idle"); ok {
			t.Error("seen ticket should be reclaimed at 30s by IdlePollTTL")
		}
	})
}

func TestIdlePollTTL_RuntimeToggleZeroHeapRebuild(t *testing.T) {
	t.Parallel()
	clock := newFakeClock(time.Now())
	wr := newTestWRWithClock(t, 1, clock) // TokenTTL=5m, IdlePollTTL=0

	now := clock.Now()
	const (
		staggeredCount = 48 // ages 5s, 10s, ..., 240s (all < 5m TokenTTL)
		refreshedCount = 12 // inserted at -200s, then polled at now (stale ttlKey exercises fix)
	)
	total := 1 + staggeredCount + refreshedCount
	wr.nextTicket.Store(int64(total))

	// Ticket 1 sits in window as a fresh live anchor so tickets 2..total are all out-of-window.
	wr.tokens.set("anchor", ticketEntry{
		ticket:    1,
		createdAt: now,
		issuedAt:  now,
		seen:      true,
	})

	ticketNum := int64(2)
	for i := 1; i <= staggeredCount; i++ {
		age := time.Duration(i) * 5 * time.Second
		wr.tokens.set(fmt.Sprintf("age-%ds", i*5), ticketEntry{
			ticket:    ticketNum,
			createdAt: now.Add(-age),
			issuedAt:  now.Add(-age),
			seen:      true,
		})
		ticketNum++
	}
	for i := 0; i < refreshedCount; i++ {
		tok := fmt.Sprintf("refreshed-%d", i)
		wr.tokens.set(tok, ticketEntry{
			ticket:    ticketNum,
			createdAt: now.Add(-200 * time.Second),
			issuedAt:  now.Add(-200 * time.Second),
			seen:      true,
		})
		// Poll refreshes node.issuedAt to now while leaving ttlKey at now-200s.
		res := wr.tokens.poll(tok, now, 1, statusPollMinInterval, DefaultTokenTTL, 0, 0)
		if res.outcome != pollAccepted {
			t.Fatalf("refresh poll %s: unexpected outcome %v", tok, res.outcome)
		}
		ticketNum++
	}

	// Baseline: IdlePollTTL=0, all tickets are < 5m TokenTTL so none evicted.
	wr.reap()
	if got := wr.tokens.len(); got != total {
		t.Fatalf("baseline: expected %d live tokens, got %d", total, got)
	}
	assertNodeHeapValid(t, wr.tokens)
	assertTicketAccounting(t, wr, "baseline")

	// Dynamically lower IdlePollTTL to 120s (2m): ages 125s..240s (24 tickets) evicted immediately;
	// ages 5s..120s (24 tickets) + 12 refreshed + anchor = 37 survive.
	if err := wr.SetIdlePollTTL(2 * time.Minute); err != nil {
		t.Fatal(err)
	}
	wr.reap()

	wantAfter120s := 1 + 24 + refreshedCount
	if got := wr.tokens.len(); got != wantAfter120s {
		t.Errorf("after lowering IdlePollTTL to 2m: live=%d, want %d", got, wantAfter120s)
	}
	for i := 1; i <= staggeredCount; i++ {
		ageSec := i * 5
		_, ok := wr.tokens.get(fmt.Sprintf("age-%ds", ageSec))
		if ageSec > 120 && ok {
			t.Errorf("age-%ds should have been evicted at IdlePollTTL=2m", ageSec)
		}
		if ageSec <= 120 && !ok {
			t.Errorf("age-%ds should have survived at IdlePollTTL=2m", ageSec)
		}
	}
	for i := 0; i < refreshedCount; i++ {
		if _, ok := wr.tokens.get(fmt.Sprintf("refreshed-%d", i)); !ok {
			t.Errorf("refreshed-%d should have survived via lazy ttlHeap.fix", i)
		}
	}
	assertNodeHeapValid(t, wr.tokens)
	assertTicketAccounting(t, wr, "after lowering to 2m")

	// Dynamically lower again to 30s: ages 35s..120s (18 more tickets) evicted immediately;
	// ages 5s..30s (6 tickets) + 12 refreshed + anchor = 19 survive.
	if err := wr.SetIdlePollTTL(30 * time.Second); err != nil {
		t.Fatal(err)
	}
	wr.reap()

	wantAfter30s := 1 + 6 + refreshedCount
	if got := wr.tokens.len(); got != wantAfter30s {
		t.Errorf("after lowering IdlePollTTL to 30s: live=%d, want %d", got, wantAfter30s)
	}
	for i := 1; i <= 24; i++ {
		ageSec := i * 5
		_, ok := wr.tokens.get(fmt.Sprintf("age-%ds", ageSec))
		if ageSec > 30 && ok {
			t.Errorf("age-%ds should have been evicted at IdlePollTTL=30s", ageSec)
		}
		if ageSec <= 30 && !ok {
			t.Errorf("age-%ds should have survived at IdlePollTTL=30s", ageSec)
		}
	}
	assertNodeHeapValid(t, wr.tokens)
	assertTicketAccounting(t, wr, "after lowering to 30s")

	// Dynamically disable IdlePollTTL (0) and advance 40s: survivors are still well within 5m TokenTTL.
	if err := wr.SetIdlePollTTL(0); err != nil {
		t.Fatal(err)
	}
	clock.Advance(40 * time.Second)
	wr.reap()
	if got := wr.tokens.len(); got != wantAfter30s {
		t.Errorf("after disabling IdlePollTTL: live=%d, want %d", got, wantAfter30s)
	}
	assertNodeHeapValid(t, wr.tokens)
	assertTicketAccounting(t, wr, "after disabling IdlePollTTL")
}

// 4. Concurrency / -race

func TestReadyGraceAndIdlePollTTL_ConcurrentPollReapAndConfig(t *testing.T) {
	clock := newFakeClock(time.Now())
	wr := newTestWRWithClock(t, 4, clock,
		WithReadyGrace(5*time.Second),
		WithIdlePollTTL(10*time.Second),
	)
	if err := wr.SetTokenTTL(tokenTTLMin); err != nil {
		t.Fatal(err)
	}

	var holdInitial atomic.Bool
	holdInitial.Store(true)
	serving := make(chan struct{}, 4)
	initialRelease := make(chan struct{})

	r := gin.New()
	wr.RegisterRoutes(r)
	r.GET("/", func(c *gin.Context) {
		if holdInitial.Load() {
			serving <- struct{}{}
			<-initialRelease
		}
		c.Status(http.StatusOK)
	})

	// Hold initial cap=4 slots to build a deterministic backlog of queued tickets.
	var initWG sync.WaitGroup
	for i := 0; i < 4; i++ {
		initWG.Add(1)
		go func() {
			defer initWG.Done()
			serveWithCookie(r, "")
		}()
		select {
		case <-serving:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out filling initial slot")
		}
	}

	const initialQueued = 80
	toks := make([]string, initialQueued)
	for i := 0; i < initialQueued; i++ {
		_, tok := serveWithCookie(r, "")
		if tok == "" {
			t.Fatalf("setup arrival %d not queued", i)
		}
		toks[i] = tok
	}
	// Mark half seen before releasing the initial holders.
	clock.Advance(2 * time.Second)
	for i := 0; i < initialQueued; i += 2 {
		pollStatusRaw(r, toks[i])
	}

	holdInitial.Store(false)
	close(initialRelease)
	initWG.Wait()

	var wg sync.WaitGroup

	// Concurrent pollers hitting /queue/status across all queued tokens.
	for p := 0; p < 6; p++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for iter := 0; iter < 60; iter++ {
				tok := toks[(id*13+iter)%len(toks)]
				pollStatusRaw(r, tok)
			}
		}(p)
	}

	// Concurrent admissions: returning queued tokens + fresh arrivals.
	for a := 0; a < 4; a++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for iter := 0; iter < 40; iter++ {
				if iter%4 == 0 {
					serveWithCookie(r, "")
				} else {
					tok := toks[(id*17+iter)%len(toks)]
					serveWithCookie(r, tok)
				}
			}
		}(a)
	}

	// Concurrent reapers.
	for k := 0; k < 4; k++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for iter := 0; iter < 40; iter++ {
				wr.reap()
			}
		}()
	}

	// Concurrent runtime config changes: SetReadyGrace, SetIdlePollTTL, SetCap.
	wg.Add(2)
	go func() {
		defer wg.Done()
		graces := []time.Duration{0, 5 * time.Second, 10 * time.Second, 30 * time.Second}
		idles := []time.Duration{0, 10 * time.Second, 20 * time.Second, time.Minute}
		for iter := 0; iter < 60; iter++ {
			if err := wr.SetReadyGrace(graces[iter%len(graces)]); err != nil {
				t.Errorf("SetReadyGrace: %v", err)
			}
			if err := wr.SetIdlePollTTL(idles[iter%len(idles)]); err != nil {
				t.Errorf("SetIdlePollTTL: %v", err)
			}
		}
	}()
	go func() {
		defer wg.Done()
		caps := []int32{2, 4, 8, 12, 3, 6}
		for iter := 0; iter < 60; iter++ {
			if err := wr.SetCap(caps[iter%len(caps)]); err != nil {
				t.Errorf("SetCap: %v", err)
			}
		}
	}()

	// Concurrent read-only observers inspecting TicketInfo.ReadyAt and queue state.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for iter := 0; iter < 60; iter++ {
			_ = wr.Queue(16)
			_, _ = wr.Ticket(toks[iter%len(toks)])
			_ = wr.QueueDepth()
			_ = wr.LiveQueueDepth()
		}
	}()

	// Advance fake clock concurrently across ReadyGrace, IdlePollTTL, and TokenTTL thresholds.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for step := 0; step < 25; step++ {
			clock.Advance(2 * time.Second)
			time.Sleep(time.Millisecond)
		}
	}()

	wg.Wait()

	if occ := wr.occupancy.Load(); occ != 0 {
		t.Fatalf("expected occupancy 0 after all workers finished, got %d", occ)
	}
	assertNodeHeapValid(t, wr.tokens)
	assertTicketAccounting(t, wr, "after concurrent churn")

	// Drain any survivors past TokenTTL and verify accounting reaches nextTicket cleanly.
	clock.Advance(time.Hour)
	wr.reap()
	if live := wr.tokens.len(); live != 0 {
		t.Errorf("expected 0 live tokens after final drain, got %d", live)
	}
	if led := wr.ledger.len(); led != 0 {
		t.Errorf("expected 0 ledger ghosts after final drain, got %d", led)
	}
	if ns, next := wr.nowServing.Load(), wr.nextTicket.Load(); ns != next {
		t.Errorf("final drain: nowServing=%d != nextTicket=%d", ns, next)
	}
	assertNodeHeapValid(t, wr.tokens)
}

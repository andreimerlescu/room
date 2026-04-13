package room

import (
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ── Event.String ─────────────────────────────────────────────────────────────

func TestEvent_String(t *testing.T) {
	t.Parallel()
	cases := []struct {
		event Event
		want  string
	}{
		{EventEnter, "Enter"},
		{EventExit, "Exit"},
		{EventFull, "Full"},
		{EventDrain, "Drain"},
		{EventQueue, "Queue"},
		{EventEvict, "Evict"},
		{EventTimeout, "Timeout"},
		{EventPromote, "Promote"},
		{Event(255), "Unknown"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.want, func(t *testing.T) {
			t.Parallel()
			if got := tc.event.String(); got != tc.want {
				t.Errorf("Event(%d).String() = %q, want %q", tc.event, got, tc.want)
			}
		})
	}
}

// ── Snapshot helpers ──────────────────────────────────────────────────────────

func TestSnapshot_Full(t *testing.T) {
	t.Parallel()
	s := Snapshot{Event: EventFull, Occupancy: 10, Capacity: 10}
	if !s.Full() {
		t.Error("expected Full() == true when Occupancy == Capacity")
	}
	s.Occupancy = 9
	if s.Full() {
		t.Error("expected Full() == false when Occupancy < Capacity")
	}
}

func TestSnapshot_Empty(t *testing.T) {
	t.Parallel()
	s := Snapshot{Event: EventDrain, Occupancy: 0, Capacity: 10}
	if !s.Empty() {
		t.Error("expected Empty() == true when Occupancy == 0")
	}
	s.Occupancy = 1
	if s.Empty() {
		t.Error("expected Empty() == false when Occupancy > 0")
	}
}

// ── On / emit — basic correctness ────────────────────────────────────────────

func TestOn_SingleHandler_Called(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 5)

	var called atomic.Int32
	wr.On(EventFull, func(s Snapshot) { called.Add(1) })
	wr.emit(EventFull, wr.snapshot(EventFull))

	waitForCount(t, &called, 1, 100*time.Millisecond)
}

func TestOn_MultipleHandlers_AllCalled(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 5)

	var count atomic.Int32
	for range 5 {
		wr.On(EventEnter, func(s Snapshot) { count.Add(1) })
	}
	wr.emit(EventEnter, wr.snapshot(EventEnter))

	waitForCount(t, &count, 5, 100*time.Millisecond)
}

func TestOn_DifferentEvents_DoNotCross(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 5)

	var fullCalled, drainCalled atomic.Int32
	wr.On(EventFull, func(s Snapshot) { fullCalled.Add(1) })
	wr.On(EventDrain, func(s Snapshot) { drainCalled.Add(1) })

	wr.emit(EventFull, wr.snapshot(EventFull))
	waitForCount(t, &fullCalled, 1, 100*time.Millisecond)

	time.Sleep(20 * time.Millisecond)
	if drainCalled.Load() != 0 {
		t.Errorf("EventDrain handler fired but EventDrain was never emitted")
	}
}

// ── Off ───────────────────────────────────────────────────────────────────────

func TestOff_RemovesAllHandlers(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 5)

	var called atomic.Int32
	wr.On(EventEvict, func(s Snapshot) { called.Add(1) })
	wr.On(EventEvict, func(s Snapshot) { called.Add(1) })
	wr.Off(EventEvict)
	wr.emit(EventEvict, wr.snapshot(EventEvict))

	time.Sleep(30 * time.Millisecond)
	if called.Load() != 0 {
		t.Errorf("expected 0 calls after Off, got %d", called.Load())
	}
}

func TestOff_DoesNotAffectOtherEvents(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 5)

	var timeoutCalled, exitCalled atomic.Int32
	wr.On(EventTimeout, func(s Snapshot) { timeoutCalled.Add(1) })
	wr.On(EventExit, func(s Snapshot) { exitCalled.Add(1) })

	wr.Off(EventTimeout)
	wr.emit(EventTimeout, wr.snapshot(EventTimeout))
	wr.emit(EventExit, wr.snapshot(EventExit))

	waitForCount(t, &exitCalled, 1, 100*time.Millisecond)
	time.Sleep(20 * time.Millisecond)

	if timeoutCalled.Load() != 0 {
		t.Errorf("EventTimeout handler fired after Off, got %d calls", timeoutCalled.Load())
	}
}

// ── emit with no handlers ─────────────────────────────────────────────────────

func TestEmit_NoHandlers_IsNoop(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 5)
	// must not panic
	wr.emit(EventDrain, wr.snapshot(EventDrain))
}

// ── Snapshot payload correctness ──────────────────────────────────────────────

func TestEmit_SnapshotDeliveredCorrectly(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 10)

	got := make(chan Snapshot, 1)
	wr.On(EventQueue, func(s Snapshot) { got <- s })

	want := Snapshot{Event: EventQueue, Occupancy: 3, Capacity: 10, QueueDepth: 2}
	wr.emit(EventQueue, want)

	select {
	case s := <-got:
		if s != want {
			t.Errorf("snapshot mismatch:\n got  %+v\n want %+v", s, want)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("timed out waiting for callback")
	}
}

// ── snapshot() builds from live WaitingRoom state ────────────────────────────

func TestSnapshot_ReflectsLiveState(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 10)

	s := wr.snapshot(EventEnter)
	if s.Capacity != 10 {
		t.Errorf("expected Capacity 10, got %d", s.Capacity)
	}
	if s.Event != EventEnter {
		t.Errorf("expected Event EventEnter, got %s", s.Event)
	}
	if s.Occupancy < 0 {
		t.Errorf("Occupancy should not be negative, got %d", s.Occupancy)
	}
	if s.QueueDepth < 0 {
		t.Errorf("QueueDepth should not be negative, got %d", s.QueueDepth)
	}
}

// ── Concurrency safety ────────────────────────────────────────────────────────

func TestConcurrent_OnAndEmit(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 5)

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wr.On(EventEnter, func(s Snapshot) {})
		}()
	}
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wr.emit(EventEnter, wr.snapshot(EventEnter))
		}()
	}
	wg.Wait()
}

func TestConcurrent_OffAndEmit(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 5)
	wr.On(EventFull, func(s Snapshot) {})

	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wr.Off(EventFull)
		}()
	}
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wr.emit(EventFull, wr.snapshot(EventFull))
		}()
	}
	wg.Wait()
}

func TestConcurrent_OnOffEmit_AllEvents(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 5)

	events := []Event{EventEnter, EventExit, EventFull, EventDrain, EventQueue, EventEvict, EventTimeout, EventPromote}
	var wg sync.WaitGroup
	for _, ev := range events {
		ev := ev
		wg.Add(3)
		go func() { defer wg.Done(); wr.On(ev, func(s Snapshot) {}) }()
		go func() { defer wg.Done(); wr.emit(ev, wr.snapshot(ev)) }()
		go func() { defer wg.Done(); wr.Off(ev) }()
	}
	wg.Wait()
}

// ── Integration: EventFull fires on transition, not every admission ──────────

func TestIntegration_EventFull_FiredWhenRoomFull(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 1)

	var fullCount atomic.Int32
	wr.On(EventFull, func(s Snapshot) { fullCount.Add(1) })

	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	r := newTestRouter(wr, serving, release)

	go func() {
		req := httptest.NewRequest("GET", "/", nil)
		r.ServeHTTP(httptest.NewRecorder(), req)
	}()
	<-serving

	waitForCount(t, &fullCount, 1, 200*time.Millisecond)
	close(release)
}

// TestIntegration_EventFull_DoesNotFireRepeatedly verifies that EventFull
// fires only on the non-full→full transition, not on every admission while
// the room is already at capacity.
func TestIntegration_EventFull_DoesNotFireRepeatedly(t *testing.T) {
	t.Parallel()
	const cap = 2
	wr := newTestWR(t, int32(cap))

	var fullCount atomic.Int32
	wr.On(EventFull, func(s Snapshot) { fullCount.Add(1) })

	serving := make(chan struct{}, cap)
	release := make(chan struct{})
	r := newTestRouter(wr, serving, release)

	// Fill both slots.
	for i := 0; i < cap; i++ {
		go func() {
			req := httptest.NewRequest("GET", "/", nil)
			r.ServeHTTP(httptest.NewRecorder(), req)
		}()
	}
	for i := 0; i < cap; i++ {
		select {
		case <-serving:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for handler %d", i)
		}
	}

	// EventFull should have fired exactly once (on the transition to full).
	waitForCount(t, &fullCount, 1, 200*time.Millisecond)
	time.Sleep(50 * time.Millisecond)

	if got := fullCount.Load(); got != 1 {
		t.Errorf("expected EventFull to fire exactly 1 time, got %d", got)
	}

	close(release)
}

// ── Integration: EventDrain fires on full→not-full transition ─────────────────

func TestIntegration_EventDrain_FiredAfterRelease(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 1)

	var drainCount atomic.Int32
	wr.On(EventDrain, func(s Snapshot) { drainCount.Add(1) })

	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	r := newTestRouter(wr, serving, release)

	go func() {
		req := httptest.NewRequest("GET", "/", nil)
		r.ServeHTTP(httptest.NewRecorder(), req)
	}()
	<-serving
	close(release)

	waitForCount(t, &drainCount, 1, 200*time.Millisecond)
}

// TestIntegration_EventDrain_OnlyFiresOnTransition verifies that EventDrain
// fires only on the full→not-full transition, not when the room goes from
// partially occupied to empty.
func TestIntegration_EventDrain_OnlyFiresOnTransition(t *testing.T) {
	t.Parallel()
	const cap = 3
	wr := newTestWR(t, int32(cap))

	var drainCount atomic.Int32
	wr.On(EventDrain, func(s Snapshot) { drainCount.Add(1) })

	serving := make(chan struct{}, cap)
	gates := make([]chan struct{}, cap)
	for i := range gates {
		gates[i] = make(chan struct{})
	}

	r := newTestRouter(wr, serving, nil)
	// Override the handler to use individual gates.
	r = newTestRouterWithGates(wr, serving, gates)

	// Fill all 3 slots.
	for i := 0; i < cap; i++ {
		go func() {
			req := httptest.NewRequest("GET", "/", nil)
			r.ServeHTTP(httptest.NewRecorder(), req)
		}()
	}
	for i := 0; i < cap; i++ {
		select {
		case <-serving:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for handler %d", i)
		}
	}

	// Release first slot: full→not-full. EventDrain should fire.
	close(gates[0])
	waitForCount(t, &drainCount, 1, 200*time.Millisecond)

	// Release second slot: not-full→still-not-full. No additional drain.
	close(gates[1])
	time.Sleep(50 * time.Millisecond)

	// Release third slot: occupancy→0. Still no additional drain.
	close(gates[2])
	time.Sleep(50 * time.Millisecond)

	if got := drainCount.Load(); got != 1 {
		t.Errorf("expected EventDrain to fire exactly 1 time, got %d", got)
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

// newTestWR builds an initialised WaitingRoom and registers Stop on cleanup.
func newTestWR(t *testing.T, cap int32) *WaitingRoom {
	t.Helper()
	wr := &WaitingRoom{}
	if err := wr.Init(cap); err != nil {
		t.Fatalf("Init(%d): %v", cap, err)
	}
	t.Cleanup(wr.Stop)
	return wr
}

// waitForCount spins until the atomic counter reaches want or deadline passes.
func waitForCount(t *testing.T, counter *atomic.Int32, want int32, deadline time.Duration) {
	t.Helper()
	timeout := time.After(deadline)
	for {
		if counter.Load() >= want {
			return
		}
		select {
		case <-timeout:
			t.Errorf("timed out: counter = %d, want %d", counter.Load(), want)
			return
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
}

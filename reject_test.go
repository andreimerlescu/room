package room

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

type rejectClock struct {
	mu  sync.Mutex
	now time.Time
}

func newRejectClock(start time.Time) *rejectClock {
	return &rejectClock{now: start}
}

func (c *rejectClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *rejectClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func newRejectTestWR(t *testing.T, cap int32, clk *rejectClock, opts ...Option) *WaitingRoom {
	t.Helper()
	wr := &WaitingRoom{nowFn: clk.Now}
	if err := wr.Init(cap, opts...); err != nil {
		t.Fatalf("Init(%d): %v", cap, err)
	}
	t.Cleanup(wr.Stop)
	return wr
}

func startBlockingSlot(t *testing.T, r http.Handler, serving <-chan struct{}) <-chan struct{} {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		r.ServeHTTP(httptest.NewRecorder(), req)
	}()
	select {
	case <-serving:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for blocking slot")
	}
	return done
}

func TestMaxQueueBreaker_SynchronousReapRecoversWithoutWaitingForTicker(t *testing.T) {
	t.Parallel()

	t.Run("cookie_middleware_idle_poll_ttl", func(t *testing.T) {
		t.Parallel()
		clk := newRejectClock(time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))
		wr := newRejectTestWR(t, 1, clk, WithIdlePollTTL(30*time.Second))
		if err := wr.SetReaperInterval(time.Hour); err != nil {
			t.Fatal(err)
		}
		if err := wr.SetMaxQueueDepth(2); err != nil {
			t.Fatal(err)
		}

		serving := make(chan struct{}, 4)
		release := make(chan struct{})
		r := newTestRouter(wr, serving, release)
		fillOneSlot(t, r, serving)

		// Fill queue to maxQueueDepth = 2.
		for i := range 2 {
			w, tok := serveWithCookie(r, "")
			if w.Code != http.StatusOK || tok == "" {
				t.Fatalf("setup arrival %d: code=%d tok=%q", i, w.Code, tok)
			}
		}
		// Armed breaker rejects immediately while tickets are live.
		if w, _ := serveWithCookie(r, ""); w.Code != http.StatusServiceUnavailable {
			t.Fatalf("armed breaker: code=%d, want 503", w.Code)
		}

		// Advance past IdlePollTTL without waiting for background ticker.
		clk.Advance(31 * time.Second)

		w, tok := serveWithCookie(r, "")
		if w.Code != http.StatusOK || tok == "" {
			t.Fatalf("post-expiry cookie arrival: code=%d tok=%q, want 200 queued", w.Code, tok)
		}
		if got := wr.LiveQueueDepth(); got != 1 {
			t.Errorf("LiveQueueDepth=%d, want 1", got)
		}

		close(release)
		waitForNowServing(t, wr, 3, 2*time.Second)
		if w, _ := serveWithCookie(r, tok); w.Code != http.StatusOK {
			t.Errorf("admit new client: code=%d, want 200", w.Code)
		}
		assertAccounting(t, wr)
	})

	t.Run("cookie_middleware_ready_grace", func(t *testing.T) {
		t.Parallel()
		clk := newRejectClock(time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))
		wr := newRejectTestWR(t, 1, clk, WithReadyGrace(time.Minute))
		if err := wr.SetReaperInterval(time.Hour); err != nil {
			t.Fatal(err)
		}
		if err := wr.SetMaxQueueDepth(1); err != nil {
			t.Fatal(err)
		}

		serving := make(chan struct{}, 4)
		release := make(chan struct{})
		r := newTestRouter(wr, serving, release)
		fillOneSlot(t, r, serving)

		w1, tok1 := serveWithCookie(r, "")
		if w1.Code != http.StatusOK || tok1 == "" {
			t.Fatalf("setup queue: code=%d tok=%q", w1.Code, tok1)
		}

		// Free the slot so tok1 enters the serving window, then poll once to stamp readyAt.
		close(release)
		waitForNowServing(t, wr, 1, 2*time.Second)
		if resp := pollStatus(r, tok1); !resp.Ready {
			t.Fatalf("expected tok1 ready, got %+v", resp)
		}

		// While within readyGrace, tok1 holds LiveQueueDepth=1 and trips maxQueueDepth=1.
		if w, _ := serveWithCookie(r, ""); w.Code != http.StatusServiceUnavailable {
			t.Fatalf("within readyGrace: code=%d, want 503", w.Code)
		}

		// Advance past ReadyGrace (still well below TokenTTL=5m); synchronous reap
		// evicts tok1, advances nowServing, and admits the new arrival immediately.
		clk.Advance(61 * time.Second)
		w2, tok2 := serveWithCookie(r, "")
		if w2.Code != http.StatusOK || tok2 != "" {
			t.Fatalf("after readyGrace: code=%d tok=%q, want 200 immediate admission", w2.Code, tok2)
		}
		if got := wr.LiveQueueDepth(); got != 0 {
			t.Errorf("LiveQueueDepth=%d, want 0", got)
		}
		assertAccounting(t, wr)
	})

	t.Run("keyed_middleware_idle_poll_ttl_and_ready_grace", func(t *testing.T) {
		t.Parallel()
		clk := newRejectClock(time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))
		wr := newRejectTestWR(t, 1, clk, WithIdlePollTTL(30*time.Second), WithReadyGrace(time.Minute))
		if err := wr.SetReaperInterval(time.Hour); err != nil {
			t.Fatal(err)
		}
		if err := wr.SetMaxQueueDepth(2); err != nil {
			t.Fatal(err)
		}

		serving := make(chan struct{}, 4)
		release := make(chan struct{})
		r := keyedRouter(wr, serving, release)
		fillOneSlot(t, r, serving)

		for _, k := range []string{"k1", "k2"} {
			w := keyedGet(r, k, nil)
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("setup %s: code=%d", k, w.Code)
			}
			if resp := decodeWait(t, w); !resp.Waiting {
				t.Fatalf("setup %s: expected wait response, got %+v", k, resp)
			}
		}

		// Breaker rejects k3 while k1 and k2 are live.
		wRej := keyedGet(r, "k3", nil)
		if wRej.Code != http.StatusServiceUnavailable {
			t.Fatalf("armed breaker: code=%d, want 503", wRej.Code)
		}
		if _, ok := wr.TicketByKey("k3"); ok {
			t.Fatal("k3 should have been rejected, not queued")
		}

		// Advance past IdlePollTTL; k3 triggers synchronous reap and queues at position 1.
		clk.Advance(31 * time.Second)
		wQ := keyedGet(r, "k3", nil)
		if wQ.Code != http.StatusServiceUnavailable {
			t.Fatalf("queued k3: code=%d, want 503 wait JSON", wQ.Code)
		}
		if resp := decodeWait(t, wQ); !resp.Waiting || resp.Position != 1 {
			t.Fatalf("k3 wait response: %+v, want waiting at position 1", resp)
		}
		if _, ok := wr.TicketByKey("k1"); ok {
			t.Error("stale k1 should have been reaped")
		}
		if _, ok := wr.TicketByKey("k2"); ok {
			t.Error("stale k2 should have been reaped")
		}

		// Now test ReadyGrace recovery for keyed middleware.
		if err := wr.SetIdlePollTTL(0); err != nil {
			t.Fatal(err)
		}
		if err := wr.SetMaxQueueDepth(1); err != nil {
			t.Fatal(err)
		}
		close(release)
		waitForNowServing(t, wr, 3, 2*time.Second)
		// Stamp k3's readyAt once it enters the serving window.
		wr.reap()
		if ti, ok := wr.TicketByKey("k3"); !ok || ti.ReadyAt.IsZero() {
			t.Fatalf("expected k3 readyAt stamped, ok=%v ti=%+v", ok, ti)
		}

		// Advance past ReadyGrace; k4 triggers synchronous reap and is admitted immediately.
		clk.Advance(61 * time.Second)
		if w4 := keyedGet(r, "k4", nil); w4.Code != http.StatusOK {
			t.Fatalf("k4 after k3 readyGrace expiry: code=%d, want 200 admission", w4.Code)
		}
		assertAccounting(t, wr)
	})

	t.Run("enter_idle_poll_ttl", func(t *testing.T) {
		t.Parallel()
		clk := newRejectClock(time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))
		wr := newRejectTestWR(t, 1, clk, WithIdlePollTTL(30*time.Second))
		if err := wr.SetReaperInterval(time.Hour); err != nil {
			t.Fatal(err)
		}
		if err := wr.SetMaxQueueDepth(2); err != nil {
			t.Fatal(err)
		}

		for _, k := range []string{"e1", "e2"} {
			if _, _, err := wr.Enter(k); err != nil {
				t.Fatalf("Enter(%s): %v", k, err)
			}
		}
		if _, _, err := wr.Enter("e3"); err == nil {
			t.Fatal("expected ErrQueueFull while live")
		} else if _, ok := err.(ErrQueueFull); !ok {
			t.Fatalf("got %T, want ErrQueueFull", err)
		}

		clk.Advance(31 * time.Second)
		id, pos, err := wr.Enter("e3")
		if err != nil || id == "" || pos != 1 {
			t.Fatalf("Enter(e3) after idle expiry: id=%q pos=%d err=%v", id, pos, err)
		}
		if got := wr.LiveQueueDepth(); got != 1 {
			t.Errorf("LiveQueueDepth=%d, want 1", got)
		}
		assertAccounting(t, wr)
	})

	t.Run("acquire_idle_poll_ttl", func(t *testing.T) {
		t.Parallel()
		clk := newRejectClock(time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC))
		wr := newRejectTestWR(t, 1, clk, WithIdlePollTTL(30*time.Second))
		if err := wr.SetReaperInterval(time.Hour); err != nil {
			t.Fatal(err)
		}
		if err := wr.SetMaxQueueDepth(2); err != nil {
			t.Fatal(err)
		}

		if ok, _, err := wr.Acquire("holder"); err != nil || !ok {
			t.Fatalf("Acquire(holder): ok=%v err=%v", ok, err)
		}
		for _, k := range []string{"a1", "a2"} {
			if ok, _, err := wr.Acquire(k); err != nil || ok {
				t.Fatalf("Acquire(%s): ok=%v err=%v", k, ok, err)
			}
		}
		if _, _, err := wr.Acquire("a3"); err == nil {
			t.Fatal("expected ErrQueueFull while live")
		} else if _, ok := err.(ErrQueueFull); !ok {
			t.Fatalf("got %T, want ErrQueueFull", err)
		}

		// Stale queued tickets reaped synchronously; a3 queues at position 1.
		clk.Advance(31 * time.Second)
		ok, pos, err := wr.Acquire("a3")
		if err != nil || ok || pos != 1 {
			t.Fatalf("Acquire(a3) after idle expiry: ok=%v pos=%d err=%v", ok, pos, err)
		}

		// Release holder, tighten breaker to 1, age a3 past idlePollTTL:
		// a4 triggers sync reap and acquires immediately.
		if err := wr.Release("holder"); err != nil {
			t.Fatal(err)
		}
		if err := wr.SetMaxQueueDepth(1); err != nil {
			t.Fatal(err)
		}
		clk.Advance(31 * time.Second)
		ok, pos, err = wr.Acquire("a4")
		if err != nil || !ok || pos != 0 {
			t.Fatalf("Acquire(a4) immediate admission: ok=%v pos=%d err=%v", ok, pos, err)
		}
		if err := wr.Release("a4"); err != nil {
			t.Fatal(err)
		}
		assertAccounting(t, wr)
	})

	t.Run("cas_throttle_250ms_concurrent_flood", func(t *testing.T) {
		t.Parallel()
		base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
		clk := newRejectClock(base)
		wr := newRejectTestWR(t, 1, clk, WithIdlePollTTL(30*time.Second))
		if err := wr.SetReaperInterval(time.Hour); err != nil {
			t.Fatal(err)
		}

		var evictEvents atomic.Int32
		wr.On(EventEvict, func(Snapshot) { evictEvents.Add(1) })

		if ok, _, err := wr.Acquire("holder"); err != nil || !ok {
			t.Fatalf("Acquire(holder): ok=%v err=%v", ok, err)
		}

		// t=0.0s: stale1 (expires > 30.0s)
		if _, _, err := wr.Enter("stale1"); err != nil {
			t.Fatal(err)
		}
		// t=1.1s: stale2 (expires > 31.1s — NOT expired at 31.0s, expired at 31.2s)
		clk.Advance(1100 * time.Millisecond)
		if _, _, err := wr.Enter("stale2"); err != nil {
			t.Fatal(err)
		}
		// t=20.0s: fresh1, fresh2 (keep queue >= maxQueueDepth=2 after stale1 & stale2 reap)
		clk.Advance(18900 * time.Millisecond)
		for _, k := range []string{"fresh1", "fresh2"} {
			if _, _, err := wr.Enter(k); err != nil {
				t.Fatal(err)
			}
		}
		if err := wr.SetMaxQueueDepth(2); err != nil {
			t.Fatal(err)
		}

		// Advance to t=31.0s: stale1 is expired, stale2 is not yet expired.
		clk.Advance(11 * time.Second)

		const flood = 32
		start := make(chan struct{})
		var wg sync.WaitGroup
		var rejected atomic.Int32
		for i := range flood {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				if _, _, err := wr.Enter(fmt.Sprintf("flood1-%d", i)); err != nil {
					if _, ok := err.(ErrQueueFull); ok {
						rejected.Add(1)
					} else {
						t.Errorf("unexpected error: %v", err)
					}
				}
			}(i)
		}
		close(start)
		wg.Wait()

		if rejected.Load() != flood {
			t.Fatalf("flood1 rejected=%d, want %d", rejected.Load(), flood)
		}
		if _, ok := wr.TicketByKey("stale1"); ok {
			t.Fatal("stale1 should have been reaped by the winning goroutine")
		}
		if _, ok := wr.TicketByKey("stale2"); !ok {
			t.Fatal("stale2 should still be live at t=31.0s")
		}
		if got := wr.LiveQueueDepth(); got != 3 {
			t.Fatalf("LiveQueueDepth=%d, want 3", got)
		}
		waitForCount(t, &evictEvents, 1, 200*time.Millisecond)

		// Advance 200ms to t=31.2s (< 250ms throttle window): stale2 is now expired
		// (age 30.1s > 30s), but the CAS throttle must suppress another reap pass.
		clk.Advance(200 * time.Millisecond)
		start2 := make(chan struct{})
		for i := range 16 {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start2
				_, _, _ = wr.Enter(fmt.Sprintf("flood2-%d", i))
			}(i)
		}
		close(start2)
		wg.Wait()

		if _, ok := wr.TicketByKey("stale2"); !ok {
			t.Fatal("stale2 was reaped inside the 250ms throttle window")
		}
		if got := wr.LiveQueueDepth(); got != 3 {
			t.Fatalf("LiveQueueDepth inside 250ms window=%d, want 3", got)
		}
		if got := evictEvents.Load(); got != 1 {
			t.Errorf("EventEvict count inside 250ms window=%d, want 1", got)
		}

		// Advance 50ms more to t=31.25s (>= 250ms since lastForceReap): next arrival reaps stale2.
		clk.Advance(50 * time.Millisecond)
		if _, _, err := wr.Enter("after-window"); err == nil {
			t.Fatal("expected ErrQueueFull (fresh1+fresh2 still occupy maxQueueDepth=2)")
		}
		if _, ok := wr.TicketByKey("stale2"); ok {
			t.Fatal("stale2 should have been reaped once >= 250ms elapsed")
		}
		if got := wr.LiveQueueDepth(); got != 2 {
			t.Fatalf("LiveQueueDepth after 250ms window=%d, want 2", got)
		}
		waitForCount(t, &evictEvents, 2, 200*time.Millisecond)
		assertAccounting(t, wr)
	})
}

func TestFullResponder_AndEventReject(t *testing.T) {
	t.Parallel()

	t.Run("default_503_headers_upstream_retry_after_and_events", func(t *testing.T) {
		t.Parallel()
		wr := newTestWR(t, 1)
		if err := wr.SetMaxQueueDepth(1); err != nil {
			t.Fatal(err)
		}
		wr.SetClientKeyFunc(func(c *gin.Context) string {
			return c.GetHeader("X-Client")
		})
		wr.SetTicketKeyFunc(func(c *gin.Context) (string, bool) {
			k := c.GetHeader("X-Key")
			return k, k != ""
		})

		rejects := collectEvents(wr, EventReject)
		serving := make(chan struct{}, 2)
		release := make(chan struct{})
		defer close(release)

		var handlerHits atomic.Int32
		r := gin.New()
		r.Use(func(c *gin.Context) {
			if ra := c.GetHeader("X-Upstream-Retry-After"); ra != "" {
				c.Header("Retry-After", ra)
			}
			c.Next()
		})
		r.GET("/queue/status", wr.StatusHandler())
		r.GET("/", wr.Middleware(), func(c *gin.Context) {
			handlerHits.Add(1)
			serving <- struct{}{}
			<-release
			c.String(http.StatusOK, "ok")
		})

		// Fill slot + queue to maxQueueDepth=1.
		fillOneSlot(t, r, serving)
		wQueued, queuedTok := serveWithCookie(r, "")
		if wQueued.Code != http.StatusOK || queuedTok == "" {
			t.Fatalf("setup queue: code=%d tok=%q", wQueued.Code, queuedTok)
		}

		// Cookie rejection with default Retry-After.
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("X-Client", "client-cookie-1")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("cookie reject: code=%d, want 503", w.Code)
		}
		if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("cookie Cache-Control=%q, want no-store", cc)
		}
		if ra := w.Header().Get("Retry-After"); ra != "3" {
			t.Errorf("cookie Retry-After=%q, want 3", ra)
		}
		snap := nextSnapshot(t, rejects)
		if snap.Event != EventReject || snap.ClientKey != "client-cookie-1" || snap.Key != "" ||
			snap.Token != "" || snap.Occupancy != 1 || snap.Capacity != 1 || snap.QueueDepth != 1 {
			t.Errorf("cookie EventReject snap=%+v", snap)
		}

		// Cookie rejection preserves upstream Retry-After.
		reqUp := httptest.NewRequest(http.MethodGet, "/", nil)
		reqUp.Header.Set("X-Upstream-Retry-After", "42")
		wUp := httptest.NewRecorder()
		r.ServeHTTP(wUp, reqUp)
		if wUp.Code != http.StatusServiceUnavailable {
			t.Fatalf("cookie upstream RA: code=%d, want 503", wUp.Code)
		}
		if cc := wUp.Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("cookie upstream Cache-Control=%q, want no-store", cc)
		}
		if ra := wUp.Header().Get("Retry-After"); ra != "42" {
			t.Errorf("cookie upstream Retry-After=%q, want 42", ra)
		}
		_ = nextSnapshot(t, rejects)

		// Keyed rejection with default Retry-After.
		wk := keyedGet(r, "api-key-1", nil)
		if wk.Code != http.StatusServiceUnavailable {
			t.Fatalf("keyed reject: code=%d, want 503", wk.Code)
		}
		if cc := wk.Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("keyed Cache-Control=%q, want no-store", cc)
		}
		if ra := wk.Header().Get("Retry-After"); ra != "3" {
			t.Errorf("keyed Retry-After=%q, want 3", ra)
		}
		ksnap := nextSnapshot(t, rejects)
		if ksnap.Event != EventReject || ksnap.Key != "api-key-1" || ksnap.Token != "" ||
			ksnap.Occupancy != 1 || ksnap.Capacity != 1 || ksnap.QueueDepth != 1 {
			t.Errorf("keyed EventReject snap=%+v", ksnap)
		}

		// Keyed rejection preserves upstream Retry-After.
		wkUp := keyedGet(r, "api-key-2", map[string]string{"X-Upstream-Retry-After": "99"})
		if wkUp.Code != http.StatusServiceUnavailable {
			t.Fatalf("keyed upstream RA: code=%d, want 503", wkUp.Code)
		}
		if ra := wkUp.Header().Get("Retry-After"); ra != "99" {
			t.Errorf("keyed upstream Retry-After=%q, want 99", ra)
		}
		_ = nextSnapshot(t, rejects)

		// Enter rejection emits EventReject with Key.
		if _, _, err := wr.Enter("enter-rej"); err == nil {
			t.Fatal("Enter: expected ErrQueueFull")
		} else if _, ok := err.(ErrQueueFull); !ok {
			t.Fatalf("Enter: got %T, want ErrQueueFull", err)
		}
		esnap := nextSnapshot(t, rejects)
		if esnap.Event != EventReject || esnap.Key != "enter-rej" {
			t.Errorf("Enter EventReject snap=%+v", esnap)
		}

		// Acquire rejection emits EventReject with Key.
		if _, _, err := wr.Acquire("acq-rej"); err == nil {
			t.Fatal("Acquire: expected ErrQueueFull")
		} else if _, ok := err.(ErrQueueFull); !ok {
			t.Fatalf("Acquire: got %T, want ErrQueueFull", err)
		}
		asnap := nextSnapshot(t, rejects)
		if asnap.Event != EventReject || asnap.Key != "acq-rej" {
			t.Errorf("Acquire EventReject snap=%+v", asnap)
		}

		// Custom FullResponder: accurate Snapshot, re-entrant wr methods without deadlock,
		// middleware aborts downstream handler automatically.
		var (
			responderSnaps []Snapshot
			reentrantOK    bool
		)
		wr.SetFullResponder(func(c *gin.Context, s Snapshot) {
			responderSnaps = append(responderSnaps, s)
			qd := wr.QueueDepth()
			lqd := wr.LiveQueueDepth()
			_, tokOK := wr.Ticket(queuedTok)
			errSet := wr.SetMaxQueueDepth(wr.MaxQueueDepth())
			reentrantOK = qd == 1 && lqd == 1 && tokOK && errSet == nil
			c.JSON(http.StatusTooManyRequests, gin.H{
				"error":      "queue_full",
				"key":        s.Key,
				"client_key": s.ClientKey,
				"depth":      s.QueueDepth,
			})
		})

		reqCustom := httptest.NewRequest(http.MethodGet, "/", nil)
		reqCustom.Header.Set("X-Client", "custom-client")
		wc := httptest.NewRecorder()
		r.ServeHTTP(wc, reqCustom)
		if wc.Code != http.StatusTooManyRequests {
			t.Fatalf("custom cookie reject: code=%d, want 429", wc.Code)
		}
		if wc.Header().Get("Cache-Control") != "no-store" || wc.Header().Get("Retry-After") != "3" {
			t.Errorf("custom cookie headers: CC=%q RA=%q", wc.Header().Get("Cache-Control"), wc.Header().Get("Retry-After"))
		}
		if !reentrantOK {
			t.Error("re-entrant WaitingRoom calls inside cookie FullResponder failed")
		}
		var body map[string]any
		if err := json.Unmarshal(wc.Body.Bytes(), &body); err != nil {
			t.Fatalf("unmarshal custom cookie body: %v", err)
		}
		if body["client_key"] != "custom-client" {
			t.Errorf("custom cookie body=%v", body)
		}
		_ = nextSnapshot(t, rejects)

		reentrantOK = false
		wkc := keyedGet(r, "custom-key", map[string]string{"X-Upstream-Retry-After": "15"})
		if wkc.Code != http.StatusTooManyRequests {
			t.Fatalf("custom keyed reject: code=%d, want 429", wkc.Code)
		}
		if wkc.Header().Get("Cache-Control") != "no-store" || wkc.Header().Get("Retry-After") != "15" {
			t.Errorf("custom keyed headers: CC=%q RA=%q", wkc.Header().Get("Cache-Control"), wkc.Header().Get("Retry-After"))
		}
		if !reentrantOK {
			t.Error("re-entrant WaitingRoom calls inside keyed FullResponder failed")
		}
		_ = nextSnapshot(t, rejects)

		if len(responderSnaps) != 2 {
			t.Fatalf("responderSnaps=%d, want 2", len(responderSnaps))
		}
		if responderSnaps[0].Event != EventReject || responderSnaps[0].ClientKey != "custom-client" ||
			responderSnaps[1].Event != EventReject || responderSnaps[1].Key != "custom-key" {
			t.Errorf("unexpected responderSnaps: %+v", responderSnaps)
		}
		if handlerHits.Load() != 1 {
			t.Errorf("downstream handler ran %d times, want 1 (rejected arrivals must abort)", handlerHits.Load())
		}

		// Clearing with SetFullResponder(nil) restores default 503.
		wr.SetFullResponder(nil)
		if wNil, _ := serveWithCookie(r, ""); wNil.Code != http.StatusServiceUnavailable {
			t.Errorf("after SetFullResponder(nil): code=%d, want 503", wNil.Code)
		}
		_ = nextSnapshot(t, rejects)
	})

	t.Run("with_full_responder_option_and_survives_init", func(t *testing.T) {
		t.Parallel()
		var called atomic.Int32
		responder := func(c *gin.Context, s Snapshot) {
			called.Add(1)
			c.String(http.StatusTooManyRequests, "full:%d", s.QueueDepth)
		}

		wr := &WaitingRoom{}
		if err := wr.Init(1, WithFullResponder(responder)); err != nil {
			t.Fatalf("Init with WithFullResponder: %v", err)
		}
		t.Cleanup(wr.Stop)
		if err := wr.SetMaxQueueDepth(1); err != nil {
			t.Fatal(err)
		}

		serving := make(chan struct{}, 2)
		release := make(chan struct{})
		r := newTestRouter(wr, serving, release)
		slotDone := startBlockingSlot(t, r, serving)
		if _, tok := serveWithCookie(r, ""); tok == "" {
			t.Fatal("setup queue failed")
		}

		w, _ := serveWithCookie(r, "")
		if w.Code != http.StatusTooManyRequests || w.Body.String() != "full:1" {
			t.Fatalf("WithFullResponder: code=%d body=%q", w.Code, w.Body.String())
		}
		if called.Load() != 1 {
			t.Fatalf("called=%d, want 1", called.Load())
		}
		close(release)
		<-slotDone

		// Re-initialising wr must preserve FullResponder.
		if err := wr.Init(1); err != nil {
			t.Fatalf("re-Init: %v", err)
		}
		if err := wr.SetMaxQueueDepth(1); err != nil {
			t.Fatal(err)
		}
		serving2 := make(chan struct{}, 2)
		release2 := make(chan struct{})
		r2 := newTestRouter(wr, serving2, release2)
		slotDone2 := startBlockingSlot(t, r2, serving2)
		defer func() {
			close(release2)
			<-slotDone2
		}()
		if _, tok := serveWithCookie(r2, ""); tok == "" {
			t.Fatal("setup queue after re-Init failed")
		}
		w2, _ := serveWithCookie(r2, "")
		if w2.Code != http.StatusTooManyRequests || called.Load() != 2 {
			t.Fatalf("after re-Init: code=%d called=%d, want 429 and 2", w2.Code, called.Load())
		}

		// SetFullResponder before initial Init must also survive Init.
		var preInitCalled atomic.Int32
		wrPre := &WaitingRoom{}
		wrPre.SetFullResponder(func(c *gin.Context, s Snapshot) {
			preInitCalled.Add(1)
			c.Status(http.StatusTooManyRequests)
		})
		if err := wrPre.Init(1); err != nil {
			t.Fatalf("pre-Init: %v", err)
		}
		t.Cleanup(wrPre.Stop)
		if err := wrPre.SetMaxQueueDepth(1); err != nil {
			t.Fatal(err)
		}
		if ok, _, err := wrPre.Acquire("h"); err != nil || !ok {
			t.Fatalf("Acquire(h): ok=%v err=%v", ok, err)
		}
		if _, _, err := wrPre.Enter("q"); err != nil {
			t.Fatal(err)
		}
		rPre := newTestRouter(wrPre, nil, nil)
		if wPre, _ := serveWithCookie(rPre, ""); wPre.Code != http.StatusTooManyRequests || preInitCalled.Load() != 1 {
			t.Fatalf("pre-Init SetFullResponder: code=%d called=%d, want 429 and 1", wPre.Code, preInitCalled.Load())
		}
	})

	t.Run("admit_timeout_unchanged", func(t *testing.T) {
		t.Parallel()
		var fullCalled atomic.Int32
		wr := newTestWR(t, 1)
		wr.SetFullResponder(func(c *gin.Context, s Snapshot) {
			fullCalled.Add(1)
			c.Status(http.StatusTooManyRequests)
		})
		timeouts := collectEvents(wr, EventTimeout)

		// Occupy semaphore directly without advancing nextTicket so the next
		// arrival enters admit() and blocks on sem.AcquireWith until ctx cancels.
		if !wr.sem.TryAcquire() {
			t.Fatal("failed to acquire semaphore")
		}
		defer wr.sem.Release()

		r := newTestRouter(wr, nil, nil)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("admit timeout: code=%d, want 503", w.Code)
		}
		if fullCalled.Load() != 0 {
			t.Errorf("FullResponder must not run on admit timeout, called=%d", fullCalled.Load())
		}
		_ = nextSnapshot(t, timeouts)
	})
}

func TestReadyGrace_MiddlewareKeyedEvicted_EnterAcquireSkipped(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	clk := newRejectClock(start)
	wr := newRejectTestWR(t, 5, clk, WithReadyGrace(time.Minute), WithLeaseFraction(0.25))
	if err := wr.SetReaperInterval(time.Hour); err != nil {
		t.Fatal(err)
	}
	if got := wr.LeaseCeiling(); got != 1 {
		t.Fatalf("LeaseCeiling=%d, want 1", got)
	}

	// 1) Hold the single allowed lease so further Acquire() calls are LeaseCeiling-blocked.
	if ok, _, err := wr.Acquire("lease-holder"); err != nil || !ok {
		t.Fatalf("Acquire(lease-holder): ok=%v err=%v", ok, err)
	}

	// 2) Fill the remaining 4 slots with blocking HTTP requests.
	serving := make(chan struct{}, 5)
	release := make(chan struct{})
	r := keyedRouter(wr, serving, release)
	for range 4 {
		fillOneSlot(t, r, serving)
	}
	if got := wr.Len(); got != 5 {
		t.Fatalf("Len=%d, want 5", got)
	}

	// 3) Queue middleware keyed tickets both before and after Enter() and Acquire() tickets
	// while the room is full (tickets 6, 7, 8, 9).
	if w := keyedGet(r, "mw-before", nil); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("mw-before: code=%d, want 503 wait", w.Code)
	}
	if _, _, err := wr.Enter("enter-key"); err != nil {
		t.Fatalf("Enter(enter-key): %v", err)
	}
	if ok, _, err := wr.Acquire("acquire-key"); err != nil || ok {
		t.Fatalf("Acquire(acquire-key): ok=%v err=%v", ok, err)
	}
	if w := keyedGet(r, "mw-after", nil); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("mw-after: code=%d, want 503 wait", w.Code)
	}

	// 4) Release the 4 blocking requests so nowServing advances to 4 (edge = 4 + 5 = 9):
	// all four queued tickets (6..9) are now inside the serving window!
	close(release)
	waitForNowServing(t, wr, 4, 2*time.Second)

	// Calling Acquire("acquire-key") while inside the serving window still stays queued
	// because Held()==1 == LeaseCeiling().
	if ok, pos, err := wr.Acquire("acquire-key"); err != nil || ok || pos < 1 {
		t.Fatalf("Acquire(acquire-key) blocked by LeaseCeiling: ok=%v pos=%d err=%v", ok, pos, err)
	}

	evicts := collectEvents(wr, EventEvict)

	// 5) First reap pass stamps readyAt on middleware keyed tickets and skips Enter/Acquire.
	wr.reap()
	for _, k := range []string{"mw-before", "mw-after"} {
		ti, ok := wr.TicketByKey(k)
		if !ok || !ti.ReadyAt.Equal(start) {
			t.Fatalf("%s after first reap: ok=%v ReadyAt=%v, want %v", k, ok, ti.ReadyAt, start)
		}
	}
	for _, k := range []string{"enter-key", "acquire-key"} {
		ti, ok := wr.TicketByKey(k)
		if !ok || !ti.ReadyAt.IsZero() {
			t.Fatalf("%s after first reap: ok=%v ReadyAt=%v, want zero", k, ok, ti.ReadyAt)
		}
	}

	// 6) Advance past ReadyGrace (1m) and reap: middleware keyed tickets are evicted,
	// while Enter and LeaseCeiling-blocked Acquire tickets survive.
	clk.Advance(61 * time.Second)
	wr.reap()
	_ = nextSnapshot(t, evicts)

	for _, k := range []string{"mw-before", "mw-after"} {
		if _, ok := wr.TicketByKey(k); ok {
			t.Errorf("%s should have been evicted past ReadyGrace", k)
		}
	}
	for _, k := range []string{"enter-key", "acquire-key"} {
		ti, ok := wr.TicketByKey(k)
		if !ok || !ti.ReadyAt.IsZero() {
			t.Errorf("%s must NOT be evicted by ReadyGrace: ok=%v ReadyAt=%v", k, ok, ti.ReadyAt)
		}
	}
	if ns := wr.nowServing.Load(); ns != 6 {
		t.Errorf("nowServing=%d, want 6 (4 released + 2 evicted)", ns)
	}

	// 7) Advance further (still below TokenTTL=5m) and reap again: Enter/Acquire still survive.
	clk.Advance(2 * time.Minute)
	wr.reap()
	for _, k := range []string{"enter-key", "acquire-key"} {
		if _, ok := wr.TicketByKey(k); !ok {
			t.Errorf("%s should still survive below TokenTTL", k)
		}
	}

	// 8) Releasing the lease holder lets acquire-key claim its lease cleanly.
	if err := wr.Release("lease-holder"); err != nil {
		t.Fatal(err)
	}
	if ok, _, err := wr.Acquire("acquire-key"); err != nil || !ok {
		t.Fatalf("Acquire(acquire-key) after holder released: ok=%v err=%v", ok, err)
	}
	if err := wr.Leave("enter-key"); err != nil {
		t.Fatal(err)
	}
	if err := wr.Release("acquire-key"); err != nil {
		t.Fatal(err)
	}
	assertIndexes(t, wr.tokens)
	assertAccounting(t, wr)
}

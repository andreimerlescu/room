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

func init() {
	gin.SetMode(gin.TestMode)
}

// newTestRouter creates a gin engine with the WaitingRoom middleware and
// a simple handler that signals when it is actively serving and blocks
// until released. This gives tests precise control over slot occupancy.
func newTestRouter(wr *WaitingRoom, serving chan struct{}, release chan struct{}) *gin.Engine {
	r := gin.New()
	wr.RegisterRoutes(r)
	r.GET("/", func(c *gin.Context) {
		if serving != nil {
			serving <- struct{}{}
		}
		if release != nil {
			<-release
		}
		c.Status(http.StatusOK)
	})
	return r
}

// waitForQueue blocks until QueueDepth reaches expected or the deadline
// passes. It polls at 5ms intervals to avoid busy-waiting.
func waitForQueue(t *testing.T, wr *WaitingRoom, expected int64, deadline time.Duration) {
	t.Helper()
	timeout := time.After(deadline)
	for {
		select {
		case <-timeout:
			t.Fatalf("timed out waiting for queue depth %d, got %d", expected, wr.QueueDepth())
		default:
			if wr.QueueDepth() == expected {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
}

// ── Constructor tests ────────────────────────────────────────────────────────

func TestNewWaitingRoom_ValidCap(t *testing.T) {
	wr := &WaitingRoom{}
	if err := wr.Init(10); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	defer wr.Stop()

	if wr.Cap() != 10 {
		t.Errorf("expected cap 10, got %d", wr.Cap())
	}
}

func TestNewWaitingRoom_ZeroCap(t *testing.T) {
	wr := &WaitingRoom{}
	err := wr.Init(0)
	if err == nil {
		t.Fatal("expected ErrInvalidCap, got nil")
	}
	if _, ok := err.(ErrInvalidCap); !ok {
		t.Errorf("expected ErrInvalidCap, got %T", err)
	}
}

func TestNewWaitingRoom_NegativeCap(t *testing.T) {
	wr := &WaitingRoom{}
	err := wr.Init(-1)
	if err == nil {
		t.Fatal("expected ErrInvalidCap, got nil")
	}
}

func TestNewWaitingRoom_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic from NewWaitingRoom(0)")
		}
	}()
	NewWaitingRoom(0)
}

// ── Fast path tests ──────────────────────────────────────────────────────────

func TestFastPath_RequestAdmittedImmediately(t *testing.T) {
	wr := &WaitingRoom{}
	if err := wr.Init(5); err != nil {
		t.Fatal(err)
	}
	defer wr.Stop()

	r := gin.New()
	wr.RegisterRoutes(r)
	r.GET("/", func(c *gin.Context) { c.Status(http.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestFastPath_AllSlotsFilledThenReleased(t *testing.T) {
	const cap = 3
	wr := &WaitingRoom{}
	if err := wr.Init(cap); err != nil {
		t.Fatal(err)
	}
	defer wr.Stop()

	serving := make(chan struct{}, cap)
	release := make(chan struct{})
	r := newTestRouter(wr, serving, release)

	// Fire cap requests — all should be admitted immediately.
	for i := 0; i < cap; i++ {
		go func() {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
		}()
	}

	// Wait for all cap handlers to signal they are serving.
	for i := 0; i < cap; i++ {
		select {
		case <-serving:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for handler %d to start", i)
		}
	}

	if wr.Len() != cap {
		t.Errorf("expected Len %d, got %d", cap, wr.Len())
	}

	// Release all.
	close(release)
}

// ── Slow path / waiting room tests ──────────────────────────────────────────

func TestSlowPath_WaitingRoomServedWhenFull(t *testing.T) {
	const cap = 1
	wr := &WaitingRoom{}
	if err := wr.Init(cap); err != nil {
		t.Fatal(err)
	}
	defer wr.Stop()

	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	r := newTestRouter(wr, serving, release)

	// Fill the single slot.
	go func() {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
	}()

	// Wait for slot to be occupied.
	select {
	case <-serving:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first handler")
	}

	// Second request should see the waiting room.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		r.ServeHTTP(w, req)
		close(done)
	}()

	// Give the second request time to hit the waiting room.
	waitForQueue(t, wr, 1, 2*time.Second)

	// Confirm waiting room HTML was served.
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for waiting room, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("expected text/html content type, got %s", ct)
	}

	// Release the first slot — second request should be admitted.
	close(release)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for second request to complete")
	}
}

func TestSlowPath_CookieIssuedWhenQueued(t *testing.T) {
	const cap = 1
	wr := &WaitingRoom{}
	if err := wr.Init(cap); err != nil {
		t.Fatal(err)
	}
	defer wr.Stop()

	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	r := newTestRouter(wr, serving, release)

	// Fill the slot.
	go func() {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
	}()
	<-serving

	// Second request hits the waiting room.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	go func() { r.ServeHTTP(w, req) }()

	waitForQueue(t, wr, 1, 2*time.Second)

	// Cookie must be set.
	cookies := w.Result().Cookies()
	found := false
	for _, c := range cookies {
		if c.Name == cookieName {
			found = true
			if c.HttpOnly != true {
				t.Error("expected HttpOnly cookie")
			}
			if len(c.Value) == 0 {
				t.Error("expected non-empty cookie value")
			}
		}
	}
	if !found {
		t.Errorf("expected cookie %q to be set", cookieName)
	}

	close(release)
}

// ── FIFO ordering tests ──────────────────────────────────────────────────────

func TestFIFO_RequestsAdmittedInOrder(t *testing.T) {
	const cap = 1
	wr := &WaitingRoom{}
	if err := wr.Init(cap); err != nil {
		t.Fatal(err)
	}
	defer wr.Stop()

	// admitOrder records the order in which handlers actually run.
	var admitOrder []int
	var orderMu sync.Mutex

	// releaseGates lets the test control when each handler finishes.
	const total = 5
	gates := make([]chan struct{}, total)
	for i := range gates {
		gates[i] = make(chan struct{})
	}

	r := gin.New()
	wr.RegisterRoutes(r)

	var started sync.WaitGroup
	started.Add(total)

	for i := 0; i < total; i++ {
		i := i
		r.GET(fmt.Sprintf("/req/%d", i), func(c *gin.Context) {
			started.Done()
			orderMu.Lock()
			admitOrder = append(admitOrder, i)
			orderMu.Unlock()
			<-gates[i]
			c.Status(http.StatusOK)
		})
	}

	// Fire all requests as close together as possible.
	// Request 0 fills the slot; 1–4 queue in arrival order.
	var wg sync.WaitGroup
	for i := 0; i < total; i++ {
		wg.Add(1)
		i := i
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/req/%d", i), nil)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
		}()
		// Small stagger ensures ticket ordering matches i.
		time.Sleep(10 * time.Millisecond)
	}

	// Release each gate in sequence and verify admit order.
	for i := 0; i < total; i++ {
		close(gates[i])
		time.Sleep(20 * time.Millisecond)
	}

	wg.Wait()

	orderMu.Lock()
	defer orderMu.Unlock()

	for i, v := range admitOrder {
		if v != i {
			t.Errorf("FIFO violation: position %d admitted request %d, expected %d", i, v, i)
		}
	}
}

// ── Status endpoint tests ────────────────────────────────────────────────────

func TestStatusEndpoint_UnknownTokenReturnsReady(t *testing.T) {
	wr := &WaitingRoom{}
	if err := wr.Init(5); err != nil {
		t.Fatal(err)
	}
	defer wr.Stop()

	r := gin.New()
	wr.RegisterRoutes(r)
	r.GET("/", func(c *gin.Context) { c.Status(http.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "/queue/status", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var resp statusResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if !resp.Ready {
		t.Error("expected ready=true for unknown token")
	}
}

func TestStatusEndpoint_KnownTokenReturnsPosition(t *testing.T) {
	const cap = 1
	wr := &WaitingRoom{}
	if err := wr.Init(cap); err != nil {
		t.Fatal(err)
	}
	defer wr.Stop()

	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	r := newTestRouter(wr, serving, release)

	// Fill the slot.
	go func() {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
	}()
	<-serving

	// Queue a second request and capture its cookie.
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	w2 := httptest.NewRecorder()
	go func() { r.ServeHTTP(w2, req2) }()
	waitForQueue(t, wr, 1, 2*time.Second)

	// Extract the cookie from the waiting room response.
	var token string
	for _, c := range w2.Result().Cookies() {
		if c.Name == cookieName {
			token = c.Value
		}
	}
	if token == "" {
		t.Fatal("no room_ticket cookie found")
	}

	// Poll status with the cookie.
	statusReq := httptest.NewRequest(http.MethodGet, "/queue/status", nil)
	statusReq.AddCookie(&http.Cookie{Name: cookieName, Value: token})
	statusW := httptest.NewRecorder()
	r.ServeHTTP(statusW, statusReq)

	var resp statusResponse
	if err := json.NewDecoder(statusW.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode status response: %v", err)
	}
	if resp.Ready {
		t.Error("expected ready=false while queued")
	}
	if resp.Position <= 0 {
		t.Errorf("expected positive position, got %d", resp.Position)
	}

	close(release)
}

func TestStatusEndpoint_ReturnsReadyWhenAdmitted(t *testing.T) {
	const cap = 1
	wr := &WaitingRoom{}
	if err := wr.Init(cap); err != nil {
		t.Fatal(err)
	}
	defer wr.Stop()

	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	r := newTestRouter(wr, serving, release)

	// Fill the slot.
	go func() {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
	}()
	<-serving

	// Queue second request.
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	w2 := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		r.ServeHTTP(w2, req2)
		close(done)
	}()
	waitForQueue(t, wr, 1, 2*time.Second)

	var token string
	for _, c := range w2.Result().Cookies() {
		if c.Name == cookieName {
			token = c.Value
		}
	}

	// Release — token should be deleted, status should return ready.
	close(release)
	<-done

	statusReq := httptest.NewRequest(http.MethodGet, "/queue/status", nil)
	statusReq.AddCookie(&http.Cookie{Name: cookieName, Value: token})
	statusW := httptest.NewRecorder()
	r.ServeHTTP(statusW, statusReq)

	var resp statusResponse
	if err := json.NewDecoder(statusW.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode status response: %v", err)
	}
	if !resp.Ready {
		t.Error("expected ready=true after admission")
	}
}

// ── SetCap tests ─────────────────────────────────────────────────────────────

func TestSetCap_ExpandAdmitsWaiters(t *testing.T) {
	const initialCap = 1
	wr := &WaitingRoom{}
	if err := wr.Init(initialCap); err != nil {
		t.Fatal(err)
	}
	defer wr.Stop()

	serving := make(chan struct{}, 10)
	release := make(chan struct{})
	r := newTestRouter(wr, serving, release)

	// Fill the single slot.
	go func() {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
	}()
	<-serving

	// Queue 4 more.
	for i := 0; i < 4; i++ {
		go func() {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
		}()
	}
	waitForQueue(t, wr, 4, 2*time.Second)

	// Expand to 5 — all waiters should be admitted.
	if err := wr.SetCap(5); err != nil {
		t.Fatalf("SetCap failed: %v", err)
	}

	// Drain serving channel — we should get 4 more admits.
	admitted := 0
	timeout := time.After(2 * time.Second)
	for admitted < 4 {
		select {
		case <-serving:
			admitted++
		case <-timeout:
			t.Fatalf("timed out: only %d of 4 waiters admitted after SetCap", admitted)
		}
	}

	close(release)
}

func TestSetCap_InvalidCapRejected(t *testing.T) {
	wr := &WaitingRoom{}
	if err := wr.Init(5); err != nil {
		t.Fatal(err)
	}
	defer wr.Stop()

	if err := wr.SetCap(0); err == nil {
		t.Error("expected error for SetCap(0)")
	}
	if err := wr.SetCap(-1); err == nil {
		t.Error("expected error for SetCap(-1)")
	}
}

// ── SetHTML tests ────────────────────────────────────────────────────────────

func TestSetHTML_CustomHTMLServed(t *testing.T) {
	const cap = 1
	wr := &WaitingRoom{}
	if err := wr.Init(cap); err != nil {
		t.Fatal(err)
	}
	defer wr.Stop()

	custom := []byte("<html><body>custom</body></html>")
	wr.SetHTML(custom)

	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	r := newTestRouter(wr, serving, release)

	// Fill the slot.
	go func() {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
	}()
	<-serving

	// Second request should get custom HTML.
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	go func() { r.ServeHTTP(w, req) }()
	waitForQueue(t, wr, 1, 2*time.Second)

	body := w.Body.String()
	if body != string(custom) {
		t.Errorf("expected custom HTML, got: %s", body)
	}

	close(release)
}

func TestSetHTML_NilRevertsToDefault(t *testing.T) {
	wr := &WaitingRoom{}
	if err := wr.Init(1); err != nil {
		t.Fatal(err)
	}
	defer wr.Stop()

	wr.SetHTML([]byte("<html>custom</html>"))
	wr.SetHTML(nil)

	html := wr.resolveHTML()
	if len(html) == 0 {
		t.Error("expected non-empty default HTML after SetHTML(nil)")
	}
}

// ── Reaper tests ─────────────────────────────────────────────────────────────

func TestReaper_EvictsExpiredTokens(t *testing.T) {
	wr := &WaitingRoom{}
	if err := wr.Init(5); err != nil {
		t.Fatal(err)
	}
	defer wr.Stop()

	// Manually insert a token with an issuedAt far in the past.
	wr.tokens.set("ghost-token", ticketEntry{
		ticket:   99,
		issuedAt: time.Now().Add(-(cookieTTL + time.Minute)),
	})

	if _, ok := wr.tokens.get("ghost-token"); !ok {
		t.Fatal("expected ghost token to exist before reap")
	}

	wr.reap()

	if _, ok := wr.tokens.get("ghost-token"); ok {
		t.Error("expected ghost token to be evicted after reap")
	}
}

func TestReaper_PreservesLiveTokens(t *testing.T) {
	wr := &WaitingRoom{}
	if err := wr.Init(5); err != nil {
		t.Fatal(err)
	}
	defer wr.Stop()

	wr.tokens.set("live-token", ticketEntry{
		ticket:   1,
		issuedAt: time.Now(),
	})

	wr.reap()

	if _, ok := wr.tokens.get("live-token"); !ok {
		t.Error("expected live token to survive reap")
	}
}

func TestSetReaperInterval_ValidRange(t *testing.T) {
	wr := &WaitingRoom{}
	if err := wr.Init(5); err != nil {
		t.Fatal(err)
	}
	defer wr.Stop()

	cases := []time.Duration{
		reaperMinInterval,
		30 * time.Second,
		5 * time.Minute,
		1 * time.Hour,
		reaperMaxInterval,
	}

	for _, d := range cases {
		if err := wr.SetReaperInterval(d); err != nil {
			t.Errorf("expected no error for interval %s, got %v", d, err)
		}
		if wr.ReaperInterval() != d {
			t.Errorf("expected interval %s, got %s", d, wr.ReaperInterval())
		}
	}
}

func TestSetReaperInterval_InvalidRange(t *testing.T) {
	wr := &WaitingRoom{}
	if err := wr.Init(5); err != nil {
		t.Fatal(err)
	}
	defer wr.Stop()

	cases := []time.Duration{
		0,
		time.Millisecond,
		reaperMinInterval - time.Nanosecond,
		reaperMaxInterval + time.Nanosecond,
	}

	for _, d := range cases {
		err := wr.SetReaperInterval(d)
		if err == nil {
			t.Errorf("expected error for interval %s, got nil", d)
		}
		if _, ok := err.(ErrReaperInterval); !ok {
			t.Errorf("expected ErrReaperInterval for %s, got %T", d, err)
		}
	}
}

// ── Introspection tests ──────────────────────────────────────────────────────

func TestQueueDepth_AccurateWhileWaiting(t *testing.T) {
	const cap = 1
	wr := &WaitingRoom{}
	if err := wr.Init(cap); err != nil {
		t.Fatal(err)
	}
	defer wr.Stop()

	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	r := newTestRouter(wr, serving, release)

	go func() {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		r.ServeHTTP(httptest.NewRecorder(), req)
	}()
	<-serving

	const queued = 3
	for i := 0; i < queued; i++ {
		go func() {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			r.ServeHTTP(httptest.NewRecorder(), req)
		}()
	}

	waitForQueue(t, wr, queued, 2*time.Second)

	if d := wr.QueueDepth(); d != queued {
		t.Errorf("expected queue depth %d, got %d", queued, d)
	}

	close(release)
}

func TestUtilization_BetweenZeroAndOne(t *testing.T) {
	wr := &WaitingRoom{}
	if err := wr.Init(10); err != nil {
		t.Fatal(err)
	}
	defer wr.Stop()

	u := wr.Utilization()
	if u < 0.0 || u > 1.0 {
		t.Errorf("utilization out of range: %f", u)
	}

	u = wr.UtilizationSmoothed()
	if u < 0.0 || u > 1.0 {
		t.Errorf("smoothed utilization out of range: %f", u)
	}
}

// ── Context cancellation tests ───────────────────────────────────────────────

func TestContextCancel_WaiterExitsCleanly(t *testing.T) {
	const cap = 1
	wr := &WaitingRoom{}
	if err := wr.Init(cap); err != nil {
		t.Fatal(err)
	}
	defer wr.Stop()

	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	r := newTestRouter(wr, serving, release)

	// Fill the slot.
	go func() {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		r.ServeHTTP(httptest.NewRecorder(), req)
	}()
	<-serving

	// Second request with a cancellable context.
	ctx, cancel := context.WithCancel(context.Background())
	req2 := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)

	done := make(chan struct{})
	go func() {
		r.ServeHTTP(httptest.NewRecorder(), req2)
		close(done)
	}()

	waitForQueue(t, wr, 1, 2*time.Second)

	// Cancel the context — waiter should exit.
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for cancelled request to exit")
	}

	// Release the active slot and give nowServing time to advance.
	close(release)
	time.Sleep(50 * time.Millisecond)

	// Queue depth should return to zero.
	if d := wr.QueueDepth(); d != 0 {
		t.Errorf("expected queue depth 0 after cancel, got %d", d)
	}
}

func TestContextCancel_NoSlotLeak(t *testing.T) {
	const cap = 2
	wr := &WaitingRoom{}
	if err := wr.Init(cap); err != nil {
		t.Fatal(err)
	}
	defer wr.Stop()

	serving := make(chan struct{}, cap)
	release := make(chan struct{})
	r := newTestRouter(wr, serving, release)

	// Fill both slots.
	for i := 0; i < cap; i++ {
		go func() {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			r.ServeHTTP(httptest.NewRecorder(), req)
		}()
		<-serving
	}

	// Cancel 5 waiters.
	const waiters = 5
	for i := 0; i < waiters; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
		go func() {
			r.ServeHTTP(httptest.NewRecorder(), req)
		}()
		// Cancel immediately after dispatch.
		time.Sleep(10 * time.Millisecond)
		cancel()
	}

	// Release the two active slots.
	close(release)

	// After everything settles, Len should be 0 — no leaked slots.
	time.Sleep(100 * time.Millisecond)
	if l := wr.Len(); l != 0 {
		t.Errorf("expected Len 0 after all releases and cancels, got %d", l)
	}
}

// ── Concurrency / race tests ─────────────────────────────────────────────────

func TestConcurrent_NoRaceUnderLoad(t *testing.T) {
	const cap = 10
	const total = 100

	wr := &WaitingRoom{}
	if err := wr.Init(cap); err != nil {
		t.Fatal(err)
	}
	defer wr.Stop()

	r := gin.New()
	wr.RegisterRoutes(r)
	r.GET("/", func(c *gin.Context) {
		time.Sleep(5 * time.Millisecond)
		c.Status(http.StatusOK)
	})

	var wg sync.WaitGroup
	for i := 0; i < total; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
		}()
	}

	wg.Wait()

	if l := wr.Len(); l != 0 {
		t.Errorf("expected Len 0 after all requests complete, got %d", l)
	}
	if d := wr.QueueDepth(); d != 0 {
		t.Errorf("expected QueueDepth 0 after all requests complete, got %d", d)
	}
}

func TestConcurrent_SetCapDuringLoad(t *testing.T) {
	wr := &WaitingRoom{}
	if err := wr.Init(5); err != nil {
		t.Fatal(err)
	}
	defer wr.Stop()

	r := gin.New()
	wr.RegisterRoutes(r)
	r.GET("/", func(c *gin.Context) {
		time.Sleep(10 * time.Millisecond)
		c.Status(http.StatusOK)
	})

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			r.ServeHTTP(httptest.NewRecorder(), req)
		}()
	}

	// Resize during load — should not panic or deadlock.
	go func() {
		for _, c := range []int32{3, 7, 5, 10, 5} {
			time.Sleep(20 * time.Millisecond)
			wr.SetCap(c)
		}
	}()

	wg.Wait()
}

// ── Benchmark tests ──────────────────────────────────────────────────────────

func BenchmarkFastPath(b *testing.B) {
	wr := &WaitingRoom{}
	if err := wr.Init(int32(b.N) + 1); err != nil {
		b.Fatal(err)
	}
	defer wr.Stop()

	r := gin.New()
	wr.RegisterRoutes(r)
	r.GET("/", func(c *gin.Context) { c.Status(http.StatusOK) })

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
		}
	})
}

func BenchmarkQueueDepth(b *testing.B) {
	wr := &WaitingRoom{}
	if err := wr.Init(10); err != nil {
		b.Fatal(err)
	}
	defer wr.Stop()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = wr.QueueDepth()
	}
}

func BenchmarkUtilizationSmoothed(b *testing.B) {
	wr := &WaitingRoom{}
	if err := wr.Init(10); err != nil {
		b.Fatal(err)
	}
	defer wr.Stop()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = wr.UtilizationSmoothed()
	}
}

// ── Fuzz tests ───────────────────────────────────────────────────────────────

func FuzzWaitingRoom(f *testing.F) {
	f.Add(int32(1), int32(1))
	f.Add(int32(10), int32(50))
	f.Add(int32(100), int32(200))

	f.Fuzz(func(t *testing.T, cap int32, requests int32) {
		if cap < 1 || cap > 1000 {
			return
		}
		if requests < 1 || requests > 500 {
			return
		}

		wr := &WaitingRoom{}
		if err := wr.Init(cap); err != nil {
			return
		}
		defer wr.Stop()

		r := gin.New()
		wr.RegisterRoutes(r)
		r.GET("/", func(c *gin.Context) {
			time.Sleep(time.Millisecond)
			c.Status(http.StatusOK)
		})

		var wg sync.WaitGroup
		var served atomic.Int32

		for i := int32(0); i < requests; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				req := httptest.NewRequest(http.MethodGet, "/", nil)
				w := httptest.NewRecorder()
				r.ServeHTTP(w, req)
				served.Add(1)
			}()
		}

		wg.Wait()

		// Invariants that must always hold.
		if l := wr.Len(); l != 0 {
			t.Errorf("invariant violated: Len=%d after all requests complete", l)
		}
		if d := wr.QueueDepth(); d != 0 {
			t.Errorf("invariant violated: QueueDepth=%d after all requests complete", d)
		}
		if int32(served.Load()) != requests {
			t.Errorf("invariant violated: served=%d expected=%d", served.Load(), requests)
		}
	})
}

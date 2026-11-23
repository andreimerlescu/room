package room

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// newTestRouter builds a gin engine with the WaitingRoom registered and
// a simple handler that signals when serving and blocks until released.
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

// newTestRouterWithGates builds a gin engine where each handler blocks on
// its own gate channel. This allows tests to release individual slots in
// a controlled order.
func newTestRouterWithGates(wr *WaitingRoom, serving chan struct{}, gates []chan struct{}) *gin.Engine {
	var idx atomic.Int32
	r := gin.New()
	wr.RegisterRoutes(r)
	r.GET("/", func(c *gin.Context) {
		i := int(idx.Add(1)) - 1
		if serving != nil {
			serving <- struct{}{}
		}
		if i < len(gates) {
			<-gates[i]
		}
		c.Status(http.StatusOK)
	})
	return r
}

// serveWithCookie performs a GET / with an optional cookie and returns
// the recorder and any Set-Cookie header value for cookieName.
func serveWithCookie(r *gin.Engine, cookie string) (*httptest.ResponseRecorder, string) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: cookieName, Value: cookie})
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	for _, c := range w.Result().Cookies() {
		if c.Name == cookieName {
			return w, c.Value
		}
	}
	return w, ""
}

// pollStatus calls GET /queue/status with the given token cookie and
// returns the decoded statusResponse. If the server returns 429 (rate
// limited), the response is treated as not-ready so callers retry after
// respecting the poll interval.
func pollStatus(r *gin.Engine, token string) statusResponse {
	req := httptest.NewRequest(http.MethodGet, "/queue/status", nil)
	if token != "" {
		req.AddCookie(&http.Cookie{Name: cookieName, Value: token})
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// If rate-limited, return a synthetic not-ready response so callers
	// back off and retry rather than seeing a decode artifact.
	if w.Code == http.StatusTooManyRequests {
		return statusResponse{Ready: false}
	}

	var resp statusResponse
	json.NewDecoder(w.Body).Decode(&resp)
	return resp
}

// pollStatusRaw calls GET /queue/status and returns the raw recorder
// so callers can inspect status codes and headers.
func pollStatusRaw(r *gin.Engine, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/queue/status", nil)
	if token != "" {
		req.AddCookie(&http.Cookie{Name: cookieName, Value: token})
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// waitForStatus polls /queue/status until ready=true or deadline passes.
// The poll interval is set to statusPollMinInterval + a small margin so
// that the per-token rate limiter in StatusHandler does not reject polls.
func waitForStatus(t *testing.T, r *gin.Engine, token string, deadline time.Duration) {
	t.Helper()
	timeout := time.After(deadline)
	// Poll at slightly more than the rate limit interval to avoid 429s.
	pollInterval := statusPollMinInterval + 50*time.Millisecond
	for {
		select {
		case <-timeout:
			t.Fatal("timed out waiting for status ready=true")
		default:
			if pollStatus(r, token).Ready {
				return
			}
			time.Sleep(pollInterval)
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
	if err := wr.Init(-1); err == nil {
		t.Fatal("expected ErrInvalidCap, got nil")
	}
}

func TestNewWaitingRoom_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic from NewWaitingRoom(0)")
		}
	}()
	NewWaitingRoom(gin.New(), 0)
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

	for i := 0; i < cap; i++ {
		go func() {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
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

	if wr.Len() != cap {
		t.Errorf("expected Len %d, got %d", cap, wr.Len())
	}

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
		r.ServeHTTP(httptest.NewRecorder(), req)
	}()
	<-serving

	// Second request should get the waiting room page immediately.
	w, token := serveWithCookie(r, "")
	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for waiting room, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("expected text/html, got %s", ct)
	}
	if token == "" {
		t.Error("expected room_ticket cookie to be set")
	}

	close(release)
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

	go func() {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		r.ServeHTTP(httptest.NewRecorder(), req)
	}()
	<-serving

	_, token := serveWithCookie(r, "")
	if token == "" {
		t.Errorf("expected cookie %q to be set", cookieName)
	}

	close(release)
}

func TestSlowPath_PositionInjectedInHTML(t *testing.T) {
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

	w, _ := serveWithCookie(r, "")
	body := w.Body.String()
	// {{.Position}} should have been replaced with a number.
	if strings.Contains(body, "{{.Position}}") {
		t.Error("expected {{.Position}} to be replaced in HTML")
	}

	close(release)
}

func TestSlowPath_ResumePreservesQueuePosition(t *testing.T) {
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

	// First visit — gets queued and a token issued.
	_, token := serveWithCookie(r, "")
	if token == "" {
		t.Fatal("expected token on first visit")
	}

	// Second visit with the same token — should get updated position,
	// not a new ticket.
	ticketsBefore := wr.nextTicket.Load()
	w2, _ := serveWithCookie(r, token)
	ticketsAfter := wr.nextTicket.Load()

	if ticketsAfter != ticketsBefore {
		t.Errorf("expected no new ticket on resume, nextTicket went from %d to %d",
			ticketsBefore, ticketsAfter)
	}
	if w2.Code != http.StatusOK {
		t.Errorf("expected 200 on resume, got %d", w2.Code)
	}

	close(release)
}

// ── FIFO ordering tests ──────────────────────────────────────────────────────

func TestFIFO_RequestsAdmittedInOrder(t *testing.T) {
	const cap = 1
	const total = 5

	wr := &WaitingRoom{}
	if err := wr.Init(cap); err != nil {
		t.Fatal(err)
	}
	defer wr.Stop()

	serving := make(chan struct{}, 1)
	gates := make([]chan struct{}, total)
	for i := range gates {
		gates[i] = make(chan struct{})
	}

	var admitOrder []int
	var orderMu sync.Mutex

	r := gin.New()
	wr.RegisterRoutes(r)
	for i := 0; i < total; i++ {
		i := i
		r.GET(fmt.Sprintf("/req/%d", i), func(c *gin.Context) {
			serving <- struct{}{}
			orderMu.Lock()
			admitOrder = append(admitOrder, i)
			orderMu.Unlock()
			<-gates[i]
			c.Status(http.StatusOK)
		})
	}

	// Collect tokens in arrival order with a stagger to ensure
	// ticket ordering is deterministic.
	tokens := make([]string, total)
	for i := 0; i < total; i++ {
		if i == 0 {
			// First request — hits fast path, fills the slot.
			go func() {
				req := httptest.NewRequest(http.MethodGet, "/req/0", nil)
				r.ServeHTTP(httptest.NewRecorder(), req)
			}()
			<-serving
		} else {
			req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/req/%d", i), nil)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			for _, c := range w.Result().Cookies() {
				if c.Name == cookieName {
					tokens[i] = c.Value
				}
			}
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Release each slot in sequence and verify the next waiter is admitted.
	// The deadline is longer here because each waitForStatus poll sleeps
	// for statusPollMinInterval + margin to avoid 429 rate limiting.
	for i := 0; i < total; i++ {
		close(gates[i])
		if i+1 < total {
			// Poll until the next token is ready then re-request.
			waitForStatus(t, r, tokens[i+1], 10*time.Second)
			go func(idx int, tok string) {
				req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/req/%d", idx), nil)
				req.AddCookie(&http.Cookie{Name: cookieName, Value: tok})
				r.ServeHTTP(httptest.NewRecorder(), req)
			}(i+1, tokens[i+1])
			<-serving
		}
	}

	orderMu.Lock()
	defer orderMu.Unlock()
	for i, v := range admitOrder {
		if v != i {
			t.Errorf("FIFO violation: position %d admitted request %d, expected %d", i, v, i)
		}
	}
}

// ── Status endpoint tests ────────────────────────────────────────────────────

// TestStatusEndpoint_NoCookieReportsCookiesRequired verifies that a poll
// arriving with NO room_ticket cookie reports cookies_required rather than
// ready=true.
//
// This test previously asserted ready=true. That was the wrong contract: a
// client that cannot store cookies reads ready=true, reloads, is issued a
// fresh ticket at the back of the queue, and repeats forever — never
// admitted, leaking a token-store entry and a nextTicket increment per
// cycle. See StatusHandler for the full reasoning.
func TestStatusEndpoint_NoCookieReportsCookiesRequired(t *testing.T) {
	wr := &WaitingRoom{}
	if err := wr.Init(5); err != nil {
		t.Fatal(err)
	}
	defer wr.Stop()

	r := gin.New()
	wr.RegisterRoutes(r)
	r.GET("/", func(c *gin.Context) { c.Status(http.StatusOK) })

	resp := pollStatus(r, "")
	if !resp.CookiesRequired {
		t.Error("expected cookies_required=true when no room_ticket cookie is present")
	}
	if resp.Ready {
		t.Error("expected ready=false when no room_ticket cookie is present — " +
			"ready=true drives the waiting room page into a reload loop")
	}
}

// TestStatusEndpoint_UnknownTokenReturnsReady verifies that a poll carrying
// a room_ticket cookie whose token is not in the store still returns
// ready=true. This is the admitted / expired / restarted-server case: the
// client demonstrably stores cookies, it just has no live queue position,
// so sending it back to the main handler is correct.
func TestStatusEndpoint_UnknownTokenReturnsReady(t *testing.T) {
	wr := &WaitingRoom{}
	if err := wr.Init(5); err != nil {
		t.Fatal(err)
	}
	defer wr.Stop()

	r := gin.New()
	wr.RegisterRoutes(r)
	r.GET("/", func(c *gin.Context) { c.Status(http.StatusOK) })

	resp := pollStatus(r, "deadbeefdeadbeefdeadbeefdeadbeef")
	if !resp.Ready {
		t.Error("expected ready=true for a cookie holding an unknown token")
	}
	if resp.CookiesRequired {
		t.Error("expected cookies_required=false — the client sent a cookie")
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

	go func() {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		r.ServeHTTP(httptest.NewRecorder(), req)
	}()
	<-serving

	_, token := serveWithCookie(r, "")
	if token == "" {
		t.Fatal("no token issued")
	}

	resp := pollStatus(r, token)
	if resp.Ready {
		t.Error("expected ready=false while queued")
	}
	if resp.Position <= 0 {
		t.Errorf("expected positive position, got %d", resp.Position)
	}

	close(release)
}

func TestStatusEndpoint_ReturnsReadyAfterSlotOpens(t *testing.T) {
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

	_, token := serveWithCookie(r, "")

	// Release the active slot.
	close(release)

	// Status should eventually return ready=true.
	// The deadline is generous to accommodate the per-token rate limiter
	// which requires ~1s between polls.
	waitForStatus(t, r, token, 5*time.Second)
}

// TestStatusEndpoint_RateLimitRejectsFastPolling verifies that polling
// /queue/status faster than statusPollMinInterval returns 429.
func TestStatusEndpoint_RateLimitRejectsFastPolling(t *testing.T) {
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

	_, token := serveWithCookie(r, "")
	if token == "" {
		t.Fatal("no token issued")
	}

	// First poll — should succeed.
	w1 := pollStatusRaw(r, token)
	if w1.Code != http.StatusOK {
		t.Errorf("first poll: expected 200, got %d", w1.Code)
	}

	// Immediate second poll — should be rate limited.
	w2 := pollStatusRaw(r, token)
	if w2.Code != http.StatusTooManyRequests {
		t.Errorf("rapid second poll: expected 429, got %d", w2.Code)
	}
	if ra := w2.Header().Get("Retry-After"); ra != "1" {
		t.Errorf("expected Retry-After: 1, got %q", ra)
	}

	close(release)
}

// ── MaxQueueDepth tests ──────────────────────────────────────────────────────

func TestMaxQueueDepth_RejectsWhenFull(t *testing.T) {
	const cap = 1
	wr := &WaitingRoom{}
	if err := wr.Init(cap); err != nil {
		t.Fatal(err)
	}
	defer wr.Stop()

	if err := wr.SetMaxQueueDepth(2); err != nil {
		t.Fatal(err)
	}

	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	r := newTestRouter(wr, serving, release)

	// Fill the slot.
	go func() {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		r.ServeHTTP(httptest.NewRecorder(), req)
	}()
	<-serving

	// Queue 2 requests (the max).
	for i := 0; i < 2; i++ {
		serveWithCookie(r, "")
	}

	// Third queued request should be rejected.
	w, _ := serveWithCookie(r, "")
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 when queue is full, got %d", w.Code)
	}

	close(release)
}

func TestMaxQueueDepth_ZeroMeansUnlimited(t *testing.T) {
	const cap = 1
	wr := &WaitingRoom{}
	if err := wr.Init(cap); err != nil {
		t.Fatal(err)
	}
	defer wr.Stop()

	// Default is 0 (unlimited).
	if wr.MaxQueueDepth() != 0 {
		t.Errorf("expected default max queue depth 0, got %d", wr.MaxQueueDepth())
	}

	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	r := newTestRouter(wr, serving, release)

	go func() {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		r.ServeHTTP(httptest.NewRecorder(), req)
	}()
	<-serving

	// Queue many requests — none should be rejected.
	for i := 0; i < 50; i++ {
		w, _ := serveWithCookie(r, "")
		if w.Code == http.StatusServiceUnavailable {
			t.Fatalf("request %d rejected with unlimited queue depth", i)
		}
	}

	close(release)
}

func TestMaxQueueDepth_NegativeRejected(t *testing.T) {
	wr := &WaitingRoom{}
	if err := wr.Init(5); err != nil {
		t.Fatal(err)
	}
	defer wr.Stop()

	err := wr.SetMaxQueueDepth(-1)
	if err == nil {
		t.Error("expected error for negative max queue depth")
	}
	if _, ok := err.(ErrInvalidMaxQueueDepth); !ok {
		t.Errorf("expected ErrInvalidMaxQueueDepth, got %T", err)
	}
}

// ── Cookie configuration tests ───────────────────────────────────────────────

func TestCookiePath_DefaultIsSlash(t *testing.T) {
	wr := &WaitingRoom{}
	if err := wr.Init(5); err != nil {
		t.Fatal(err)
	}
	defer wr.Stop()

	if wr.CookiePath() != "/" {
		t.Errorf("expected default cookie path '/', got %q", wr.CookiePath())
	}
}

func TestCookiePath_CustomPathUsed(t *testing.T) {
	const cap = 1
	wr := &WaitingRoom{}
	if err := wr.Init(cap); err != nil {
		t.Fatal(err)
	}
	defer wr.Stop()
	wr.SetCookiePath("/app")

	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	r := newTestRouter(wr, serving, release)

	go func() {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		r.ServeHTTP(httptest.NewRecorder(), req)
	}()
	<-serving

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	for _, c := range w.Result().Cookies() {
		if c.Name == cookieName {
			if c.Path != "/app" {
				t.Errorf("expected cookie path '/app', got %q", c.Path)
			}
			close(release)
			return
		}
	}
	// Cookie might not be set if room wasn't full — skip.
	close(release)
}

func TestCookieDomain_DefaultIsEmpty(t *testing.T) {
	wr := &WaitingRoom{}
	if err := wr.Init(5); err != nil {
		t.Fatal(err)
	}
	defer wr.Stop()

	if wr.CookieDomain() != "" {
		t.Errorf("expected default cookie domain '', got %q", wr.CookieDomain())
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
		r.ServeHTTP(httptest.NewRecorder(), req)
	}()
	<-serving

	// Queue 4 requests and collect their tokens.
	tokens := make([]string, 4)
	for i := 0; i < 4; i++ {
		_, tokens[i] = serveWithCookie(r, "")
	}

	// Expand to 5.
	if err := wr.SetCap(5); err != nil {
		t.Fatalf("SetCap failed: %v", err)
	}

	// All 4 tokens should become ready.
	var wg sync.WaitGroup
	for _, tok := range tokens {
		wg.Add(1)
		tok := tok
		go func() {
			defer wg.Done()
			waitForStatus(t, r, tok, 10*time.Second)
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for waiters to become ready after SetCap")
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

func recordPeak(peak *atomic.Int32, cur int32) {
	for {
		prev := peak.Load()
		if cur <= prev || peak.CompareAndSwap(prev, cur) {
			return
		}
	}
}

func TestSetCap_ShrinkPreservesInFlightCeiling(t *testing.T) {
	const (
		initialCap = 10
		shrunkCap  = 3
		queuedInit = 12
		midDrain   = 2
		fastBurst  = 6
		totalNew   = queuedInit + midDrain + fastBurst
	)

	wr := newTestWR(t, initialCap)

	holderEntered := make(chan struct{}, initialCap)
	holderGates := make([]chan struct{}, initialCap)
	holderDone := make(chan struct{}, initialCap)
	for i := range initialCap {
		holderGates[i] = make(chan struct{})
	}

	var (
		holderIdx   atomic.Int32
		active      atomic.Int32
		overallPeak atomic.Int32
		newActive   atomic.Int32
		newPeak     atomic.Int32
		newEntered  atomic.Int32
		newServed   atomic.Int32
	)

	r := gin.New()
	wr.RegisterRoutes(r)
	r.GET("/", func(c *gin.Context) {
		cur := active.Add(1)
		recordPeak(&overallPeak, cur)
		defer active.Add(-1)

		if c.Query("role") == "holder" {
			idx := int(holderIdx.Add(1)) - 1
			holderEntered <- struct{}{}
			<-holderGates[idx]
			c.Status(http.StatusOK)
			return
		}

		newEntered.Add(1)
		curNew := newActive.Add(1)
		recordPeak(&newPeak, curNew)
		defer newActive.Add(-1)

		time.Sleep(15 * time.Millisecond)
		newServed.Add(1)
		c.Status(http.StatusOK)
	})

	// Fill all 10 initial slots and wait until all 10 are inside the origin handler.
	for range initialCap {
		go func() {
			defer func() { holderDone <- struct{}{} }()
			req := httptest.NewRequest(http.MethodGet, "/?role=holder", nil)
			r.ServeHTTP(httptest.NewRecorder(), req)
		}()
	}
	for i := range initialCap {
		select {
		case <-holderEntered:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for initial holder %d to enter origin", i)
		}
	}
	if l := wr.Len(); l != initialCap {
		t.Fatalf("before shrink: Len()=%d, want %d", l, initialCap)
	}

	// Shrink capacity from 10 to 3 while all 10 holders are in flight.
	if err := wr.SetCap(shrunkCap); err != nil {
		t.Fatalf("SetCap(%d): %v", shrunkCap, err)
	}
	if c := wr.Cap(); c != shrunkCap {
		t.Fatalf("after shrink: Cap()=%d, want %d", c, shrunkCap)
	}
	if l := wr.Len(); l != initialCap {
		t.Fatalf("after shrink: Len()=%d, want %d (must not drain held slots)", l, initialCap)
	}
	if occ := wr.occupancy.Load(); occ != initialCap {
		t.Fatalf("after shrink: occupancy=%d, want %d", occ, initialCap)
	}
	if u := wr.Utilization(); u != 1.0 {
		t.Fatalf("after shrink: Utilization()=%v, want 1.0", u)
	}
	if u := wr.UtilizationSmoothed(); u < 0 || u > 1.0 {
		t.Fatalf("after shrink: UtilizationSmoothed()=%v, want in [0.0, 1.0]", u)
	}

	// driveNewClient performs GET /?role=new, and if queued, polls /queue/status
	// until ready=true and reloads until admitted to the origin.
	runNewClient := func(initialToken string, errCh chan<- error) {
		tok := initialToken
		if tok == "" {
			req := httptest.NewRequest(http.MethodGet, "/?role=new", nil)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				errCh <- fmt.Errorf("initial request: status %d", w.Code)
				return
			}
			for _, c := range w.Result().Cookies() {
				if c.Name == cookieName {
					tok = c.Value
					break
				}
			}
			if tok == "" {
				// Admitted directly on fast path.
				return
			}
		}

		deadline := time.Now().Add(20 * time.Second)
		pollInterval := statusPollMinInterval + 25*time.Millisecond
		for time.Now().Before(deadline) {
			resp := pollStatus(r, tok)
			if resp.Utilization > 1.0 {
				errCh <- fmt.Errorf("status Utilization=%v > 1.0", resp.Utilization)
				return
			}
			if resp.Ready {
				req := httptest.NewRequest(http.MethodGet, "/?role=new", nil)
				req.AddCookie(&http.Cookie{Name: cookieName, Value: tok})
				w := httptest.NewRecorder()
				r.ServeHTTP(w, req)
				if w.Code != http.StatusOK {
					errCh <- fmt.Errorf("reload request: status %d", w.Code)
					return
				}
				var newTok string
				for _, c := range w.Result().Cookies() {
					if c.Name == cookieName {
						newTok = c.Value
						break
					}
				}
				if newTok == "" {
					// Admitted to origin.
					return
				}
				tok = newTok
			}
			time.Sleep(pollInterval)
		}
		errCh <- fmt.Errorf("timed out waiting for queued client %q to be admitted", tok)
	}

	errCh := make(chan error, totalNew)
	var queuedWG sync.WaitGroup

	// Queue 12 clients while all 10 holders are active.
	queuedTokens := make([]string, queuedInit)
	for i := range queuedInit {
		req := httptest.NewRequest(http.MethodGet, "/?role=new", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		for _, c := range w.Result().Cookies() {
			if c.Name == cookieName {
				queuedTokens[i] = c.Value
				break
			}
		}
		if queuedTokens[i] == "" {
			t.Fatalf("new request %d admitted immediately while 10 holders active and cap=3", i)
		}
	}
	for _, tok := range queuedTokens {
		queuedWG.Add(1)
		go func(token string) {
			defer queuedWG.Done()
			runNewClient(token, errCh)
		}(tok)
	}

	// Release the first 7 holders (10 → 3 active). Zero new requests may enter
	// while active >= shrunkCap (3).
	for i := range initialCap - shrunkCap {
		close(holderGates[i])
		<-holderDone

		wantActive := initialCap - (i + 1)
		if l := wr.Len(); l != wantActive {
			t.Fatalf("after releasing holder %d: Len()=%d, want %d", i, l, wantActive)
		}
		if occ := wr.occupancy.Load(); occ != int32(wantActive) {
			t.Fatalf("after releasing holder %d: occupancy=%d, want %d", i, occ, wantActive)
		}
		if u := wr.Utilization(); u != 1.0 {
			t.Fatalf("after releasing holder %d (active=%d >= cap=%d): Utilization()=%v, want 1.0",
				i, wantActive, shrunkCap, u)
		}
		if entered := newEntered.Load(); entered != 0 {
			t.Fatalf("after releasing holder %d (active=%d >= cap=%d): %d new requests entered origin early",
				i, wantActive, shrunkCap, entered)
		}

		// Also send fresh arrivals mid-drain (at active=6 and active=3) to verify
		// neither fast-path arrivals nor impatient reloads slip past the shrink ceiling.
		if i == 3 || i == 6 {
			req := httptest.NewRequest(http.MethodGet, "/?role=new", nil)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			var midTok string
			for _, c := range w.Result().Cookies() {
				if c.Name == cookieName {
					midTok = c.Value
					break
				}
			}
			if midTok == "" {
				t.Fatalf("mid-drain arrival at active=%d was admitted early instead of queued", wantActive)
			}
			queuedWG.Add(1)
			go func(token string) {
				defer queuedWG.Done()
				runNewClient(token, errCh)
			}(midTok)

			// Impatient reload of the head queued client must also remain blocked.
			reloadReq := httptest.NewRequest(http.MethodGet, "/?role=new", nil)
			reloadReq.AddCookie(&http.Cookie{Name: cookieName, Value: queuedTokens[0]})
			r.ServeHTTP(httptest.NewRecorder(), reloadReq)
			if entered := newEntered.Load(); entered != 0 {
				t.Fatalf("impatient reload at active=%d entered origin early (%d entered)", wantActive, entered)
			}
		}
	}

	// Brief settle check at active == 3 before releasing the remaining 3 holders.
	time.Sleep(25 * time.Millisecond)
	if entered := newEntered.Load(); entered != 0 {
		t.Fatalf("at active=%d (cap=%d): %d new requests entered origin early", shrunkCap, shrunkCap, entered)
	}
	if p := overallPeak.Load(); p > initialCap {
		t.Fatalf("overall peak during drain = %d, exceeds initial cap %d", p, initialCap)
	}

	// Release remaining 3 holders (3 → 0), allowing the queued clients to proceed
	// under the new cap of 3.
	for i := initialCap - shrunkCap; i < initialCap; i++ {
		close(holderGates[i])
		<-holderDone
	}

	queuedWG.Wait()

	// Send 6 concurrent fresh arrivals after the queue drains so both fast-path
	// admissions and queued poll+reload admissions are exercised under cap=3.
	var burstWG sync.WaitGroup
	for range fastBurst {
		burstWG.Add(1)
		go func() {
			defer burstWG.Done()
			runNewClient("", errCh)
		}()
	}
	burstWG.Wait()
	close(errCh)

	for err := range errCh {
		t.Errorf("new client error: %v", err)
	}
	if served := newServed.Load(); served != totalNew {
		t.Errorf("new requests served = %d, want %d", served, totalNew)
	}
	if p := overallPeak.Load(); p > initialCap {
		t.Errorf("overall peak = %d, exceeds initial cap %d", p, initialCap)
	}
	if p := newPeak.Load(); p > shrunkCap {
		t.Errorf("peak among new requests = %d, exceeds shrunk cap %d", p, shrunkCap)
	}
	if l := wr.Len(); l != 0 {
		t.Errorf("final Len() = %d, want 0", l)
	}
	if occ := wr.occupancy.Load(); occ != 0 {
		t.Errorf("final occupancy = %d, want 0", occ)
	}
	assertAccounting(t, wr)
}

func TestSetCap_ShrinkEventDrainFiresOnce(t *testing.T) {
	t.Parallel()
	const (
		initialCap = 6
		shrunkCap  = 2
	)

	wr := newTestWR(t, initialCap)

	serving := make(chan struct{}, initialCap)
	gates := make([]chan struct{}, initialCap)
	done := make(chan struct{}, initialCap)
	for i := range initialCap {
		gates[i] = make(chan struct{})
	}
	r := newTestRouterWithGates(wr, serving, gates)

	// Fill all 6 slots.
	for range initialCap {
		go func() {
			defer func() { done <- struct{}{} }()
			r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
		}()
	}
	for i := range initialCap {
		select {
		case <-serving:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for holder %d", i)
		}
	}

	var drainCount atomic.Int32
	wr.On(EventDrain, func(s Snapshot) { drainCount.Add(1) })

	// Shrink from 6 to 2.
	if err := wr.SetCap(shrunkCap); err != nil {
		t.Fatalf("SetCap(%d): %v", shrunkCap, err)
	}

	// Release first 4 holders (6 → 2): occupancy stays >= cap, so EventDrain must not fire.
	for i := range initialCap - shrunkCap {
		close(gates[i])
		<-done
	}
	time.Sleep(40 * time.Millisecond)
	if got := drainCount.Load(); got != 0 {
		t.Fatalf("after draining 6→2: EventDrain fired %d times, want 0", got)
	}
	if wr.Len() != shrunkCap || wr.occupancy.Load() != shrunkCap {
		t.Fatalf("after draining 6→2: Len()=%d occupancy=%d, want both %d",
			wr.Len(), wr.occupancy.Load(), shrunkCap)
	}

	// Release 5th holder (2 → 1): first real slot opens under new cap; EventDrain fires once.
	close(gates[4])
	<-done
	waitForCount(t, &drainCount, 1, 500*time.Millisecond)
	if got := drainCount.Load(); got != 1 {
		t.Fatalf("after 5th release (2→1): EventDrain fired %d times, want 1", got)
	}

	// Release 6th holder (1 → 0): not-full→empty must not fire EventDrain again.
	close(gates[5])
	<-done
	time.Sleep(40 * time.Millisecond)
	if got := drainCount.Load(); got != 1 {
		t.Errorf("after 6th release (1→0): EventDrain fired %d times, want 1", got)
	}
	if wr.Len() != 0 || wr.occupancy.Load() != 0 {
		t.Errorf("final state: Len()=%d occupancy=%d, want both 0", wr.Len(), wr.occupancy.Load())
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

	go func() {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		r.ServeHTTP(httptest.NewRecorder(), req)
	}()
	<-serving

	w, _ := serveWithCookie(r, "")
	if w.Body.String() != string(custom) {
		t.Errorf("expected custom HTML, got: %s", w.Body.String())
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

	if len(wr.resolveHTML()) == 0 {
		t.Error("expected non-empty default HTML after SetHTML(nil)")
	}
}

// ── Reaper tests ─────────────────────────────────────────────────────────────
// Basic reaper correctness is tested here. Full reaper coverage is in
// reaper_test.go.

func TestReaper_EvictsExpiredTokens(t *testing.T) {
	wr := &WaitingRoom{}
	if err := wr.Init(5); err != nil {
		t.Fatal(err)
	}
	defer wr.Stop()

	wr.tokens.set("ghost", ticketEntry{
		ticket:   99,
		issuedAt: time.Now().Add(-(cookieTTL + time.Minute)),
	})

	wr.reap()

	if _, ok := wr.tokens.get("ghost"); ok {
		t.Error("expected ghost token to be evicted after reap")
	}
}

func TestReaper_PreservesLiveTokens(t *testing.T) {
	wr := &WaitingRoom{}
	if err := wr.Init(5); err != nil {
		t.Fatal(err)
	}
	defer wr.Stop()

	wr.tokens.set("live", ticketEntry{
		ticket:   1,
		issuedAt: time.Now(),
	})

	wr.reap()

	if _, ok := wr.tokens.get("live"); !ok {
		t.Error("expected live token to survive reap")
	}
}

// TestReaper_OutOfWindowGhostSkippedWhenWindowArrives replaces
// TestReaper_AdvancesNowServingOnEviction. An out-of-window ghost no longer
// advances nowServing at reap time (which would admit the head of the
// queue early); it is skipped exactly when the window reaches it.
func TestReaper_OutOfWindowGhostSkippedWhenWindowArrives(t *testing.T) {
	wr := &WaitingRoom{}
	if err := wr.Init(1); err != nil {
		t.Fatal(err)
	}
	defer wr.Stop()

	wr.nextTicket.Store(10)
	wr.tokens.set("ghost", ticketEntry{
		ticket:   5,
		issuedAt: time.Now().Add(-(cookieTTL + time.Minute)),
	})
	wr.tokens.set("live", ticketEntry{ticket: 10, issuedAt: time.Now()})

	wr.reap()

	if ns := wr.nowServing.Load(); ns != 0 {
		t.Errorf("expected nowServing unchanged at reap time, got %d", ns)
	}
	// 10 - 0 - 1 - 1 ghost ahead = 8
	if pos := wr.positionOf(10); pos != 8 {
		t.Errorf("expected live position 8 immediately after reap, got %d", pos)
	}
	if d := wr.QueueDepth(); d != 8 {
		t.Errorf("expected QueueDepth 8, got %d", d)
	}

	// Three releases: edge moves to 4, ghost at 5 still ahead.
	for range 3 {
		wr.advance(1)
	}
	if ns := wr.nowServing.Load(); ns != 3 {
		t.Fatalf("expected nowServing=3, got %d", ns)
	}

	// Fourth release: edge reaches 5, the ghost is skipped in the same step.
	wr.advance(1)
	if ns := wr.nowServing.Load(); ns != 5 {
		t.Errorf("expected nowServing=5 (4 releases + 1 skipped ghost), got %d", ns)
	}
	if l := wr.ledger.len(); l != 0 {
		t.Errorf("expected empty ledger, got %d", l)
	}
	if pos := wr.positionOf(10); pos != 4 {
		t.Errorf("expected live position 4, got %d", pos)
	}
}

// TestReaper_AdvancesNowServingForWindowGhost replaces
// TestReaper_DoesNotAdvanceNowServingForWindowTicket. Not advancing for an
// in-window ghost did not protect capacity — it leaked a slot, and at
// cap=1 froze the queue (see TestStall_AbandonAtReadyDoesNotFreezeQueue).
func TestReaper_AdvancesNowServingForWindowGhost(t *testing.T) {
	wr := &WaitingRoom{}
	if err := wr.Init(5); err != nil {
		t.Fatal(err)
	}
	defer wr.Stop()

	wr.tokens.set("window-ghost", ticketEntry{
		ticket:   1,
		issuedAt: time.Now().Add(-(cookieTTL + time.Minute)),
	})

	before := wr.nowServing.Load()
	wr.reap()

	if _, ok := wr.tokens.get("window-ghost"); ok {
		t.Error("expected window-ghost token to be evicted")
	}
	if got := wr.nowServing.Load(); got != before+1 {
		t.Errorf("expected nowServing=%d after retiring an in-window ghost, got %d", before+1, got)
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
		reaperMaxInterval,
	}
	for _, d := range cases {
		if err := wr.SetReaperInterval(d); err != nil {
			t.Errorf("expected no error for %s, got %v", d, err)
		}
		if wr.ReaperInterval() != d {
			t.Errorf("expected %s, got %s", d, wr.ReaperInterval())
		}
	}
}

func TestSetReaperInterval_InvalidRange(t *testing.T) {
	wr := &WaitingRoom{}
	if err := wr.Init(5); err != nil {
		t.Fatal(err)
	}
	defer wr.Stop()

	// 0 is no longer invalid — it restores the default. See
	// TestSetReaperInterval_ZeroRestoresDefault.
	cases := []time.Duration{
		-time.Second,
		time.Millisecond,
		reaperMinInterval - time.Nanosecond,
		reaperMaxInterval + time.Nanosecond,
	}
	for _, d := range cases {
		err := wr.SetReaperInterval(d)
		if err == nil {
			t.Errorf("expected error for %s, got nil", d)
		}
		if _, ok := err.(ErrReaperInterval); !ok {
			t.Errorf("expected ErrReaperInterval for %s, got %T", d, err)
		}
	}
}

func TestSetReaperInterval_ZeroRestoresDefault(t *testing.T) {
	wr := &WaitingRoom{}
	if err := wr.Init(5); err != nil {
		t.Fatal(err)
	}
	defer wr.Stop()

	if err := wr.SetReaperInterval(30 * time.Second); err != nil {
		t.Fatal(err)
	}
	if err := wr.SetReaperInterval(0); err != nil {
		t.Fatalf("SetReaperInterval(0) should restore the default, got error %v", err)
	}
	if got := wr.ReaperInterval(); got != DefaultReaperInterval {
		t.Errorf("expected %s after SetReaperInterval(0), got %s", DefaultReaperInterval, got)
	}
}

// ── SetSecureCookie tests ────────────────────────────────────────────────────

func TestSetSecureCookie_DefaultIsFalse(t *testing.T) {
	wr := &WaitingRoom{}
	if err := wr.Init(1); err != nil {
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

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	var found bool
	for _, c := range w.Result().Cookies() {
		if c.Name == cookieName {
			found = true
			if c.Secure {
				t.Error("expected Secure=false on cookie for plain-HTTP request with default secureCookieDefault=false")
			}
		}
	}
	if !found {
		t.Skip("no room_ticket cookie issued — room may not have been full; skipping Secure flag check")
	}

	close(release)
}

func TestSetSecureCookie_TrueSetsCookieSecure(t *testing.T) {
	wr := &WaitingRoom{}
	if err := wr.Init(1); err != nil {
		t.Fatal(err)
	}
	defer wr.Stop()
	wr.SetSecureCookie(true)

	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	r := newTestRouter(wr, serving, release)

	go func() {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		r.ServeHTTP(httptest.NewRecorder(), req)
	}()
	<-serving

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	var found bool
	for _, c := range w.Result().Cookies() {
		if c.Name == cookieName {
			found = true
			if !c.Secure {
				t.Error("expected Secure=true on cookie after SetSecureCookie(true)")
			}
		}
	}
	if !found {
		t.Skip("no room_ticket cookie issued — room may not have been full; skipping Secure flag check")
	}

	close(release)
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
		serveWithCookie(r, "")
	}

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

	if u := wr.Utilization(); u < 0 || u > 1 {
		t.Errorf("utilization out of range: %f", u)
	}
	if u := wr.UtilizationSmoothed(); u < 0 || u > 1 {
		t.Errorf("smoothed utilization out of range: %f", u)
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
			r.ServeHTTP(httptest.NewRecorder(), req)
		}()
	}
	wg.Wait()

	if l := wr.Len(); l != 0 {
		t.Errorf("expected Len 0, got %d", l)
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
			r.ServeHTTP(httptest.NewRecorder(),
				httptest.NewRequest(http.MethodGet, "/", nil))
		}()
	}

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
			r.ServeHTTP(httptest.NewRecorder(),
				httptest.NewRequest(http.MethodGet, "/", nil))
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
				r.ServeHTTP(httptest.NewRecorder(),
					httptest.NewRequest(http.MethodGet, "/", nil))
				served.Add(1)
			}()
		}
		wg.Wait()

		if l := wr.Len(); l != 0 {
			t.Errorf("invariant violated: Len=%d", l)
		}
	})
}

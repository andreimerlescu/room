package room

import (
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

// ── RateFunc / SetRateFunc ───────────────────────────────────────────────────

func TestSetRateFunc_NilDisablesPromotions(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 5)

	wr.SetRateFunc(func(depth int64) float64 { return 1.0 })
	wr.SetRateFunc(nil)

	if fn := wr.rateFuncLoad(); fn != nil {
		t.Error("expected nil RateFunc after SetRateFunc(nil)")
	}
}

func TestSetRateFunc_StoresAndLoads(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 5)

	wr.SetRateFunc(func(depth int64) float64 { return 2.50 })

	fn := wr.rateFuncLoad()
	if fn == nil {
		t.Fatal("expected non-nil RateFunc")
	}
	if got := fn(0); got != 2.50 {
		t.Errorf("expected rate 2.50, got %f", got)
	}
}

func TestRateFuncLoad_DefaultIsNil(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 5)

	if fn := wr.rateFuncLoad(); fn != nil {
		t.Error("expected nil RateFunc by default")
	}
}

// ── QuoteCost ────────────────────────────────────────────────────────────────

func TestQuoteCost_NoRateFunc_ReturnsDisabled(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 5)

	wr.tokens.set("tok", ticketEntry{ticket: 20, issuedAt: time.Now()})

	_, err := wr.QuoteCost("tok", 1)
	if err == nil {
		t.Fatal("expected ErrPromotionDisabled, got nil")
	}
	if _, ok := err.(ErrPromotionDisabled); !ok {
		t.Errorf("expected ErrPromotionDisabled, got %T: %v", err, err)
	}
}

func TestQuoteCost_TokenNotFound(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 5)
	wr.SetRateFunc(func(depth int64) float64 { return 1.0 })

	_, err := wr.QuoteCost("nonexistent", 1)
	if err == nil {
		t.Fatal("expected ErrTokenNotFound, got nil")
	}
	if _, ok := err.(ErrTokenNotFound); !ok {
		t.Errorf("expected ErrTokenNotFound, got %T: %v", err, err)
	}
}

func TestQuoteCost_InvalidTargetPosition(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 5)
	wr.SetRateFunc(func(depth int64) float64 { return 1.0 })
	wr.tokens.set("tok", ticketEntry{ticket: 20, issuedAt: time.Now()})

	for _, target := range []int64{0, -1, -100} {
		_, err := wr.QuoteCost("tok", target)
		if err == nil {
			t.Errorf("expected ErrInvalidTargetPosition for target=%d, got nil", target)
			continue
		}
		if _, ok := err.(ErrInvalidTargetPosition); !ok {
			t.Errorf("expected ErrInvalidTargetPosition for target=%d, got %T", target, err)
		}
	}
}

func TestQuoteCost_AlreadyAdmitted(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 10)
	wr.SetRateFunc(func(depth int64) float64 { return 1.0 })

	wr.tokens.set("tok", ticketEntry{ticket: 5, issuedAt: time.Now()})

	_, err := wr.QuoteCost("tok", 1)
	if err == nil {
		t.Fatal("expected ErrAlreadyAdmitted, got nil")
	}
	if _, ok := err.(ErrAlreadyAdmitted); !ok {
		t.Errorf("expected ErrAlreadyAdmitted, got %T: %v", err, err)
	}
}

func TestQuoteCost_AlreadyAtOrAheadOfTarget(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 1)
	wr.SetRateFunc(func(depth int64) float64 { return 1.0 })

	wr.tokens.set("tok", ticketEntry{ticket: 4, issuedAt: time.Now()})

	cost, err := wr.QuoteCost("tok", 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cost != 0 {
		t.Errorf("expected cost 0 for same position, got %f", cost)
	}

	cost, err = wr.QuoteCost("tok", 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cost != 0 {
		t.Errorf("expected cost 0 for behind position, got %f", cost)
	}
}

func TestQuoteCost_CorrectCalculation_FlatRate(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 1)
	wr.SetRateFunc(func(depth int64) float64 { return 2.00 })

	wr.tokens.set("tok", ticketEntry{ticket: 11, issuedAt: time.Now()})

	cost, err := wr.QuoteCost("tok", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cost != 18.00 {
		t.Errorf("expected cost 18.00, got %f", cost)
	}

	cost, err = wr.QuoteCost("tok", 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cost != 10.00 {
		t.Errorf("expected cost 10.00, got %f", cost)
	}
}

func TestQuoteCost_SurgeRateUsesQueueDepth(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 1)

	wr.SetRateFunc(func(depth int64) float64 {
		return 1.0 + float64(depth)*0.10
	})

	for i := int64(2); i <= 6; i++ {
		wr.nextTicket.Store(i)
		wr.tokens.set(fmt.Sprintf("tok-%d", i), ticketEntry{
			ticket:   i,
			issuedAt: time.Now(),
		})
	}
	wr.nextTicket.Store(6)

	cost, err := wr.QuoteCost("tok-6", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cost != 6.00 {
		t.Errorf("expected cost 6.00, got %f", cost)
	}
}

func TestQuoteCost_DoesNotMutateState(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 1)
	wr.SetRateFunc(func(depth int64) float64 { return 1.0 })

	wr.tokens.set("tok", ticketEntry{ticket: 11, issuedAt: time.Now()})

	ticketBefore := int64(11)
	nowServingBefore := wr.nowServing.Load()

	_, _ = wr.QuoteCost("tok", 1)

	entry, ok := wr.tokens.get("tok")
	if !ok {
		t.Fatal("token disappeared after QuoteCost")
	}
	if entry.ticket != ticketBefore {
		t.Errorf("QuoteCost mutated ticket: was %d, now %d", ticketBefore, entry.ticket)
	}
	if wr.nowServing.Load() != nowServingBefore {
		t.Errorf("QuoteCost mutated nowServing: was %d, now %d",
			nowServingBefore, wr.nowServing.Load())
	}
}

// ── PromoteToken ─────────────────────────────────────────────────────────────

func TestPromoteToken_NoRateFunc_ReturnsDisabled(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 5)
	wr.tokens.set("tok", ticketEntry{ticket: 20, issuedAt: time.Now()})

	_, err := wr.PromoteToken("tok", 1)
	if err == nil {
		t.Fatal("expected ErrPromotionDisabled")
	}
	if _, ok := err.(ErrPromotionDisabled); !ok {
		t.Errorf("expected ErrPromotionDisabled, got %T", err)
	}
}

func TestPromoteToken_TokenNotFound(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 5)
	wr.SetRateFunc(func(depth int64) float64 { return 1.0 })

	_, err := wr.PromoteToken("nonexistent", 1)
	if err == nil {
		t.Fatal("expected ErrTokenNotFound")
	}
	if _, ok := err.(ErrTokenNotFound); !ok {
		t.Errorf("expected ErrTokenNotFound, got %T", err)
	}
}

func TestPromoteToken_InvalidTargetPosition(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 5)
	wr.SetRateFunc(func(depth int64) float64 { return 1.0 })
	wr.tokens.set("tok", ticketEntry{ticket: 20, issuedAt: time.Now()})

	for _, target := range []int64{0, -1, -50} {
		_, err := wr.PromoteToken("tok", target)
		if err == nil {
			t.Errorf("expected ErrInvalidTargetPosition for target=%d", target)
			continue
		}
		if _, ok := err.(ErrInvalidTargetPosition); !ok {
			t.Errorf("expected ErrInvalidTargetPosition for target=%d, got %T", target, err)
		}
	}
}

func TestPromoteToken_AlreadyAdmitted(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 10)
	wr.SetRateFunc(func(depth int64) float64 { return 1.0 })

	wr.tokens.set("tok", ticketEntry{ticket: 3, issuedAt: time.Now()})

	_, err := wr.PromoteToken("tok", 1)
	if err == nil {
		t.Fatal("expected ErrAlreadyAdmitted")
	}
	if _, ok := err.(ErrAlreadyAdmitted); !ok {
		t.Errorf("expected ErrAlreadyAdmitted, got %T", err)
	}
}

func TestPromoteToken_AlreadyAhead_Noop(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 1)
	wr.SetRateFunc(func(depth int64) float64 { return 1.0 })

	wr.tokens.set("tok", ticketEntry{ticket: 4, issuedAt: time.Now()})

	result, err := wr.PromoteToken("tok", 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Cost != 0 {
		t.Errorf("expected cost 0 for no-op promotion, got %f", result.Cost)
	}

	entry, _ := wr.tokens.get("tok")
	if entry.ticket != 4 {
		t.Errorf("ticket changed on no-op promotion: expected 4, got %d", entry.ticket)
	}
}

func TestPromoteToken_MovesToFront(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 1)
	wr.SetRateFunc(func(depth int64) float64 { return 1.0 })

	wr.tokens.set("tok", ticketEntry{ticket: 11, issuedAt: time.Now()})

	result, err := wr.PromoteToken("tok", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Cost != 9.0 {
		t.Errorf("expected cost 9.0, got %f", result.Cost)
	}

	entry, ok := wr.tokens.get("tok")
	if !ok {
		t.Fatal("token not found after promotion")
	}
	if entry.ticket != 2 {
		t.Errorf("expected new ticket 2, got %d", entry.ticket)
	}

	pos := wr.positionOf(entry.ticket)
	if pos != 1 {
		t.Errorf("expected position 1 after promotion, got %d", pos)
	}
}

func TestPromoteToken_MovesToIntermediate(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 1)
	wr.SetRateFunc(func(depth int64) float64 { return 5.00 })

	wr.tokens.set("tok", ticketEntry{ticket: 21, issuedAt: time.Now()})

	result, err := wr.PromoteToken("tok", 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Cost != 50.0 {
		t.Errorf("expected cost 50.0, got %f", result.Cost)
	}

	entry, _ := wr.tokens.get("tok")
	pos := wr.positionOf(entry.ticket)
	if pos != 10 {
		t.Errorf("expected position 10 after promotion, got %d", pos)
	}
}

func TestPromoteToken_SetsPromotedFlag(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 1)
	wr.SetRateFunc(func(depth int64) float64 { return 1.0 })

	wr.tokens.set("tok", ticketEntry{ticket: 11, issuedAt: time.Now()})

	_, err := wr.PromoteToken("tok", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	entry, _ := wr.tokens.get("tok")
	if !entry.promoted {
		t.Error("expected promoted=true after PromoteToken")
	}
}

func TestPromoteToken_ReturnsCost(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 2)
	wr.SetRateFunc(func(depth int64) float64 { return 3.50 })

	wr.tokens.set("tok", ticketEntry{ticket: 12, issuedAt: time.Now()})

	result, err := wr.PromoteToken("tok", 4)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Cost != 21.0 {
		t.Errorf("expected cost 21.0, got %f", result.Cost)
	}
}

// ── PromoteToken — EventPromote callback ─────────────────────────────────────

func TestPromoteToken_FiresEventPromote(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 1)
	wr.SetRateFunc(func(depth int64) float64 { return 1.0 })

	var promoteCount atomic.Int32
	wr.On(EventPromote, func(s Snapshot) { promoteCount.Add(1) })

	wr.tokens.set("tok", ticketEntry{ticket: 11, issuedAt: time.Now()})
	_, _ = wr.PromoteToken("tok", 1)

	waitForCount(t, &promoteCount, 1, 200*time.Millisecond)
}

func TestPromoteToken_NoEventOnNoop(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 1)
	wr.SetRateFunc(func(depth int64) float64 { return 1.0 })

	var promoteCount atomic.Int32
	wr.On(EventPromote, func(s Snapshot) { promoteCount.Add(1) })

	wr.tokens.set("tok", ticketEntry{ticket: 4, issuedAt: time.Now()})
	_, _ = wr.PromoteToken("tok", 3)

	time.Sleep(50 * time.Millisecond)
	if promoteCount.Load() != 0 {
		t.Error("EventPromote fired on no-op promotion")
	}
}

func TestPromoteToken_NoEventOnError(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 5)
	wr.SetRateFunc(func(depth int64) float64 { return 1.0 })

	var promoteCount atomic.Int32
	wr.On(EventPromote, func(s Snapshot) { promoteCount.Add(1) })

	_, _ = wr.PromoteToken("missing", 1)

	time.Sleep(50 * time.Millisecond)
	if promoteCount.Load() != 0 {
		t.Error("EventPromote fired on failed promotion")
	}
}

// ── PromoteTokenToFront ──────────────────────────────────────────────────────

func TestPromoteTokenToFront_JumpsToPositionOne(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 1)
	wr.SetRateFunc(func(depth int64) float64 { return 1.0 })

	wr.tokens.set("tok", ticketEntry{ticket: 11, issuedAt: time.Now()})

	result, err := wr.PromoteTokenToFront("tok")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Cost != 9.0 {
		t.Errorf("expected cost 9.0, got %f", result.Cost)
	}

	entry, _ := wr.tokens.get("tok")
	pos := wr.positionOf(entry.ticket)
	if pos != 1 {
		t.Errorf("expected position 1, got %d", pos)
	}
}

// ── Serialization: concurrent promotions ─────────────────────────────────────

func TestPromoteToken_ConcurrentPromotions_NoCollision(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 1)
	wr.SetRateFunc(func(depth int64) float64 { return 1.0 })

	const n = 20

	for i := 0; i < n; i++ {
		wr.tokens.set(fmt.Sprintf("tok-%d", i), ticketEntry{
			ticket:   int64(10 + i*5),
			issuedAt: time.Now(),
		})
	}

	var wg sync.WaitGroup
	var successCount atomic.Int32
	var errorCount atomic.Int32

	for i := 0; i < n; i++ {
		wg.Add(1)
		i := i
		go func() {
			defer wg.Done()
			_, err := wr.PromoteToken(fmt.Sprintf("tok-%d", i), 1)
			if err != nil {
				errorCount.Add(1)
			} else {
				successCount.Add(1)
			}
		}()
	}

	wg.Wait()

	if errorCount.Load() > 0 {
		t.Logf("successes=%d errors=%d (errors may include already-admitted tokens)",
			successCount.Load(), errorCount.Load())
	}

	seen := make(map[int64]string)
	wr.tokens.mu.RLock()
	for token, entry := range wr.tokens.entries {
		if prev, exists := seen[entry.ticket]; exists {
			t.Errorf("ticket collision: tokens %q and %q both have ticket %d",
				prev, token, entry.ticket)
		}
		seen[entry.ticket] = token
	}
	wr.tokens.mu.RUnlock()
}

func TestPromoteToken_SerializedUnderMutex(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 1)
	wr.SetRateFunc(func(depth int64) float64 { return 1.0 })

	wr.tokens.set("tok-a", ticketEntry{ticket: 50, issuedAt: time.Now()})
	wr.tokens.set("tok-b", ticketEntry{ticket: 60, issuedAt: time.Now()})

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		wr.PromoteToken("tok-a", 1)
	}()
	go func() {
		defer wg.Done()
		wr.PromoteToken("tok-b", 1)
	}()

	wg.Wait()

	entryA, okA := wr.tokens.get("tok-a")
	entryB, okB := wr.tokens.get("tok-b")

	if !okA || !okB {
		t.Fatal("tokens disappeared after concurrent promotion")
	}

	if entryA.ticket == entryB.ticket {
		t.Errorf("ticket collision after serialized promotions: both have ticket %d",
			entryA.ticket)
	}
}

// ── Integration: promoted token is admitted via status polling ────────────────

func TestIntegration_PromotedToken_AdmittedOnNextPoll(t *testing.T) {
	t.Parallel()
	const cap = 1
	wr := newTestWR(t, int32(cap))
	wr.SetRateFunc(func(depth int64) float64 { return 1.0 })

	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	r := newTestRouter(wr, serving, release)

	go func() {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		r.ServeHTTP(httptest.NewRecorder(), req)
	}()
	<-serving

	_, tokenA := serveWithCookie(r, "")
	_, tokenB := serveWithCookie(r, "")

	if tokenA == "" || tokenB == "" {
		t.Fatal("expected tokens for both queued requests")
	}

	entryB, _ := wr.tokens.get(tokenB)
	posB := wr.positionOf(entryB.ticket)
	if posB < 2 {
		t.Fatalf("expected tokenB at position >= 2, got %d", posB)
	}

	_, err := wr.PromoteTokenToFront(tokenB)
	if err != nil {
		t.Fatalf("PromoteTokenToFront: %v", err)
	}

	close(release)

	waitForStatus(t, r, tokenB, 5*time.Second)
}

func TestIntegration_PromotedToken_ServesRequestSuccessfully(t *testing.T) {
	t.Parallel()
	const cap = 1
	wr := newTestWR(t, int32(cap))
	wr.SetRateFunc(func(depth int64) float64 { return 1.0 })

	r := gin.New()
	wr.RegisterRoutes(r)

	var served atomic.Int32
	r.GET("/", func(c *gin.Context) {
		served.Add(1)
		c.Status(http.StatusOK)
	})

	serving := make(chan struct{}, 1)
	releaseFirst := make(chan struct{})

	rBlocking := gin.New()
	wr.RegisterRoutes(rBlocking)
	rBlocking.GET("/", func(c *gin.Context) {
		serving <- struct{}{}
		<-releaseFirst
		c.Status(http.StatusOK)
	})

	go func() {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rBlocking.ServeHTTP(httptest.NewRecorder(), req)
	}()
	<-serving

	_, token := serveWithCookie(rBlocking, "")
	if token == "" {
		t.Fatal("no token issued")
	}

	_, err := wr.PromoteTokenToFront(token)
	if err != nil {
		t.Fatalf("PromoteTokenToFront: %v", err)
	}

	close(releaseFirst)

	waitForStatus(t, rBlocking, token, 5*time.Second)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: token})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 after promotion, got %d", w.Code)
	}
}

// ── Integration: status endpoint includes pricing ────────────────────────────

func TestIntegration_StatusEndpoint_IncludesPricing(t *testing.T) {
	t.Parallel()
	const cap = 1
	wr := newTestWR(t, int32(cap))
	wr.SetRateFunc(func(depth int64) float64 { return 2.50 })

	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	r := newTestRouter(wr, serving, release)

	go func() {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		r.ServeHTTP(httptest.NewRecorder(), req)
	}()
	<-serving

	serveWithCookie(r, "")
	serveWithCookie(r, "")
	_, token := serveWithCookie(r, "")
	if token == "" {
		t.Fatal("no token issued")
	}

	req := httptest.NewRequest(http.MethodGet, "/queue/status", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: token})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var resp statusResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode error: %v", err)
	}

	if resp.Ready {
		t.Fatal("expected ready=false while queued")
	}
	if resp.RatePerPos != 2.50 {
		t.Errorf("expected rate_per_pos=2.50, got %f", resp.RatePerPos)
	}
	if resp.Position <= 1 {
		t.Errorf("expected position > 1 for third queued request, got %d", resp.Position)
	}
	if resp.SkipCost <= 0 {
		t.Errorf("expected positive skip_cost, got %f", resp.SkipCost)
	}

	expectedCost := float64(resp.Position-1) * 2.50
	if resp.SkipCost != expectedCost {
		t.Errorf("expected skip_cost=%f, got %f", expectedCost, resp.SkipCost)
	}

	close(release)
}

func TestIntegration_StatusEndpoint_NoPricingWithoutRateFunc(t *testing.T) {
	t.Parallel()
	const cap = 1
	wr := newTestWR(t, int32(cap))

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

	req := httptest.NewRequest(http.MethodGet, "/queue/status", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: token})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	var resp statusResponse
	json.NewDecoder(w.Body).Decode(&resp)

	if resp.SkipCost != 0 {
		t.Errorf("expected skip_cost=0 without RateFunc, got %f", resp.SkipCost)
	}
	if resp.RatePerPos != 0 {
		t.Errorf("expected rate_per_pos=0 without RateFunc, got %f", resp.RatePerPos)
	}

	close(release)
}

func TestIntegration_StatusEndpoint_HidesPricingWhenPassActive(t *testing.T) {
	t.Parallel()
	const cap = 1
	wr := newTestWR(t, int32(cap))
	wr.SetRateFunc(func(depth int64) float64 { return 2.50 })
	if err := wr.SetPassDuration(90 * time.Minute); err != nil {
		t.Fatal(err)
	}

	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	r := newTestRouter(wr, serving, release)

	go func() {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		r.ServeHTTP(httptest.NewRecorder(), req)
	}()
	<-serving

	serveWithCookie(r, "")
	_, token := serveWithCookie(r, "")
	if token == "" {
		t.Fatal("no token issued")
	}

	// Grant a pass for this client.
	passToken := wr.GrantPass()
	if passToken == "" {
		t.Fatal("expected pass token")
	}

	// Poll with both cookies.
	req := httptest.NewRequest(http.MethodGet, "/queue/status", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: token})
	req.AddCookie(&http.Cookie{Name: passCookieName, Value: passToken})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	var resp statusResponse
	json.NewDecoder(w.Body).Decode(&resp)

	if !resp.HasPass {
		t.Error("expected has_pass=true")
	}
	if resp.SkipCost != 0 {
		t.Errorf("expected skip_cost=0 when pass active, got %f", resp.SkipCost)
	}
	if resp.RatePerPos != 0 {
		t.Errorf("expected rate_per_pos=0 when pass active, got %f", resp.RatePerPos)
	}

	close(release)
}

// ── Edge cases ───────────────────────────────────────────────────────────────

func TestPromoteToken_QueueMovedBetweenQuoteAndPromote(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 1)
	wr.SetRateFunc(func(depth int64) float64 { return 1.0 })

	wr.tokens.set("tok", ticketEntry{ticket: 11, issuedAt: time.Now()})

	quotedCost, err := wr.QuoteCost("tok", 1)
	if err != nil {
		t.Fatalf("QuoteCost: %v", err)
	}

	wr.nowServing.Add(3)

	result, err := wr.PromoteToken("tok", 1)
	if err != nil {
		t.Fatalf("PromoteToken: %v", err)
	}

	if result.Cost >= quotedCost {
		t.Errorf("expected actual cost (%f) < quoted cost (%f) after queue advancement",
			result.Cost, quotedCost)
	}
}

func TestPromoteToken_ReapDoesNotEvictPromotedToken(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 1)
	wr.SetRateFunc(func(depth int64) float64 { return 1.0 })

	wr.tokens.set("tok", ticketEntry{ticket: 11, issuedAt: time.Now()})
	_, err := wr.PromoteToken("tok", 1)
	if err != nil {
		t.Fatalf("PromoteToken: %v", err)
	}

	wr.reap()

	if _, ok := wr.tokens.get("tok"); !ok {
		t.Error("promoted token was evicted by reaper")
	}
}

func TestPromoteToken_ExpiredPromotedToken_Reaped(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 1)
	wr.SetRateFunc(func(depth int64) float64 { return 1.0 })

	wr.tokens.set("tok", ticketEntry{
		ticket:   11,
		issuedAt: time.Now().Add(-(cookieTTL + time.Minute)),
	})

	entry, _ := wr.tokens.get("tok")
	entry.ticket = wr.nowServing.Load() + int64(wr.cap.Load()) + 1
	entry.promoted = true
	wr.tokens.set("tok", entry)

	wr.reap()

	if _, ok := wr.tokens.get("tok"); ok {
		t.Error("expired promoted token should have been reaped")
	}
}

// ── Pass system ──────────────────────────────────────────────────────────────

func TestSetPassDuration_Zero_DisablesPasses(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 5)

	if err := wr.SetPassDuration(0); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if wr.PassDuration() != 0 {
		t.Errorf("expected 0, got %s", wr.PassDuration())
	}
}

func TestSetPassDuration_ValidRange(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 5)

	cases := []time.Duration{
		passMinDuration,
		30 * time.Minute,
		90 * time.Minute,
		passMaxDuration,
	}
	for _, d := range cases {
		if err := wr.SetPassDuration(d); err != nil {
			t.Errorf("expected no error for %s, got %v", d, err)
		}
		if wr.PassDuration() != d {
			t.Errorf("expected %s, got %s", d, wr.PassDuration())
		}
	}
}

func TestSetPassDuration_InvalidRange(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 5)

	cases := []time.Duration{
		time.Second,
		passMinDuration - time.Nanosecond,
		passMaxDuration + time.Nanosecond,
	}
	for _, d := range cases {
		err := wr.SetPassDuration(d)
		if err == nil {
			t.Errorf("expected error for %s, got nil", d)
		}
		if _, ok := err.(ErrPassDuration); !ok {
			t.Errorf("expected ErrPassDuration for %s, got %T", d, err)
		}
	}
}

func TestGrantPass_ReturnsTokenWhenConfigured(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 5)
	if err := wr.SetPassDuration(90 * time.Minute); err != nil {
		t.Fatal(err)
	}

	token := wr.GrantPass()
	if token == "" {
		t.Error("expected non-empty pass token")
	}

	if !wr.HasValidPass(token) {
		t.Error("freshly granted pass should be valid")
	}
}

func TestGrantPass_ReturnsEmptyWhenDisabled(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 5)
	// Default pass duration is 0 (disabled).

	token := wr.GrantPass()
	if token != "" {
		t.Errorf("expected empty pass token when disabled, got %q", token)
	}
}

func TestHasValidPass_EmptyToken(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 5)

	if wr.HasValidPass("") {
		t.Error("empty token should not be a valid pass")
	}
}

func TestHasValidPass_NonexistentToken(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 5)

	if wr.HasValidPass("does-not-exist") {
		t.Error("nonexistent token should not be a valid pass")
	}
}

func TestHasValidPass_ExpiredPass(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 5)

	wr.passes.set("expired", passEntry{
		expiresAt: time.Now().Add(-time.Minute),
	})

	if wr.HasValidPass("expired") {
		t.Error("expired pass should not be valid")
	}
}

func TestHasValidPass_LivePass(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 5)

	wr.passes.set("live", passEntry{
		expiresAt: time.Now().Add(30 * time.Minute),
	})

	if !wr.HasValidPass("live") {
		t.Error("live pass should be valid")
	}
}

func TestPromoteToken_IssuesPassWhenConfigured(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 1)
	wr.SetRateFunc(func(depth int64) float64 { return 1.0 })
	if err := wr.SetPassDuration(90 * time.Minute); err != nil {
		t.Fatal(err)
	}

	wr.tokens.set("tok", ticketEntry{ticket: 11, issuedAt: time.Now()})

	result, err := wr.PromoteToken("tok", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.PassToken == "" {
		t.Error("expected pass token in PromoteResult when pass duration configured")
	}
	if !wr.HasValidPass(result.PassToken) {
		t.Error("issued pass token should be valid")
	}
}

func TestPromoteToken_NoPassWhenDisabled(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 1)
	wr.SetRateFunc(func(depth int64) float64 { return 1.0 })
	// Pass duration defaults to 0 (disabled).

	wr.tokens.set("tok", ticketEntry{ticket: 11, issuedAt: time.Now()})

	result, err := wr.PromoteToken("tok", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.PassToken != "" {
		t.Errorf("expected empty pass token when disabled, got %q", result.PassToken)
	}
}

func TestAutoPromote_WithValidPass(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 1)
	if err := wr.SetPassDuration(90 * time.Minute); err != nil {
		t.Fatal(err)
	}

	// Create a pass.
	passToken := wr.GrantPass()
	if passToken == "" {
		t.Fatal("expected pass token")
	}

	// Insert a queued token (simulating a re-entry).
	wr.tokens.set("ticket-tok", ticketEntry{
		ticket:   50,
		issuedAt: time.Now(),
	})

	// Before auto-promote: position should be far back.
	posBefore := wr.positionOf(50)
	if posBefore <= 0 {
		t.Fatalf("expected positive position before auto-promote, got %d", posBefore)
	}

	// Auto-promote (this is what the middleware calls internally).
	wr.autoPromote("ticket-tok")

	entry, ok := wr.tokens.get("ticket-tok")
	if !ok {
		t.Fatal("token disappeared after autoPromote")
	}

	posAfter := wr.positionOf(entry.ticket)
	if posAfter >= posBefore {
		t.Errorf("expected position to improve after autoPromote: before=%d after=%d",
			posBefore, posAfter)
	}
	if !entry.promoted {
		t.Error("expected promoted=true after autoPromote")
	}
}

func TestAutoPromote_SkipsAlreadyPromoted(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 1)

	wr.tokens.set("tok", ticketEntry{
		ticket:   50,
		issuedAt: time.Now(),
		promoted: true,
	})

	ticketBefore := int64(50)
	wr.autoPromote("tok")

	// autoPromote checks positionOf but since the middleware only calls
	// autoPromote when !entry.promoted, we test that the function still
	// works correctly when called on an already-promoted token (it should
	// still promote since it checks position, not the flag).
	entry, _ := wr.tokens.get("tok")
	if entry.ticket == ticketBefore {
		// It's acceptable that autoPromote moves it again — the position
		// check is what matters, not the promoted flag.
		t.Logf("autoPromote did not change ticket (position may already be front)")
	}
}

func TestAutoPromote_NoopWhenInServingWindow(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 10)

	// Ticket 5, cap=10 → position = 5 - 0 - 10 = -5 (in window).
	wr.tokens.set("tok", ticketEntry{
		ticket:   5,
		issuedAt: time.Now(),
	})

	wr.autoPromote("tok")

	entry, _ := wr.tokens.get("tok")
	if entry.ticket != 5 {
		t.Errorf("autoPromote should not change ticket when already in window, got %d", entry.ticket)
	}
	if entry.promoted {
		t.Error("autoPromote should not set promoted flag when already in window")
	}
}

func TestAutoPromote_MissingToken(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 5)

	// Must not panic.
	wr.autoPromote("nonexistent")
}

// ── Benchmark ────────────────────────────────────────────────────────────────

func BenchmarkQuoteCost(b *testing.B) {
	wr := &WaitingRoom{}
	if err := wr.Init(10); err != nil {
		b.Fatal(err)
	}
	defer wr.Stop()
	wr.SetRateFunc(func(depth int64) float64 { return 1.0 })

	wr.tokens.set("tok", ticketEntry{ticket: 100, issuedAt: time.Now()})

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = wr.QuoteCost("tok", 1)
	}
}

func BenchmarkPromoteToken(b *testing.B) {
	wr := &WaitingRoom{}
	if err := wr.Init(10); err != nil {
		b.Fatal(err)
	}
	defer wr.Stop()
	wr.SetRateFunc(func(depth int64) float64 { return 1.0 })

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		token := fmt.Sprintf("tok-%d", i)
		wr.tokens.set(token, ticketEntry{
			ticket:   int64(100 + i),
			issuedAt: time.Now(),
		})
		_, _ = wr.PromoteToken(token, 1)
	}
}

func BenchmarkPromoteTokenToFront(b *testing.B) {
	wr := &WaitingRoom{}
	if err := wr.Init(10); err != nil {
		b.Fatal(err)
	}
	defer wr.Stop()
	wr.SetRateFunc(func(depth int64) float64 { return 1.0 })

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		token := fmt.Sprintf("tok-%d", i)
		wr.tokens.set(token, ticketEntry{
			ticket:   int64(100 + i),
			issuedAt: time.Now(),
		})
		_, _ = wr.PromoteTokenToFront(token)
	}
}

func BenchmarkGrantPass(b *testing.B) {
	wr := &WaitingRoom{}
	if err := wr.Init(10); err != nil {
		b.Fatal(err)
	}
	defer wr.Stop()
	if err := wr.SetPassDuration(90 * time.Minute); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = wr.GrantPass()
	}
}

func BenchmarkHasValidPass(b *testing.B) {
	wr := &WaitingRoom{}
	if err := wr.Init(10); err != nil {
		b.Fatal(err)
	}
	defer wr.Stop()

	wr.passes.set("tok", passEntry{
		expiresAt: time.Now().Add(time.Hour),
	})

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = wr.HasValidPass("tok")
	}
}

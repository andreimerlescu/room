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

	// Manually insert a token so the test isn't about token lookup.
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

	// Ticket 5 with cap=10 and nowServing=0 → position = 5 - 0 - 10 = -5 (admitted).
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

	// cap=1, nowServing=0 → position = ticket - 0 - 1 = ticket - 1.
	// ticket=4 → position=3.
	wr.tokens.set("tok", ticketEntry{ticket: 4, issuedAt: time.Now()})

	// Target position 3 (same as current) → cost should be 0.
	cost, err := wr.QuoteCost("tok", 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cost != 0 {
		t.Errorf("expected cost 0 for same position, got %f", cost)
	}

	// Target position 5 (behind current) → cost should be 0.
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

	// cap=1, nowServing=0 → position = ticket - 1.
	// ticket=11 → position=10.
	wr.tokens.set("tok", ticketEntry{ticket: 11, issuedAt: time.Now()})

	// Jump from position 10 to position 1: distance=9, cost = 9 * 2.00 = 18.00.
	cost, err := wr.QuoteCost("tok", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cost != 18.00 {
		t.Errorf("expected cost 18.00, got %f", cost)
	}

	// Jump from position 10 to position 5: distance=5, cost = 5 * 2.00 = 10.00.
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

	// Surge pricing: base $1 + $0.10 per queued request.
	wr.SetRateFunc(func(depth int64) float64 {
		return 1.0 + float64(depth)*0.10
	})

	// Insert several tokens to build queue depth.
	// cap=1, nowServing=0 → window is ticket [1].
	// Tickets 2..6 are outside the window → queue depth = 5.
	for i := int64(2); i <= 6; i++ {
		wr.nextTicket.Store(i)
		wr.tokens.set(fmt.Sprintf("tok-%d", i), ticketEntry{
			ticket:   i,
			issuedAt: time.Now(),
		})
	}
	wr.nextTicket.Store(6)

	// tok-6: position = 6 - 0 - 1 = 5. Queue depth = 5.
	// rate = 1.0 + 5*0.10 = 1.50.
	// Jump to position 1: distance = 4, cost = 4 * 1.50 = 6.00.
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

	// Ticket 3, cap=10, nowServing=0 → position = 3 - 0 - 10 = -7 (in window).
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

	// cap=1, nowServing=0 → position = ticket - 1.
	// ticket=4 → position=3.
	wr.tokens.set("tok", ticketEntry{ticket: 4, issuedAt: time.Now()})

	cost, err := wr.PromoteToken("tok", 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cost != 0 {
		t.Errorf("expected cost 0 for no-op promotion, got %f", cost)
	}

	// Ticket should be unchanged.
	entry, _ := wr.tokens.get("tok")
	if entry.ticket != 4 {
		t.Errorf("ticket changed on no-op promotion: expected 4, got %d", entry.ticket)
	}
}

func TestPromoteToken_MovesToFront(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 1)
	wr.SetRateFunc(func(depth int64) float64 { return 1.0 })

	// cap=1, nowServing=0.
	// ticket=11 → position = 11 - 0 - 1 = 10.
	wr.tokens.set("tok", ticketEntry{ticket: 11, issuedAt: time.Now()})

	cost, err := wr.PromoteToken("tok", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// distance = 10 - 1 = 9, cost = 9 * 1.0 = 9.0.
	if cost != 9.0 {
		t.Errorf("expected cost 9.0, got %f", cost)
	}

	// New ticket should place the token at position 1.
	// ceiling = nowServing + cap + targetPosition = 0 + 1 + 1 = 2.
	// promoteInsert was 0, so insert = ceiling = 2. newTicket = 2.
	entry, ok := wr.tokens.get("tok")
	if !ok {
		t.Fatal("token not found after promotion")
	}
	if entry.ticket != 2 {
		t.Errorf("expected new ticket 2, got %d", entry.ticket)
	}

	// Verify positionOf confirms position 1.
	pos := wr.positionOf(entry.ticket)
	if pos != 1 {
		t.Errorf("expected position 1 after promotion, got %d", pos)
	}
}

func TestPromoteToken_MovesToIntermediate(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 1)
	wr.SetRateFunc(func(depth int64) float64 { return 5.00 })

	// cap=1, nowServing=0.
	// ticket=21 → position = 21 - 0 - 1 = 20.
	wr.tokens.set("tok", ticketEntry{ticket: 21, issuedAt: time.Now()})

	// Promote to position 10: distance = 20 - 10 = 10, cost = 10 * 5.00 = 50.00.
	cost, err := wr.PromoteToken("tok", 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cost != 50.0 {
		t.Errorf("expected cost 50.0, got %f", cost)
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

	// cap=2, nowServing=0 → position = ticket - 2.
	// ticket=12 → position=10.
	wr.tokens.set("tok", ticketEntry{ticket: 12, issuedAt: time.Now()})

	// Jump to position 4: distance = 10 - 4 = 6, cost = 6 * 3.50 = 21.00.
	cost, err := wr.PromoteToken("tok", 4)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cost != 21.0 {
		t.Errorf("expected cost 21.0, got %f", cost)
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

	// Position 3, target 3 → no-op.
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

	// Token does not exist → error, no event.
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

	// position = 11 - 0 - 1 = 10.
	wr.tokens.set("tok", ticketEntry{ticket: 11, issuedAt: time.Now()})

	cost, err := wr.PromoteTokenToFront("tok")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// distance = 10 - 1 = 9, cost = 9 * 1.0 = 9.0.
	if cost != 9.0 {
		t.Errorf("expected cost 9.0, got %f", cost)
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

	// Insert n tokens at various positions.
	for i := 0; i < n; i++ {
		wr.tokens.set(fmt.Sprintf("tok-%d", i), ticketEntry{
			ticket:   int64(10 + i*5), // positions spread apart
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

	// All should succeed (no panics, no data races).
	if errorCount.Load() > 0 {
		t.Logf("successes=%d errors=%d (errors may include already-admitted tokens)",
			successCount.Load(), errorCount.Load())
	}

	// Verify no two surviving tokens have the same ticket number.
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

	// Promote both to position 1 concurrently.
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

	// Because promotions are serialized via promoteMu and each claims
	// a unique slot via promoteInsert, tickets must differ.
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

	// Fill the single slot.
	go func() {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		r.ServeHTTP(httptest.NewRecorder(), req)
	}()
	<-serving

	// Queue two requests and collect tokens.
	_, tokenA := serveWithCookie(r, "")
	_, tokenB := serveWithCookie(r, "")

	if tokenA == "" || tokenB == "" {
		t.Fatal("expected tokens for both queued requests")
	}

	// tokenB should be further back in the queue than tokenA.
	entryB, _ := wr.tokens.get(tokenB)
	posB := wr.positionOf(entryB.ticket)
	if posB < 2 {
		t.Fatalf("expected tokenB at position >= 2, got %d", posB)
	}

	// Promote tokenB to front.
	_, err := wr.PromoteTokenToFront(tokenB)
	if err != nil {
		t.Fatalf("PromoteTokenToFront: %v", err)
	}

	// Release the active slot.
	close(release)

	// tokenB should be admitted before tokenA now.
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

	// Override the handler for the first request to block.
	rBlocking := gin.New()
	wr.RegisterRoutes(rBlocking)
	rBlocking.GET("/", func(c *gin.Context) {
		serving <- struct{}{}
		<-releaseFirst
		c.Status(http.StatusOK)
	})

	// Fill the slot with a blocking request.
	go func() {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rBlocking.ServeHTTP(httptest.NewRecorder(), req)
	}()
	<-serving

	// Queue a request and get its token.
	_, token := serveWithCookie(rBlocking, "")
	if token == "" {
		t.Fatal("no token issued")
	}

	// Promote to front.
	_, err := wr.PromoteTokenToFront(token)
	if err != nil {
		t.Fatalf("PromoteTokenToFront: %v", err)
	}

	// Release the blocking request.
	close(releaseFirst)

	// Wait until the promoted token is ready.
	waitForStatus(t, rBlocking, token, 5*time.Second)

	// Now make the actual request with the promoted cookie — it should
	// pass through the middleware and hit the handler.
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

	// Fill the slot.
	go func() {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		r.ServeHTTP(httptest.NewRecorder(), req)
	}()
	<-serving

	// Queue multiple requests so the last one is at position > 1.
	serveWithCookie(r, "")
	serveWithCookie(r, "")
	_, token := serveWithCookie(r, "")
	if token == "" {
		t.Fatal("no token issued")
	}

	// Poll status — should include pricing fields.
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

	// skip_cost should be (position - 1) * rate.
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
	// No SetRateFunc — pricing should be absent.

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

// ── Edge cases ───────────────────────────────────────────────────────────────

func TestPromoteToken_QueueMovedBetweenQuoteAndPromote(t *testing.T) {
	// Simulates the race where the queue advances between QuoteCost
	// and PromoteToken — the actual cost should reflect the state at
	// promotion time, not quote time.
	t.Parallel()
	wr := newTestWR(t, 1)
	wr.SetRateFunc(func(depth int64) float64 { return 1.0 })

	// position = 11 - 0 - 1 = 10.
	wr.tokens.set("tok", ticketEntry{ticket: 11, issuedAt: time.Now()})

	quotedCost, err := wr.QuoteCost("tok", 1)
	if err != nil {
		t.Fatalf("QuoteCost: %v", err)
	}

	// Simulate queue advancement: nowServing moves forward by 3.
	wr.nowServing.Add(3)

	// Now position = 11 - 3 - 1 = 7. Jump to 1: distance=6, cost=6.0.
	actualCost, err := wr.PromoteToken("tok", 1)
	if err != nil {
		t.Fatalf("PromoteToken: %v", err)
	}

	if actualCost >= quotedCost {
		t.Errorf("expected actual cost (%f) < quoted cost (%f) after queue advancement",
			actualCost, quotedCost)
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

	// Token was just promoted — issuedAt was set at creation time, which
	// is recent. Reap should not evict it.
	wr.reap()

	if _, ok := wr.tokens.get("tok"); !ok {
		t.Error("promoted token was evicted by reaper")
	}
}

func TestPromoteToken_ExpiredPromotedToken_Reaped(t *testing.T) {
	// A promoted token that stops polling should eventually expire
	// and be reaped, just like any other token.
	t.Parallel()
	wr := newTestWR(t, 1)
	wr.SetRateFunc(func(depth int64) float64 { return 1.0 })

	wr.tokens.set("tok", ticketEntry{
		ticket:   11,
		issuedAt: time.Now().Add(-(cookieTTL + time.Minute)),
	})

	// Promote the already-expired token (simulating a payment that
	// was processed very late).
	// Note: the token's position would be based on current state.
	// Since it's expired, it may be reaped before promotion in real
	// usage, but here we're testing that promotion doesn't grant
	// immunity from expiration.
	entry, _ := wr.tokens.get("tok")
	entry.ticket = wr.nowServing.Load() + int64(wr.cap.Load()) + 1
	entry.promoted = true
	// Keep the old issuedAt to simulate an abandoned promoted token.
	wr.tokens.set("tok", entry)

	wr.reap()

	if _, ok := wr.tokens.get("tok"); ok {
		t.Error("expired promoted token should have been reaped")
	}
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

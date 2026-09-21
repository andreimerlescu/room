package room

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// ── helpers ───────────────────────────────────────────────────────────────────

// cookiesByName performs a GET / and returns every Set-Cookie the response
// carried, keyed by cookie name. Unlike serveWithCookie it does not discard
// cookies other than room_ticket, which is what these tests need.
func cookiesByName(r *gin.Engine, ticket string) (*httptest.ResponseRecorder, map[string]*http.Cookie) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if ticket != "" {
		req.AddCookie(&http.Cookie{Name: cookieName, Value: ticket})
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	out := make(map[string]*http.Cookie)
	for _, c := range w.Result().Cookies() {
		out[c.Name] = c
	}
	return w, out
}

// ageAllTokens rewinds every token's issuedAt by d, simulating the passage
// of time without sleeping. Used to drive the reaper deterministically.
func ageAllTokens(ts *tokenStore, d time.Duration) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	for token, entry := range ts.entries {
		entry.issuedAt = entry.issuedAt.Add(-d)
		ts.entries[token] = entry
	}
}

// ageOfToken returns how long ago the token's issuedAt was set. Tests use
// this to assert that a poll actually reset the sliding window.
func ageOfToken(t *testing.T, ts *tokenStore, token string) time.Duration {
	t.Helper()
	entry, ok := ts.get(token)
	if !ok {
		t.Fatalf("token %q not found in store", token)
	}
	return time.Since(entry.issuedAt)
}

// fillOneSlot occupies the room's single slot. The caller is responsible
// for closing the release channel passed to newTestRouter.
func fillOneSlot(t *testing.T, r *gin.Engine, serving chan struct{}) {
	t.Helper()
	go func() {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		r.ServeHTTP(httptest.NewRecorder(), req)
	}()
	select {
	case <-serving:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out filling the slot")
	}
}

// ── probe cookie ──────────────────────────────────────────────────────────────

// TestProbeCookie_SetOnFirstQueueRender verifies the probe cookie accompanies
// room_ticket on the first waiting-room render. The page cannot read
// room_ticket (HttpOnly), so without the probe it has no way to tell whether
// the browser is storing our cookies at all.
func TestProbeCookie_SetOnFirstQueueRender(t *testing.T) {
	wr := newTestWR(t, 1)

	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	r := newTestRouter(wr, serving, release)
	defer close(release)

	fillOneSlot(t, r, serving)

	_, cookies := cookiesByName(r, "")

	probe, ok := cookies[probeCookieName]
	if !ok {
		t.Fatalf("expected %q cookie on the waiting room render", probeCookieName)
	}
	if probe.HttpOnly {
		t.Error("probe cookie must NOT be HttpOnly — the page reads it via document.cookie")
	}
	if probe.Value != probeCookieValue {
		t.Errorf("expected probe value %q, got %q", probeCookieValue, probe.Value)
	}
}

// TestProbeCookie_MatchesTicketCookieAttributes verifies the probe carries the
// same Path, Secure and SameSite as room_ticket. A probe that survives a
// misconfiguration which blocks room_ticket would report "cookies fine" to a
// client that cannot actually hold a queue position — worse than no probe.
func TestProbeCookie_MatchesTicketCookieAttributes(t *testing.T) {
	wr := newTestWR(t, 1)
	wr.SetCookiePath("/app")
	wr.SetSecureCookie(true)

	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	r := newTestRouter(wr, serving, release)
	defer close(release)

	fillOneSlot(t, r, serving)

	_, cookies := cookiesByName(r, "")

	ticket, ok := cookies[cookieName]
	if !ok {
		t.Fatalf("expected %q cookie", cookieName)
	}
	probe, ok := cookies[probeCookieName]
	if !ok {
		t.Fatalf("expected %q cookie", probeCookieName)
	}

	if probe.Path != ticket.Path {
		t.Errorf("probe Path %q != ticket Path %q", probe.Path, ticket.Path)
	}
	if probe.Secure != ticket.Secure {
		t.Errorf("probe Secure %v != ticket Secure %v", probe.Secure, ticket.Secure)
	}
	if probe.SameSite != ticket.SameSite {
		t.Errorf("probe SameSite %v != ticket SameSite %v", probe.SameSite, ticket.SameSite)
	}
	if !probe.Secure {
		t.Error("expected probe Secure=true after SetSecureCookie(true)")
	}
}

// TestProbeCookie_RefreshedOnResumeRender verifies the probe is re-sent when a
// returning client is served an updated position. The probe has a finite
// MaxAge; if it expired while the ticket remained valid the page would show a
// false "cookies disabled" panel.
func TestProbeCookie_RefreshedOnResumeRender(t *testing.T) {
	wr := newTestWR(t, 1)

	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	r := newTestRouter(wr, serving, release)
	defer close(release)

	fillOneSlot(t, r, serving)

	_, token := serveWithCookie(r, "")
	if token == "" {
		t.Fatal("no token issued")
	}

	_, cookies := cookiesByName(r, token)
	if _, ok := cookies[probeCookieName]; !ok {
		t.Errorf("expected %q to be refreshed on the resume render", probeCookieName)
	}
}

// TestProbeCookie_UsesTokenTTLForMaxAge keeps the probe and ticket lifetimes
// in lockstep with the configured TTL.
func TestProbeCookie_UsesTokenTTLForMaxAge(t *testing.T) {
	wr := newTestWR(t, 1)
	if err := wr.SetTokenTTL(2 * time.Minute); err != nil {
		t.Fatal(err)
	}

	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	r := newTestRouter(wr, serving, release)
	defer close(release)

	fillOneSlot(t, r, serving)

	_, cookies := cookiesByName(r, "")

	want := int((2 * time.Minute).Seconds())
	for _, name := range []string{cookieName, probeCookieName} {
		c, ok := cookies[name]
		if !ok {
			t.Fatalf("expected %q cookie", name)
		}
		if c.MaxAge != want {
			t.Errorf("%s MaxAge = %d, want %d", name, c.MaxAge, want)
		}
	}
}

// ── cookieless client accounting ──────────────────────────────────────────────

// TestCookieless_TokensDoNotAccumulateIndefinitely is the regression test for
// the underlying leak. A client that cannot store cookies is a brand-new
// arrival on every request, so each attempt burns a ticket and a token-store
// entry. Those entries must be reclaimable by the reaper within the TTL.
func TestCookieless_TokensDoNotAccumulateIndefinitely(t *testing.T) {
	wr := newTestWR(t, 1)

	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	r := newTestRouter(wr, serving, release)
	defer close(release)

	fillOneSlot(t, r, serving)

	// 25 arrivals, none of which send a cookie back.
	const arrivals = 25
	for i := 0; i < arrivals; i++ {
		if _, tok := serveWithCookie(r, ""); tok == "" {
			t.Fatalf("arrival %d: expected a token to be issued", i)
		}
	}

	if got := wr.tokens.len(); got != arrivals {
		t.Fatalf("expected %d orphaned tokens before reap, got %d", arrivals, got)
	}

	// Age them past the TTL and run a reap cycle.
	ageAllTokens(wr.tokens, wr.TokenTTL()+time.Minute)
	wr.reap()

	if got := wr.tokens.len(); got != 0 {
		t.Errorf("expected all orphaned tokens reclaimed, %d remain", got)
	}
	if got := wr.LiveQueueDepth(); got != 0 {
		t.Errorf("expected LiveQueueDepth 0 after reap, got %d", got)
	}
}

// TestMaxQueueDepth_TripsOnLiveTokenCount verifies the circuit breaker also
// counts live tokens, not just the ticket-counter derived depth. The two are
// decoupled here: tokens are inserted directly so QueueDepth stays at 0 while
// the live count exceeds the limit.
func TestMaxQueueDepth_TripsOnLiveTokenCount(t *testing.T) {
	wr := newTestWR(t, 1)
	if err := wr.SetMaxQueueDepth(3); err != nil {
		t.Fatal(err)
	}

	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	r := newTestRouter(wr, serving, release)
	defer close(release)

	fillOneSlot(t, r, serving)

	// Insert 5 live tokens without touching nextTicket.
	for i := 0; i < 5; i++ {
		wr.tokens.set(fmt.Sprintf("live-%d", i), ticketEntry{
			ticket:   int64(500 + i),
			issuedAt: time.Now(),
		})
	}

	if d := wr.QueueDepth(); d >= 3 {
		t.Fatalf("setup: QueueDepth should be below the limit to isolate the live count, got %d", d)
	}

	w, _ := serveWithCookie(r, "")
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 once the live token count reaches the limit, got %d", w.Code)
	}
}

// TestLiveQueueDepth_ExcludesBurnedTickets contrasts the two depth measures.
func TestLiveQueueDepth_ExcludesBurnedTickets(t *testing.T) {
	wr := newTestWR(t, 1)

	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	r := newTestRouter(wr, serving, release)
	defer close(release)

	fillOneSlot(t, r, serving)

	for i := 0; i < 4; i++ {
		serveWithCookie(r, "")
	}

	if live := wr.LiveQueueDepth(); live != 4 {
		t.Fatalf("expected LiveQueueDepth 4, got %d", live)
	}

	// Drop the tokens as the reaper would, without rewinding nextTicket.
	ageAllTokens(wr.tokens, wr.TokenTTL()+time.Minute)
	wr.reap()

	if live := wr.LiveQueueDepth(); live != 0 {
		t.Errorf("expected LiveQueueDepth 0 after reap, got %d", live)
	}
	if depth := wr.QueueDepth(); depth < 0 {
		t.Errorf("QueueDepth should never be negative, got %d", depth)
	}
}

// ── SetTokenTTL ───────────────────────────────────────────────────────────────

func TestSetTokenTTL_DefaultIsFiveMinutes(t *testing.T) {
	wr := newTestWR(t, 5)

	if got := wr.TokenTTL(); got != defaultTokenTTL {
		t.Errorf("expected default TTL %s, got %s", defaultTokenTTL, got)
	}
}

func TestSetTokenTTL_ValidRange(t *testing.T) {
	wr := newTestWR(t, 5)

	cases := []time.Duration{
		tokenTTLMin,
		time.Minute,
		30 * time.Minute,
		tokenTTLMax,
	}
	for _, d := range cases {
		if err := wr.SetTokenTTL(d); err != nil {
			t.Errorf("expected no error for %s, got %v", d, err)
		}
		if wr.TokenTTL() != d {
			t.Errorf("expected %s, got %s", d, wr.TokenTTL())
		}
	}
}

func TestSetTokenTTL_InvalidRange(t *testing.T) {
	wr := newTestWR(t, 5)

	cases := []time.Duration{
		0,
		time.Second,
		tokenTTLMin - time.Nanosecond,
		tokenTTLMax + time.Nanosecond,
	}
	for _, d := range cases {
		err := wr.SetTokenTTL(d)
		if err == nil {
			t.Errorf("expected error for %s, got nil", d)
			continue
		}
		if _, ok := err.(ErrTokenTTL); !ok {
			t.Errorf("expected ErrTokenTTL for %s, got %T", d, err)
		}
	}
}

// TestSetTokenTTL_AffectsReapEligibility confirms the reaper reads the
// configured TTL rather than a compile-time constant.
func TestSetTokenTTL_AffectsReapEligibility(t *testing.T) {
	wr := newTestWR(t, 1)

	if err := wr.SetTokenTTL(time.Hour); err != nil {
		t.Fatal(err)
	}
	wr.tokens.set("tok", ticketEntry{
		ticket:   100,
		issuedAt: time.Now().Add(-10 * time.Minute),
	})

	wr.reap()
	if _, ok := wr.tokens.get("tok"); !ok {
		t.Fatal("token 10 minutes old should survive a 1h TTL")
	}

	if err := wr.SetTokenTTL(time.Minute); err != nil {
		t.Fatal(err)
	}
	wr.reap()
	if _, ok := wr.tokens.get("tok"); ok {
		t.Error("token 10 minutes old should be reaped under a 1m TTL")
	}
}

// TestPollingClientSurvivesLongerThanTTL verifies the sliding window: a client
// that keeps polling stays alive well past the raw TTL measured from its
// original issuance, because each poll resets issuedAt.
//
// The token is aged to just UNDER the TTL before each poll. That ordering
// matters — StatusHandler calls deleteIfExpired before touchIssuedAt, so a
// poll arriving after the TTA has already elapsed finds the token gone and
// correctly reports ready=true. See TestExpiredTokenPollIsNotResurrected for
// that side of the boundary.
func TestPollingClientSurvivesLongerThanTTL(t *testing.T) {
	wr := newTestWR(t, 1)

	const ttl = time.Minute
	// Each cycle advances the clock by most of the TTL but never past it.
	const step = 50 * time.Second

	if err := wr.SetTokenTTL(ttl); err != nil {
		t.Fatal(err)
	}

	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	r := newTestRouter(wr, serving, release)
	defer close(release)

	fillOneSlot(t, r, serving)

	_, token := serveWithCookie(r, "")
	if token == "" {
		t.Fatal("no token issued")
	}

	// Three poll cycles: 150s of simulated elapsed time against a 60s TTL.
	for cycle := 1; cycle <= 3; cycle++ {
		ageAllTokens(wr.tokens, step)

		if age := ageOfToken(t, wr.tokens, token); age <= step-time.Second {
			t.Fatalf("cycle %d: expected the token to be aged ~%s, got %s", cycle, step, age)
		}

		resp := pollStatus(r, token)
		if resp.CookiesRequired {
			t.Fatalf("cycle %d: unexpected cookies_required for a client that sent its cookie", cycle)
		}
		if resp.Ready {
			t.Fatalf("cycle %d: token expired despite polling within the TTL", cycle)
		}

		// The poll must have reset the sliding window.
		if age := ageOfToken(t, wr.tokens, token); age > time.Second {
			t.Fatalf("cycle %d: poll did not refresh issuedAt, age is %s", cycle, age)
		}

		wr.reap()
		if _, ok := wr.tokens.get(token); !ok {
			t.Fatalf("cycle %d: an actively polling client must not be reaped", cycle)
		}

		// Polls are rate limited per token; wait out the window.
		time.Sleep(statusPollMinInterval + 50*time.Millisecond)
	}
}

// TestExpiredTokenPollIsNotResurrected pins the other side of the boundary: a
// poll arriving AFTER the TTL has elapsed must not revive the token. The
// server may already have handed that queue position to someone else, so
// deleteIfExpired running ahead of touchIssuedAt is deliberate.
func TestExpiredTokenPollIsNotResurrected(t *testing.T) {
	wr := newTestWR(t, 1)
	if err := wr.SetTokenTTL(time.Minute); err != nil {
		t.Fatal(err)
	}

	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	r := newTestRouter(wr, serving, release)
	defer close(release)

	fillOneSlot(t, r, serving)

	_, token := serveWithCookie(r, "")
	if token == "" {
		t.Fatal("no token issued")
	}

	// Age past the TTL, then poll.
	ageAllTokens(wr.tokens, wr.TokenTTL()+time.Second)

	resp := pollStatus(r, token)
	if !resp.Ready {
		t.Error("expected ready=true so an expired client retries the main handler")
	}
	if resp.CookiesRequired {
		t.Error("expected cookies_required=false — the client did send a cookie")
	}
	if _, ok := wr.tokens.get(token); ok {
		t.Error("expected the expired token to be reclaimed by the poll, not refreshed")
	}
}

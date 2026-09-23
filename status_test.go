package room

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// ── helpers ───────────────────────────────────────────────────────────────────

// setCookies returns the Set-Cookie headers of a response keyed by name.
func setCookies(w *httptest.ResponseRecorder) map[string]*http.Cookie {
	out := make(map[string]*http.Cookie)
	for _, c := range w.Result().Cookies() {
		out[c.Name] = c
	}
	return out
}

// ageEntries shifts every time field of every token back by d, simulating
// the passage of time without sleeping. Unlike ageAllTokens it also ages
// lastPoll and cookieSetAt, so simulated polls are neither rate limited
// nor mistaken for fresh cookie issuance.
func ageEntries(ts *tokenStore, d time.Duration) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	for token, e := range ts.entries {
		e.issuedAt = e.issuedAt.Add(-d)
		if !e.lastPoll.IsZero() {
			e.lastPoll = e.lastPoll.Add(-d)
		}
		e.cookieSetAt = e.cookieSetAt.Add(-d)
		ts.entries[token] = e
	}
}

// ageCookieSetAt shifts only the cookie-refresh clock back by d.
func ageCookieSetAt(ts *tokenStore, d time.Duration) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	for token, e := range ts.entries {
		e.cookieSetAt = e.cookieSetAt.Add(-d)
		ts.entries[token] = e
	}
}

// ── Cookie lifetime: the long-wait regression ────────────────────────────────

// TestLongWait_CookiesOutliveTokenTTL simulates a browser cookie jar
// through a wait three times longer than the token TTL, polling every 3s.
// Before cookie refresh, room_ticket (MaxAge = TTL) was set once and never
// re-sent, so at t = TTL the browser dropped it and the next poll reported
// cookies_required — a terminal error for anyone waiting past the TTL.
func TestLongWait_CookiesOutliveTokenTTL(t *testing.T) {
	wr := newTestWR(t, 1)
	const ttl = time.Minute
	if err := wr.SetTokenTTL(ttl); err != nil {
		t.Fatal(err)
	}

	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	r := newTestRouter(wr, serving, release)
	defer close(release)

	fillOneSlot(t, r, serving)

	w, tok := serveWithCookie(r, "")
	if tok == "" {
		t.Fatal("no token issued")
	}
	issued := setCookies(w)
	ticketExpiry := time.Duration(issued[cookieName].MaxAge) * time.Second
	probeExpiry := time.Duration(issued[probeCookieName].MaxAge) * time.Second

	const step = 3 * time.Second
	var sim time.Duration
	refreshes := 0

	for sim < 3*ttl {
		sim += step
		ageEntries(wr.tokens, step)

		if sim >= ticketExpiry {
			t.Fatalf("at t=%s the browser has dropped room_ticket (expired at %s); "+
				"the next poll would report cookies_required", sim, ticketExpiry)
		}
		if sim >= probeExpiry {
			t.Fatalf("at t=%s the browser has dropped room_probe (expired at %s); "+
				"admit() would show a false cookies-disabled panel", sim, probeExpiry)
		}

		pw := pollStatusRaw(r, tok)
		if pw.Code != http.StatusOK {
			t.Fatalf("at t=%s: expected 200, got %d", sim, pw.Code)
		}

		got := setCookies(pw)
		if tc, ok := got[cookieName]; ok {
			if tc.Value != tok {
				t.Fatalf("refresh changed the ticket value: %q -> %q", tok, tc.Value)
			}
			ticketExpiry = sim + time.Duration(tc.MaxAge)*time.Second
			refreshes++
		}
		if pc, ok := got[probeCookieName]; ok {
			probeExpiry = sim + time.Duration(pc.MaxAge)*time.Second
		}
	}

	if refreshes < 3 {
		t.Errorf("expected at least 3 cookie refreshes over %s, got %d", 3*ttl, refreshes)
	}
}

func TestStatusPoll_NoCookieRefreshWhenFresh(t *testing.T) {
	wr := newTestWR(t, 1)
	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	r := newTestRouter(wr, serving, release)
	defer close(release)

	fillOneSlot(t, r, serving)
	_, tok := serveWithCookie(r, "")

	pw := pollStatusRaw(r, tok)
	if pw.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", pw.Code)
	}
	if got := setCookies(pw); len(got) != 0 {
		t.Errorf("fresh cookies must not be re-sent on every poll, got %v", got)
	}
}

func TestStatusPoll_NoCookieRefreshOnRateLimited(t *testing.T) {
	wr := newTestWR(t, 1)
	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	r := newTestRouter(wr, serving, release)
	defer close(release)

	fillOneSlot(t, r, serving)
	_, tok := serveWithCookie(r, "")

	if pw := pollStatusRaw(r, tok); pw.Code != http.StatusOK {
		t.Fatalf("first poll: expected 200, got %d", pw.Code)
	}

	// Cookies are now overdue, but the next poll is too fast.
	ageCookieSetAt(wr.tokens, wr.TokenTTL())
	pw := pollStatusRaw(r, tok)
	if pw.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", pw.Code)
	}
	if got := setCookies(pw); len(got) != 0 {
		t.Errorf("rate-limited responses must not carry Set-Cookie, got %v", got)
	}
}

// TestStatusPoll_RefreshedCookiesMatchIssuedAttributes pins the refreshed
// cookies to the issued ones. A refresh with a different Path or Domain
// would create a second cookie instead of extending the first.
func TestStatusPoll_RefreshedCookiesMatchIssuedAttributes(t *testing.T) {
	wr := newTestWR(t, 1)
	wr.SetCookiePath("/app")
	wr.SetCookieDomain("example.test")
	wr.SetSecureCookie(true)

	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	r := newTestRouter(wr, serving, release)
	defer close(release)

	fillOneSlot(t, r, serving)
	w, tok := serveWithCookie(r, "")
	issued := setCookies(w)

	ageCookieSetAt(wr.tokens, wr.TokenTTL())
	pw := pollStatusRaw(r, tok)
	if pw.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", pw.Code)
	}
	refreshed := setCookies(pw)

	for _, name := range []string{cookieName, probeCookieName} {
		a, ok := issued[name]
		if !ok {
			t.Fatalf("issuance missing %s", name)
		}
		b, ok := refreshed[name]
		if !ok {
			t.Fatalf("refresh missing %s", name)
		}
		if a.Value != b.Value || a.Path != b.Path || a.Domain != b.Domain ||
			a.Secure != b.Secure || a.HttpOnly != b.HttpOnly ||
			a.SameSite != b.SameSite || a.MaxAge != b.MaxAge {
			t.Errorf("%s attributes differ:\n issued    %+v\n refreshed %+v", name, a, b)
		}
	}
}

// ── Keep-alive ────────────────────────────────────────────────────────────────

// TestStatusPoll_RateLimitedPollKeepsTokenAlive covers a client that polls
// faster than the rate limit. It receives only 429s, but it is actively
// polling and must not be reaped. Previously the 429 path skipped the
// keep-alive, so such a client expired at the TTL.
func TestStatusPoll_RateLimitedPollKeepsTokenAlive(t *testing.T) {
	wr := newTestWR(t, 1)
	if err := wr.SetTokenTTL(time.Minute); err != nil {
		t.Fatal(err)
	}
	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	r := newTestRouter(wr, serving, release)
	defer close(release)

	fillOneSlot(t, r, serving)
	_, tok := serveWithCookie(r, "")

	if pw := pollStatusRaw(r, tok); pw.Code != http.StatusOK {
		t.Fatalf("first poll: expected 200, got %d", pw.Code)
	}

	// 150s of simulated time against a 60s TTL, every poll rate-limited.
	for i := 0; i < 3; i++ {
		ageAllTokens(wr.tokens, 50*time.Second)
		pw := pollStatusRaw(r, tok)
		if pw.Code != http.StatusTooManyRequests {
			t.Fatalf("cycle %d: expected 429, got %d (token expired despite polling?)", i, pw.Code)
		}
	}

	wr.reap()
	if _, ok := wr.tokens.get(tok); !ok {
		t.Error("a client that keeps polling must not be reaped, even when rate limited")
	}
}

func TestStatusPoll_MarksSeen(t *testing.T) {
	wr := newTestWR(t, 1)
	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	r := newTestRouter(wr, serving, release)
	defer close(release)

	fillOneSlot(t, r, serving)
	_, polled := serveWithCookie(r, "")
	_, reloaded := serveWithCookie(r, "")

	for _, tok := range []string{polled, reloaded} {
		if e, _ := wr.tokens.get(tok); e.seen {
			t.Fatal("a freshly issued token must start unseen")
		}
	}

	pollStatus(r, polled)
	serveWithCookie(r, reloaded) // resume render

	for name, tok := range map[string]string{"polled": polled, "reloaded": reloaded} {
		if e, _ := wr.tokens.get(tok); !e.seen {
			t.Errorf("%s token should be marked seen", name)
		}
	}
}

// ── Single-lock poll ──────────────────────────────────────────────────────────

func TestTokenStorePoll_Outcomes(t *testing.T) {
	t.Parallel()
	ts := newTokenStore()
	now := time.Now()

	if res := ts.poll("missing", now, 0, time.Second, time.Minute); res.outcome != pollUnknown {
		t.Errorf("missing token: outcome %d, want pollUnknown", res.outcome)
	}

	ts.set("old", ticketEntry{ticket: 7, issuedAt: now.Add(-(ts.ttl() + time.Second))})
	res := ts.poll("old", now, 0, time.Second, time.Minute)
	if res.outcome != pollExpired || res.entry.ticket != 7 {
		t.Errorf("expired token: got %+v", res)
	}
	if _, ok := ts.get("old"); ok {
		t.Error("expired token should have been removed")
	}

	ts.set("live", ticketEntry{ticket: 9, issuedAt: now, cookieSetAt: now.Add(-2 * time.Minute)})
	res = ts.poll("live", now, 0, time.Second, time.Minute)
	if res.outcome != pollAccepted || !res.refreshCookie || !res.entry.seen {
		t.Errorf("first live poll: got %+v", res)
	}
	res = ts.poll("live", now.Add(100*time.Millisecond), 0, time.Second, time.Minute)
	if res.outcome != pollRateLimited || res.refreshCookie {
		t.Errorf("fast second poll: got %+v", res)
	}

	// A ticket inside the window (ticket <= edge) never gets a refresh.
	ts.set("ready", ticketEntry{ticket: 3, issuedAt: now})
	res = ts.poll("ready", now, 5, time.Second, 0)
	if res.refreshCookie {
		t.Error("in-window ticket must not be scheduled for a cookie refresh")
	}
}

// ── passStore read path ───────────────────────────────────────────────────────

func TestPassStore_ConcurrentGetWithExpiry(t *testing.T) {
	t.Parallel()
	ps := newPassStore()
	ps.set("live", passEntry{expiresAt: time.Now().Add(time.Hour)})
	ps.set("dead", passEntry{expiresAt: time.Now().Add(-time.Hour)})

	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				if _, ok := ps.get("live"); !ok {
					t.Error("live pass reported missing")
					return
				}
				if _, ok := ps.get("dead"); ok {
					t.Error("expired pass reported valid")
					return
				}
			}
		}()
	}
	wg.Wait()

	if ps.len() != 1 {
		t.Errorf("expected only the live pass to remain, len=%d", ps.len())
	}
}

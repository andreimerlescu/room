package room

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// ── helpers ───────────────────────────────────────────────────────────────────

// keyedRouter is newTestRouter with SetTicketKeyFunc reading X-Key.
func keyedRouter(wr *WaitingRoom, serving, release chan struct{}) *gin.Engine {
	wr.SetTicketKeyFunc(func(c *gin.Context) (string, bool) {
		k := c.GetHeader("X-Key")
		return k, k != ""
	})
	return newTestRouter(wr, serving, release)
}

// keyedGet performs GET / with X-Key and optional extra headers.
func keyedGet(r http.Handler, key string, header map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	if key != "" {
		req.Header.Set("X-Key", key)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func decodeWait(t *testing.T, w *httptest.ResponseRecorder) waitResponse {
	t.Helper()
	var resp waitResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode wait response %q: %v", w.Body.String(), err)
	}
	return resp
}

// getPath performs GET path with an optional Accept header.
func getPath(r http.Handler, path, accept string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// assertAccounting checks that every issued ticket number is accounted
// for exactly once: released or skipped (nowServing), retired but not yet
// reached (ledger), waiting (store), or held by a lease.
func assertAccounting(t *testing.T, wr *WaitingRoom) {
	t.Helper()
	got := wr.nowServing.Load() + wr.ledger.len() + int64(wr.tokens.len()) + int64(wr.Held())
	if issued := wr.nextTicket.Load(); got != issued {
		t.Errorf("accounting: nowServing(%d)+ledger(%d)+live(%d)+held(%d)=%d, issued %d",
			wr.nowServing.Load(), wr.ledger.len(), wr.tokens.len(), wr.Held(), got, issued)
	}
}

// assertIndexes checks the key and ID indexes against the entries.
func assertIndexes(t *testing.T, ts *tokenStore) {
	t.Helper()
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	keyed := 0
	for tok, e := range ts.entries {
		if e.key == "" {
			continue
		}
		keyed++
		if ts.byKey[e.key] != tok || ts.byID[e.id] != tok {
			t.Errorf("entry %q (key %q, id %q) is not indexed", tok, e.key, e.id)
		}
	}
	if len(ts.byKey) != keyed || len(ts.byID) != keyed {
		t.Errorf("indexes hold %d keys and %d IDs for %d keyed entries", len(ts.byKey), len(ts.byID), keyed)
	}
}

// ── Enter / Position / Leave ─────────────────────────────────────────────────

func TestEnter_SameKeyKeepsTicketAndPlace(t *testing.T) {
	wr := newTestWR(t, 1)
	id1, pos1, err := wr.Enter("agent-1")
	if err != nil {
		t.Fatal(err)
	}
	id2, pos2, err := wr.Enter("agent-1")
	if err != nil {
		t.Fatal(err)
	}
	if id1 == "" || id1 != id2 || pos1 != 1 || pos2 != 1 {
		t.Errorf("second Enter: id %q→%q, place %d→%d; want the same ticket at place 1", id1, id2, pos1, pos2)
	}
	if n := wr.nextTicket.Load(); n != 1 {
		t.Errorf("second Enter issued another ticket: nextTicket=%d", n)
	}
	if d := wr.LiveQueueDepth(); d != 1 {
		t.Errorf("LiveQueueDepth=%d, want 1", d)
	}
	if ti, ok := wr.TicketByKey("agent-1"); !ok || !ti.Seen || ti.ID != id1 || ti.Place != 1 {
		t.Errorf("TicketByKey: %+v ok=%v; want seen, same ID, place 1", ti, ok)
	}
}

// TestEnter_ConcurrentPlacesAreExact is the PR's 1,000-goroutine case:
// every place 1..1000 is reported exactly once, whatever the capacity.
func TestEnter_ConcurrentPlacesAreExact(t *testing.T) {
	const keys = 1000
	wr := newTestWR(t, 25)

	ids := make([]string, keys)
	places := make([]int64, keys)
	var wg sync.WaitGroup
	for i := range keys {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, pos, err := wr.Enter(fmt.Sprintf("k%d", i))
			if err != nil {
				t.Errorf("Enter k%d: %v", i, err)
				return
			}
			ids[i], places[i] = id, pos
		}()
	}
	wg.Wait()

	seen := make(map[int64]bool, keys)
	for i, p := range places {
		if p < 1 || p > keys || seen[p] {
			t.Fatalf("k%d reported place %d (repeat or out of range)", i, p)
		}
		seen[p] = true
		if now, ok := wr.Position(ids[i]); !ok || now != p {
			t.Errorf("k%d: Position=%d ok=%v, Enter said %d", i, now, ok, p)
		}
	}
	assertIndexes(t, wr.tokens)
	assertAccounting(t, wr)
}

func TestLeave_KeysBehindMoveUp(t *testing.T) {
	wr := newTestWR(t, 1)
	removes := collectEvents(wr, EventRemove)
	var ids []string
	for _, k := range []string{"a", "b", "c"} {
		id, _, err := wr.Enter(k)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := wr.Leave("b"); err != nil {
		t.Fatal(err)
	}
	if s := nextSnapshot(t, removes); s.Key != "b" || s.Token != "" {
		t.Errorf("EventRemove: Key=%q Token=%q, want key b and no token", s.Key, s.Token)
	}
	if p, ok := wr.Position(ids[0]); !ok || p != 1 {
		t.Errorf("a: place %d ok=%v, want 1", p, ok)
	}
	if p, ok := wr.Position(ids[2]); !ok || p != 2 {
		t.Errorf("c: place %d ok=%v, want 2", p, ok)
	}
	if _, ok := wr.Position(ids[1]); ok {
		t.Error("b's ticket is still in line after Leave")
	}
	if err := wr.Leave("b"); err != nil {
		t.Errorf("Leave of a key not in line: %v, want nil", err)
	}
	assertIndexes(t, wr.tokens)
	assertAccounting(t, wr)
}

func TestEnter_Errors(t *testing.T) {
	var zero WaitingRoom
	if _, _, err := zero.Enter("k"); err == nil {
		t.Error("Enter on a zero room: want ErrNotInitialised")
	} else if _, ok := err.(ErrNotInitialised); !ok {
		t.Errorf("got %T", err)
	}
	if err := zero.Leave("k"); err == nil {
		t.Error("Leave on a zero room: want ErrNotInitialised")
	}
	if _, ok := zero.Position("x"); ok {
		t.Error("Position on a zero room reported a place")
	}

	wr := newTestWR(t, 1)
	for _, k := range []string{"", strings.Repeat("k", MaxKeyLen+1)} {
		if _, _, err := wr.Enter(k); err == nil {
			t.Errorf("Enter(%d bytes): want ErrInvalidKey", len(k))
		} else if _, ok := err.(ErrInvalidKey); !ok {
			t.Errorf("Enter(%d bytes): got %T", len(k), err)
		}
	}
	// Two keys that share their first MaxKeyLen bytes must not share a place.
	long := strings.Repeat("k", MaxKeyLen)
	if _, _, err := wr.Enter(long); err != nil {
		t.Fatalf("a key of exactly MaxKeyLen bytes: %v", err)
	}

	if err := wr.SetMaxQueueDepth(2); err != nil {
		t.Fatal(err)
	}
	if _, _, err := wr.Enter("second"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := wr.Enter("third"); err == nil {
		t.Error("Enter past SetMaxQueueDepth: want ErrQueueFull")
	} else if _, ok := err.(ErrQueueFull); !ok {
		t.Errorf("got %T", err)
	}
	if _, _, err := wr.Enter("second"); err != nil {
		t.Errorf("a key already in line must not be refused by the breaker: %v", err)
	}
}

// TestEnter_ReapedAfterTTL: a key that stops asking leaves the line, the
// keys behind it move up by one, and EventEvict fires once.
func TestEnter_ReapedAfterTTL(t *testing.T) {
	wr := newTestWR(t, 1)
	if err := wr.SetTokenTTL(time.Minute); err != nil {
		t.Fatal(err)
	}
	evicts := collectEvents(wr, EventEvict)
	ids := map[string]string{}
	for _, k := range []string{"gone", "b", "c"} {
		id, _, err := wr.Enter(k)
		if err != nil {
			t.Fatal(err)
		}
		ids[k] = id
	}
	// Only "gone" stops asking.
	wr.tokens.mu.Lock()
	tok := wr.tokens.byKey["gone"]
	e := wr.tokens.entries[tok]
	e.issuedAt = e.issuedAt.Add(-2 * time.Minute)
	wr.tokens.entries[tok] = e
	wr.tokens.mu.Unlock()

	wr.reap()

	if _, ok := wr.Position(ids["gone"]); ok {
		t.Fatal("the key that stopped asking is still in line")
	}
	for k, want := range map[string]int64{"b": 1, "c": 2} {
		if p, ok := wr.Position(ids[k]); !ok || p != want {
			t.Errorf("%s: place %d ok=%v, want %d", k, p, ok, want)
		}
	}
	nextSnapshot(t, evicts)
	select {
	case s := <-evicts:
		t.Errorf("EventEvict fired twice: %+v", s)
	case <-time.After(100 * time.Millisecond):
	}
	assertIndexes(t, wr.tokens)
	assertAccounting(t, wr)
}

// TestPosition_DoesNotKeepAlive: watching a place never refreshes it.
func TestPosition_DoesNotKeepAlive(t *testing.T) {
	wr := newTestWR(t, 1)
	id, _, err := wr.Enter("k")
	if err != nil {
		t.Fatal(err)
	}
	before, _ := wr.TicketByKey("k")
	ageAllTokens(wr.tokens, time.Minute)
	wr.Position(id)
	r := newTestRouter(wr, nil, nil)
	getPath(r, "/queue/status/"+id, "application/json")
	after, _ := wr.TicketByKey("k")
	if !after.LastSeen.Before(before.LastSeen) {
		t.Errorf("Position or the status page refreshed LastSeen: %s → %s", before.LastSeen, after.LastSeen)
	}
}

// ── Middleware: keyed requests ───────────────────────────────────────────────

// TestKeyedRequest_KeepsPlaceWithoutCookies: no Set-Cookie, the same place
// across requests, a 503 with Retry-After and JSON, a status URL that
// shows the same place, and admission once the slot frees.
func TestKeyedRequest_KeepsPlaceWithoutCookies(t *testing.T) {
	wr := newTestWR(t, 1)
	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	r := keyedRouter(wr, serving, release)
	fillOneSlot(t, r, serving)

	w := keyedGet(r, "agent", nil)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("queued keyed request: status %d, want 503", w.Code)
	}
	if ra := w.Header().Get("Retry-After"); ra != "3" {
		t.Errorf("Retry-After=%q, want 3", ra)
	}
	if c := w.Result().Cookies(); len(c) != 0 {
		t.Errorf("keyed request was sent cookies: %v", c)
	}
	first := decodeWait(t, w)
	if !first.Waiting || first.Position != 1 || first.RetryAfter != 3 ||
		!strings.HasPrefix(first.StatusURL, "/queue/status/") {
		t.Fatalf("wait body: %+v", first)
	}
	issued := wr.nextTicket.Load()

	w = keyedGet(r, "agent", nil)
	if again := decodeWait(t, w); again.Position != 1 || again.StatusURL != first.StatusURL {
		t.Errorf("second request: %+v, want the same place and URL as %+v", again, first)
	}
	if len(w.Result().Cookies()) != 0 {
		t.Error("second keyed request was sent cookies")
	}
	if n := wr.nextTicket.Load(); n != issued {
		t.Errorf("second request issued a ticket: %d → %d", issued, n)
	}

	sw := getPath(r, first.StatusURL, "application/json")
	var st ticketStatusResponse
	if err := json.Unmarshal(sw.Body.Bytes(), &st); err != nil || sw.Code != http.StatusOK {
		t.Fatalf("status URL: %d %q", sw.Code, sw.Body.String())
	}
	if !st.Waiting || st.Position != first.Position {
		t.Errorf("status URL shows %+v, the 503 said position %d", st, first.Position)
	}

	close(release)
	waitForNowServing(t, wr, 1, 2*time.Second)
	if w := keyedGet(r, "agent", nil); w.Code != http.StatusOK {
		t.Errorf("keyed request after the slot freed: %d, want 200", w.Code)
	}
	if _, ok := wr.TicketByKey("agent"); ok {
		t.Error("admitted keyed ticket is still in the store")
	}
	if sw := getPath(r, first.StatusURL, "application/json"); sw.Code != http.StatusNotFound {
		t.Errorf("status URL after admission: %d, want 404", sw.Code)
	}
	assertIndexes(t, wr.tokens)
	assertAccounting(t, wr)
}

// TestKeyedRequest_CookieClientsUnchanged: a request the key function does
// not recognise gets the waiting-room page and cookies, as before.
func TestKeyedRequest_CookieClientsUnchanged(t *testing.T) {
	wr := newTestWR(t, 1)
	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	r := keyedRouter(wr, serving, release)
	defer close(release)
	fillOneSlot(t, r, serving)

	w, tok := serveWithCookie(r, "")
	if w.Code != http.StatusOK || tok == "" || !strings.Contains(w.Body.String(), "You're in the queue") {
		t.Errorf("cookie request: code %d token %q; want the waiting page with a cookie", w.Code, tok)
	}
}

func TestKeyedRequest_InvalidKeyIsBadRequest(t *testing.T) {
	wr := newTestWR(t, 1)
	r := keyedRouter(wr, nil, nil)
	if w := keyedGet(r, strings.Repeat("x", MaxKeyLen+1), nil); w.Code != http.StatusBadRequest {
		t.Errorf("over-long key: %d, want 400", w.Code)
	}
}

// TestKeyedTicket_TokenCannotBeUsedAsCookie: a keyed ticket's internal
// token, presented as room_ticket, claims nothing.
func TestKeyedTicket_TokenCannotBeUsedAsCookie(t *testing.T) {
	wr := newTestWR(t, 1)
	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	r := keyedRouter(wr, serving, release)
	fillOneSlot(t, r, serving)

	if w := keyedGet(r, "agent", nil); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("setup: %d", w.Code)
	}
	ti, _ := wr.TicketByKey("agent")

	if resp := pollStatus(r, ti.Token); !resp.Ready || resp.Position != 0 {
		t.Errorf("poll with a keyed token: %+v, want it treated as unknown", resp)
	}
	close(release)
	waitForNowServing(t, wr, 1, 2*time.Second)
	if _, issued := serveWithCookie(r, ti.Token); issued == "" {
		t.Error("a cookie carrying a keyed token was admitted instead of queued as a stranger")
	}
	if after, ok := wr.TicketByKey("agent"); !ok || after.Token != ti.Token {
		t.Error("the keyed ticket was claimed through a cookie")
	}
}

func TestKeyedRequest_LeaseHolderPassesWithoutASlot(t *testing.T) {
	wr := newTestWR(t, 1)
	r := keyedRouter(wr, nil, nil)
	if ok, _, err := wr.Acquire("holder"); err != nil || !ok {
		t.Fatalf("Acquire: ok=%v err=%v", ok, err)
	}
	if w := keyedGet(r, "holder", nil); w.Code != http.StatusOK {
		t.Errorf("lease holder's request: %d, want 200", w.Code)
	}
	if w := keyedGet(r, "other", nil); w.Code != http.StatusServiceUnavailable {
		t.Errorf("another key while the room is full: %d, want 503", w.Code)
	}
	if n := wr.Len(); n != 1 {
		t.Errorf("Len=%d after the holder's request, want 1 (no second slot)", n)
	}
}

// ── Wait responses ───────────────────────────────────────────────────────────

// TestWaitJSON_OffByDefault is the compatibility guarantee: without
// SetWaitJSON, an API client still gets the waiting-room page.
func TestWaitJSON_OffByDefault(t *testing.T) {
	wr := newTestWR(t, 1)
	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	r := newTestRouter(wr, serving, release)
	defer close(release)
	fillOneSlot(t, r, serving)

	for _, accept := range []string{"", "*/*", "application/json"} {
		w := keyedGet(r, "", map[string]string{"Accept": accept})
		if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
			t.Errorf("Accept %q: %d %s, want the 200 HTML page", accept, w.Code, w.Header().Get("Content-Type"))
		}
	}
}

func TestWaitJSON_OptIn(t *testing.T) {
	wr := newTestWR(t, 1)
	wr.SetWaitJSON(true)
	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	r := newTestRouter(wr, serving, release)
	defer close(release)
	fillOneSlot(t, r, serving)

	w := keyedGet(r, "", map[string]string{"Accept": "application/json"})
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") != "3" {
		t.Fatalf("JSON client: %d Retry-After=%q", w.Code, w.Header().Get("Retry-After"))
	}
	resp := decodeWait(t, w)
	if !resp.Waiting || resp.Position != 1 || resp.StatusURL != "/queue/status" {
		t.Errorf("JSON body: %+v", resp)
	}
	var tok string
	for _, c := range w.Result().Cookies() {
		if c.Name == cookieName {
			tok = c.Value
		}
	}
	if tok == "" {
		t.Fatal("an opted-in cookie client must still get room_ticket")
	}
	if p := pollStatus(r, tok); p.Ready || p.Position != 1 {
		t.Errorf("its cookie poll: %+v", p)
	}

	w = keyedGet(r, "", map[string]string{"Accept": "text/html,application/xhtml+xml;q=0.9,*/*;q=0.8"})
	if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
		t.Errorf("browser with SetWaitJSON on: %d, want the HTML page", w.Code)
	}
}

func TestAcceptsHTML(t *testing.T) {
	for accept, want := range map[string]bool{
		"":                           false,
		"*/*":                        false,
		"application/json":           false,
		"text/html":                  true,
		"TEXT/HTML":                  true,
		"text/html;q=0":              false,
		"text/html;q=0.0, */*":       false,
		"application/xhtml+xml":      true,
		"text/plain, text/html;q=.5": true,
		"garbage;;;":                 false,
	} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Accept", accept)
		if got := acceptsHTML(req); got != want {
			t.Errorf("acceptsHTML(%q) = %v, want %v", accept, got, want)
		}
	}
}

func TestSetWaitResponder(t *testing.T) {
	wr := newTestWR(t, 1)
	got := make(chan TicketInfo, 4)
	wr.SetWaitResponder(func(c *gin.Context, ti TicketInfo) {
		got <- ti
		c.String(http.StatusAccepted, "wait %d", ti.Place)
	})
	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	r := keyedRouter(wr, serving, release)
	defer close(release)
	fillOneSlot(t, r, serving)

	if w := keyedGet(r, "agent", nil); w.Code != http.StatusAccepted || w.Body.String() != "wait 1" {
		t.Errorf("keyed: %d %q", w.Code, w.Body.String())
	}
	if ti := <-got; ti.Token != "" || ti.Key != "agent" || ti.ID == "" || ti.Place != 1 {
		t.Errorf("keyed TicketInfo: %+v; want no token, the key, an ID and place 1", ti)
	}

	w, tok := serveWithCookie(r, "")
	if w.Code != http.StatusAccepted || tok == "" || w.Body.String() != "wait 2" {
		t.Errorf("cookie: %d %q token %q; want the responder's body, and the cookie still set", w.Code, w.Body.String(), tok)
	}
	if ti := <-got; ti.Token != tok || ti.Place != 2 {
		t.Errorf("cookie TicketInfo: %+v", ti)
	}

	wr.SetWaitResponder(nil)
	if w := keyedGet(r, "agent", nil); w.Code != http.StatusServiceUnavailable {
		t.Errorf("after SetWaitResponder(nil): %d, want the JSON default", w.Code)
	}
}

// ── Leases ───────────────────────────────────────────────────────────────────

// TestAcquire_ExactlyCapSucceed is the PR's cap-25, 1,000-key case.
func TestAcquire_ExactlyCapSucceed(t *testing.T) {
	const keys, cap = 1000, 25
	wr := newTestWR(t, cap)

	type result struct {
		ok    bool
		place int64
	}
	results := make([]result, keys)
	var wg sync.WaitGroup
	for i := range keys {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, pos, err := wr.Acquire(fmt.Sprintf("k%d", i))
			if err != nil {
				t.Errorf("Acquire k%d: %v", i, err)
				return
			}
			results[i] = result{ok, pos}
		}()
	}
	wg.Wait()

	granted := 0
	byPlace := map[int64]string{}
	for i, r := range results {
		if r.ok {
			granted++
			continue
		}
		if r.place < 1 || byPlace[r.place] != "" {
			t.Fatalf("k%d waits at place %d (repeat or invalid)", i, r.place)
		}
		byPlace[r.place] = fmt.Sprintf("k%d", i)
	}
	if granted != cap || wr.Held() != cap {
		t.Fatalf("%d granted, Held()=%d; want exactly %d", granted, wr.Held(), cap)
	}
	if len(byPlace) != keys-cap {
		t.Fatalf("%d keys in line, want %d", len(byPlace), keys-cap)
	}
	// The rest wait in the order they first asked.
	q := wr.Queue(keys)
	for i, ti := range q {
		if want := byPlace[int64(i+1)]; ti.Key != want {
			t.Fatalf("place %d holds %s, but %s was told it is there", i+1, ti.Key, want)
		}
	}
	assertAccounting(t, wr)
}

// TestAcquire_FirstComeFirstServed: with one slot freed, the key at place 2
// is refused while place 1 is still in line, and granted once place 1 has
// acquired or left.
func TestAcquire_FirstComeFirstServed(t *testing.T) {
	for _, mode := range []string{"acquires", "leaves"} {
		t.Run(mode, func(t *testing.T) {
			wr := newTestWR(t, 2)
			for _, k := range []string{"a", "b"} {
				if ok, _, _ := wr.Acquire(k); !ok {
					t.Fatalf("setup: %s not granted", k)
				}
			}
			for want, k := range []string{"first", "second"} {
				if ok, pos, _ := wr.Acquire(k); ok || pos != int64(want+1) {
					t.Fatalf("setup: %s ok=%v place=%d", k, ok, pos)
				}
			}
			if err := wr.Release("a"); err != nil {
				t.Fatal(err)
			}
			if ok, pos, _ := wr.Acquire("second"); ok || pos != 2 {
				t.Fatalf("place 2 granted ahead of place 1 (ok=%v place=%d)", ok, pos)
			}
			if mode == "acquires" {
				if ok, _, _ := wr.Acquire("first"); !ok {
					t.Fatal("place 1 refused with a slot free")
				}
				if ok, _, _ := wr.Acquire("second"); ok {
					t.Fatal("second granted with no slot free")
				}
				if err := wr.Release("b"); err != nil {
					t.Fatal(err)
				}
			} else if err := wr.Leave("first"); err != nil {
				t.Fatal(err)
			}
			if ok, _, _ := wr.Acquire("second"); !ok {
				t.Error("second refused after place 1 " + mode)
			}
			assertAccounting(t, wr)
		})
	}
}

func TestRelease_ThenHeadAcquires(t *testing.T) {
	wr := newTestWR(t, 1)
	leases := collectEvents(wr, EventLease)
	releases := collectEvents(wr, EventRelease)
	if ok, _, _ := wr.Acquire("a"); !ok {
		t.Fatal("a not granted")
	}
	if s := nextSnapshot(t, leases); s.Key != "a" {
		t.Errorf("EventLease Key=%q", s.Key)
	}
	if ok, pos, _ := wr.Acquire("b"); ok || pos != 1 {
		t.Fatalf("b: ok=%v place=%d", ok, pos)
	}
	if ok, pos, _ := wr.Acquire("a"); !ok || pos != 0 {
		t.Errorf("Acquire by a holder must renew and succeed: ok=%v pos=%d", ok, pos)
	}
	if _, _, err := wr.Enter("a"); err == nil {
		t.Error("Enter by a lease holder: want ErrLeaseHeld")
	} else if _, ok := err.(ErrLeaseHeld); !ok {
		t.Errorf("got %T", err)
	}
	if err := wr.Release("a"); err != nil {
		t.Fatal(err)
	}
	if s := nextSnapshot(t, releases); s.Key != "a" {
		t.Errorf("EventRelease Key=%q", s.Key)
	}
	if ok, _, _ := wr.Acquire("b"); !ok {
		t.Error("the head of the line was refused after a Release")
	}
	if err := wr.Release("a"); err == nil {
		t.Error("Release without a lease: want ErrNoLease")
	} else if _, ok := err.(ErrNoLease); !ok {
		t.Errorf("got %T", err)
	}
	if err := wr.Release("never"); err == nil {
		t.Error("Release of an unknown key: want ErrNoLease")
	}
	assertAccounting(t, wr)
}

// TestSetCap_BelowHeld: no lease is revoked, and none is granted until
// enough are released to bring the room under the new capacity.
func TestSetCap_BelowHeld(t *testing.T) {
	wr := newTestWR(t, 5)
	for i := range 5 {
		if ok, _, _ := wr.Acquire(fmt.Sprintf("h%d", i)); !ok {
			t.Fatalf("setup: h%d not granted", i)
		}
	}
	if ok, _, _ := wr.Acquire("w"); ok {
		t.Fatal("setup: w granted in a full room")
	}
	if err := wr.SetCap(2); err != nil {
		t.Fatal(err)
	}
	if h := wr.Held(); h != 5 {
		t.Fatalf("SetCap revoked leases: Held()=%d, want 5", h)
	}
	if l := wr.Len(); l != 5 {
		t.Fatalf("SetCap drained semaphore: Len()=%d, want 5", l)
	}
	for i := range 3 { // 5 → 2 held: still not under the new cap of 2
		if err := wr.Release(fmt.Sprintf("h%d", i)); err != nil {
			t.Fatal(err)
		}
		want := int32(5 - (i + 1))
		if wr.Held() != want || wr.Len() != int(wr.Held()) {
			t.Fatalf("after releasing h%d: Len()=%d Held()=%d, want both %d", i, wr.Len(), wr.Held(), want)
		}
		if ok, _, _ := wr.Acquire("w"); ok {
			t.Fatalf("w granted with %d held and cap 2", wr.Held())
		}
	}
	if err := wr.Release("h3"); err != nil {
		t.Fatal(err)
	}
	if wr.Held() != 1 || wr.Len() != 1 {
		t.Fatalf("after releasing h3: Len()=%d Held()=%d, want both 1", wr.Len(), wr.Held())
	}
	if ok, _, _ := wr.Acquire("w"); !ok {
		t.Error("w refused with 1 held and cap 2")
	}
	if wr.Len() != 2 || wr.Held() != 2 {
		t.Errorf("after granting w: Len()=%d Held()=%d, want both 2", wr.Len(), wr.Held())
	}
	assertAccounting(t, wr)
}

func TestLeaseTTL_ExpiresAndRenews(t *testing.T) {
	wr := newTestWR(t, 1)
	expired := collectEvents(wr, EventLeaseExpire)
	for _, d := range []time.Duration{-time.Second, time.Second, leaseTTLMax + time.Second} {
		if err := wr.SetLeaseTTL(d); err == nil {
			t.Errorf("SetLeaseTTL(%s): want ErrLeaseTTL", d)
		}
	}
	if err := wr.SetLeaseTTL(time.Minute); err != nil {
		t.Fatal(err)
	}
	age := func(key string, d time.Duration) {
		wr.leaseMu.Lock()
		l := wr.leases[key]
		l.renewedAt = l.renewedAt.Add(-d)
		wr.leases[key] = l
		wr.leaseMu.Unlock()
	}
	if ok, _, _ := wr.Acquire("a"); !ok {
		t.Fatal("a not granted")
	}
	if ok, _, _ := wr.Acquire("b"); ok {
		t.Fatal("b granted in a full room")
	}

	age("a", 50*time.Second)
	if ok, _, _ := wr.Acquire("a"); !ok { // renews
		t.Fatal("renewal refused")
	}
	age("a", 50*time.Second)
	wr.reap()
	if !wr.HasLease("a") {
		t.Fatal("a renewed lease expired")
	}

	age("a", 2*time.Minute)
	wr.reap()
	if wr.HasLease("a") {
		t.Fatal("a lease past its TTL survived the reaper")
	}
	if s := nextSnapshot(t, expired); s.Key != "a" {
		t.Errorf("EventLeaseExpire Key=%q", s.Key)
	}
	if ok, _, _ := wr.Acquire("b"); !ok {
		t.Error("b refused after a's lease expired")
	}
	assertAccounting(t, wr)
}

// ── Export / Import ──────────────────────────────────────────────────────────

func TestExportImport_KeepsLeasesAndLineOrder(t *testing.T) {
	a := newTestWR(t, 2)
	for _, k := range []string{"h1", "h2"} {
		if ok, _, _ := a.Acquire(k); !ok {
			t.Fatalf("setup: %s", k)
		}
	}
	ids := map[string]string{}
	for _, k := range []string{"w1", "w2", "w3"} {
		id, _, err := a.Enter(k)
		if err != nil {
			t.Fatal(err)
		}
		ids[k] = id
	}
	buf := exportRoom(t, a)
	var raw persistFile
	if err := json.Unmarshal(buf.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if raw.Version != persistFormatLeases || len(raw.Leases) != 2 {
		t.Fatalf("export: version %d with %d leases, want version 2 with 2", raw.Version, len(raw.Leases))
	}

	b := newTestWR(t, 2)
	stats, err := b.Import(buf)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Restored != 3 || stats.LeasesRestored != 2 {
		t.Errorf("stats %+v", stats)
	}
	if b.Held() != 2 || !b.HasLease("h1") || !b.HasLease("h2") {
		t.Fatalf("leases not restored: Held()=%d", b.Held())
	}
	for i, k := range []string{"w1", "w2", "w3"} {
		if p, ok := b.Position(ids[k]); !ok || p != int64(i+1) {
			t.Errorf("%s: place %d ok=%v, want %d", k, p, ok, i+1)
		}
	}
	if ok, _, _ := b.Acquire("w1"); ok {
		t.Fatal("w1 granted although the restored leases fill the room")
	}
	if err := b.Release("h1"); err != nil {
		t.Fatal(err)
	}
	if ok, _, _ := b.Acquire("w1"); !ok {
		t.Error("w1 refused after a restored lease was released")
	}
	assertIndexes(t, b.tokens)
	assertAccounting(t, b)
}

func TestExport_VersionOneWithoutLeases(t *testing.T) {
	a := newTestWR(t, 1)
	if _, _, err := a.Enter("k"); err != nil {
		t.Fatal(err)
	}
	var raw persistFile
	if err := json.Unmarshal(exportRoom(t, a).Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if raw.Version != persistFormatVersion || len(raw.Tickets) != 1 || raw.Tickets[0].Key != "k" || raw.Tickets[0].ID == "" {
		t.Errorf("export without leases: %+v", raw)
	}
}

func TestImport_RejectsBadKeyedData(t *testing.T) {
	now := time.Now()
	for name, f := range map[string]persistFile{
		"leases in v1":      {Version: persistFormatVersion, ExportedAt: now, Leases: []persistedLease{{Key: "a"}}},
		"duplicate key":     {Version: persistFormatVersion, ExportedAt: now, Tickets: []persistedTicket{{Token: "t1", Key: "a", LastSeen: now}, {Token: "t2", Key: "a", LastSeen: now}}},
		"leased and queued": {Version: persistFormatLeases, ExportedAt: now, Tickets: []persistedTicket{{Token: "t1", Key: "a", LastSeen: now}}, Leases: []persistedLease{{Key: "a"}}},
		"long key":          {Version: persistFormatVersion, ExportedAt: now, Tickets: []persistedTicket{{Token: "t1", Key: strings.Repeat("k", MaxKeyLen+1), LastSeen: now}}},
		"duplicate id":      {Version: persistFormatVersion, ExportedAt: now, Tickets: []persistedTicket{{Token: "t1", Key: "a", ID: "x", LastSeen: now}, {Token: "t2", Key: "b", ID: "x", LastSeen: now}}},
		"version 3":         {Version: 3, ExportedAt: now},
	} {
		b := newTestWR(t, 1)
		if _, err := b.Import(encodeFile(t, f)); err == nil {
			t.Errorf("%s: imported", name)
		} else if _, ok := err.(ErrImportFormat); !ok {
			t.Errorf("%s: got %T: %v", name, err, err)
		}
		if b.nextTicket.Load() != 0 || b.Held() != 0 {
			t.Errorf("%s: a rejected import changed the room", name)
		}
	}
}

// ── net/http Handler ─────────────────────────────────────────────────────────

func TestHandler_ServesStatusRoutes(t *testing.T) {
	wr := newTestWR(t, 1)
	mux := http.NewServeMux()
	mux.Handle("/wait/", http.StripPrefix("/wait", wr.Handler()))
	srv := httptest.NewServer(mux)
	defer srv.Close()

	if _, _, err := wr.Enter("first"); err != nil {
		t.Fatal(err)
	}
	id, _, err := wr.Enter("agent")
	if err != nil {
		t.Fatal(err)
	}

	get := func(path, accept string) (*http.Response, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var sb strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := resp.Body.Read(buf)
			sb.Write(buf[:n])
			if err != nil {
				break
			}
		}
		return resp, sb.String()
	}

	resp, body := get("/wait/queue/status", "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"cookies_required":true`) {
		t.Errorf("cookie status without a cookie: %d %s", resp.StatusCode, body)
	}

	resp, body = get("/wait/queue/status/"+id, "application/json")
	var st ticketStatusResponse
	if err := json.Unmarshal([]byte(body), &st); err != nil || resp.StatusCode != http.StatusOK || !st.Waiting || st.Position != 2 {
		t.Errorf("ticket status JSON: %d %s", resp.StatusCode, body)
	}

	resp, _ = get("/wait/queue/status/"+id, "application/json")
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") != "1" {
		t.Errorf("immediate second view: %d, want 429 with Retry-After", resp.StatusCode)
	}

	// Let the rate limit pass without sleeping: move the last view back.
	wr.tokens.mu.Lock()
	tok := wr.tokens.byID[id]
	e := wr.tokens.entries[tok]
	e.viewedAt = e.viewedAt.Add(-2 * statusPollMinInterval)
	wr.tokens.entries[tok] = e
	wr.tokens.mu.Unlock()

	resp, body = get("/wait/queue/status/"+id, "text/html")
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") ||
		!strings.Contains(body, `id="position">2<`) {
		t.Errorf("ticket status page: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}

	resp, body = get("/wait/queue/status/no-such-ticket", "")
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(body, `"waiting":false`) {
		t.Errorf("unknown ID: %d %s", resp.StatusCode, body)
	}

	req, _ := http.NewRequest(http.MethodOptions, srv.URL+"/wait/queue/status", nil)
	if r, err := srv.Client().Do(req); err != nil || r.StatusCode != http.StatusNoContent {
		t.Errorf("OPTIONS: %v %v", r, err)
	} else {
		r.Body.Close()
	}
}

// TestRegisterRoutes_TicketStatusRoute: the gin route serves the same
// endpoint, and does not shadow GET /queue/status.
func TestRegisterRoutes_TicketStatusRoute(t *testing.T) {
	wr := newTestWR(t, 1)
	r := newTestRouter(wr, nil, nil)
	id, _, err := wr.Enter("k")
	if err != nil {
		t.Fatal(err)
	}
	if w := getPath(r, "/queue/status/"+id, ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"position":1`) {
		t.Errorf("gin ticket status: %d %s", w.Code, w.Body.String())
	}
	if w := pollStatusRaw(r, ""); !strings.Contains(w.Body.String(), "cookies_required") {
		t.Errorf("GET /queue/status changed: %s", w.Body.String())
	}
}

// ── Invariants under concurrency ─────────────────────────────────────────────

// TestKeyed_ConcurrentMixKeepsAccounting runs Enter, Leave, Acquire,
// Release, keyed requests and reaps together; afterwards every ticket is
// accounted for once and the indexes match the store.
func TestKeyed_ConcurrentMixKeepsAccounting(t *testing.T) {
	wr := newTestWR(t, 3)
	r := keyedRouter(wr, nil, nil)

	var wg sync.WaitGroup
	for i := range 60 {
		key := fmt.Sprintf("k%d", i%15)
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch i % 5 {
			case 0:
				_, _, _ = wr.Enter(key)
			case 1:
				_ = wr.Leave(key)
			case 2:
				if ok, _, _ := wr.Acquire(key); ok {
					_ = wr.Release(key)
				}
			case 3:
				keyedGet(r, key, nil)
			case 4:
				wr.reap()
			}
		}()
	}
	wg.Wait()

	if wr.Len() != int(wr.Held()) {
		t.Errorf("Len()=%d with %d leases and no request in flight", wr.Len(), wr.Held())
	}
	assertIndexes(t, wr.tokens)
	assertAccounting(t, wr)

	keys := make([]string, 0)
	for _, ti := range wr.Queue(100) {
		if ti.Key != "" {
			keys = append(keys, ti.Key)
		}
	}
	sort.Strings(keys)
	for i := 1; i < len(keys); i++ {
		if keys[i] == keys[i-1] {
			t.Errorf("key %s is in line twice", keys[i])
		}
	}
}

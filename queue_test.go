package room

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// arriveWith performs a GET / with optional headers and cookies and
// returns the room_ticket issued, if any.
func arriveWith(r *gin.Engine, header map[string]string, cookies ...*http.Cookie) string {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	for _, c := range w.Result().Cookies() {
		if c.Name == cookieName {
			return c.Value
		}
	}
	return ""
}

// collectEvents registers a buffered collector for one event.
func collectEvents(wr *WaitingRoom, ev Event) chan Snapshot {
	ch := make(chan Snapshot, 64)
	wr.On(ev, func(s Snapshot) { ch <- s })
	return ch
}

func nextSnapshot(t *testing.T, ch chan Snapshot) Snapshot {
	t.Helper()
	select {
	case s := <-ch:
		return s
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for event")
		return Snapshot{}
	}
}

// ── Queue / Ticket ────────────────────────────────────────────────────────────

func TestQueue_OrderAndPositions(t *testing.T) {
	wr := newTestWR(t, 1)
	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	r := newTestRouter(wr, serving, release)
	defer close(release)

	fillOneSlot(t, r, serving)
	toks := queueTokens(t, r, 5)

	q := wr.Queue(10)
	if len(q) != 5 {
		t.Fatalf("expected 5 tickets, got %d", len(q))
	}
	for i, ti := range q {
		if ti.Token != toks[i] {
			t.Errorf("slot %d: expected token of arrival %d", i, i)
		}
		if ti.Position != int64(i+1) {
			t.Errorf("slot %d: expected position %d, got %d", i, i+1, ti.Position)
		}
		if ti.IssuedAt.IsZero() || ti.LastSeen.IsZero() {
			t.Errorf("slot %d: missing timestamps %+v", i, ti)
		}
		if ti.Seen {
			t.Errorf("slot %d: fresh ticket should not be Seen", i)
		}
		if i > 0 && ti.IssuedAt.Before(q[i-1].IssuedAt) {
			t.Errorf("slot %d: IssuedAt out of order", i)
		}
	}
}

func TestQueue_LimitBoundsResult(t *testing.T) {
	wr := newTestWR(t, 1)
	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	r := newTestRouter(wr, serving, release)
	defer close(release)

	fillOneSlot(t, r, serving)
	toks := queueTokens(t, r, 5)

	q := wr.Queue(2)
	if len(q) != 2 {
		t.Fatalf("expected 2 tickets, got %d", len(q))
	}
	if q[0].Token != toks[0] || q[1].Token != toks[1] {
		t.Error("Queue(2) must return the two front-most tickets")
	}
}

func TestQueue_NonPositiveLimitAndUninitialised(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 1)
	if q := wr.Queue(0); q != nil {
		t.Errorf("Queue(0) should return nil, got %v", q)
	}
	if q := wr.Queue(-1); q != nil {
		t.Errorf("Queue(-1) should return nil, got %v", q)
	}

	var zero WaitingRoom
	if q := zero.Queue(10); q != nil {
		t.Errorf("uninitialised Queue should return nil, got %v", q)
	}
	if _, ok := zero.Ticket("x"); ok {
		t.Error("uninitialised Ticket should report not found")
	}
}

func TestQueue_ReadyNotReloadedComesFirst(t *testing.T) {
	wr := newTestWR(t, 1)
	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	r := newTestRouter(wr, serving, release)

	fillOneSlot(t, r, serving)
	toks := queueTokens(t, r, 2)

	close(release)
	waitForNowServing(t, wr, 1, 2*time.Second)

	q := wr.Queue(10)
	if len(q) != 2 {
		t.Fatalf("expected 2 tickets, got %d", len(q))
	}
	if q[0].Token != toks[0] || q[0].Position > 0 {
		t.Errorf("head should be the ready ticket with Position <= 0, got %+v", q[0])
	}
	if q[1].Position != 1 {
		t.Errorf("second ticket should be at position 1, got %d", q[1].Position)
	}
}

// TestQueue_TiesOrderedByIssueTime covers duplicate ticket numbers, which
// promotions can create.
func TestQueue_TiesOrderedByIssueTime(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 1)
	now := time.Now()
	wr.nextTicket.Store(10)
	wr.tokens.set("later", ticketEntry{ticket: 5, createdAt: now, issuedAt: now})
	wr.tokens.set("earlier", ticketEntry{ticket: 5, createdAt: now.Add(-time.Second), issuedAt: now})
	wr.tokens.set("behind", ticketEntry{ticket: 7, createdAt: now.Add(-time.Hour), issuedAt: now})

	q := wr.Queue(3)
	got := []string{q[0].Token, q[1].Token, q[2].Token}
	want := []string{"earlier", "later", "behind"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func TestTicket_LookupAndStableIssuedAt(t *testing.T) {
	wr := newTestWR(t, 1)
	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	r := newTestRouter(wr, serving, release)
	defer close(release)

	fillOneSlot(t, r, serving)
	tok := queueTokens(t, r, 1)[0]

	before, ok := wr.Ticket(tok)
	if !ok {
		t.Fatal("expected ticket to be found")
	}
	if !before.LastPoll.IsZero() {
		t.Error("LastPoll should be zero before any poll")
	}

	ageAllTokens(wr.tokens, time.Minute) // moves LastSeen back, not IssuedAt
	pollStatus(r, tok)

	after, ok := wr.Ticket(tok)
	if !ok {
		t.Fatal("ticket vanished after poll")
	}
	if !after.IssuedAt.Equal(before.IssuedAt) {
		t.Errorf("IssuedAt must not change: %s -> %s", before.IssuedAt, after.IssuedAt)
	}
	if !after.Seen || after.LastPoll.IsZero() {
		t.Errorf("poll should mark Seen and set LastPoll, got %+v", after)
	}
	if !after.LastSeen.After(after.IssuedAt) {
		t.Error("LastSeen should advance past IssuedAt after a poll")
	}

	if _, ok := wr.Ticket("unknown"); ok {
		t.Error("unknown token should not be found")
	}
}

// ── Client keys ───────────────────────────────────────────────────────────────

func TestClientKeyFunc_RecordedAndUsableForRemoval(t *testing.T) {
	wr := newTestWR(t, 1)
	wr.SetClientKeyFunc(func(c *gin.Context) string { return c.GetHeader("X-Client") })

	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	r := newTestRouter(wr, serving, release)
	defer close(release)

	fillOneSlot(t, r, serving)

	for i := 0; i < 6; i++ {
		key := "10.0.0.1"
		if i%2 == 1 {
			key = "192.168.1.9"
		}
		if tok := arriveWith(r, map[string]string{"X-Client": key}); tok == "" {
			t.Fatalf("arrival %d not queued", i)
		}
	}

	for _, ti := range wr.Queue(10) {
		if ti.ClientKey != "10.0.0.1" && ti.ClientKey != "192.168.1.9" {
			t.Errorf("unexpected ClientKey %q", ti.ClientKey)
		}
	}

	n := wr.RemoveTokensFunc(func(ti TicketInfo) bool {
		return strings.HasPrefix(ti.ClientKey, "192.168.")
	})
	if n != 3 {
		t.Errorf("expected 3 removals by client key, got %d", n)
	}
	for _, ti := range wr.Queue(10) {
		if ti.ClientKey != "10.0.0.1" {
			t.Errorf("banned-range ticket survived: %+v", ti)
		}
	}
}

func TestClientKeyFunc_TruncatedAndNilDisables(t *testing.T) {
	wr := newTestWR(t, 1)
	long := strings.Repeat("k", maxClientKeyLen*4)
	wr.SetClientKeyFunc(func(*gin.Context) string { return long })

	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	r := newTestRouter(wr, serving, release)
	defer close(release)

	fillOneSlot(t, r, serving)
	tok := arriveWith(r, nil)
	ti, _ := wr.Ticket(tok)
	if len(ti.ClientKey) != maxClientKeyLen {
		t.Errorf("expected key truncated to %d bytes, got %d", maxClientKeyLen, len(ti.ClientKey))
	}

	wr.SetClientKeyFunc(nil)
	tok2 := arriveWith(r, nil)
	ti2, _ := wr.Ticket(tok2)
	if ti2.ClientKey != "" {
		t.Errorf("expected empty key after SetClientKeyFunc(nil), got %q", ti2.ClientKey)
	}
	// Tickets already queued keep their key.
	if ti, _ := wr.Ticket(tok); len(ti.ClientKey) != maxClientKeyLen {
		t.Error("existing ticket lost its key after SetClientKeyFunc(nil)")
	}
}

func TestTicketInfo_HasPass(t *testing.T) {
	wr := newTestWR(t, 1)
	if err := wr.SetPassDuration(90 * time.Minute); err != nil {
		t.Fatal(err)
	}
	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	r := newTestRouter(wr, serving, release)
	defer close(release)

	fillOneSlot(t, r, serving)
	queueTokens(t, r, 2) // someone ahead, so the pass holder is not already first

	pass := wr.GrantPass()
	tok := arriveWith(r, nil, &http.Cookie{Name: passCookieName, Value: pass})
	ti, ok := wr.Ticket(tok)
	if !ok {
		t.Fatal("pass holder not queued")
	}
	if !ti.HasPass || !ti.Promoted {
		t.Errorf("expected HasPass and Promoted, got %+v", ti)
	}
}

// ── Event payloads ────────────────────────────────────────────────────────────

func TestEventQueue_CarriesTokenKeyAndStaleFlag(t *testing.T) {
	wr := newTestWR(t, 1)
	wr.SetClientKeyFunc(func(c *gin.Context) string { return c.GetHeader("X-Client") })
	events := collectEvents(wr, EventQueue)

	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	r := newTestRouter(wr, serving, release)
	defer close(release)

	fillOneSlot(t, r, serving)

	fresh := arriveWith(r, map[string]string{"X-Client": "a"})
	s := nextSnapshot(t, events)
	if s.Token != fresh || s.ClientKey != "a" || s.StaleTicket {
		t.Errorf("no-cookie arrival: got Token=%q Key=%q Stale=%v", s.Token, s.ClientKey, s.StaleTicket)
	}

	stale := arriveWith(r, map[string]string{"X-Client": "b"},
		&http.Cookie{Name: cookieName, Value: "deadbeefdeadbeefdeadbeefdeadbeef"})
	s = nextSnapshot(t, events)
	if s.Token != stale || s.ClientKey != "b" || !s.StaleTicket {
		t.Errorf("stale-cookie arrival: got Token=%q Key=%q Stale=%v", s.Token, s.ClientKey, s.StaleTicket)
	}
}

func TestEventRemove_CarriesTokenForSingleRemoval(t *testing.T) {
	wr := newTestWR(t, 1)
	wr.SetClientKeyFunc(func(c *gin.Context) string { return c.GetHeader("X-Client") })
	events := collectEvents(wr, EventRemove)

	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	r := newTestRouter(wr, serving, release)
	defer close(release)

	fillOneSlot(t, r, serving)
	tok := arriveWith(r, map[string]string{"X-Client": "k"})
	arriveWith(r, nil)

	if err := wr.RemoveToken(tok); err != nil {
		t.Fatal(err)
	}
	s := nextSnapshot(t, events)
	if s.Token != tok || s.ClientKey != "k" {
		t.Errorf("RemoveToken event: got Token=%q Key=%q", s.Token, s.ClientKey)
	}

	wr.RemoveTokensFunc(func(TicketInfo) bool { return true })
	s = nextSnapshot(t, events)
	if s.Token != "" || s.ClientKey != "" {
		t.Errorf("batch removal event must not carry a token, got %+v", s)
	}
}

// ── Concurrency ───────────────────────────────────────────────────────────────

func TestQueue_ConcurrentWithTraffic(t *testing.T) {
	wr := newTestWR(t, 1)
	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	r := newTestRouter(wr, serving, release)
	defer close(release)

	fillOneSlot(t, r, serving)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(4)
		go func() { defer wg.Done(); arriveWith(r, nil) }()
		go func() { defer wg.Done(); _ = wr.Queue(25) }()
		go func() {
			defer wg.Done()
			for _, ti := range wr.Queue(5) {
				pollStatus(r, ti.Token)
			}
		}()
		go func(i int) {
			defer wg.Done()
			if i%5 == 0 {
				wr.RemoveTokensFunc(func(ti TicketInfo) bool { return ti.Ticket%3 == 0 })
			}
		}(i)
	}
	wg.Wait()

	if q := wr.Queue(1000); int64(len(q)) != wr.LiveQueueDepth() {
		t.Errorf("Queue(all)=%d disagrees with LiveQueueDepth=%d", len(q), wr.LiveQueueDepth())
	}
}

// ── Benchmark ─────────────────────────────────────────────────────────────────

func BenchmarkQueue_100of100k(b *testing.B) {
	wr := &WaitingRoom{}
	if err := wr.Init(1); err != nil {
		b.Fatal(err)
	}
	defer wr.Stop()

	now := time.Now()
	for i := 0; i < 100_000; i++ {
		wr.tokens.set(fmt.Sprintf("tok-%d", i), ticketEntry{
			ticket:    int64(10 + i),
			createdAt: now,
			issuedAt:  now,
		})
	}
	wr.nextTicket.Store(100_010)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = wr.Queue(100)
	}
}

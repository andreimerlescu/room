package room

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// ── helpers ───────────────────────────────────────────────────────────────────

func exportRoom(t *testing.T, wr *WaitingRoom) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	if err := wr.Export(&buf); err != nil {
		t.Fatalf("Export: %v", err)
	}
	return &buf
}

func encodeFile(t *testing.T, f persistFile) *bytes.Buffer {
	t.Helper()
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.NewBuffer(b)
}

func within(a, b time.Time, d time.Duration) bool {
	diff := a.Sub(b)
	if diff < 0 {
		diff = -diff
	}
	return diff <= d
}

// ── round trip ────────────────────────────────────────────────────────────────

func TestExportImport_RoundTripPreservesLine(t *testing.T) {
	a := newTestWR(t, 1)
	a.SetClientKeyFunc(func(c *gin.Context) string { return c.GetHeader("X-Client") })
	if err := a.SetPassDuration(90 * time.Minute); err != nil {
		t.Fatal(err)
	}

	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	ra := newTestRouter(a, serving, release)
	defer close(release)

	fillOneSlot(t, ra, serving)
	toks := make([]string, 5)
	for i := range toks {
		toks[i] = arriveWith(ra, map[string]string{"X-Client": fmt.Sprintf("c%d", i)})
		if toks[i] == "" {
			t.Fatalf("arrival %d not queued", i)
		}
	}
	pollStatus(ra, toks[0])
	if err := a.AdminPromote(toks[4], 2); err != nil {
		t.Fatal(err)
	}
	pass := a.GrantPass()

	want := a.Queue(10)
	buf := exportRoom(t, a)

	b := newTestWR(t, 1)
	stats, err := b.Import(buf)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if stats.Restored != 5 || stats.DroppedStale != 0 || stats.PassesRestored != 1 {
		t.Errorf("unexpected stats %+v", stats)
	}

	got := b.Queue(10)
	if len(got) != len(want) {
		t.Fatalf("restored %d tickets, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Token != want[i].Token {
			t.Errorf("slot %d: order changed", i)
		}
		if got[i].ClientKey != want[i].ClientKey || got[i].Seen != want[i].Seen ||
			got[i].Promoted != want[i].Promoted {
			t.Errorf("slot %d: metadata changed:\n want %+v\n got  %+v", i, want[i], got[i])
		}
		if !within(got[i].IssuedAt, want[i].IssuedAt, time.Second) {
			t.Errorf("slot %d: IssuedAt drifted", i)
		}
		// Nobody is active after a restart: the head is ready, the rest
		// are at exact positions 1..4.
		if got[i].Position != int64(i) {
			t.Errorf("slot %d: expected position %d, got %d", i, i, got[i].Position)
		}
	}

	if !b.HasValidPass(pass) {
		t.Error("VIP pass was not restored")
	}
	if b.nowServing.Load() != 0 || b.ledger.len() != 0 || b.nextTicket.Load() != 5 {
		t.Errorf("unexpected counters: nowServing=%d ledger=%d nextTicket=%d",
			b.nowServing.Load(), b.ledger.len(), b.nextTicket.Load())
	}
	if d := b.QueueDepth(); d != 4 {
		t.Errorf("expected QueueDepth 4, got %d", d)
	}
}

// TestImport_ClientsResumeOverHTTP checks what the restored browsers see:
// the head is admitted on reload, the others keep their place and get
// their cookies re-sent on the first poll.
func TestImport_ClientsResumeOverHTTP(t *testing.T) {
	a := newTestWR(t, 1)
	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	ra := newTestRouter(a, serving, release)
	defer close(release)
	fillOneSlot(t, ra, serving)
	toks := queueTokens(t, ra, 3)
	buf := exportRoom(t, a)

	b := newTestWR(t, 1)
	if _, err := b.Import(buf); err != nil {
		t.Fatal(err)
	}
	rb := newTestRouter(b, nil, nil)

	w := pollStatusRaw(rb, toks[1])
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var resp statusResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Ready || resp.CookiesRequired || resp.Position != 1 {
		t.Errorf("restored client: expected waiting at position 1, got %+v", resp)
	}
	if c := setCookies(w); c[cookieName] == nil || c[cookieName].Value != toks[1] {
		t.Error("first poll after import should re-send the same room_ticket")
	}

	if !pollStatus(rb, toks[0]).Ready {
		t.Fatal("restored head should be ready")
	}
	aw, issued := serveWithCookie(rb, toks[0])
	if aw.Code != http.StatusOK || issued != "" {
		t.Errorf("restored head should be admitted on reload, got code=%d new token=%q", aw.Code, issued)
	}
	if _, ok := b.Ticket(toks[0]); ok {
		t.Error("admitted token should be consumed")
	}
}

func TestImport_CapacityChangeHandled(t *testing.T) {
	a := newTestWR(t, 1)
	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	ra := newTestRouter(a, serving, release)
	defer close(release)
	fillOneSlot(t, ra, serving)
	queueTokens(t, ra, 5)
	buf := exportRoom(t, a)

	b := newTestWR(t, 3)
	if _, err := b.Import(buf); err != nil {
		t.Fatal(err)
	}
	q := b.Queue(10)
	ready := 0
	for _, ti := range q {
		if ti.Position <= 0 {
			ready++
		}
	}
	if ready != 3 {
		t.Errorf("with cap=3 the first 3 restored clients should be ready, got %d", ready)
	}
	if q[3].Position != 1 || q[4].Position != 2 {
		t.Errorf("expected positions 1,2 for the rest, got %d,%d", q[3].Position, q[4].Position)
	}
}

func TestImport_ArrivalsQueueBehindRestored(t *testing.T) {
	a := newTestWR(t, 1)
	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	ra := newTestRouter(a, serving, release)
	defer close(release)
	fillOneSlot(t, ra, serving)
	queueTokens(t, ra, 5)
	buf := exportRoom(t, a)

	b := newTestWR(t, 1)
	if _, err := b.Import(buf); err != nil {
		t.Fatal(err)
	}
	rb := newTestRouter(b, nil, nil)
	tok := arriveWith(rb, nil)
	ti, ok := b.Ticket(tok)
	if !ok {
		t.Fatal("new arrival after import was not queued")
	}
	if ti.Position != 5 {
		t.Errorf("new arrival should queue behind the 5 restored clients, position=%d", ti.Position)
	}
}

func TestExportImport_EmptyRoom(t *testing.T) {
	t.Parallel()
	a := newTestWR(t, 1)
	buf := exportRoom(t, a)

	b := newTestWR(t, 1)
	stats, err := b.Import(buf)
	if err != nil {
		t.Fatal(err)
	}
	if stats != (ImportStats{}) {
		t.Errorf("expected zero stats, got %+v", stats)
	}
	if b.nextTicket.Load() != 0 {
		t.Error("empty import must not consume ticket numbers")
	}
}

// ── staleness and downtime ────────────────────────────────────────────────────

func TestImport_DropsTicketsDeadAtExport(t *testing.T) {
	t.Parallel()
	now := time.Now()
	f := persistFile{
		Version:    persistFormatVersion,
		ExportedAt: now,
		Tickets: []persistedTicket{
			{Token: "expired", CreatedAt: now.Add(-time.Hour), LastSeen: now.Add(-(DefaultTokenTTL + time.Minute)), Seen: true},
			{Token: "unseen-old", CreatedAt: now.Add(-20 * time.Second), LastSeen: now.Add(-20 * time.Second)},
			{Token: "seen-old", CreatedAt: now.Add(-20 * time.Second), LastSeen: now.Add(-5 * time.Second), Seen: true},
			{Token: "unseen-new", CreatedAt: now.Add(-2 * time.Second), LastSeen: now.Add(-2 * time.Second)},
		},
	}

	b := newTestWR(t, 1)
	if err := b.SetFirstPollGrace(10 * time.Second); err != nil {
		t.Fatal(err)
	}
	stats, err := b.Import(encodeFile(t, f))
	if err != nil {
		t.Fatal(err)
	}
	if stats.Restored != 2 || stats.DroppedStale != 2 {
		t.Errorf("expected 2 restored / 2 dropped, got %+v", stats)
	}
	for tok, want := range map[string]bool{"expired": false, "unseen-old": false, "seen-old": true, "unseen-new": true} {
		if _, ok := b.Ticket(tok); ok != want {
			t.Errorf("%s: present=%v, want %v", tok, ok, want)
		}
	}
}

// TestImport_DowntimeNotCharged: a client last seen 1 minute before a
// 10-minute outage is restored as last seen 1 minute ago, and survives a
// reap under the default 5-minute TTL.
func TestImport_DowntimeNotCharged(t *testing.T) {
	t.Parallel()
	exported := time.Now().Add(-10 * time.Minute)
	f := persistFile{
		Version:    persistFormatVersion,
		ExportedAt: exported,
		Tickets: []persistedTicket{{
			Token:     "tok",
			CreatedAt: exported.Add(-2 * time.Minute),
			LastSeen:  exported.Add(-time.Minute),
			Seen:      true,
		}},
	}

	b := newTestWR(t, 1)
	if _, err := b.Import(encodeFile(t, f)); err != nil {
		t.Fatal(err)
	}
	ti, ok := b.Ticket("tok")
	if !ok {
		t.Fatal("ticket not restored")
	}
	if !within(ti.LastSeen, time.Now().Add(-time.Minute), 2*time.Second) {
		t.Errorf("LastSeen should be shifted by the downtime, got %s ago", time.Since(ti.LastSeen))
	}
	b.reap()
	if _, ok := b.Ticket("tok"); !ok {
		t.Error("downtime was charged: the restored ticket was reaped")
	}
}

func TestImport_PassesKeepAbsoluteExpiry(t *testing.T) {
	t.Parallel()
	now := time.Now()
	f := persistFile{
		Version:    persistFormatVersion,
		ExportedAt: now.Add(-time.Minute),
		Passes: []persistedPass{
			{Token: "live", ExpiresAt: now.Add(time.Hour)},
			{Token: "lapsed", ExpiresAt: now.Add(-time.Second)},
		},
	}
	b := newTestWR(t, 1)
	stats, err := b.Import(encodeFile(t, f))
	if err != nil {
		t.Fatal(err)
	}
	if stats.PassesRestored != 1 || stats.PassesExpired != 1 {
		t.Errorf("unexpected pass stats %+v", stats)
	}
	if !b.HasValidPass("live") || b.HasValidPass("lapsed") {
		t.Error("pass restoration wrong")
	}
}

// ── refusals ──────────────────────────────────────────────────────────────────

func TestImport_RefusesUsedRoom(t *testing.T) {
	a := newTestWR(t, 1)
	buf := exportRoom(t, a)

	used := newTestWR(t, 1)
	r := newTestRouter(used, nil, nil)
	serveWithCookie(r, "") // fast-path admission issues ticket 1

	if _, err := used.Import(bytes.NewReader(buf.Bytes())); err == nil {
		t.Fatal("expected ErrImportNotEmpty")
	} else if _, ok := err.(ErrImportNotEmpty); !ok {
		t.Errorf("expected ErrImportNotEmpty, got %T: %v", err, err)
	}

	withPass := newTestWR(t, 1)
	_ = withPass.SetPassDuration(time.Hour)
	withPass.GrantPass()
	if _, err := withPass.Import(bytes.NewReader(buf.Bytes())); err == nil {
		t.Error("a room holding a pass should refuse import")
	}
}

func TestImport_InvalidInputLeavesRoomUntouched(t *testing.T) {
	t.Parallel()
	now := time.Now()
	cases := map[string]string{
		"garbage": "not json",
	}
	mk := func(f persistFile) string {
		b, _ := json.Marshal(f)
		return string(b)
	}
	cases["version"] = mk(persistFile{Version: 2, ExportedAt: now})
	cases["no-exported-at"] = mk(persistFile{Version: persistFormatVersion})
	cases["empty-token"] = mk(persistFile{Version: persistFormatVersion, ExportedAt: now,
		Tickets: []persistedTicket{{Token: "", LastSeen: now}}})
	cases["duplicate"] = mk(persistFile{Version: persistFormatVersion, ExportedAt: now,
		Tickets: []persistedTicket{{Token: "a", LastSeen: now}, {Token: "a", LastSeen: now}}})
	cases["long-token"] = mk(persistFile{Version: persistFormatVersion, ExportedAt: now,
		Tickets: []persistedTicket{{Token: strings.Repeat("x", maxPersistedTokenLen+1), LastSeen: now}}})

	for name, input := range cases {
		b := newTestWR(t, 1)
		_, err := b.Import(strings.NewReader(input))
		if _, ok := err.(ErrImportFormat); !ok {
			t.Errorf("%s: expected ErrImportFormat, got %T: %v", name, err, err)
		}
		if b.nextTicket.Load() != 0 || b.tokens.len() != 0 {
			t.Errorf("%s: room modified by a rejected import", name)
		}
	}
}

func TestExportImport_Uninitialised(t *testing.T) {
	t.Parallel()
	var zero WaitingRoom
	if _, ok := zero.Export(&bytes.Buffer{}).(ErrNotInitialised); !ok {
		t.Error("Export on zero room should return ErrNotInitialised")
	}
	if _, err := zero.Import(strings.NewReader("{}")); err == nil {
		t.Error("Import on zero room should fail")
	} else if _, ok := err.(ErrNotInitialised); !ok {
		t.Errorf("expected ErrNotInitialised, got %T", err)
	}
}

// ── concurrency ───────────────────────────────────────────────────────────────

func TestExport_ConcurrentWithTraffic(t *testing.T) {
	a := newTestWR(t, 1)
	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	ra := newTestRouter(a, serving, release)
	defer close(release)
	fillOneSlot(t, ra, serving)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(3)
		go func() { defer wg.Done(); arriveWith(ra, nil) }()
		go func() {
			defer wg.Done()
			var buf bytes.Buffer
			if err := a.Export(&buf); err != nil {
				t.Errorf("Export: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			for _, ti := range a.Queue(3) {
				pollStatus(ra, ti.Token)
			}
		}()
	}
	wg.Wait()

	b := newTestWR(t, 1)
	stats, err := b.Import(exportRoom(t, a))
	if err != nil {
		t.Fatal(err)
	}
	if int64(stats.Restored) != a.LiveQueueDepth() {
		t.Errorf("restored %d, source had %d live tickets", stats.Restored, a.LiveQueueDepth())
	}
}

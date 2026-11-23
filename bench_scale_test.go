package room

import (
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// benchRoomWithN builds a cap=1 room populated with n queued tokens (mix of
// keyed and cookie tokens) positioned well past the serving window.
func benchRoomWithN(b *testing.B, n int, keyed bool) (*WaitingRoom, *gin.Engine, []string, []string) {
	b.Helper()
	wr := &WaitingRoom{}
	if err := wr.Init(1); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(wr.Stop)

	r := gin.New()
	wr.RegisterRoutes(r)
	r.GET("/", func(c *gin.Context) { c.Status(http.StatusOK) })

	now := time.Now()
	wr.nextTicket.Store(int64(n + 1000))
	tokens := make([]string, n)
	ids := make([]string, n)

	wr.tokens.mu.Lock()
	for i := range n {
		tok := fmt.Sprintf("bench-tok-%06d", i)
		e := ticketEntry{
			ticket:      int64(1000 + i),
			createdAt:   now.Add(time.Duration(i) * time.Nanosecond),
			issuedAt:    now,
			cookieSetAt: now,
			seen:        true,
		}
		if keyed {
			e.key = fmt.Sprintf("bench-key-%06d", i)
			e.id = fmt.Sprintf("bench-id-%06d", i)
			ids[i] = e.id
		}
		wr.tokens.putLocked(tok, e)
		tokens[i] = tok
	}
	wr.tokens.mu.Unlock()
	return wr, r, tokens, ids
}

// BenchmarkEnter_ExistingKey_100k measures Enter(key) for an existing key in
// a 100,000-client queue (previously O(N) under ts.mu.Lock; now O(log N) under
// ts.mu.RLock + node.mu).
func BenchmarkEnter_ExistingKey_100k(b *testing.B) {
	const n = 100_000
	wr, _, _, _ := benchRoomWithN(b, n, true)
	keys := make([]string, 1024)
	for i := range keys {
		keys[i] = fmt.Sprintf("bench-key-%06d", (i*97)%n)
	}

	var idx atomic.Int64
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			k := keys[idx.Add(1)%int64(len(keys))]
			if _, _, err := wr.Enter(k); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkView_TicketStatus_100k measures tokenStore.view (backing
// GET /queue/status/{id}) in a 100,000-client queue (previously O(N) under
// ts.mu.Lock; now O(log N) under ts.mu.RLock + node.mu).
func BenchmarkView_TicketStatus_100k(b *testing.B) {
	const n = 100_000
	wr, _, _, ids := benchRoomWithN(b, n, true)

	var idx atomic.Int64
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			id := ids[idx.Add(1)%n]
			_, _ = wr.tokens.view(id, time.Now(), 0)
		}
	})
}

// BenchmarkSetTicketRank_100k measures SetTicketRank jumping to the front of a
// 100,000-client queue (previously 2× O(N) full-map scans under ts.mu.Lock;
// now O(log N) treap split + lazy range-add + merge).
func BenchmarkSetTicketRank_100k(b *testing.B) {
	const n = 100_000
	wr, _, tokens, _ := benchRoomWithN(b, n, false)

	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		tok := tokens[n-1-(i%n)]
		rank := (i / n) + 1
		if _, err := wr.SetTicketRank(tok, rank); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkReap_NoExpired_100k measures wr.reap() when 100,000 live tokens are
// waiting and none are expired (previously O(N) full-map scan under RLock;
// now O(1) heap peek).
func BenchmarkReap_NoExpired_100k(b *testing.B) {
	const n = 100_000
	wr, _, _, _ := benchRoomWithN(b, n, false)

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		wr.reap()
	}
}

// BenchmarkStatusPoll_Parallel_100k measures parallel tokenStore.poll across
// 100,000 queued tokens under RLock + node.mu (previously serialized on
// ts.mu.Lock).
func BenchmarkStatusPoll_Parallel_100k(b *testing.B) {
	const n = 100_000
	wr, _, tokens, _ := benchRoomWithN(b, n, false)

	var idx atomic.Int64
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			tok := tokens[idx.Add(1)%n]
			wr.tokens.poll(tok, time.Now(), 0, statusPollMinInterval, time.Minute)
		}
	})
}

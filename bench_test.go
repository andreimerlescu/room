package room

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func benchRoom(b *testing.B) (*WaitingRoom, *gin.Engine) {
	b.Helper()
	wr := &WaitingRoom{}
	if err := wr.Init(1); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(wr.Stop)

	r := gin.New()
	wr.RegisterRoutes(r)
	r.GET("/", func(c *gin.Context) { c.Status(http.StatusOK) })

	wr.nextTicket.Store(1_000_000)
	return wr, r
}

func issueTokens(b *testing.B, r *gin.Engine, n int) []string {
	b.Helper()
	out := make([]string, n)
	for i := range out {
		_, tok := serveWithCookie(r, "")
		if tok == "" {
			b.Fatalf("issue %d: request was not queued", i)
		}
		out[i] = tok
	}
	return out
}

func pollOnce(r *gin.Engine, token string) {
	req := httptest.NewRequest(http.MethodGet, "/queue/status", nil)
	req.AddCookie(&http.Cookie{Name: cookieName, Value: token})
	r.ServeHTTP(httptest.NewRecorder(), req)
}

func benchStore(n int) (*tokenStore, []string) {
	ts := newTokenStore()
	tokens := make([]string, n)
	now := time.Now()
	for i := range tokens {
		tokens[i] = fmt.Sprintf("bench-%d", i)
		ts.set(tokens[i], ticketEntry{
			ticket:      int64(1_000_000 + i),
			issuedAt:    now,
			cookieSetAt: now,
		})
	}
	return ts, tokens
}

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

func BenchmarkStatusPoll_Accepted(b *testing.B) {
	_, r := benchRoom(b)
	tokens := issueTokens(b, r, b.N)

	var next atomic.Int64
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			i := next.Add(1) - 1
			pollOnce(r, tokens[i])
		}
	})
}

func BenchmarkStatusPoll_RateLimited(b *testing.B) {
	_, r := benchRoom(b)
	const pool = 64
	tokens := issueTokens(b, r, pool)

	var next atomic.Int64
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		tok := tokens[next.Add(1)%pool]
		for pb.Next() {
			pollOnce(r, tok)
		}
	})
}

func BenchmarkResumeRender(b *testing.B) {
	_, r := benchRoom(b)
	const pool = 1024
	tokens := issueTokens(b, r, pool)

	var next atomic.Int64
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			i := next.Add(1) % pool
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.AddCookie(&http.Cookie{Name: cookieName, Value: tokens[i]})
			r.ServeHTTP(httptest.NewRecorder(), req)
		}
	})
}

func BenchmarkSlowPathIssue(b *testing.B) {
	_, r := benchRoom(b)

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
		}
	})
}

func BenchmarkTokenStore_PollSingleLock(b *testing.B) {
	ts, tokens := benchStore(4096)
	n := int64(len(tokens))
	var next atomic.Int64

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			tok := tokens[next.Add(1)%n]
			ts.poll(tok, time.Now(), 0, statusPollMinInterval, time.Minute)
		}
	})
}

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

func BenchmarkReap_NoExpired_100k(b *testing.B) {
	const n = 100_000
	wr, _, _, _ := benchRoomWithN(b, n, false)

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		wr.reap()
	}
}

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

func BenchmarkAcquireRelease(b *testing.B) {
	wr := &WaitingRoom{}
	if err := wr.Init(64); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(wr.Stop)

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		ok, _, err := wr.Acquire("bench-lease")
		if err != nil || !ok {
			b.Fatalf("Acquire: ok=%v err=%v", ok, err)
		}
		if err := wr.Release("bench-lease"); err != nil {
			b.Fatalf("Release: %v", err)
		}
	}
}

func BenchmarkAcquire_Renew(b *testing.B) {
	wr := &WaitingRoom{}
	if err := wr.Init(64); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(wr.Stop)

	if ok, _, err := wr.Acquire("bench-lease"); err != nil || !ok {
		b.Fatalf("setup Acquire: ok=%v err=%v", ok, err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		ok, _, err := wr.Acquire("bench-lease")
		if err != nil || !ok {
			b.Fatalf("renew Acquire: ok=%v err=%v", ok, err)
		}
	}
}

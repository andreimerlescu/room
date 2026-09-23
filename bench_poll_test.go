package room

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
)

// These benchmarks measure the request hot paths through the full gin
// stack, so absolute numbers include httptest request/recorder overhead.
// That overhead is identical before and after a change, so deltas are
// meaningful. They deliberately use only APIs that existed in v1.2.1 plus
// Part 2, so the same file can produce a "before" baseline.

// benchRoom builds a cap=1 room whose ticket counter is far past the
// serving window, so every new arrival is queued without needing an
// active request to hold the slot.
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

// issueTokens queues n real clients through the middleware and returns
// their room_ticket values.
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

// BenchmarkStatusPoll_Accepted measures the non-rate-limited poll path:
// every iteration polls a distinct, freshly issued token exactly once.
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

// BenchmarkStatusPoll_RateLimited measures the 429 path: a small set of
// tokens polled back-to-back, as an over-eager or custom page would.
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

// BenchmarkResumeRender measures a queued client reloading the waiting
// room page (the resume path, which renders HTML and sets cookies).
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

// BenchmarkSlowPathIssue measures a new arrival being queued: ticket,
// token generation, token-store insert, cookies and HTML render.
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

// ensure fmt stays referenced if the helpers above change.
var _ = fmt.Sprintf

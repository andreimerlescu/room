package room

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// These benchmarks isolate the token-store work done per /queue/status
// poll, with no HTTP stack, so the lock-level change is visible on its
// own. The "V121" variant replays the exact call sequence the v1.2.1
// status handler made — four separate lock acquisitions, three of them
// write locks — using methods that still exist in types.go. Both run in
// the same tree and the same process, so no historical baseline is needed.

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

// BenchmarkTokenStore_PollSequenceV121 replays the v1.2.1 per-poll sequence:
// deleteIfExpired, get, touchLastPoll, touchIssuedAt.
func BenchmarkTokenStore_PollSequenceV121(b *testing.B) {
	ts, tokens := benchStore(4096)
	n := int64(len(tokens))
	var next atomic.Int64

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			tok := tokens[next.Add(1)%n]
			ts.deleteIfExpired(tok)
			ts.get(tok)
			ts.touchLastPoll(tok)
			ts.touchIssuedAt(tok)
		}
	})
}

// BenchmarkTokenStore_PollSingleLock runs the same work through poll,
// which takes the write lock once.
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

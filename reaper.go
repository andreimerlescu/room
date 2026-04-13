package room

import (
	"context"
	"time"
)

// SetReaperInterval changes the interval between eviction passes at runtime.
// The reaper goroutine picks up the new interval immediately without
// restarting the WaitingRoom.
//
// Valid range: 5s – 24h. Values outside this range return ErrReaperInterval.
//
// Tightening the interval (e.g. to 30s) reduces the window in which a
// ghost ticket can stall the queue, at the cost of more frequent lock
// contention on the token store. Loosening it (e.g. to 30m) is appropriate
// when queue depth is low and memory pressure is not a concern.
//
// Usage:
//
//	if err := wr.SetReaperInterval(30 * time.Second); err != nil {
//	    log.Fatal(err)
//	}
//
// Related: WaitingRoom.ReaperInterval, WaitingRoom.startReaper
func (wr *WaitingRoom) SetReaperInterval(d time.Duration) error {
	if d < reaperMinInterval || d > reaperMaxInterval {
		return ErrReaperInterval{Given: d, Min: reaperMinInterval, Max: reaperMaxInterval}
	}

	wr.reaperInterval.Store(int64(d))

	// Non-blocking send — if a restart signal is already pending,
	// the reaper will pick up the new interval on that pass anyway.
	select {
	case wr.reaperRestart <- struct{}{}:
	default:
	}

	return nil
}

// ReaperInterval returns the current eviction pass interval.
//
// Related: WaitingRoom.SetReaperInterval
func (wr *WaitingRoom) ReaperInterval() time.Duration {
	return time.Duration(wr.reaperInterval.Load())
}

// startReaper launches the background eviction goroutine. It responds to
// three signals:
//
//   - ctx.Done()      → clean shutdown via Stop
//   - reaperRestart   → interval changed via SetReaperInterval
//   - ticker.C        → scheduled eviction pass
//
// Related: WaitingRoom.Stop, WaitingRoom.SetReaperInterval
func (wr *WaitingRoom) startReaper(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(time.Duration(wr.reaperInterval.Load()))
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return

			case <-wr.reaperRestart:
				ticker.Stop()
				ticker = time.NewTicker(time.Duration(wr.reaperInterval.Load()))

			case <-ticker.C:
				wr.reap()
			}
		}
	}()
}

// reap performs a single eviction pass over the token store.
// Expired tokens are collected under a read lock, then deleted under
// a write lock. The double-check on TTL inside the write lock guards
// against a token being legitimately deleted between the two passes.
//
// For each evicted token, nowServing is advanced and waiters are
// broadcast so the FIFO queue does not stall behind a ghost ticket.
//
// Related: WaitingRoom.startReaper, WaitingRoom.SetReaperInterval
func (wr *WaitingRoom) reap() {
	now := time.Now()

	// Collect expired tokens under read lock.
	wr.tokens.mu.RLock()
	expired := make([]string, 0, min(len(wr.tokens.entries), reaperBatchSize))
	for token, entry := range wr.tokens.entries {
		if now.Sub(entry.issuedAt) > cookieTTL {
			expired = append(expired, token)
		}
		if len(expired) >= reaperBatchSize {
			break
		}
	}
	wr.tokens.mu.RUnlock()

	if len(expired) == 0 {
		return
	}

	// Evict under write lock with double-check.
	wr.tokens.mu.Lock()
	for _, token := range expired {
		if entry, ok := wr.tokens.entries[token]; ok {
			if now.Sub(entry.issuedAt) > cookieTTL {
				delete(wr.tokens.entries, token)
				wr.nowServing.Add(1)
				wr.cond.Broadcast()
			}
		}
	}
	wr.tokens.mu.Unlock()
}


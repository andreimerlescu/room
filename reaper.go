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
// Expired tokens are collected under the token store read lock, then
// deleted under the token store write lock. nowServing is advanced and
// cond is broadcast under wr.mu so waiters cannot miss the wakeup.
//
// NOTE: evicted tickets may not be contiguous. Advancing nowServing
// by the total eviction count can admit later tickets slightly out
// of strict FIFO order when ghost tickets are non-adjacent. This is
// an accepted trade-off documented in the WaitingRoom type comment;
// a gap-tracking structure could tighten this in a future release.
//
// Related: WaitingRoom.startReaper, WaitingRoom.SetReaperInterval
func (wr *WaitingRoom) reap() {
	now := time.Now()

	// Collect expired tokens under token store read lock.
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

	// Evict under token store write lock with double-check.
	// Count evictions separately so we only take wr.mu once.
	var evicted int64
	wr.tokens.mu.Lock()
	for _, token := range expired {
		if entry, ok := wr.tokens.entries[token]; ok {
			// Re-check under write lock: the entry may have been
			// deleted or updated between the read-lock scan and now.
			if now.Sub(entry.issuedAt) > cookieTTL {
				delete(wr.tokens.entries, token)
				evicted++
			}
		}
	}
	wr.tokens.mu.Unlock()

	if evicted == 0 {
		return
	}

	// Advance nowServing and wake waiters under wr.mu so no wakeup
	// can be missed between a waiter checking the condition and
	// calling cond.Wait().
	wr.mu.Lock()
	wr.nowServing.Add(evicted)
	wr.cond.Broadcast()
	wr.mu.Unlock()
}

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

// reap performs a full eviction cycle over the token store and the pass
// store. It loops over batch-sized scans until all expired tokens have
// been removed, then sweeps the pass store for expired VIP passes.
//
// Only tokens whose ticket number is OUTSIDE the current serving window
// (i.e. ticket > nowServing + cap) are counted toward nowServing advances.
// Tickets inside the serving window already have an allocated semaphore
// slot conceptually; advancing nowServing for them would inflate the window
// beyond the configured capacity and allow more concurrent requests than cap.
//
// Because active pollers have their issuedAt refreshed on each
// /queue/status call, only genuinely abandoned (ghost) clients will be
// reaped under normal operation.
//
// Related: WaitingRoom.startReaper, WaitingRoom.SetReaperInterval
func (wr *WaitingRoom) reap() {
	for {
		evictedCount := wr.reapBatch()
		if evictedCount < reaperBatchSize {
			break
		}
	}

	// Sweep expired VIP passes. This is cheap — passes are typically
	// few (one per paying customer) and the sweep is a single lock
	// acquisition with a linear scan.
	wr.passes.reap()
}

// reapBatch performs a single bounded eviction pass. It returns the number
// of tokens that were expired in the scan phase (before double-check).
// The caller uses this to decide whether another pass is needed.
func (wr *WaitingRoom) reapBatch() int {
	now := time.Now()

	// Collect expired tokens under token store read lock.
	wr.tokens.mu.RLock()
	type expiredEntry struct {
		token  string
		ticket int64
	}
	expired := make([]expiredEntry, 0, min(len(wr.tokens.entries), reaperBatchSize))
	for token, entry := range wr.tokens.entries {
		if now.Sub(entry.issuedAt) > cookieTTL {
			expired = append(expired, expiredEntry{token: token, ticket: entry.ticket})
		}
		if len(expired) >= reaperBatchSize {
			break
		}
	}
	wr.tokens.mu.RUnlock()

	if len(expired) == 0 {
		return 0
	}

	scanned := len(expired)

	// Evict under token store write lock with double-check.
	// Count only tickets that were genuinely blocking the queue
	// (outside the serving window) so we don't inflate nowServing
	// beyond the configured capacity.
	nowServing := wr.nowServing.Load()
	cap := int64(wr.cap.Load())

	var evicted int64
	wr.tokens.mu.Lock()
	for _, e := range expired {
		if entry, ok := wr.tokens.entries[e.token]; ok {
			// Re-check expiry under write lock to close the TOCTOU
			// window between the read-lock scan and now.
			if now.Sub(entry.issuedAt) > cookieTTL {
				delete(wr.tokens.entries, e.token)
				// Only advance nowServing for tickets that were outside
				// the serving window. Tickets inside the window already
				// consumed a semaphore slot allocation; advancing for
				// them would double-count capacity.
				if entry.ticket > nowServing+cap {
					evicted++
				}
			}
		}
	}
	wr.tokens.mu.Unlock()

	if evicted > 0 {
		// Advance nowServing atomically. No mutex or broadcast needed:
		// admission is poll-driven. The next /queue/status poll from a
		// waiting client will see the updated nowServing and return
		// ready=true if their ticket is now within the window.
		wr.nowServing.Add(evicted)
		wr.emit(EventEvict, wr.snapshot(EventEvict))
	}

	return scanned
}

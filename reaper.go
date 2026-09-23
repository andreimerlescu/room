package room

import (
	"context"
	"time"
)

// SetReaperInterval changes the interval between eviction passes at runtime.
// The reaper goroutine picks up the new interval immediately without
// restarting the WaitingRoom.
//
// Passing 0 restores DefaultReaperInterval. Otherwise the valid range is
// 5s – 24h; values outside it (including negative values) return
// ErrReaperInterval.
//
// Tightening the interval (e.g. to 30s) reduces the window in which a
// ghost ticket inflates QueueDepth and the positions shown to clients
// behind it, at the cost of more frequent lock contention on the token
// store. Loosening it (e.g. to 30m) is appropriate when queue depth is
// low and memory pressure is not a concern.
//
// The reaper interval and the token TTL work together: the TTL decides
// when a token becomes eligible for eviction, the interval decides how
// soon after that it is actually removed. Worst-case ghost residency is
// roughly TTL + interval. Tickets the application already knows are dead
// can be released immediately with RemoveToken instead.
//
// When SetFirstPollGrace is enabled with a shorter duration, the reaper
// actually runs at that shorter cadence; ReaperInterval still reports the
// value configured here.
//
// Usage:
//
//	if err := wr.SetReaperInterval(30 * time.Second); err != nil {
//	    log.Fatal(err)
//	}
//
// Related: WaitingRoom.ReaperInterval, DefaultReaperInterval, WaitingRoom.SetTokenTTL, WaitingRoom.SetFirstPollGrace
func (wr *WaitingRoom) SetReaperInterval(d time.Duration) error {
	if d == 0 {
		d = DefaultReaperInterval
	}
	if d < reaperMinInterval || d > reaperMaxInterval {
		return ErrReaperInterval{Given: d, Min: reaperMinInterval, Max: reaperMaxInterval}
	}

	wr.reaperInterval.Store(int64(d))
	wr.signalReaperRestart()
	return nil
}

// ReaperInterval returns the configured eviction pass interval. The
// reaper may run more often than this if SetFirstPollGrace is enabled
// with a shorter duration.
//
// Related: WaitingRoom.SetReaperInterval
func (wr *WaitingRoom) ReaperInterval() time.Duration {
	return time.Duration(wr.reaperInterval.Load())
}

// SetFirstPollGrace sets how long a freshly issued ticket may go without
// its client ever contacting the server again — no status poll (including
// a rate-limited one) and no reload of the waiting page — before the
// reaper treats it as abandoned. Once a client has made any contact, only
// the ordinary sliding TokenTTL applies to it.
//
// # Why
//
// Most ghost tickets are never seen twice: cookieless clients (their
// polls carry no room_ticket, so the ticket is never touched), bots and
// crawlers that do not run the page's JavaScript, and visitors who close
// the tab within seconds. Under the TTL alone each of these occupies a
// token-store entry, inflates QueueDepth, positions, surge pricing and
// the SetMaxQueueDepth breaker for TokenTTL + reaper interval — ten
// minutes by default. A grace of 30s reclaims them in under a minute.
//
// # Reaper cadence
//
// While a grace is set, the reaper runs every min(ReaperInterval, grace),
// so a never-seen ticket is reclaimed within roughly 2×grace of issuance.
// ReaperInterval still reports the configured interval.
//
// # Trade-off
//
// A visitor who backgrounds the tab before its first poll (about 3s after
// the page loads) on a platform that suspends background tabs, such as
// iOS Safari, will not be seen and loses its place after the grace
// instead of after the TTL. Keep the grace comfortably above the page's
// first-poll delay; 30s is a good default for live events.
//
// # Values
//
// 0 disables the check (the default). Otherwise the valid range is
// 10s – 24h; values outside it, including negative values, return
// ErrFirstPollGrace. Values at or above TokenTTL are accepted but have no
// effect, since the TTL reaps the token first.
//
// Safe to call at any time.
//
// Related: WaitingRoom.FirstPollGrace, WaitingRoom.SetTokenTTL, WaitingRoom.SetReaperInterval
func (wr *WaitingRoom) SetFirstPollGrace(d time.Duration) error {
	if d != 0 && (d < firstPollGraceMin || d > firstPollGraceMax) {
		return ErrFirstPollGrace{Given: d, Min: firstPollGraceMin, Max: firstPollGraceMax}
	}
	wr.firstPollGrace.Store(int64(d))
	wr.signalReaperRestart()
	return nil
}

// FirstPollGrace returns the current first-poll grace. Zero means
// disabled.
//
// Related: WaitingRoom.SetFirstPollGrace
func (wr *WaitingRoom) FirstPollGrace() time.Duration {
	return time.Duration(wr.firstPollGrace.Load())
}

// effectiveReaperInterval is the cadence the reaper actually runs at:
// the configured interval, shortened to the first-poll grace when that is
// enabled and shorter.
func (wr *WaitingRoom) effectiveReaperInterval() time.Duration {
	d := time.Duration(wr.reaperInterval.Load())
	if g := time.Duration(wr.firstPollGrace.Load()); g > 0 && g < d {
		d = g
	}
	return d
}

// signalReaperRestart asks the reaper goroutine to pick up a new cadence.
// Non-blocking: if a restart signal is already pending, the reaper will
// read the current settings when it handles that one.
func (wr *WaitingRoom) signalReaperRestart() {
	select {
	case wr.reaperRestart <- struct{}{}:
	default:
	}
}

// startReaper launches the background eviction goroutine. It responds to
// three signals:
//
//   - ctx.Done()      → clean shutdown via Stop
//   - reaperRestart   → cadence changed via SetReaperInterval or
//     SetFirstPollGrace
//   - ticker.C        → scheduled eviction pass
//
// Related: WaitingRoom.Stop, WaitingRoom.SetReaperInterval, WaitingRoom.SetFirstPollGrace
func (wr *WaitingRoom) startReaper(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(wr.effectiveReaperInterval())
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return

			case <-wr.reaperRestart:
				ticker.Stop()
				ticker = time.NewTicker(wr.effectiveReaperInterval())

			case <-ticker.C:
				wr.reap()
			}
		}
	}()
}

// reap performs a full eviction cycle over the token store and the pass
// store. It loops over batch-sized scans until all dead tokens have been
// removed, then sweeps the pass store for expired VIP passes.
//
// A token is dead if its sliding TTL has elapsed, or — when
// SetFirstPollGrace is enabled — if its client has never been seen since
// issuance and the grace has elapsed.
//
// Every evicted token's ticket is retired, regardless of where it sits
// relative to the serving window:
//
//   - A ticket already inside the window (a client told ready that never
//     reloaded) is skipped immediately, restoring the slot it occupied.
//   - A ticket still behind the window goes into the ghost ledger and is
//     skipped when the window reaches it; positions behind it improve at
//     once, and nobody ahead of it is admitted early.
//
// An earlier version advanced nowServing only for out-of-window tickets,
// on the theory that in-window tickets had already consumed a slot. They
// had not — a token in the store has by construction not been admitted —
// so each in-window ghost permanently removed a slot, and with cap=1 a
// single one froze the queue.
//
// The scan holds the token store's read lock while it walks the map, which
// blocks status polls (they need the write lock) for the duration of the
// walk. At very large queue depths, keep the reaper cadence in mind.
//
// Related: WaitingRoom.startReaper, WaitingRoom.SetReaperInterval, WaitingRoom.retire
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
// of tokens that were dead in the scan phase (before double-check). The
// caller uses this to decide whether another pass is needed.
func (wr *WaitingRoom) reapBatch() int {
	now := time.Now()

	// Snapshot the TTL and grace once so the scan phase and the
	// double-check phase agree even if a setter is called mid-pass.
	ttl := wr.tokens.ttl()
	grace := time.Duration(wr.firstPollGrace.Load())

	dead := func(e ticketEntry) bool {
		age := now.Sub(e.issuedAt)
		if age > ttl {
			return true
		}
		// issuedAt is only ever advanced by client contact, which also
		// sets seen; for an unseen token it is still the issuance time.
		return grace > 0 && !e.seen && age > grace
	}

	// Collect dead tokens under token store read lock.
	wr.tokens.mu.RLock()
	expired := make([]string, 0, min(len(wr.tokens.entries), reaperBatchSize))
	for token, entry := range wr.tokens.entries {
		if dead(entry) {
			expired = append(expired, token)
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

	// Evict under token store write lock with double-check. Whatever we
	// delete here is ours alone — admission claims tokens with take under
	// the same lock — so we retire exactly those tickets.
	retired := make([]int64, 0, scanned)
	wr.tokens.mu.Lock()
	for _, token := range expired {
		if entry, ok := wr.tokens.entries[token]; ok {
			// Re-check under the write lock to close the TOCTOU window
			// between the read-lock scan and now: a poll in between
			// refreshes issuedAt and marks the token seen.
			if dead(entry) {
				delete(wr.tokens.entries, token)
				retired = append(retired, entry.ticket)
			}
		}
	}
	wr.tokens.mu.Unlock()

	if len(retired) > 0 {
		wr.retireMany(retired)
		wr.emit(EventEvict, wr.snapshot(EventEvict))
	}

	return scanned
}

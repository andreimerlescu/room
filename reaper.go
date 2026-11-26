package room

import (
	"context"
	"time"
)

// SetReaperInterval changes the eviction cadence at runtime. Passing 0
// restores [DefaultReaperInterval]; otherwise valid values are 5s–24h
// (returns [ErrReaperInterval] outside that range). When
// [WaitingRoom.SetFirstPollGrace], [WaitingRoom.SetIdlePollTTL], or
// [WaitingRoom.SetReadyGrace] requires a shorter cadence, the reaper ticks at
// that shorter cadence while ReaperInterval still reports the value set here.
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

// ReaperInterval returns the configured eviction pass interval.
func (wr *WaitingRoom) ReaperInterval() time.Duration {
	return time.Duration(wr.reaperInterval.Load())
}

// SetFirstPollGrace sets how long a newly issued ticket may go without any
// client contact (status poll or reload) before being reclaimed. Passing 0
// disables the check (default); otherwise valid values are 10s–24h (returns
// [ErrFirstPollGrace] outside that range). Safe to call at any time.
func (wr *WaitingRoom) SetFirstPollGrace(d time.Duration) error {
	if d != 0 && (d < firstPollGraceMin || d > firstPollGraceMax) {
		return ErrFirstPollGrace{Given: d, Min: firstPollGraceMin, Max: firstPollGraceMax}
	}
	wr.firstPollGrace.Store(int64(d))
	wr.signalReaperRestart()
	return nil
}

// FirstPollGrace returns the current first-poll grace (0 means disabled).
func (wr *WaitingRoom) FirstPollGrace() time.Duration {
	return time.Duration(wr.firstPollGrace.Load())
}

// SetReadyGrace sets how long a ticket inside the serving window may remain
// unclaimed before being reclaimed. Passing 0 disables the check (default);
// otherwise valid values are 5s–10m (returns [ErrReadyGrace] outside that
// range). Applies to cookie tickets and Middleware-queued keyed tickets;
// programmatic Enter/Acquire tickets are skipped. When enabled, the background
// reaper clamps its cadence to max(1s, d/2). Safe to call at any time.
func (wr *WaitingRoom) SetReadyGrace(d time.Duration) error {
	if d != 0 && (d < readyGraceMin || d > readyGraceMax) {
		return ErrReadyGrace{Given: d, Min: readyGraceMin, Max: readyGraceMax}
	}
	wr.readyGrace.Store(int64(d))
	wr.signalReaperRestart()
	return nil
}

// ReadyGrace returns the current ready-claim deadline (0 means disabled).
func (wr *WaitingRoom) ReadyGrace() time.Duration {
	return time.Duration(wr.readyGrace.Load())
}

// SetIdlePollTTL sets an independent cap on how long a queued ticket may go
// without client contact before eviction, without shortening TokenTTL's cookie
// MaxAge or refresh cadence. Passing 0 disables the cap (default); otherwise
// valid values are 10s–24h (returns [ErrIdlePollTTL] outside that range). When
// shorter than ReaperInterval, the background reaper clamps its cadence to d.
// Safe to call at any time.
func (wr *WaitingRoom) SetIdlePollTTL(d time.Duration) error {
	if d != 0 && (d < idlePollTTLMin || d > idlePollTTLMax) {
		return ErrIdlePollTTL{Given: d, Min: idlePollTTLMin, Max: idlePollTTLMax}
	}
	wr.idlePollTTL.Store(int64(d))
	wr.signalReaperRestart()
	return nil
}

// IdlePollTTL returns the current independent idle-poll TTL (0 means disabled).
func (wr *WaitingRoom) IdlePollTTL() time.Duration {
	return time.Duration(wr.idlePollTTL.Load())
}

func (wr *WaitingRoom) effectiveReaperInterval() time.Duration {
	d := time.Duration(wr.reaperInterval.Load())
	if g := time.Duration(wr.firstPollGrace.Load()); g > 0 && g < d {
		d = g
	}
	if idle := time.Duration(wr.idlePollTTL.Load()); idle > 0 && idle < d {
		d = idle
	}
	if rg := time.Duration(wr.readyGrace.Load()); rg > 0 {
		half := max(readyGraceReaperFloor, rg/2)
		if half < d {
			d = half
		}
	}
	return d
}

func (wr *WaitingRoom) signalReaperRestart() {
	select {
	case wr.reaperRestart <- struct{}{}:
	default:
	}
}

func (wr *WaitingRoom) startReaper(ctx context.Context) {
	restart := wr.reaperRestart
	done := make(chan struct{})
	wr.reaperDone = done
	go func() {
		defer close(done)
		ticker := time.NewTicker(wr.effectiveReaperInterval())
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-restart:
				ticker.Stop()
				ticker = time.NewTicker(wr.effectiveReaperInterval())
			case <-ticker.C:
				wr.reap()
			}
		}
	}()
}

func (wr *WaitingRoom) haltReaper() {
	if wr.stopReaper != nil {
		wr.stopReaper()
	}
	if wr.reaperDone != nil {
		<-wr.reaperDone
	}
}

// reap evicts expired tokens in bounded batches via the store's intrusive
// lazy min-heaps, compacts post-spike maps/heaps if needed, sweeps expired
// VIP passes, and releases expired leases.
func (wr *WaitingRoom) reap() {
	for {
		if wr.reapBatch() < reaperBatchSize {
			break
		}
	}
	wr.tokens.compactIfNeeded()
	wr.passes.reap()
	wr.reapLeases(wr.now())
}

// reapBatch inspects up to reaperBatchSize due entries across unseenHeap,
// ttlHeap, and (when ReadyGrace is enabled) the serving-window prefix of tree
// (O(1) when nothing is due, O(k log N) otherwise).
func (wr *WaitingRoom) reapBatch() int {
	now := wr.now()
	ttl := wr.tokens.ttl()
	effTTL := ttl
	if idleTTL := time.Duration(wr.idlePollTTL.Load()); idleTTL > 0 && idleTTL < effTTL {
		effTTL = idleTTL
	}
	grace := time.Duration(wr.firstPollGrace.Load())
	readyGrace := time.Duration(wr.readyGrace.Load())

	ts := wr.tokens
	ts.mu.Lock()

	var retired []int64
	work := 0

	if grace > 0 {
		for ts.unseenHeap.len() > 0 && work < reaperBatchSize {
			top := ts.unseenHeap.peek()
			if now.Sub(top.unseenKey) <= grace {
				break
			}
			work++
			top.mu.Lock()
			seen := top.seen
			issuedAt := top.issuedAt
			top.mu.Unlock()

			if seen {
				ts.unseenHeap.pop()
				continue
			}
			if now.Sub(issuedAt) <= grace {
				top.unseenKey = issuedAt
				ts.unseenHeap.fix(top)
				continue
			}
			eff := effectiveTicket(top, ts.hasLazyShifts)
			ts.removeLocked(top.token, top)
			retired = append(retired, eff)
		}
	}

	for ts.ttlHeap.len() > 0 && work < reaperBatchSize {
		top := ts.ttlHeap.peek()
		if now.Sub(top.ttlKey) <= effTTL {
			break
		}
		work++
		top.mu.Lock()
		issuedAt := top.issuedAt
		top.mu.Unlock()

		if now.Sub(issuedAt) <= effTTL {
			top.ttlKey = issuedAt
			ts.ttlHeap.fix(top)
			continue
		}
		eff := effectiveTicket(top, ts.hasLazyShifts)
		ts.removeLocked(top.token, top)
		retired = append(retired, eff)
	}

	if readyGrace > 0 && work < reaperBatchSize {
		edge := wr.nowServing.Load() + int64(wr.cap.Load())
		limit := reaperBatchSize - work
		type evictCandidate struct {
			node *tokenNode
			eff  int64
		}
		var toEvict []evictCandidate
		ts.tree.walkWhile(func(node *tokenNode, eff int64) bool {
			if eff > edge || len(toEvict) >= limit {
				return false
			}
			if node.skipReadyGrace {
				return true
			}
			node.mu.Lock()
			if node.readyAt.IsZero() {
				node.readyAt = now
			} else if now.Sub(node.readyAt) > readyGrace {
				toEvict = append(toEvict, evictCandidate{node: node, eff: eff})
			}
			node.mu.Unlock()
			return len(toEvict) < limit
		})
		for _, c := range toEvict {
			ts.removeLocked(c.node.token, c.node)
			retired = append(retired, c.eff)
		}
		work += len(toEvict)
	}

	ts.mu.Unlock()

	if len(retired) > 0 {
		wr.retireMany(retired)
		wr.emit(EventEvict, wr.snapshot(EventEvict))
	}
	return work
}

// reapIfDue runs at most one bounded reapBatch per minInterval using a
// single-flight CAS on lastForceReap. It intentionally never calls reapLeases
// so callers holding leaseMu preserve the leaseMu -> tokens.mu lock order.
func (wr *WaitingRoom) reapIfDue(now time.Time, minInterval time.Duration) bool {
	nowNs := now.UnixNano()
	last := wr.lastForceReap.Load()
	if last != 0 && nowNs >= last && nowNs-last < int64(minInterval) {
		return false
	}
	if !wr.lastForceReap.CompareAndSwap(last, nowNs) {
		return false
	}
	wr.reapBatch()
	return true
}

// queueFullWithReap checks whether maxQueueDepth is reached; if full, it runs
// at most one rate-limited synchronous reapBatch and re-evaluates queueFull so
// stale tickets cannot cause spurious rejections between background ticks.
func (wr *WaitingRoom) queueFullWithReap(now time.Time) (int64, bool) {
	maxDepth, full := wr.queueFull()
	if !full {
		return maxDepth, false
	}
	wr.reapIfDue(now, forceReapMinInterval)
	return wr.queueFull()
}

package room

import (
	"context"
	"time"
)

// SetReaperInterval changes the eviction cadence at runtime. Passing 0
// restores [DefaultReaperInterval]; otherwise valid values are 5s–24h
// (returns [ErrReaperInterval] outside that range). When
// [WaitingRoom.SetFirstPollGrace] is enabled with a shorter duration, the
// reaper ticks at that shorter cadence while ReaperInterval still reports the
// value set here.
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

func (wr *WaitingRoom) effectiveReaperInterval() time.Duration {
	d := time.Duration(wr.reaperInterval.Load())
	if g := time.Duration(wr.firstPollGrace.Load()); g > 0 && g < d {
		d = g
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

// reapBatch inspects up to reaperBatchSize due entries across unseenHeap and
// ttlHeap (O(1) when nothing is due, O(k log N) otherwise).
func (wr *WaitingRoom) reapBatch() int {
	now := time.Now()
	ttl := wr.tokens.ttl()
	grace := time.Duration(wr.firstPollGrace.Load())

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
		if now.Sub(top.ttlKey) <= ttl {
			break
		}
		work++
		top.mu.Lock()
		issuedAt := top.issuedAt
		top.mu.Unlock()

		if now.Sub(issuedAt) <= ttl {
			top.ttlKey = issuedAt
			ts.ttlHeap.fix(top)
			continue
		}
		eff := effectiveTicket(top, ts.hasLazyShifts)
		ts.removeLocked(top.token, top)
		retired = append(retired, eff)
	}

	ts.mu.Unlock()

	if len(retired) > 0 {
		wr.retireMany(retired)
		wr.emit(EventEvict, wr.snapshot(EventEvict))
	}
	return work
}

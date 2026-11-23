package room

import "time"

// Acquire grants key a slot held until [WaitingRoom.Release] (or lease TTL
// expiry) if key's ticket is inside the serving window and a slot is free.
// Otherwise key joins or stays in line (restarting its TokenTTL) and Acquire
// returns ok=false with its 1-based place in line.
//
// Calling Acquire for a key that already holds a lease renews its TTL and
// returns ok=true, position=0. Both the serving window and underlying
// semaphore (sema v1.2.0) preserve held slots on [WaitingRoom.SetCap] shrink,
// keeping Len() and Held() aligned until enough leases are released.
//
// Fires [EventLease], [EventEnter], and (on edge) [EventFull] when granted, or
// [EventQueue] with Snapshot.Key when joining the line. Returns
// [ErrNotInitialised], [ErrInvalidKey], [ErrQueueFull], or a crypto/rand error.
func (wr *WaitingRoom) Acquire(key string) (ok bool, position int64, err error) {
	if !wr.initialised.Load() {
		return false, 0, ErrNotInitialised{}
	}
	if err := validKey(key); err != nil {
		return false, 0, err
	}
	now := time.Now()

	wr.leaseMu.Lock()
	if l, held := wr.leases[key]; held {
		l.renewedAt = now
		wr.leases[key] = l
		wr.leaseMu.Unlock()
		return true, 0, nil
	}

	var (
		granted bool
		filled  bool
		place   int64
	)
	for attempt := 0; attempt < 2 && !granted; attempt++ {
		k, err := wr.enterLine(key, now)
		if err != nil {
			wr.leaseMu.Unlock()
			return false, 0, err
		}
		place = k.place
		if !wr.ticketReady(k.entry.ticket) || !wr.sem.TryAcquire() {
			break
		}
		claimed, won := wr.tokens.take(k.token)
		if !won {
			_ = wr.sem.Release()
			continue
		}
		filled = wr.enter()
		wr.leases[key] = leaseEntry{ticket: claimed.ticket, acquiredAt: now, renewedAt: now}
		granted = true
	}
	wr.leaseMu.Unlock()

	if !granted {
		return false, place, nil
	}
	wr.emit(EventEnter, wr.snapshot(EventEnter))
	if filled {
		wr.emit(EventFull, wr.snapshot(EventFull))
	}
	wr.emit(EventLease, wr.snapshotForKey(EventLease, key))
	return true, 0, nil
}

// Release returns key's leased slot, advancing the serving window by one.
// Fires [EventRelease], [EventExit], and (on edge) [EventDrain]. Returns
// [ErrNotInitialised], [ErrInvalidKey], or [ErrNoLease].
func (wr *WaitingRoom) Release(key string) error {
	if !wr.initialised.Load() {
		return ErrNotInitialised{}
	}
	if err := validKey(key); err != nil {
		return err
	}
	wr.leaseMu.Lock()
	_, held := wr.leases[key]
	if held {
		delete(wr.leases, key)
	}
	wr.leaseMu.Unlock()
	if !held {
		return ErrNoLease{}
	}
	wr.release()
	wr.emit(EventRelease, wr.snapshotForKey(EventRelease, key))
	return nil
}

// Held returns the number of slots currently held by leases.
func (wr *WaitingRoom) Held() int32 {
	if !wr.initialised.Load() {
		return 0
	}
	wr.leaseMu.RLock()
	defer wr.leaseMu.RUnlock()
	return int32(len(wr.leases))
}

// HasLease reports whether key currently holds a lease.
func (wr *WaitingRoom) HasLease(key string) bool {
	if !wr.initialised.Load() {
		return false
	}
	wr.leaseMu.RLock()
	defer wr.leaseMu.RUnlock()
	_, held := wr.leases[key]
	return held
}

// SetLeaseTTL sets how long a lease lasts without renewal via
// [WaitingRoom.Acquire]. Zero disables expiry (default); otherwise valid
// values are 1m–720h (returns [ErrLeaseTTL] outside that range). Reset to 0
// by Init. Safe to call at any time.
func (wr *WaitingRoom) SetLeaseTTL(d time.Duration) error {
	if d != 0 && (d < leaseTTLMin || d > leaseTTLMax) {
		return ErrLeaseTTL{Given: d, Min: leaseTTLMin, Max: leaseTTLMax}
	}
	wr.leaseTTL.Store(int64(d))
	return nil
}

// LeaseTTL returns the current lease TTL (0 means leases do not expire).
func (wr *WaitingRoom) LeaseTTL() time.Duration {
	return time.Duration(wr.leaseTTL.Load())
}

func (wr *WaitingRoom) reapLeases(now time.Time) {
	ttl := time.Duration(wr.leaseTTL.Load())
	if ttl <= 0 {
		return
	}
	var expired []string
	wr.leaseMu.Lock()
	for key, l := range wr.leases {
		if now.Sub(l.renewedAt) > ttl {
			delete(wr.leases, key)
			expired = append(expired, key)
		}
	}
	wr.leaseMu.Unlock()
	for _, key := range expired {
		wr.release()
		wr.emit(EventLeaseExpire, wr.snapshotForKey(EventLeaseExpire, key))
	}
}

package room

import "time"

// Leases.
//
// A request admitted by the middleware holds its slot until its handler
// returns. A lease holds a slot for a key until the application releases
// it — across any number of requests, for minutes or hours — which is what
// a server needs when the thing it rations is sessions or passes rather
// than requests.
//
// # How a lease is accounted
//
// Acquire puts the key in line (or finds it there) exactly like Enter. When
// the key's ticket is inside the serving window, the ticket is claimed and
// admitted just as the middleware admits one, and its slot is kept until
// Release (or until the lease TTL runs out), whose release advances the
// window. Leases and requests therefore share one capacity and one line,
// first come, first served, and every ticket is still accounted for
// exactly once.
//
// # Capacity changes
//
// The serving window, not the semaphore, is what bounds leases: the
// semaphore forgets its held slots when shrunk below them (see
// sema.Semaphore.SetCap), while the window keeps counting. SetCap below
// Held therefore revokes nothing, and grants no new slot — to a lease or a
// request — until enough leases have been released to bring the room back
// under the new capacity.
//
// # Requests from a key that holds a lease
//
// With SetTicketKeyFunc, a request whose key holds a lease passes through
// the middleware to the handler without taking another slot. A request
// that arrives at the moment its lease is released may still pass.

// Acquire takes a slot for key if key's place in line has reached the
// serving window and a slot is free; the slot is then held until
// Release(key), not until any handler returns. Otherwise key is put in
// line (or stays there, keeping its place and restarting its TokenTTL) and
// ok is false, with position its place in line, 1 being the front: the
// caller asks again later.
//
// Acquire for a key that already holds a lease returns ok=true and renews
// the lease (see SetLeaseTTL). position is 0 whenever ok is true.
//
// Returns ErrNotInitialised, ErrInvalidKey, ErrQueueFull when
// SetMaxQueueDepth is reached for a key not yet in line, or an error from
// crypto/rand. Fires EventLease, EventEnter and (on the edge) EventFull
// when a slot is granted; EventQueue, with Snapshot.Key set, when the key
// joins the line.
//
// Acquire calls are serialized with each other and with Release.
//
// Related: WaitingRoom.Release, WaitingRoom.Held, WaitingRoom.Enter
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
	// Two attempts: the first may find the key's ticket reaped or removed
	// between entering the line and claiming it, in which case the key is
	// a new arrival and enters again.
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
			// The slot was taken here a moment ago, so this release
			// cannot exceed the count.
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

// Release returns key's slot. The serving window moves forward by one, so
// the next client in line can be admitted (a keyed client on its next
// Acquire, a browser on its next poll).
//
// Returns ErrNotInitialised, ErrInvalidKey, or ErrNoLease for a key that
// holds no lease — including one whose lease already expired. Fires
// EventRelease, EventExit and (on the edge) EventDrain.
//
// Related: WaitingRoom.Acquire
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
	wr.release("")
	wr.emit(EventRelease, wr.snapshotForKey(EventRelease, key))
	return nil
}

// Held returns the number of slots held by leases. Like Len, it counts
// slots in use, but only those held by leases.
func (wr *WaitingRoom) Held() int32 {
	if !wr.initialised.Load() {
		return 0
	}
	wr.leaseMu.RLock()
	defer wr.leaseMu.RUnlock()
	return int32(len(wr.leases))
}

// HasLease reports whether key holds a lease.
func (wr *WaitingRoom) HasLease(key string) bool {
	if !wr.initialised.Load() {
		return false
	}
	wr.leaseMu.RLock()
	defer wr.leaseMu.RUnlock()
	_, held := wr.leases[key]
	return held
}

// SetLeaseTTL sets how long a lease lasts after it was granted or last
// renewed by Acquire. When it runs out the reaper releases the slot, as
// Release would, and fires EventLeaseExpire. A lease is a slot nobody
// else can use, so a TTL is the safety net for a holder that crashes or
// forgets to call Release.
//
// 0, the default, means leases last until Release. Otherwise the valid
// range is 1m–720h (30 days); values outside it return ErrLeaseTTL. A
// change applies to leases already held, from the next reaper pass.
//
// Safe to call at any time. Reset to 0 by Init.
//
// Related: WaitingRoom.Acquire, WaitingRoom.SetReaperInterval
func (wr *WaitingRoom) SetLeaseTTL(d time.Duration) error {
	if d != 0 && (d < leaseTTLMin || d > leaseTTLMax) {
		return ErrLeaseTTL{Given: d, Min: leaseTTLMin, Max: leaseTTLMax}
	}
	wr.leaseTTL.Store(int64(d))
	return nil
}

// LeaseTTL returns the current lease TTL; 0 means leases do not expire.
func (wr *WaitingRoom) LeaseTTL() time.Duration {
	return time.Duration(wr.leaseTTL.Load())
}

// reapLeases releases every lease not renewed for the lease TTL.
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
		wr.release("")
		wr.emit(EventLeaseExpire, wr.snapshotForKey(EventLeaseExpire, key))
	}
}

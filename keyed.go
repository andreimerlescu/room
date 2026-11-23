package room

import (
	"time"

	"github.com/gin-gonic/gin"
)

// TicketKeyFunc identifies a request by an application key instead of a cookie.
// Returning ok=false leaves the request on the standard cookie flow.
type TicketKeyFunc func(c *gin.Context) (key string, ok bool)

// SetTicketKeyFunc enables keyed tickets in [WaitingRoom.Middleware]. Requests
// where fn returns ok=true use key instead of cookies and receive a JSON wait
// response (503, Retry-After) unless [WaitingRoom.SetWaitResponder] overrides
// it. Invalid keys (empty or > [MaxKeyLen] bytes) receive 400 Bad Request;
// lease holders pass straight through. Not reset by [WaitingRoom.Init].
func (wr *WaitingRoom) SetTicketKeyFunc(fn TicketKeyFunc) {
	if fn == nil {
		wr.ticketKeyFunc.Store(nil)
		return
	}
	wr.ticketKeyFunc.Store(&fn)
}

func (wr *WaitingRoom) ticketKey(c *gin.Context) (string, bool) {
	p := wr.ticketKeyFunc.Load()
	if p == nil || *p == nil {
		return "", false
	}
	return (*p)(c)
}

func validKey(key string) error {
	if key == "" || len(key) > MaxKeyLen {
		return ErrInvalidKey{Len: len(key)}
	}
	return nil
}

func (wr *WaitingRoom) newKeyedEntry(key string, now time.Time) (string, ticketEntry, error) {
	token, err := generateToken()
	if err != nil {
		return "", ticketEntry{}, err
	}
	id, err := generateToken()
	if err != nil {
		return "", ticketEntry{}, err
	}
	return token, ticketEntry{
		createdAt:   now,
		issuedAt:    now,
		cookieSetAt: now,
		key:         key,
		id:          id,
	}, nil
}

func (wr *WaitingRoom) reserveTicket() int64 {
	return wr.nextTicket.Add(1)
}

func (wr *WaitingRoom) queueFull() (int64, bool) {
	max := wr.maxQueueDepth.Load()
	if max > 0 && (wr.QueueDepth() >= max || int64(wr.tokens.len()) >= max) {
		return max, true
	}
	return max, false
}

// enterLine puts key in line or refreshes it there. Caller holds wr.leaseMu
// (read or write) and has verified key holds no lease.
func (wr *WaitingRoom) enterLine(key string, now time.Time) (keyedEntry, error) {
	if k := wr.tokens.enterKey(key, "", ticketEntry{}, nil, now); k.token != "" {
		return k, nil
	}
	if max, full := wr.queueFull(); full {
		return keyedEntry{}, ErrQueueFull{Max: max}
	}
	token, fresh, err := wr.newKeyedEntry(key, now)
	if err != nil {
		return keyedEntry{}, err
	}
	k := wr.tokens.enterKey(key, token, fresh, wr.reserveTicket, now)
	if !k.created {
		return k, nil
	}
	wr.emit(EventQueue, wr.snapshotForKey(EventQueue, key))
	return k, nil
}

// Enter places key in line (or refreshes its TokenTTL if already queued) and
// returns its public status ID and 1-based place in line in O(log N).
//
// Enter never admits anyone; pair it with [WaitingRoom.Leave] when the
// application manages admission itself, or use [WaitingRoom.Acquire] for
// room-managed leases. Fires [EventQueue] with Snapshot.Key when creating a
// ticket. Returns [ErrNotInitialised], [ErrInvalidKey], [ErrLeaseHeld],
// [ErrQueueFull], or a crypto/rand error.
func (wr *WaitingRoom) Enter(key string) (ticket string, position int64, err error) {
	if !wr.initialised.Load() {
		return "", 0, ErrNotInitialised{}
	}
	if err := validKey(key); err != nil {
		return "", 0, err
	}
	wr.leaseMu.RLock()
	defer wr.leaseMu.RUnlock()
	if _, held := wr.leases[key]; held {
		return "", 0, ErrLeaseHeld{}
	}
	k, err := wr.enterLine(key, time.Now())
	if err != nil {
		return "", 0, err
	}
	return k.entry.id, k.place, nil
}

// Position returns the read-only 1-based place in line of a keyed ticket's
// public ID in O(log N) without restarting its TTL.
func (wr *WaitingRoom) Position(ticket string) (position int64, ok bool) {
	if !wr.initialised.Load() || ticket == "" {
		return 0, false
	}
	return wr.tokens.placeOfID(ticket)
}

// Leave removes key from the line and retires its ticket. Calling Leave for a
// key not in line is a no-op. Fires [EventRemove] with Snapshot.Key when
// removed. Returns [ErrNotInitialised] or [ErrInvalidKey].
func (wr *WaitingRoom) Leave(key string) error {
	if !wr.initialised.Load() {
		return ErrNotInitialised{}
	}
	if err := validKey(key); err != nil {
		return err
	}
	_, e, ok := wr.tokens.takeKey(key)
	if !ok {
		return nil
	}
	wr.retire(e.ticket)
	wr.emit(EventRemove, wr.snapshotForKey(EventRemove, key))
	return nil
}

// TicketByKey returns key's TicketInfo with Place populated in O(log N).
// Token holds the internal token for token-based admin APIs and must never be
// sent to clients.
func (wr *WaitingRoom) TicketByKey(key string) (TicketInfo, bool) {
	if !wr.initialised.Load() {
		return TicketInfo{}, false
	}
	wr.tokens.mu.RLock()
	t, ok := wr.tokens.byKey[key]
	var (
		e     ticketEntry
		place int64
	)
	if ok {
		node := wr.tokens.entries[t]
		eff := effectiveTicket(node, wr.tokens.hasLazyShifts)
		e = node.snapshotEntry(eff)
		place = wr.tokens.tree.place(node)
	}
	wr.tokens.mu.RUnlock()
	if !ok {
		return TicketInfo{}, false
	}
	ti := wr.ticketInfo(t, e)
	ti.Place = place
	return ti, true
}

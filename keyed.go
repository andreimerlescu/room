package room

import (
	"time"

	"github.com/gin-gonic/gin"
)

// Keyed tickets.
//
// A cookie ticket belongs to whoever holds its room_ticket cookie. A keyed
// ticket belongs to an application key instead — typically the ID of the
// bearer token an API client or AI agent presents — so a client with no
// cookie jar keeps its place for as long as it keeps asking with the same
// key. Keyed tickets live in the same line as cookie tickets: they are
// numbered, ordered, promoted, ranked, reaped (on TokenTTL and the
// first-poll grace) and exported exactly like them.
//
// A keyed ticket has two names, both random:
//
//   - its token, which is internal: it is never sent to the client, and the
//     cookie paths (Middleware, /queue/status) ignore a cookie that carries
//     one, so it cannot be used to claim the ticket's admission;
//   - its public ID, which names the place at /queue/status/{id} so a
//     person can watch it, and grants nothing.
//
// The line can be driven three ways: through the middleware
// (SetTicketKeyFunc), directly (Enter, Position, Leave), or with leases
// (Acquire, Release), which hold a slot across many requests.

// TicketKeyFunc identifies the client of a request by an application key.
// It returns ok=false for requests it does not recognise, which then use
// the room_ticket cookie as before.
//
// Related: WaitingRoom.SetTicketKeyFunc
type TicketKeyFunc func(c *gin.Context) (key string, ok bool)

// ticketKeyHolder wraps a TicketKeyFunc so atomic.Value always stores the
// same concrete type (see rateFuncHolder).
type ticketKeyHolder struct {
	fn TicketKeyFunc
}

// SetTicketKeyFunc makes fn's key the identity of a request's ticket. For a
// request where fn returns ok=true, the middleware neither reads nor sets
// the room_ticket or room_probe cookies: a request with the same key finds
// its ticket, and a queued one is answered with the JSON wait response
// (503, Retry-After) instead of the waiting-room page, unless
// SetWaitResponder overrides it. For a request where fn returns ok=false,
// the cookie flow is unchanged, so one room can serve browsers and agents.
//
// A key that is empty or longer than MaxKeyLen bytes gets 400 Bad Request.
// A key that holds a lease (see Acquire) passes straight through to the
// handler without taking another slot.
//
// fn is called once per request that reaches the middleware, outside any
// lock; it must be safe for concurrent use and cheap. Pass nil to stop
// keying requests; keyed tickets already in line stay there. Like
// SetClientKeyFunc, it is not reset by Init.
//
// Safe to call at any time.
//
// Related: TicketKeyFunc, WaitingRoom.Enter, WaitingRoom.SetWaitResponder
func (wr *WaitingRoom) SetTicketKeyFunc(fn TicketKeyFunc) {
	if fn == nil {
		wr.ticketKeyFunc.Store((*ticketKeyHolder)(nil))
		return
	}
	wr.ticketKeyFunc.Store(&ticketKeyHolder{fn: fn})
}

// ticketKey runs the registered TicketKeyFunc, if any.
func (wr *WaitingRoom) ticketKey(c *gin.Context) (string, bool) {
	h, _ := wr.ticketKeyFunc.Load().(*ticketKeyHolder)
	if h == nil || h.fn == nil {
		return "", false
	}
	return h.fn(c)
}

// validKey checks an application key.
func validKey(key string) error {
	if key == "" || len(key) > MaxKeyLen {
		return ErrInvalidKey{Len: len(key)}
	}
	return nil
}

// ── Token store: keyed operations ────────────────────────────────────────────

// keyedEntry is the outcome of tokenStore.enterKey.
type keyedEntry struct {
	token string
	entry ticketEntry
	place int64

	// created reports that this call created the ticket (and consumed
	// the ticket number it was given).
	created bool
}

// enterKey finds key's ticket and marks it seen (restarting its TTL), or,
// if the key has none and reserve is non-nil, stores fresh under token
// with a ticket number from reserve. The check, the number and the insert
// happen under one write lock, so concurrent calls with one key create one
// ticket, and keyed tickets enter the store in number order (their places
// never repeat). It reports the ticket's place in line.
func (ts *tokenStore) enterKey(key, token string, fresh ticketEntry, reserve func() int64, now time.Time) keyedEntry {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if t, ok := ts.byKey[key]; ok {
		e := ts.entries[t]
		e.issuedAt = now
		e.seen = true
		ts.entries[t] = e
		return keyedEntry{token: t, entry: e, place: ts.placeLocked(t, e)}
	}
	if reserve == nil {
		return keyedEntry{}
	}
	fresh.ticket = reserve()
	ts.putLocked(token, fresh)
	return keyedEntry{token: token, entry: fresh, place: ts.placeLocked(token, fresh), created: true}
}

// lookupKey returns key's ticket, if it has one.
func (ts *tokenStore) lookupKey(key string) (string, ticketEntry, bool) {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	t, ok := ts.byKey[key]
	if !ok {
		return "", ticketEntry{}, false
	}
	return t, ts.entries[t], true
}

// takeKey is take for a key: it removes key's ticket and returns it. The
// caller owns the ticket's accounting.
func (ts *tokenStore) takeKey(key string) (string, ticketEntry, bool) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	t, ok := ts.byKey[key]
	if !ok {
		return "", ticketEntry{}, false
	}
	e := ts.entries[t]
	ts.removeLocked(t, e)
	return t, e, true
}

// placeLocked is the place of token's entry among all entries, 1 being the
// front, in Queue order. One pass over the store. Caller holds ts.mu.
func (ts *tokenStore) placeLocked(token string, e ticketEntry) int64 {
	me := tokenSnapshot{token: token, entry: e}
	var ahead int64
	for t, o := range ts.entries {
		if t != token && lessQueued(tokenSnapshot{token: t, entry: o}, me) {
			ahead++
		}
	}
	return ahead + 1
}

// placeOfID is the place of the ticket with public ID id.
func (ts *tokenStore) placeOfID(id string) (int64, bool) {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	t, ok := ts.byID[id]
	if !ok {
		return 0, false
	}
	return ts.placeLocked(t, ts.entries[t]), true
}

// placeOfToken is the place of token's entry.
func (ts *tokenStore) placeOfToken(token string) (int64, bool) {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	e, ok := ts.entries[token]
	if !ok {
		return 0, false
	}
	return ts.placeLocked(token, e), true
}

// ── WaitingRoom: the line without the middleware ─────────────────────────────

// newKeyedEntry builds a fresh keyed entry, without a ticket number, with
// a new random token and ID. It returns the token too.
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

// reserveTicket issues the next ticket number.
func (wr *WaitingRoom) reserveTicket() int64 {
	return wr.nextTicket.Add(1)
}

// queueFull reports whether the SetMaxQueueDepth breaker is tripped, by
// the same two measures the middleware uses.
func (wr *WaitingRoom) queueFull() (int64, bool) {
	max := wr.maxQueueDepth.Load()
	if max > 0 && (wr.QueueDepth() >= max || int64(wr.tokens.len()) >= max) {
		return max, true
	}
	return max, false
}

// enterLine puts key in line or finds it there. Caller holds wr.leaseMu
// (read or write) and has checked that key holds no lease.
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
		// Another call with this key got there between the two locks.
		return k, nil
	}
	wr.emit(EventQueue, wr.snapshotForKey(EventQueue, key))
	return k, nil
}

// Enter puts key in line, or finds it there, and restarts its TokenTTL. It
// returns the ticket's public ID, which names the place at
// /queue/status/{id} without naming the key, and the key's place in line:
// 1 is the front.
//
// The place counts every ticket in the line ahead of the key — cookie and
// keyed, including tickets already told ready that have not come back — so
// with nobody admitted it runs 1, 2, 3, … in the order keys first entered,
// whatever the room's capacity. A second Enter with the same key never
// moves it and never issues a second ticket. Enter never admits anyone:
// pair it with Leave when the application decides for itself who goes in
// (as a server keeping its own count of places does), or use Acquire to let
// the room decide.
//
// A key that is not seen again for TokenTTL (or, with SetFirstPollGrace,
// never seen again after its first Enter for the grace) is reaped like any
// abandoned ticket, and the keys behind it move up.
//
// Returns ErrNotInitialised, ErrInvalidKey, ErrLeaseHeld if the key holds a
// lease, ErrQueueFull when SetMaxQueueDepth is reached for a key not yet in
// line, or an error from crypto/rand. Fires EventQueue, with Snapshot.Key
// set, when it creates a ticket.
//
// Cost: one pass over the line under its write lock, to compute the place
// (like Queue). Call it once per client request, not in a tight loop.
//
// Related: WaitingRoom.Position, WaitingRoom.Leave, WaitingRoom.Acquire
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

// Position returns the current place in line of the ticket with public ID
// ticket (as returned by Enter), 1 being the front. ok is false once the
// ticket has left the line — admitted, left, removed or reaped — or if the
// room is not initialised. Position is read-only: it does not restart the
// ticket's TTL.
//
// Cost: one pass over the line under its read lock.
//
// Related: WaitingRoom.Enter
func (wr *WaitingRoom) Position(ticket string) (position int64, ok bool) {
	if !wr.initialised.Load() || ticket == "" {
		return 0, false
	}
	return wr.tokens.placeOfID(ticket)
}

// Leave takes key out of the line. A key that is not in line is not an
// error. The ticket is retired exactly as RemoveToken retires one: the
// keys behind it move up at once and nobody is admitted early.
//
// Returns ErrNotInitialised or ErrInvalidKey. Fires EventRemove, with
// Snapshot.Key set, when a ticket was removed.
//
// Related: WaitingRoom.Enter, WaitingRoom.RemoveToken
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

// TicketByKey returns the current view of key's ticket, with Place filled
// in. ok is false if key is not in line (or holds a lease instead) or the
// room is not initialised.
//
// The returned Token is the ticket's internal token, for use with the
// token-based APIs (SetTicketRank, AdminPromote, RemoveToken). Never send
// it to the client: it would let the client's cookie claim the ticket.
//
// Cost: one pass over the line under its read lock.
//
// Related: WaitingRoom.Ticket, TicketInfo
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
		e = wr.tokens.entries[t]
		place = wr.tokens.placeLocked(t, e)
	}
	wr.tokens.mu.RUnlock()
	if !ok {
		return TicketInfo{}, false
	}
	ti := wr.ticketInfo(t, e)
	ti.Place = place
	return ti, true
}

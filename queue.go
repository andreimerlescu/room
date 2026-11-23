package room

import (
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// maxClientKeyLen bounds the per-ticket memory a ClientKeyFunc can cause.
// Longer keys are truncated (bytewise) and copied.
const maxClientKeyLen = 256

// ClientKeyFunc derives an application-defined key for an arriving
// request — typically the client IP as resolved by the application's
// trusted-proxy configuration, e.g. func(c *gin.Context) string {
// return c.ClientIP() }.
//
// Related: WaitingRoom.SetClientKeyFunc
type ClientKeyFunc func(c *gin.Context) string

// clientKeyHolder wraps a ClientKeyFunc so atomic.Value always stores the
// same concrete type (see rateFuncHolder for the reasoning).
type clientKeyHolder struct {
	fn ClientKeyFunc
}

// SetClientKeyFunc registers a function that derives a key for each
// request that is queued. The key is stored with the ticket and exposed
// as TicketInfo.ClientKey and Snapshot.ClientKey, so applications can
// list, count and remove tickets by client — for example, dropping every
// ticket behind a banned address range with RemoveTokensFunc — without
// keeping their own token→client map.
//
// The function is called once per queued arrival (never for requests
// admitted on the fast path and never for status polls), outside any
// WaitingRoom lock. It must be safe for concurrent use and should be
// cheap. A panic propagates to the gin handler chain like any other
// middleware panic.
//
// Keys longer than 256 bytes are truncated and copied, bounding memory
// per ticket. Pass nil to stop recording keys; tickets already queued
// keep theirs. Like SetHTML, the function is not reset by Init.
//
// Safe to call at any time.
//
// Related: TicketInfo.ClientKey, Snapshot.ClientKey, WaitingRoom.RemoveTokensFunc
func (wr *WaitingRoom) SetClientKeyFunc(fn ClientKeyFunc) {
	if fn == nil {
		wr.clientKeyFunc.Store((*clientKeyHolder)(nil))
		return
	}
	wr.clientKeyFunc.Store(&clientKeyHolder{fn: fn})
}

// clientKey returns the key for this request, or "" if no ClientKeyFunc
// is registered.
func (wr *WaitingRoom) clientKey(c *gin.Context) string {
	v := wr.clientKeyFunc.Load()
	if v == nil {
		return ""
	}
	h, ok := v.(*clientKeyHolder)
	if !ok || h == nil || h.fn == nil {
		return ""
	}
	k := h.fn(c)
	if len(k) > maxClientKeyLen {
		// Clone so the stored key does not pin the original (possibly
		// large) string's backing array.
		k = strings.Clone(k[:maxClientKeyLen])
	}
	return k
}

// TicketInfo is a point-in-time, read-only view of one queued ticket.
//
// It is returned by Queue and Ticket and delivered to the predicate of
// RemoveTokensFunc. Later versions may add fields; use named fields
// rather than positional struct literals.
//
// Related: WaitingRoom.Queue, WaitingRoom.Ticket, WaitingRoom.RemoveTokensFunc
type TicketInfo struct {
	// Token is the room_ticket cookie value identifying the client. It is
	// a bearer credential: do not log it or send it off-host.
	Token string

	// Ticket is the internal ordering key. Lower is further ahead. It is
	// opaque: do not persist it or compare it across restarts.
	Ticket int64

	// Position is the client's place in line at the moment the view was
	// taken. >= 1 means waiting (1 = next to be admitted). <= 0 means the
	// ticket is already inside the serving window: the client has been or
	// will be told ready=true and has not reloaded yet.
	Position int64

	// IssuedAt is when the ticket was issued. Unlike LastSeen it never
	// changes.
	IssuedAt time.Time

	// LastSeen is the last time the client contacted the server (initial
	// render, page reload, or any status poll). It is the anchor of the
	// sliding token TTL.
	LastSeen time.Time

	// LastPoll is the last time the client hit /queue/status, including
	// rate-limited polls. Zero if it has never polled.
	LastPoll time.Time

	// ClientKey is the key recorded by SetClientKeyFunc when the ticket
	// was issued, or "" if none was registered.
	ClientKey string

	// Seen reports whether the client has contacted the server at least
	// once since the ticket was issued. Tickets that are never seen are
	// typically cookieless clients, bots, or instant abandons.
	Seen bool

	// Promoted reports whether the ticket was moved forward by a paid
	// promotion or a VIP pass.
	Promoted bool

	// HasPass reports whether the client presented a valid VIP pass when
	// the ticket was issued or at its most recent waiting-page render.
	HasPass bool

	// Rank is the rank recorded by SetTicketRank, or 0.
	Rank int

	// Key is the application key of a keyed ticket — one created by
	// Enter, Acquire, or a request that SetTicketKeyFunc recognised — or ""
	// for a cookie ticket.
	Key string

	// ID is the public ticket ID of a keyed ticket, which names its place
	// at /queue/status/{id} and grants nothing; "" for a cookie ticket.
	ID string

	// Place is the ticket's place among every ticket in the store, 1 being
	// the front, counting tickets already told ready that have not come
	// back. It is filled by Queue, TicketByKey and the wait responder, and
	// is 0 everywhere else.
	Place int64
}

// ticketInfo builds the public view of a token-store entry.
func (wr *WaitingRoom) ticketInfo(token string, e ticketEntry) TicketInfo {
	return TicketInfo{
		Token:     token,
		Ticket:    e.ticket,
		Position:  wr.positionOf(e.ticket),
		IssuedAt:  e.createdAt,
		LastSeen:  e.issuedAt,
		LastPoll:  e.lastPoll,
		ClientKey: e.clientKey,
		Seen:      e.seen,
		Promoted:  e.promoted,
		HasPass:   e.hasPass,
		Rank:      e.rank,
		Key:       e.key,
		ID:        e.id,
	}
}

// Ticket returns the current view of a single queued ticket. ok is false
// if the token is unknown (never issued, admitted, removed, or reaped) or
// the WaitingRoom is not initialised.
//
// Cost: one read-locked map lookup. Safe for concurrent use.
//
// Related: WaitingRoom.Queue, TicketInfo
func (wr *WaitingRoom) Ticket(token string) (TicketInfo, bool) {
	if !wr.initialised.Load() {
		return TicketInfo{}, false
	}
	e, ok := wr.tokens.get(token)
	if !ok {
		return TicketInfo{}, false
	}
	return wr.ticketInfo(token, e), true
}

// Queue returns up to limit queued tickets in line order: the ticket that
// will be admitted next first. Tickets already inside the serving window
// (told ready, not yet reloaded) come before those still waiting. Tickets
// that share a ticket number (possible after promotions) are ordered by
// issue time.
//
// # Cost
//
// An O(log N + limit) in-order prefix walk of the order-statistic tree under
// the token store's read lock, running concurrently with status polls. Positions
// are computed after the lock is released and may drift slightly if the queue
// moves meanwhile.
//
// A limit <= 0, or an uninitialised WaitingRoom, returns nil. For the
// total count use LiveQueueDepth.
//
// Related: WaitingRoom.Ticket, WaitingRoom.LiveQueueDepth, TicketInfo
func (wr *WaitingRoom) Queue(limit int) []TicketInfo {
	if limit <= 0 || !wr.initialised.Load() {
		return nil
	}

	wr.tokens.mu.RLock()
	n := min(limit, wr.tokens.tree.len())
	snaps := make([]tokenSnapshot, 0, n)
	wr.tokens.tree.firstN(n, func(node *tokenNode, effTicket int64) {
		snaps = append(snaps, tokenSnapshot{
			token: node.token,
			entry: node.snapshotEntry(effTicket),
		})
	})
	wr.tokens.mu.RUnlock()

	out := make([]TicketInfo, len(snaps))
	for i, s := range snaps {
		out[i] = wr.ticketInfo(s.token, s.entry)
		out[i].Place = int64(i + 1)
	}
	return out
}

// lessQueued orders tickets by line position: ticket number, then issue
// time, then token (for determinism).
func lessQueued(a, b tokenSnapshot) bool {
	return lessTicketKey(a.entry.ticket, a.entry.createdAt, a.token, b.entry.ticket, b.entry.createdAt, b.token)
}

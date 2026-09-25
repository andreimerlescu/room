package room

import (
	"container/heap"
	"sort"
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
// One pass over the token store under its read lock, selecting the
// front-most limit entries with a bounded heap: O(n log limit) time and
// O(limit) memory. The read lock blocks status polls (which need the
// write lock) for the length of the pass, so call Queue every few seconds
// for an admin view, not per request. Positions are computed after the
// lock is released and may drift slightly if the queue moves meanwhile.
//
// A limit <= 0, or an uninitialised WaitingRoom, returns nil. For the
// total count use LiveQueueDepth.
//
// Related: WaitingRoom.Ticket, WaitingRoom.LiveQueueDepth, TicketInfo
func (wr *WaitingRoom) Queue(limit int) []TicketInfo {
	if limit <= 0 || !wr.initialised.Load() {
		return nil
	}

	var h ticketMaxHeap
	wr.tokens.mu.RLock()
	h = make(ticketMaxHeap, 0, min(limit, len(wr.tokens.entries)))
	for token, e := range wr.tokens.entries {
		item := tokenSnapshot{token: token, entry: e}
		if len(h) < limit {
			heap.Push(&h, item)
			continue
		}
		// h[0] is the back-most of the current front set.
		if lessQueued(item, h[0]) {
			h[0] = item
			heap.Fix(&h, 0)
		}
	}
	wr.tokens.mu.RUnlock()

	sort.Slice(h, func(i, j int) bool { return lessQueued(h[i], h[j]) })

	out := make([]TicketInfo, len(h))
	for i, s := range h {
		out[i] = wr.ticketInfo(s.token, s.entry)
	}
	return out
}

// lessQueued orders tickets by line position: ticket number, then issue
// time, then token (for determinism).
func lessQueued(a, b tokenSnapshot) bool {
	if a.entry.ticket != b.entry.ticket {
		return a.entry.ticket < b.entry.ticket
	}
	if !a.entry.createdAt.Equal(b.entry.createdAt) {
		return a.entry.createdAt.Before(b.entry.createdAt)
	}
	return a.token < b.token
}

// ticketMaxHeap keeps the back-most ticket of the selected set at the
// root, so a better candidate can replace it in O(log k).
type ticketMaxHeap []tokenSnapshot

func (h ticketMaxHeap) Len() int           { return len(h) }
func (h ticketMaxHeap) Less(i, j int) bool { return lessQueued(h[j], h[i]) }
func (h ticketMaxHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *ticketMaxHeap) Push(x any)        { *h = append(*h, x.(tokenSnapshot)) }
func (h *ticketMaxHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

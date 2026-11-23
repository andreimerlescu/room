package room

import (
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// maxClientKeyLen bounds the per-ticket memory a ClientKeyFunc can retain.
const maxClientKeyLen = 256

// ClientKeyFunc derives an application-defined client key (for example
// c.ClientIP()) recorded on each queued ticket.
type ClientKeyFunc func(c *gin.Context) string

// SetClientKeyFunc registers fn to tag newly queued tickets with a client key
// (truncated and cloned at 256 bytes). Called once per queued arrival outside
// room locks; not reset by [WaitingRoom.Init]. Pass nil to disable.
func (wr *WaitingRoom) SetClientKeyFunc(fn ClientKeyFunc) {
	if fn == nil {
		wr.clientKeyFunc.Store(nil)
		return
	}
	wr.clientKeyFunc.Store(&fn)
}

func (wr *WaitingRoom) clientKey(c *gin.Context) string {
	p := wr.clientKeyFunc.Load()
	if p == nil || *p == nil {
		return ""
	}
	k := (*p)(c)
	if len(k) > maxClientKeyLen {
		k = strings.Clone(k[:maxClientKeyLen])
	}
	return k
}

// TicketInfo is a point-in-time, read-only view of one queued ticket.
type TicketInfo struct {
	Token     string // room_ticket bearer credential — do not log
	Ticket    int64  // opaque internal ordering key
	Position  int64  // >= 1 waiting; <= 0 inside serving window
	IssuedAt  time.Time
	LastSeen  time.Time
	LastPoll  time.Time
	ClientKey string
	Seen      bool
	Promoted  bool
	HasPass   bool
	Rank      int
	Key       string // application key for keyed tickets, else ""
	ID        string // public status ID for keyed tickets, else ""
	Place     int64  // 1-based line place (filled by Queue, TicketByKey, WaitResponder)
}

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

// Ticket returns a read-only view of token, or false if unknown or uninitialised.
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

// Queue returns up to limit queued tickets in line order in O(log N + limit)
// under the token store's read lock, running concurrently with status polls.
// Returns nil if limit <= 0 or uninitialised.
func (wr *WaitingRoom) Queue(limit int) []TicketInfo {
	if limit <= 0 || !wr.initialised.Load() {
		return nil
	}

	wr.tokens.mu.RLock()
	n := min(limit, wr.tokens.tree.len())
	snaps := make([]tokenSnapshot, 0, n)
	wr.tokens.tree.walk(n, func(node *tokenNode, effTicket int64) {
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

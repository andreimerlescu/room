package room

import "sync"

// Event identifies a lifecycle event dispatched asynchronously by
// [WaitingRoom.On].
type Event uint8

const (
	EventEnter       Event = iota // request or lease acquired a slot
	EventExit                     // request or lease released a slot
	EventFull                     // room crossed from below capacity to full (edge)
	EventDrain                    // room crossed from full to having a free slot (edge)
	EventQueue                    // client joined the waiting line
	EventEvict                    // expired or unseen token reclaimed
	EventTimeout                  // request context cancelled before acquiring a slot
	EventPromote                  // paid, admin, or VIP-pass promotion moved a ticket
	EventRemove                   // ticket removed via RemoveToken, RemoveTokensFunc, or Leave
	EventRank                     // SetTicketRank moved a ticket ahead of lower ranks
	EventLease         // Acquire or verified remote lease reserved a lease slot
	EventRelease       // Release returned a lease slot
	EventLeaseExpire   // reaper reclaimed a lease past LeaseTTL or signed expiry
	EventTokenRejected // signed pass or lease failed verification
)

// String returns the canonical name of e.
func (e Event) String() string {
	switch e {
	case EventEnter:
		return "Enter"
	case EventExit:
		return "Exit"
	case EventFull:
		return "Full"
	case EventDrain:
		return "Drain"
	case EventQueue:
		return "Queue"
	case EventEvict:
		return "Evict"
	case EventTimeout:
		return "Timeout"
	case EventPromote:
		return "Promote"
	case EventRemove:
		return "Remove"
	case EventRank:
		return "Rank"
	case EventLease:
		return "Lease"
	case EventRelease:
		return "Release"
	case EventLeaseExpire:
		return "LeaseExpire"
	case EventTokenRejected:
		return "TokenRejected"
	default:
		return "Unknown"
	}
}

// Snapshot is a comparable point-in-time view delivered to [CallbackFunc].
// Token is a bearer credential when populated — do not log it.
type Snapshot struct {
	Event          Event
	Occupancy      int
	Capacity       int
	QueueDepth     int64
	Token          string // set on EventQueue (cookie) and EventRemove (RemoveToken)
	ClientKey      string // ClientKeyFunc key for Token, if any
	StaleTicket    bool   // EventQueue: arrival carried an unrecognised room_ticket cookie
	Key            string // set for keyed tickets, leases, and EventTokenRejected
	Reason         string // EventTokenRejected: verification failure reason (never raw token)
	TokenTransport string // EventTokenRejected: "cookie" or "header"
}

// Full reports whether Occupancy equals or exceeds Capacity.
func (s Snapshot) Full() bool { return s.Occupancy >= s.Capacity }

// Empty reports whether no slots are occupied.
func (s Snapshot) Empty() bool { return s.Occupancy == 0 }

// CallbackFunc handles WaitingRoom lifecycle events.
type CallbackFunc func(snap Snapshot)

type callbackRegistry struct {
	mu        sync.RWMutex
	callbacks map[Event][]CallbackFunc
}

func newCallbackRegistry() *callbackRegistry {
	return &callbackRegistry{
		callbacks: make(map[Event][]CallbackFunc),
	}
}

// On registers fn to run in its own goroutine whenever event fires. Keep
// handlers for high-frequency events non-blocking. Safe for concurrent use.
func (wr *WaitingRoom) On(event Event, fn CallbackFunc) {
	wr.callbacks.mu.Lock()
	defer wr.callbacks.mu.Unlock()
	wr.callbacks.callbacks[event] = append(wr.callbacks.callbacks[event], fn)
}

// Off removes all handlers registered for event. Safe for concurrent use.
func (wr *WaitingRoom) Off(event Event) {
	wr.callbacks.mu.Lock()
	defer wr.callbacks.mu.Unlock()
	delete(wr.callbacks.callbacks, event)
}

func (wr *WaitingRoom) emit(event Event, snap Snapshot) {
	wr.callbacks.mu.RLock()
	handlers := make([]CallbackFunc, len(wr.callbacks.callbacks[event]))
	copy(handlers, wr.callbacks.callbacks[event])
	wr.callbacks.mu.RUnlock()

	for _, fn := range handlers {
		go fn(snap)
	}
}

func (wr *WaitingRoom) snapshot(event Event) Snapshot {
	return Snapshot{
		Event:      event,
		Occupancy:  wr.Len(),
		Capacity:   int(wr.Cap()),
		QueueDepth: wr.QueueDepth(),
	}
}

func (wr *WaitingRoom) snapshotFor(event Event, token, clientKey string, staleTicket bool) Snapshot {
	s := wr.snapshot(event)
	s.Token = token
	s.ClientKey = clientKey
	s.StaleTicket = staleTicket
	return s
}

func (wr *WaitingRoom) snapshotForKey(event Event, key string) Snapshot {
	s := wr.snapshot(event)
	s.Key = key
	return s
}

func (wr *WaitingRoom) snapshotForRejection(reason, transport, key, clientKey string) Snapshot {
	s := wr.snapshot(EventTokenRejected)
	s.Reason = reason
	s.TokenTransport = transport
	s.Key = key
	s.ClientKey = clientKey
	return s
}

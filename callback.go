package room

import "sync"

// Event describes a lifecycle moment in the WaitingRoom's operation.
// Handlers are registered via On and fired asynchronously — each in its
// own goroutine — so that slow callbacks never stall the middleware hot path.
//
// Related: WaitingRoom.On, WaitingRoom.Off, WaitingRoom.emit
type Event uint8

const (
	// EventEnter fires each time a request acquires a semaphore slot and
	// is admitted into active service. Use this to track throughput or
	// update external load-balancer weights.
	EventEnter Event = iota

	// EventExit fires each time a request completes and releases its slot.
	// Paired with EventEnter it gives you a complete picture of slot lifetime.
	EventExit

	// EventFull fires once when the room transitions from below capacity to
	// at capacity — i.e. every slot is now occupied. It does NOT fire on
	// every admission while full; only on the transition edge. Use this to
	// trigger scale-out logic such as provisioning a new host.
	EventFull

	// EventDrain fires once when the room transitions from full (all slots
	// occupied) back to having at least one free slot. It does NOT fire
	// when the room becomes completely empty — only on the full→available
	// edge. Use this to signal that scale-in is safe or to re-enable a
	// previously throttled upstream.
	EventDrain

	// EventQueue fires when an arriving request cannot be admitted
	// immediately and is issued a ticket for the waiting room. The
	// Snapshot carries the issued Token, its ClientKey, and StaleTicket
	// (whether the request presented a room_ticket the room did not
	// recognise).
	EventQueue

	// EventEvict fires when an expired token is removed and its ticket is
	// considered abandoned: either by the reaper (once per eviction batch
	// that removed anything) or by /queue/status discovering the expiry
	// on a poll (once per such token). Evictions of tickets already inside
	// the serving window are included.
	EventEvict

	// EventTimeout fires when a request's context is cancelled or its
	// deadline expires before it could acquire a slot.
	EventTimeout

	// EventPromote fires when a queued token is promoted to a higher
	// position via PromoteToken or a VIP pass. Use this to track revenue
	// events or log queue jumps for fairness monitoring.
	EventPromote

	// EventRemove fires when the application removes queued tickets via
	// RemoveToken (once per successful call, with Token and ClientKey set)
	// or RemoveTokensFunc (once per call that removed at least one ticket,
	// with Token empty). It is distinct from EventEvict so that deliberate
	// removals — bans, kicks — are not counted as abandonment.
	EventRemove
)

// String returns the canonical name of the Event, suitable for logging.
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
	default:
		return "Unknown"
	}
}

// Snapshot is a point-in-time view of the WaitingRoom delivered to every
// callback. All fields are copied at trigger time and are safe to read
// after the room's state has changed. Snapshot is comparable.
type Snapshot struct {
	// Event is the lifecycle event that produced this snapshot.
	Event Event

	// Occupancy is the number of semaphore slots in use at the moment of
	// the event.
	Occupancy int

	// Capacity is the maximum number of concurrent occupants allowed.
	Capacity int

	// QueueDepth is the number of requests currently waiting for a slot.
	QueueDepth int64

	// Token is the room_ticket value of the single ticket the event
	// concerns, when there is exactly one: the newly issued ticket for
	// EventQueue, and the removed ticket for EventRemove raised by
	// RemoveToken. Empty for all other events.
	//
	// It is the client's bearer credential for its queue position: treat
	// it as a secret — do not log it or send it off-host.
	Token string

	// ClientKey is the SetClientKeyFunc key of the ticket in Token, or ""
	// if Token is empty or no key function was registered.
	ClientKey string

	// StaleTicket is set on EventQueue when the arriving request carried a
	// room_ticket cookie that the room did not recognise — typically a
	// previously admitted visitor returning, a ticket that expired or was
	// removed, or a ticket from before a restart. It is false when the
	// request carried no room_ticket at all, which is the signature of a
	// client that is discarding cookies.
	StaleTicket bool
}

// Full returns true when Occupancy equals or exceeds Capacity.
func (s Snapshot) Full() bool { return s.Occupancy >= s.Capacity }

// Empty returns true when no slots are in use.
func (s Snapshot) Empty() bool { return s.Occupancy == 0 }

// CallbackFunc is the function signature for all WaitingRoom lifecycle
// callbacks. The Snapshot argument is safe to retain beyond the call.
type CallbackFunc func(snap Snapshot)

// callbackRegistry stores per-Event handler slices. It is embedded in
// WaitingRoom and owns its own RWMutex so that callback registration and
// dispatch never contend with wr.mu, which is held on the request hot path.
type callbackRegistry struct {
	mu        sync.RWMutex
	callbacks map[Event][]CallbackFunc
}

func newCallbackRegistry() *callbackRegistry {
	return &callbackRegistry{
		callbacks: make(map[Event][]CallbackFunc),
	}
}

// On registers fn to be called whenever event fires. Multiple handlers
// may be registered for the same event; all are invoked, each in its own
// goroutine, in registration order. On is safe for concurrent use and may
// be called after the WaitingRoom is running.
//
// Because every handler runs in a fresh goroutine, a handler for a
// high-frequency event (EventEnter, EventQueue) that blocks on I/O can
// accumulate goroutines during a traffic burst. Keep such handlers
// non-blocking — increment a counter or send on a buffered channel.
//
// Example — scale out when the room is full:
//
//	wr.On(room.EventFull, func(s room.Snapshot) {
//	    log.Printf("room full (%d/%d) — provisioning new host", s.Occupancy, s.Capacity)
//	    go provisionHost()
//	})
//
// Related: WaitingRoom.Off, WaitingRoom.emit
func (wr *WaitingRoom) On(event Event, fn CallbackFunc) {
	wr.callbacks.mu.Lock()
	defer wr.callbacks.mu.Unlock()
	wr.callbacks.callbacks[event] = append(wr.callbacks.callbacks[event], fn)
}

// Off removes all handlers registered for event. It is safe for concurrent
// use. Handlers that are already executing are not interrupted.
//
// Related: WaitingRoom.On
func (wr *WaitingRoom) Off(event Event) {
	wr.callbacks.mu.Lock()
	defer wr.callbacks.mu.Unlock()
	delete(wr.callbacks.callbacks, event)
}

// emit fires all handlers registered for event, each in its own goroutine.
// snap must be constructed immediately before calling emit so that it
// reflects the room's state at the moment the event occurred.
// emit is safe to call with no registered handlers — it is a no-op.
//
// Related: WaitingRoom.On, WaitingRoom.Off
func (wr *WaitingRoom) emit(event Event, snap Snapshot) {
	wr.callbacks.mu.RLock()
	handlers := make([]CallbackFunc, len(wr.callbacks.callbacks[event]))
	copy(handlers, wr.callbacks.callbacks[event])
	wr.callbacks.mu.RUnlock()

	for _, fn := range handlers {
		go fn(snap)
	}
}

// snapshot builds a Snapshot from the WaitingRoom's current state.
// Call this immediately before emit to capture the state at event time.
func (wr *WaitingRoom) snapshot(event Event) Snapshot {
	return Snapshot{
		Event:      event,
		Occupancy:  wr.Len(),
		Capacity:   int(wr.Cap()),
		QueueDepth: wr.QueueDepth(),
	}
}

// snapshotFor builds a Snapshot for an event that concerns one specific
// ticket.
func (wr *WaitingRoom) snapshotFor(event Event, token, clientKey string, staleTicket bool) Snapshot {
	s := wr.snapshot(event)
	s.Token = token
	s.ClientKey = clientKey
	s.StaleTicket = staleTicket
	return s
}

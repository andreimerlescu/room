package room

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/andreimerlescu/sema"
)

// WaitingRoom is a FIFO-ordered capacity gate for gin HTTP handlers.
//
// It combines a semaphore (capacity management via sema) with a ticket
// queue (arrival ordering) to ensure that when your application is at
// capacity, excess requests are held in strict arrival order and admitted
// one-by-one as slots open.
//
// The zero value is not usable. Always construct via NewWaitingRoom or
// initialise manually with Init.
//
// Related: NewWaitingRoom, Init, Middleware, RegisterRoutes
type WaitingRoom struct {
	// sem manages how many requests are actively being served.
	// Ordering is the ticket queue's responsibility, not the semaphore's.
	sem sema.Semaphore

	// cap is the maximum number of concurrently active requests.
	cap int32

	// nextTicket is atomically incremented on every request arrival,
	// giving each request a unique monotonically increasing position.
	nextTicket atomic.Int64

	// nowServing is the lower bound of the current serving window.
	// A ticket is ready when ticket <= nowServing + cap.
	nowServing atomic.Int64

	// mu protects cond and is also used to guard html writes.
	mu sync.Mutex

	// cond is broadcast every time nowServing advances, waking all
	// goroutines waiting for their ticket to be called.
	cond *sync.Cond

	// html holds the custom waiting room page bytes.
	// If nil, the embedded default waiting_room.html is used.
	html []byte

	// tokens maps random session tokens to ticket entries.
	// It is used by the /queue/status endpoint to identify callers.
	tokens *tokenStore

	// stopReaper cancels the background eviction goroutine.
	stopReaper context.CancelFunc

	// reaperInterval stores the current eviction interval as nanoseconds.
	reaperInterval atomic.Int64

	// reaperRestart signals the reaper goroutine to rebuild its ticker
	// after a SetReaperInterval call.
	reaperRestart chan struct{}
}

// ticketEntry holds the state for a single queued client.
type ticketEntry struct {
	// ticket is the FIFO position assigned on arrival.
	ticket int64

	// issuedAt records when the token was created for TTL enforcement.
	issuedAt time.Time
}

// tokenStore maps random token strings to ticketEntry values.
// It is protected by its own RWMutex to avoid holding the WaitingRoom
// mutex during I/O-bound status polling.
type tokenStore struct {
	mu      sync.RWMutex
	entries map[string]ticketEntry
}

func newTokenStore() *tokenStore {
	return &tokenStore{
		entries: make(map[string]ticketEntry),
	}
}

func (ts *tokenStore) set(token string, entry ticketEntry) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.entries[token] = entry
}

func (ts *tokenStore) get(token string) (ticketEntry, bool) {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	e, ok := ts.entries[token]
	return e, ok
}

func (ts *tokenStore) delete(token string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	delete(ts.entries, token)
}

// statusResponse is the JSON payload served by StatusHandler.
type statusResponse struct {
	// Ready signals the client to stop polling and reload the page.
	Ready bool `json:"ready"`

	// Position is the client's current place in the queue.
	// Omitted when Ready is true.
	Position int64 `json:"position,omitempty"`

	// Utilization is the EWMA-smoothed semaphore utilization, 0.0–1.0.
	// Useful for displaying a load indicator on the waiting room page.
	Utilization float64 `json:"utilization,omitempty"`
}

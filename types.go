package room

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/andreimerlescu/sema"
)

// WaitingRoom is a ticket-ordered capacity gate for gin HTTP handlers.
//
// It combines a semaphore (capacity management via sema) with a ticket
// queue (arrival tracking) so that when your application is at capacity,
// excess requests wait for capacity to become available and are served in
// ticket order as slots open, without guaranteeing strict one-by-one FIFO
// admission among clients that become eligible at the same time.
//
// The zero value is not usable. Always construct via NewWaitingRoom or
// initialise manually with Init.
//
// Related: NewWaitingRoom, Init, Middleware, RegisterRoutes
type WaitingRoom struct {
	sem            sema.Semaphore
	cap            atomic.Int32
	nextTicket     atomic.Int64
	nowServing     atomic.Int64
	mu             sync.Mutex
	cond           *sync.Cond
	html           []byte
	tokens         *tokenStore
	stopReaper     context.CancelFunc
	reaperInterval atomic.Int64
	reaperRestart  chan struct{}
	initialised    atomic.Bool
	callbacks      *callbackRegistry
}

// ticketEntry holds the state for a single queued client.
type ticketEntry struct {
	ticket   int64
	issuedAt time.Time
}

// tokenStore maps random token strings to ticketEntry values.
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
	Ready       bool    `json:"ready"`
	Position    int64   `json:"position,omitempty"`
	Utilization float64 `json:"utilization,omitempty"`
}

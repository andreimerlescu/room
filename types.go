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
// # Cookie security
//
// By default the waiting-room session cookie is issued WITHOUT the Secure
// flag so that plain-HTTP local development works without configuration.
// Call SetSecureCookie(true) before traffic arrives in any deployment that
// serves the application over HTTPS (directly or via a TLS-terminating
// proxy such as Cloudflare, nginx, or an AWS ALB). Alternatively, use
// SetSecureCookieFromRequest to derive the flag from each incoming request.
//
// Related: NewWaitingRoom, Init, Middleware, RegisterRoutes
type WaitingRoom struct {
	sem            sema.Semaphore
	cap            atomic.Int32
	nextTicket     atomic.Int64
	nowServing     atomic.Int64
	mu             sync.Mutex
	html           []byte
	tokens         *tokenStore
	stopReaper     context.CancelFunc
	reaperInterval atomic.Int64
	reaperRestart  chan struct{}
	initialised    atomic.Bool
	callbacks      *callbackRegistry
	secureCookie   atomic.Bool
	maxQueueDepth  atomic.Int64
	cookiePath     atomic.Value // string
	cookieDomain   atomic.Value // string
	rateFunc       atomic.Value // *rateFuncHolder
	promoteMu      sync.Mutex   // serializes ticket reassignment in PromoteToken
	promoteInsert  atomic.Int64 // lowest ticket assigned via promotion; math.MaxInt64 = unused
	skipURL        atomic.Value // string
	passes         *passStore
	passDuration   atomic.Int64 // nanoseconds; 0 = passes disabled
}

// ticketEntry holds the state for a single queued client.
type ticketEntry struct {
	ticket   int64
	issuedAt time.Time
	lastPoll time.Time
	promoted bool
}

// passEntry holds a time-limited VIP pass issued after a skip-the-line
// purchase. The pass survives across queue tickets — if the client is
// evicted, times out, or re-enters the queue, the pass auto-promotes
// them without requiring another payment.
type passEntry struct {
	expiresAt time.Time
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

// deleteIfExpired atomically checks expiry and deletes the token under a
// single write lock. Returns true if the token existed and was expired.
// This eliminates the TOCTOU window between separate isExpired + delete calls.
func (ts *tokenStore) deleteIfExpired(token string) bool {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	entry, ok := ts.entries[token]
	if !ok {
		return false
	}
	if time.Since(entry.issuedAt) > cookieTTL {
		delete(ts.entries, token)
		return true
	}
	return false
}

// touchIssuedAt resets the issuedAt timestamp for a token to now,
// preventing the reaper from evicting a client that is actively polling.
func (ts *tokenStore) touchIssuedAt(token string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	entry, ok := ts.entries[token]
	if !ok {
		return
	}
	entry.issuedAt = time.Now()
	ts.entries[token] = entry
}

// touchLastPoll updates the lastPoll timestamp and returns the previous
// value. Callers use this to enforce per-token poll rate limits.
func (ts *tokenStore) touchLastPoll(token string) (previous time.Time, ok bool) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	entry, exists := ts.entries[token]
	if !exists {
		return time.Time{}, false
	}
	previous = entry.lastPoll
	entry.lastPoll = time.Now()
	ts.entries[token] = entry
	return previous, true
}

// len returns the number of entries in the token store.
func (ts *tokenStore) len() int {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	return len(ts.entries)
}

// isExpired reports whether the token exists and has exceeded cookieTTL.
// Deprecated: prefer deleteIfExpired to avoid the TOCTOU window.
func (ts *tokenStore) isExpired(token string) bool {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	entry, ok := ts.entries[token]
	if !ok {
		return true
	}
	return time.Since(entry.issuedAt) > cookieTTL
}

// passStore maps pass tokens (from the room_pass cookie) to their
// expiration times. It is separate from tokenStore because passes
// outlive individual queue tickets.
type passStore struct {
	mu      sync.RWMutex
	entries map[string]passEntry
}

func newPassStore() *passStore {
	return &passStore{
		entries: make(map[string]passEntry),
	}
}

func (ps *passStore) set(token string, entry passEntry) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.entries[token] = entry
}

// get returns the pass entry and true if the pass exists AND has not
// expired. Expired passes are deleted on read (lazy eviction).
func (ps *passStore) get(token string) (passEntry, bool) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	entry, ok := ps.entries[token]
	if !ok {
		return passEntry{}, false
	}
	if time.Now().After(entry.expiresAt) {
		delete(ps.entries, token)
		return passEntry{}, false
	}
	return entry, true
}

func (ps *passStore) delete(token string) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	delete(ps.entries, token)
}

// len returns the number of entries (including potentially expired ones
// that haven't been lazily evicted yet).
func (ps *passStore) len() int {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	return len(ps.entries)
}

// reap removes all expired passes. Called by the token reaper on each
// eviction cycle so expired passes don't accumulate unboundedly.
func (ps *passStore) reap() {
	now := time.Now()
	ps.mu.Lock()
	defer ps.mu.Unlock()
	for token, entry := range ps.entries {
		if now.After(entry.expiresAt) {
			delete(ps.entries, token)
		}
	}
}

// statusResponse is the JSON payload served by StatusHandler.
type statusResponse struct {
	Ready       bool    `json:"ready"`
	Position    int64   `json:"position,omitempty"`
	Utilization float64 `json:"utilization,omitempty"`
	SkipCost    float64 `json:"skip_cost,omitempty"`
	RatePerPos  float64 `json:"rate_per_pos,omitempty"`
	HasPass     bool    `json:"has_pass,omitempty"`
}

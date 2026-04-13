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

// statusResponse is the JSON payload served by StatusHandler.
type statusResponse struct {
	Ready       bool    `json:"ready"`
	Position    int64   `json:"position,omitempty"`
	Utilization float64 `json:"utilization,omitempty"`
}

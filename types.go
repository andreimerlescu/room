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
// # Cookie availability
//
// A client that cannot store cookies cannot hold a queue position: every
// request it makes is indistinguishable from a brand-new arrival. The
// WaitingRoom detects this condition rather than letting such a client
// spin — see probeCookieName and statusResponse.CookiesRequired.
//
// # Cookie lifetime
//
// room_ticket and room_probe carry MaxAge = TokenTTL. Because the waiting
// page polls with fetch and never reloads, both cookies are re-sent on
// status polls (at most once per TokenTTL/3 per client) and on every
// waiting-page render, so a client can wait indefinitely without its
// browser discarding them.
//
// # Ticket accounting
//
// Every ticket number ends its life exactly once: either it is admitted
// (and its release advances nowServing), or it is retired (and the ghost
// ledger advances nowServing when the window reaches it). Every path that
// removes a queued token claims it with a single delete, and only the
// claimant accounts for it. See ghostLedger and WaitingRoom.retire.
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
	clientKeyFunc  atomic.Value // *clientKeyHolder; not reset by Init
	promoteMu      sync.Mutex   // serializes ticket reassignment in PromoteToken
	promoteInsert  atomic.Int64 // lowest ticket assigned via promotion; math.MaxInt64 = unused
	skipURL        atomic.Value // string
	passes         *passStore
	passDuration   atomic.Int64 // nanoseconds; 0 = passes disabled

	// firstPollGrace is how long a freshly issued token may go without
	// the client ever contacting the server again before the reaper
	// treats it as abandoned. Nanoseconds; 0 disables the check. See
	// SetFirstPollGrace.
	firstPollGrace atomic.Int64

	// occupancy mirrors the number of semaphore slots currently held. It
	// exists solely so that EventFull and EventDrain can be attributed to
	// the single request that CAUSED each transition.
	//
	// Reading wr.Len() before and after acquiring cannot do this: under
	// concurrency several goroutines each observe "was below capacity,
	// now at capacity" for the same crossing and all emit EventFull. The
	// return value of an atomic Add is unique to the caller, so exactly
	// one request sees occupancy reach cap and exactly one sees it fall
	// back below.
	//
	// Maintained by enter and exit. wr.Len() remains the public,
	// semaphore-backed occupancy reading.
	occupancy atomic.Int32

	// ledger holds retired ticket numbers the serving window has not yet
	// reached. It keeps abandoned, expired and removed tickets from
	// permanently occupying a slot in the window (which would freeze the
	// queue at small capacities) without admitting anyone early.
	//
	// Related: ghostLedger, WaitingRoom.retire, WaitingRoom.drainLedger
	ledger *ghostLedger
}

// ticketEntry holds the state for a single queued client.
type ticketEntry struct {
	ticket int64

	// createdAt is when the ticket was issued. Never changes.
	createdAt time.Time

	// issuedAt is the anchor of the sliding TTL. Despite the name it is
	// reset on every client contact (poll or waiting-page render), so it
	// means "last seen".
	issuedAt time.Time

	// lastPoll is the time of the last /queue/status poll, including
	// rate-limited ones. Used for the per-token rate limit.
	lastPoll time.Time

	// cookieSetAt is when room_ticket/room_probe were last sent to this
	// client with a fresh MaxAge. Drives the cookie refresh on polls.
	cookieSetAt time.Time

	// clientKey is the SetClientKeyFunc key recorded at issuance.
	clientKey string

	// seen reports whether the client has contacted the server at least
	// once since the token was issued (any poll or a waiting-page
	// render). A token that is never seen belongs to a client that
	// cannot or will not come back — see SetFirstPollGrace.
	seen bool

	promoted bool

	// hasPass records whether the client presented a valid VIP pass at
	// issuance or at its most recent waiting-page render.
	hasPass bool

	// rank is the rank recorded by SetTicketRank, or 0.
	rank int
}

// passEntry holds a time-limited VIP pass issued after a skip-the-line
// purchase. The pass survives across queue tickets — if the client is
// evicted, times out, or re-enters the queue, the pass auto-promotes
// them without requiring another payment.
type passEntry struct {
	expiresAt time.Time
}

// tokenStore maps random token strings to ticketEntry values.
//
// The store owns its own TTL so that expiry checks remain a single
// lock-scoped operation with no reference back to the WaitingRoom. The
// TTL is a sliding window: every client contact resets it, so it governs
// how long an ABANDONED token lingers, not how long a client may
// legitimately wait.
//
// Removal operations that end a queued ticket's life (take,
// deleteIfExpired, poll's expiry branch, the reaper's write-locked
// delete) return the removed entry so that exactly one caller — the one
// that actually removed it — accounts for the ticket. See
// WaitingRoom.retire.
type tokenStore struct {
	mu       sync.RWMutex
	entries  map[string]ticketEntry
	ttlNanos atomic.Int64
}

func newTokenStore() *tokenStore {
	ts := &tokenStore{
		entries: make(map[string]ticketEntry),
	}
	ts.ttlNanos.Store(int64(defaultTokenTTL))
	return ts
}

// ttl returns the current sliding-window token lifetime.
func (ts *tokenStore) ttl() time.Duration {
	return time.Duration(ts.ttlNanos.Load())
}

// setTTL updates the sliding-window token lifetime. Safe to call at any
// time; takes effect on the next expiry check.
func (ts *tokenStore) setTTL(d time.Duration) {
	ts.ttlNanos.Store(int64(d))
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

// delete removes a token WITHOUT reporting what was removed. It must only
// be used where the ticket is accounted for elsewhere (for example, a
// token whose ticket was already admitted). Anything that ends a queued
// ticket's life must use take or deleteIfExpired and retire the result.
func (ts *tokenStore) delete(token string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	delete(ts.entries, token)
}

// take atomically removes the token and returns its entry. It is the
// claim operation for ticket accounting: when admission and eviction race
// for the same token, exactly one of them gets ok=true, and only that one
// may account for the ticket (release if admitted, retire if not).
func (ts *tokenStore) take(token string) (ticketEntry, bool) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	e, ok := ts.entries[token]
	if ok {
		delete(ts.entries, token)
	}
	return e, ok
}

// deleteIfExpired atomically checks expiry and deletes the token under a
// single write lock. It returns the removed entry and true if the token
// existed and was expired; the caller then owns the ticket and must
// retire it. This eliminates the TOCTOU window between separate
// isExpired + delete calls.
func (ts *tokenStore) deleteIfExpired(token string) (ticketEntry, bool) {
	ttl := ts.ttl()
	ts.mu.Lock()
	defer ts.mu.Unlock()
	entry, ok := ts.entries[token]
	if !ok {
		return ticketEntry{}, false
	}
	if time.Since(entry.issuedAt) > ttl {
		delete(ts.entries, token)
		return entry, true
	}
	return ticketEntry{}, false
}

// touchIssuedAt resets the issuedAt timestamp for a token to now and
// marks it seen, preventing the reaper from evicting a client that is
// actively in contact.
func (ts *tokenStore) touchIssuedAt(token string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	entry, ok := ts.entries[token]
	if !ok {
		return
	}
	entry.issuedAt = time.Now()
	entry.seen = true
	ts.entries[token] = entry
}

// markRendered records that the client reloaded the waiting page and is
// about to receive both cookies again: it restarts the sliding TTL, marks
// the token seen, restarts the cookie-refresh clock, and records whether
// the client presented a valid VIP pass on this request.
func (ts *tokenStore) markRendered(token string, now time.Time, hasPass bool) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	entry, ok := ts.entries[token]
	if !ok {
		return
	}
	entry.issuedAt = now
	entry.seen = true
	entry.cookieSetAt = now
	entry.hasPass = hasPass
	ts.entries[token] = entry
}

// touchLastPoll updates the lastPoll timestamp and returns the previous
// value. Callers use this to enforce per-token poll rate limits.
//
// StatusHandler now uses poll, which does this as part of a single
// locked operation; touchLastPoll remains for tests and diagnostics.
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

// pollOutcome classifies a /queue/status poll for a token that was
// presented in a cookie.
type pollOutcome uint8

const (
	// pollUnknown: no such token (admitted, removed, reaped, or never
	// issued by this process).
	pollUnknown pollOutcome = iota

	// pollExpired: the token existed but its TTL had elapsed; it has been
	// removed and the caller owns (and must retire) its ticket.
	pollExpired

	// pollRateLimited: the poll arrived less than the minimum interval
	// after the previous one. The token was still kept alive.
	pollRateLimited

	// pollAccepted: a normal poll.
	pollAccepted
)

// pollResult is the outcome of tokenStore.poll.
type pollResult struct {
	outcome pollOutcome

	// entry is the token's state after the poll was applied (or, for
	// pollExpired, the removed entry).
	entry ticketEntry

	// refreshCookie reports that the cookies are due to be re-sent on
	// this response. When true, cookieSetAt has already been advanced.
	refreshCookie bool
}

// poll applies one /queue/status poll under a single write lock:
//
//  1. Unknown token → pollUnknown.
//  2. TTL elapsed → delete and return pollExpired with the entry.
//  3. Otherwise keep the token alive (issuedAt = now), mark it seen and
//     record lastPoll — on BOTH the accepted and the rate-limited path,
//     so a client polling too fast is throttled but never reaped while
//     it is still polling.
//  4. Rate limit: previous poll within minInterval → pollRateLimited.
//  5. Accepted: if the ticket is still waiting (ticket > edge) and the
//     cookies were last sent at least refreshEvery ago, advance
//     cookieSetAt and report refreshCookie.
//
// edge is nowServing+cap read by the caller just before the call. The
// window only moves forward, so a slightly stale edge can at worst skip
// a refresh for a client that is about to be admitted anyway.
//
// This replaces the former deleteIfExpired + get + touchLastPoll +
// touchIssuedAt sequence, which took the write lock three times per poll.
func (ts *tokenStore) poll(token string, now time.Time, edge int64, minInterval, refreshEvery time.Duration) pollResult {
	ttl := ts.ttl()

	ts.mu.Lock()
	defer ts.mu.Unlock()

	entry, ok := ts.entries[token]
	if !ok {
		return pollResult{outcome: pollUnknown}
	}
	if now.Sub(entry.issuedAt) > ttl {
		delete(ts.entries, token)
		return pollResult{outcome: pollExpired, entry: entry}
	}

	prev := entry.lastPoll
	entry.lastPoll = now
	entry.issuedAt = now
	entry.seen = true

	res := pollResult{outcome: pollAccepted}
	if !prev.IsZero() && now.Sub(prev) < minInterval {
		res.outcome = pollRateLimited
	} else if entry.ticket > edge && now.Sub(entry.cookieSetAt) >= refreshEvery {
		entry.cookieSetAt = now
		res.refreshCookie = true
	}

	ts.entries[token] = entry
	res.entry = entry
	return res
}

// len returns the number of entries in the token store. This is the count
// of LIVE queued clients — unlike QueueDepth, which is derived from the
// monotonic ticket counter and therefore also counts tickets burned by
// clients that never came back.
func (ts *tokenStore) len() int {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	return len(ts.entries)
}

// isExpired reports whether the token exists and has exceeded the TTL.
// Deprecated: prefer deleteIfExpired to avoid the TOCTOU window.
func (ts *tokenStore) isExpired(token string) bool {
	ttl := ts.ttl()
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	entry, ok := ts.entries[token]
	if !ok {
		return true
	}
	return time.Since(entry.issuedAt) > ttl
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
//
// The common case — a live pass, or no pass — takes only the read lock.
// The write lock is taken only to delete an expired pass, with a re-check
// so that a pass renewed in between is not deleted.
func (ps *passStore) get(token string) (passEntry, bool) {
	now := time.Now()

	ps.mu.RLock()
	entry, ok := ps.entries[token]
	ps.mu.RUnlock()

	if !ok {
		return passEntry{}, false
	}
	if !now.After(entry.expiresAt) {
		return entry, true
	}

	ps.mu.Lock()
	if cur, still := ps.entries[token]; still && now.After(cur.expiresAt) {
		delete(ps.entries, token)
	}
	ps.mu.Unlock()
	return passEntry{}, false
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

	// CookiesRequired is set when a poll arrives with no room_ticket
	// cookie at all. The client cannot hold a queue position, so the
	// page must stop its reload cycle and surface an error rather than
	// treating the absent position as an admission signal.
	CookiesRequired bool `json:"cookies_required,omitempty"`
}

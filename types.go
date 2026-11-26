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
// It combines a semaphore (capacity management via sema) with a ticket queue
// so excess requests wait in ticket order rather than overloading the origin.
// The zero value is not usable; construct via [NewWaitingRoom] or initialise
// with [WaitingRoom.Init].
//
// # Cookie security and lifetime
//
// Cookies are issued with HttpOnly and SameSite=Lax. By default Secure is set
// only when the request arrived over TLS directly (r.TLS != nil); call
// [WaitingRoom.SetSecureCookie](true) behind TLS-terminating proxies. Both
// room_ticket and room_probe carry MaxAge = TokenTTL and are refreshed on
// status polls (at most once per TokenTTL/3) and waiting-page reloads.
//
// # Ticket accounting
//
// Every ticket ends its life exactly once: either admitted (its release
// advances nowServing) or retired (its ghost ledger entry advances nowServing
// once the window reaches it).
type WaitingRoom struct {
	sem            sema.Semaphore
	cap            atomic.Int32
	nextTicket     atomic.Int64
	nowServing     atomic.Int64
	mu             sync.Mutex
	html           []byte
	tokens         *tokenStore
	stopReaper     context.CancelFunc
	reaperDone     chan struct{} // closed when the reaper goroutine exits
	reaperInterval atomic.Int64
	reaperRestart  chan struct{}
	initialised    atomic.Bool
	callbacks      *callbackRegistry
	secureCookie   atomic.Bool
	maxQueueDepth  atomic.Int64
	cookiePath     atomic.Pointer[string]
	cookieDomain   atomic.Pointer[string]
	rateFunc       atomic.Pointer[RateFunc]
	clientKeyFunc  atomic.Pointer[ClientKeyFunc] // not reset by Init
	promoteMu      sync.Mutex                    // serializes ticket reassignment in PromoteToken/SetTicketRank
	skipURL        atomic.Pointer[string]
	passes         *passStore
	passDuration   atomic.Int64 // nanoseconds; 0 disables passes
	firstPollGrace atomic.Int64 // nanoseconds; 0 disables grace
	readyGrace     atomic.Int64 // nanoseconds; 0 disables ready-claim deadline
	idlePollTTL    atomic.Int64 // nanoseconds; 0 disables independent idle-poll cap
	lastForceReap  atomic.Int64 // UnixNano of last rate-limited synchronous reapBatch
	occupancy      atomic.Int32 // mirrors active slots for edge-triggered EventFull/EventDrain
	ledger         *ghostLedger

	// Not reset by Init.
	ticketKeyFunc  atomic.Pointer[TicketKeyFunc]
	waitResponder  atomic.Pointer[WaitResponder]
	fullResponder  atomic.Pointer[FullResponder]
	tokenExtractor atomic.Pointer[TokenExtractor]

	waitJSON      atomic.Bool
	statusBaseURL atomic.Pointer[string]

	// Signed token, scope, cookie prefix, and bounded lease pool config.
	scope             atomic.Pointer[string]
	cookiePrefix      atomic.Pointer[string]
	ticketCookie      atomic.Pointer[string]
	passCookie        atomic.Pointer[string]
	probeCookie       atomic.Pointer[string]
	signer            atomic.Pointer[Signer]
	verifier          atomic.Pointer[Verifier]
	signedMaxTTL      atomic.Int64 // nanoseconds; default DefaultSignedMaxTTL
	clockSkew         atomic.Int64 // nanoseconds; default DefaultClockSkew
	leaseFractionBits atomic.Uint64
	leaseFractionSet  atomic.Bool

	// Lock order: leaseMu -> promoteMu -> tokens.mu -> node.mu / ledger.mu.
	// leaseMu is never held across callbacks, handlers, or response writes.
	leaseMu  sync.RWMutex
	leases   map[string]leaseEntry
	leaseTTL atomic.Int64 // nanoseconds; 0 means leases last until Release

	nowFn func() time.Time // nil defaults to time.Now; overridden in tests
}

// leaseEntry records one key's held slot from Acquire (or verified remote
// signed lease reservation) until Release or expiry.
type leaseEntry struct {
	ticket     int64
	acquiredAt time.Time
	renewedAt  time.Time
	expiresAt  time.Time // non-zero for remote signed leases; hard expiry ceiling
}

// ticketEntry is a point-in-time snapshot of a queued client's state.
type ticketEntry struct {
	ticket         int64
	createdAt      time.Time
	issuedAt       time.Time // sliding TTL anchor (last client contact)
	lastPoll       time.Time
	readyAt        time.Time
	cookieSetAt    time.Time
	clientKey      string
	seen           bool
	promoted       bool
	hasPass        bool
	skipReadyGrace bool
	rank           int
	key            string
	id             string
	viewedAt       time.Time
}

// statusResponse is the JSON payload served by StatusHandler.
type statusResponse struct {
	Ready           bool    `json:"ready"`
	Position        int64   `json:"position,omitempty"`
	Utilization     float64 `json:"utilization,omitempty"`
	SkipCost        float64 `json:"skip_cost,omitempty"`
	RatePerPos      float64 `json:"rate_per_pos,omitempty"`
	HasPass         bool    `json:"has_pass,omitempty"`
	CookiesRequired bool    `json:"cookies_required,omitempty"`
}

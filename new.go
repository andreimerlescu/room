package room

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/andreimerlescu/sema"
	"github.com/gin-gonic/gin"
)

// NewWaitingRoom constructs an initialised WaitingRoom, registers the status
// routes on r, and returns its middleware. It panics if cap < 1.
//
// Because the caller receives no *WaitingRoom handle, its background reaper
// goroutine lives for the process lifetime. For explicit shutdown or runtime
// configuration, construct a WaitingRoom manually with [WaitingRoom.Init],
// [WaitingRoom.RegisterRoutes], and [WaitingRoom.Stop].
func NewWaitingRoom(r *gin.Engine, cap int32) gin.HandlerFunc {
	wr := &WaitingRoom{}
	if err := wr.Init(cap); err != nil {
		panic(fmt.Sprintf("room.NewWaitingRoom: %v", err))
	}
	r.OPTIONS("/queue/status", func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})
	r.GET("/queue/status", wr.StatusHandler())
	r.GET("/queue/status/:ticket", wr.TicketStatusHandler())
	return wr.Middleware()
}

// NewWaitingRoomFromStruct returns wr.Middleware(), panicking if wr is nil.
//
// Deprecated: call wr.Middleware() directly.
func NewWaitingRoomFromStruct(wr *WaitingRoom) gin.HandlerFunc {
	if wr == nil {
		panic("room.NewWaitingRoomFromStruct: nil WaitingRoom")
	}
	return wr.Middleware()
}

// Init initialises the WaitingRoom with capacity cap (>= 1) and starts its
// background reaper goroutine. Call Init once before serving traffic; use
// [WaitingRoom.SetCap] and other setters for runtime reconfiguration.
//
// Returns [ErrInvalidCap] if cap < 1.
func (wr *WaitingRoom) Init(cap int32) error {
	if cap < 1 {
		return ErrInvalidCap{Given: cap}
	}

	wr.haltReaper()

	wr.cap.Store(cap)
	wr.sem = sema.Must(int(cap))
	wr.tokens = newTokenStore()
	wr.tokens.setTTL(DefaultTokenTTL)
	wr.passes = newPassStore()
	wr.ledger = newGhostLedger()
	wr.reaperRestart = make(chan struct{}, 1)
	wr.nowServing.Store(0)
	wr.nextTicket.Store(0)
	wr.occupancy.Store(0)
	wr.reaperInterval.Store(int64(DefaultReaperInterval))
	wr.firstPollGrace.Store(0)
	wr.secureCookie.Store(false)
	wr.maxQueueDepth.Store(0)
	wr.SetCookiePath("/")
	wr.SetCookieDomain("")
	wr.rateFunc.Store(nil)
	wr.SetSkipURL("")
	wr.passDuration.Store(0)
	wr.callbacks = newCallbackRegistry()
	wr.waitJSON.Store(false)
	wr.SetStatusBaseURL(defaultStatusBaseURL)
	wr.leaseTTL.Store(0)
	wr.leaseMu.Lock()
	wr.leases = make(map[string]leaseEntry)
	wr.leaseMu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	wr.stopReaper = cancel
	wr.startReaper(ctx)

	wr.initialised.Store(true)
	return nil
}

// Stop cancels the background reaper goroutine and waits for it to exit.
// Calling Stop more than once is harmless.
func (wr *WaitingRoom) Stop() {
	wr.haltReaper()
}

// SetSecureCookie forces the Secure flag on waiting-room cookies when true.
// When false (the default), Secure is set only on requests arriving directly
// over TLS (r.TLS != nil). Call SetSecureCookie(true) behind TLS-terminating
// proxies. Safe to call at any time.
func (wr *WaitingRoom) SetSecureCookie(secure bool) {
	wr.secureCookie.Store(secure)
}

// SetTokenTTL sets the sliding-window lifetime of an abandoned queued token.
// Passing 0 restores [DefaultTokenTTL]; otherwise valid values are 30s–24h
// (returns [ErrTokenTTL] outside that range). Safe to call at any time.
func (wr *WaitingRoom) SetTokenTTL(d time.Duration) error {
	if d == 0 {
		d = DefaultTokenTTL
	}
	if d < tokenTTLMin || d > tokenTTLMax {
		return ErrTokenTTL{Given: d, Min: tokenTTLMin, Max: tokenTTLMax}
	}
	wr.tokens.setTTL(d)
	return nil
}

// TokenTTL returns the current sliding-window token lifetime.
func (wr *WaitingRoom) TokenTTL() time.Duration {
	return wr.tokens.ttl()
}

// SetMaxQueueDepth sets the maximum waiting queue depth before new arrivals
// receive 503 Service Unavailable. Zero disables the limit (default); negative
// values return [ErrInvalidMaxQueueDepth]. Safe to call at any time.
func (wr *WaitingRoom) SetMaxQueueDepth(max int64) error {
	if max < 0 {
		return ErrInvalidMaxQueueDepth{Given: max}
	}
	wr.maxQueueDepth.Store(max)
	return nil
}

// MaxQueueDepth returns the current maximum queue depth (0 means unlimited).
func (wr *WaitingRoom) MaxQueueDepth() int64 {
	return wr.maxQueueDepth.Load()
}

// LiveQueueDepth returns the number of live tokens currently in the store
// (including clients told ready that have not reloaded yet).
func (wr *WaitingRoom) LiveQueueDepth() int64 {
	return int64(wr.tokens.len())
}

// SetCookiePath sets the Path attribute for waiting-room cookies (empty
// restores "/"). Safe to call at any time.
func (wr *WaitingRoom) SetCookiePath(path string) {
	if path == "" {
		path = "/"
	}
	wr.cookiePath.Store(&path)
}

// CookiePath returns the current cookie Path setting ("/" if unset).
func (wr *WaitingRoom) CookiePath() string {
	if p := wr.cookiePath.Load(); p != nil && *p != "" {
		return *p
	}
	return "/"
}

// SetCookieDomain sets the Domain attribute for waiting-room cookies.
// Safe to call at any time.
func (wr *WaitingRoom) SetCookieDomain(domain string) {
	wr.cookieDomain.Store(&domain)
}

// CookieDomain returns the current cookie Domain setting (empty if unset).
func (wr *WaitingRoom) CookieDomain() string {
	if p := wr.cookieDomain.Load(); p != nil {
		return *p
	}
	return ""
}

// SetSkipURL sets the payment URL for the "Pay to skip" button on the waiting
// room page. Both [WaitingRoom.SetRateFunc] and SetSkipURL must be configured
// for the offer to appear. Safe to call at any time.
func (wr *WaitingRoom) SetSkipURL(url string) {
	wr.skipURL.Store(&url)
}

// SkipURL returns the current skip-the-line payment URL (empty if unset).
func (wr *WaitingRoom) SkipURL() string {
	if p := wr.skipURL.Load(); p != nil {
		return *p
	}
	return ""
}

// SetPassDuration sets the lifetime of VIP passes issued by [WaitingRoom.GrantPass]
// and [WaitingRoom.PromoteToken]. Zero disables passes (default); non-zero
// values must lie in 1m–24h (otherwise returns [ErrPassDuration]). Safe to
// call at any time.
func (wr *WaitingRoom) SetPassDuration(d time.Duration) error {
	if d == 0 {
		wr.passDuration.Store(0)
		return nil
	}
	if d < passMinDuration || d > passMaxDuration {
		return ErrPassDuration{Given: d, Min: passMinDuration, Max: passMaxDuration}
	}
	wr.passDuration.Store(int64(d))
	return nil
}

// PassDuration returns the current VIP pass lifetime (0 means disabled).
func (wr *WaitingRoom) PassDuration() time.Duration {
	return time.Duration(wr.passDuration.Load())
}

// GrantPass creates a time-limited VIP pass token, or returns "" if passes
// are disabled. Set the returned token as the room_pass cookie on the response.
func (wr *WaitingRoom) GrantPass() string {
	d := wr.PassDuration()
	if d == 0 {
		return ""
	}
	token, err := generateToken()
	if err != nil {
		return ""
	}
	wr.passes.set(token, passEntry{expiresAt: time.Now().Add(d)})
	return token
}

// HasValidPass reports whether passToken exists and has not expired.
func (wr *WaitingRoom) HasValidPass(passToken string) bool {
	if passToken == "" {
		return false
	}
	_, ok := wr.passes.get(passToken)
	return ok
}

// checkInitialised aborts with 500 and returns false if Init was not called.
func (wr *WaitingRoom) checkInitialised(c *gin.Context) bool {
	if !wr.initialised.Load() {
		c.AbortWithStatus(http.StatusInternalServerError)
		return false
	}
	return true
}

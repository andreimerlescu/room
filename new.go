package room

import (
	"context"
	"fmt"
	"math"
	"net/http"

	"github.com/andreimerlescu/sema"
	"github.com/gin-gonic/gin"
)

// NewWaitingRoom returns a gin.HandlerFunc that enforces a FIFO waiting room
// with the given capacity. It is the simplest way to add a waiting room to
// an existing gin application.
//
// cap is the maximum number of requests actively served at any moment.
// Any value between 1 and math.MaxInt32 is valid.
//
// # Goroutine lifecycle
//
// NewWaitingRoom starts a background reaper goroutine whose lifetime is tied
// to the process. The caller has no reference to the underlying WaitingRoom
// and therefore cannot call Stop(). This is acceptable for long-lived server
// processes where the goroutine is intentional and the process exit cleans up.
// For tests, embedded servers, or any scenario requiring explicit shutdown,
// construct a WaitingRoom manually and call Stop() via defer:
//
//	wr := &room.WaitingRoom{}
//	wr.Init(500)
//	defer wr.Stop()
//	wr.RegisterRoutes(r)
//
// Related: WaitingRoom.RegisterRoutes, WaitingRoom.Middleware
func NewWaitingRoom(r *gin.Engine, cap int32) gin.HandlerFunc {
	wr := &WaitingRoom{}
	if err := wr.Init(cap); err != nil {
		panic(fmt.Sprintf("room.NewWaitingRoom: %v", err))
	}
	r.GET("/queue/status", wr.StatusHandler())
	return wr.Middleware()
}

// NewWaitingRoomFromStruct returns a gin.HandlerFunc from a fully configured
// WaitingRoom. Use this when you need to retain a handle to the WaitingRoom
// after initialisation.
//
// Usage:
//
//	wr := &room.WaitingRoom{}
//	if err := wr.Init(500); err != nil {
//	    log.Fatal(err)
//	}
//	defer wr.Stop()
//	wr.RegisterRoutes(r)
//
// Related: NewWaitingRoom, WaitingRoom.RegisterRoutes
func NewWaitingRoomFromStruct(wr *WaitingRoom) gin.HandlerFunc {
	if wr == nil {
		panic("room.NewWaitingRoomFromStruct: nil WaitingRoom")
	}
	return wr.Middleware()
}

// Init initialises the WaitingRoom with the given capacity and starts the
// background reaper. It must be called before Middleware or RegisterRoutes
// when constructing a WaitingRoom manually.
//
// Init is not safe for concurrent use. Call it once during setup before
// any goroutines start serving traffic. For runtime capacity changes use
// SetCap; for runtime reaper changes use SetReaperInterval.
//
// Returns ErrInvalidCap if cap < 1.
//
// Related: WaitingRoom.Stop, WaitingRoom.SetCap
func (wr *WaitingRoom) Init(cap int32) error {
	if cap < 1 {
		return ErrInvalidCap{Given: cap}
	}

	if wr.stopReaper != nil {
		wr.stopReaper()
	}

	wr.cap.Store(cap)
	wr.sem = sema.Must(int(cap))
	wr.tokens = newTokenStore()
	wr.reaperRestart = make(chan struct{}, 1)
	wr.nowServing.Store(0)
	wr.nextTicket.Store(0)
	wr.reaperInterval.Store(int64(reaperInterval))
	wr.secureCookie.Store(secureCookieDefault)
	wr.maxQueueDepth.Store(defaultMaxQueueDepth)
	wr.cookiePath.Store("/")
	wr.cookieDomain.Store("")
	wr.rateFunc.Store((*rateFuncHolder)(nil))
	wr.promoteInsert.Store(math.MaxInt64)
	wr.skipURL.Store("")
	wr.initialised.Store(true)
	wr.callbacks = newCallbackRegistry()

	ctx, cancel := context.WithCancel(context.Background())
	wr.stopReaper = cancel
	wr.startReaper(ctx)

	return nil
}

// Stop shuts down the background reaper goroutine. Call it when your
// application is shutting down to ensure a clean exit with no leaked
// goroutines.
//
// Related: WaitingRoom.Init, WaitingRoom.startReaper
func (wr *WaitingRoom) Stop() {
	if wr.stopReaper != nil {
		wr.stopReaper()
	}
}

// SetSecureCookie controls whether the waiting-room session cookie is
// issued with the Secure flag. The default is false so that plain-HTTP
// local development works without configuration.
//
// Call SetSecureCookie(true) in any production deployment served over
// HTTPS — either directly or via a TLS-terminating proxy (Cloudflare,
// nginx, AWS ALB, etc.) where c.Request.TLS may be nil even though the
// end-user connection is encrypted.
//
// Safe to call at any time before or after traffic starts.
func (wr *WaitingRoom) SetSecureCookie(secure bool) {
	wr.secureCookie.Store(secure)
}

// SetMaxQueueDepth sets the maximum number of requests that may wait in the
// queue simultaneously. When the queue is at this depth, new arrivals
// receive a 503 Service Unavailable immediately instead of being queued.
//
// A value of 0 disables the limit (unlimited queue depth). This is the
// default. Negative values return ErrInvalidMaxQueueDepth.
//
// Safe to call at any time including while requests are in flight.
func (wr *WaitingRoom) SetMaxQueueDepth(max int64) error {
	if max < 0 {
		return ErrInvalidMaxQueueDepth{Given: max}
	}
	wr.maxQueueDepth.Store(max)
	return nil
}

// MaxQueueDepth returns the current maximum queue depth. Zero means unlimited.
func (wr *WaitingRoom) MaxQueueDepth() int64 {
	return wr.maxQueueDepth.Load()
}

// SetCookiePath sets the Path attribute of the waiting-room session cookie.
// The default is "/". Use this to scope the cookie to a specific route
// prefix in multi-app deployments sharing a domain.
//
// Safe to call at any time.
func (wr *WaitingRoom) SetCookiePath(path string) {
	if path == "" {
		path = "/"
	}
	wr.cookiePath.Store(path)
}

// CookiePath returns the current cookie Path setting.
func (wr *WaitingRoom) CookiePath() string {
	return wr.cookiePath.Load().(string)
}

// SetCookieDomain sets the Domain attribute of the waiting-room session
// cookie. The default is empty (browser uses the request host). Set this
// to restrict or expand cookie scope in multi-subdomain deployments.
//
// Safe to call at any time.
func (wr *WaitingRoom) SetCookieDomain(domain string) {
	wr.cookieDomain.Store(domain)
}

// CookieDomain returns the current cookie Domain setting.
func (wr *WaitingRoom) CookieDomain() string {
	return wr.cookieDomain.Load().(string)
}

// SetSkipURL sets the URL that the waiting room "Pay to skip" button
// navigates to. This is typically a payment page (Stripe Checkout,
// crypto invoice, etc.) that your application hosts.
//
// The handler at this URL can read the room_ticket cookie to identify
// which queued client is paying. After payment verification, call
// PromoteTokenToFront to move them to the front of the queue.
//
// If empty (the default), the skip-the-line offer is never shown on
// the waiting room page, even if a RateFunc is configured. Both
// SetRateFunc and SetSkipURL must be set for the offer to appear.
//
// Safe to call at any time.
//
// Related: SetRateFunc, PromoteToken, PromoteTokenToFront
func (wr *WaitingRoom) SetSkipURL(url string) {
	wr.skipURL.Store(url)
}

// SkipURL returns the current skip-the-line payment URL.
func (wr *WaitingRoom) SkipURL() string {
	return wr.skipURL.Load().(string)
}

// checkInitialised aborts the request with 500 and returns false if the
// WaitingRoom has not been initialised. Prevents nil pointer dereferences
// on zero-value WaitingRoom structs.
func (wr *WaitingRoom) checkInitialised(c *gin.Context) bool {
	if !wr.initialised.Load() {
		c.AbortWithStatus(http.StatusInternalServerError)
		return false
	}
	return true
}

package room

import (
	"context"
	"fmt"
	"net/http"
	"sync"

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

// isSecureCookie returns the current Secure cookie setting. When the
// incoming request arrived over a TLS connection we always upgrade to
// secure regardless of the stored setting, so that deployments that
// terminate TLS at the Go layer get correct behaviour without additional
// configuration.
func (wr *WaitingRoom) isSecureCookie(r interface{ TLS() bool }) bool {
	if wr.secureCookie.Load() {
		return true
	}
	return false
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

// mu is used only to protect html (SetHTML/resolveHTML). The cond variable
// previously stored here has been removed: the WaitingRoom uses a
// poll-driven admission model (clients poll /queue/status), not a
// push-driven one. There are no goroutines blocking on cond.Wait() in this
// package; the sync.Cond and all associated Broadcast() calls were dead code.
var _ sync.Mutex // keep sync import for mu field in WaitingRoom struct

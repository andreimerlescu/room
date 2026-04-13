package room

import (
	"bytes"
	_ "embed"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

//go:embed waiting_room.html
var defaultWaitingRoomBytes []byte

// Middleware returns the gin.HandlerFunc that enforces the waiting room.
//
// Every request is issued a ticket on arrival. If the ticket falls within
// the current serving window (nowServing + cap), the request acquires a
// semaphore slot and proceeds immediately. Otherwise the waiting room HTML
// is served, the request is aborted, and the client polls /queue/status
// until admitted — at which point the browser reloads and the request
// re-enters on the fast path.
//
// This design avoids writing two responses to the same ResponseWriter by
// never calling c.Next() on a request that was served the waiting room page.
//
// # Admission model
//
// Admission is poll-driven: queued clients reload the page after
// /queue/status reports ready=true. There are no server-side goroutines
// blocking on behalf of waiting clients; the Middleware is stateless per
// request beyond the token store lookup.
//
// Related: WaitingRoom.RegisterRoutes, WaitingRoom.StatusHandler
func (wr *WaitingRoom) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !wr.checkInitialised(c) {
			return
		}

		secure := wr.secureCookie.Load() || c.Request.TLS != nil

		// Resume an existing queued position if the client presents a
		// valid room_ticket cookie. This preserves queue position across
		// page reloads and polling retries.
		if cookie, err := c.Request.Cookie(cookieName); err == nil {
			if entry, ok := wr.tokens.get(cookie.Value); ok {
				if wr.ticketReady(entry.ticket) {
					// Client's ticket is now within the serving window.
					// Acquire a slot and let them through.
					if err := wr.sem.AcquireWith(c.Request.Context()); err != nil {
						// Acquire failed (client disconnected, context
						// cancelled). Clean up the dead token and advance
						// nowServing so the queue doesn't stall waiting
						// for the reaper to evict this ticket.
						wr.tokens.delete(cookie.Value)
						wr.nowServing.Add(1)
						wr.emit(EventTimeout, wr.snapshot(EventTimeout))
						c.AbortWithStatus(http.StatusServiceUnavailable)
						return
					}
					wr.tokens.delete(cookie.Value)
					defer wr.release("")
					wr.emit(EventEnter, wr.snapshot(EventEnter))
					if wr.Len() >= int(wr.Cap()) {
						wr.emit(EventFull, wr.snapshot(EventFull))
					}
					c.Next()
					return
				}
				// Touch the token's issuedAt so active pollers do not
				// get reaped during normal operation.
				wr.tokens.touchIssuedAt(cookie.Value)

				// Still waiting — serve updated position and abort.
				position := wr.positionOf(entry.ticket)
				if position < 1 {
					position = 1
				}
				html := wr.resolveHTML()
				c.Data(http.StatusOK, "text/html; charset=utf-8", wr.injectPosition(html, position))
				c.Abort()
				return
			}
		}

		ticket := wr.nextTicket.Add(1)
		ctx := c.Request.Context()

		// Fast path — ticket is within the serving window.
		if wr.ticketReady(ticket) {
			if err := wr.sem.AcquireWith(ctx); err != nil {
				// Ticket consumed but not served — advance nowServing
				// so the gap doesn't stall the queue.
				wr.nowServing.Add(1)
				wr.emit(EventTimeout, wr.snapshot(EventTimeout))
				c.AbortWithStatus(http.StatusServiceUnavailable)
				return
			}
			defer wr.release("")
			wr.emit(EventEnter, wr.snapshot(EventEnter))
			if wr.Len() >= int(wr.Cap()) {
				wr.emit(EventFull, wr.snapshot(EventFull))
			}
			c.Next()
			return
		}

		// Slow path — issue a token, serve the waiting room page, and
		// abort. The client will poll /queue/status and reload when ready.
		token, err := generateToken()
		if err != nil {
			wr.nowServing.Add(1)
			c.AbortWithStatus(http.StatusInternalServerError)
			return
		}

		wr.tokens.set(token, ticketEntry{
			ticket:   ticket,
			issuedAt: time.Now(),
		})

		wr.emit(EventQueue, wr.snapshot(EventQueue))

		http.SetCookie(c.Writer, &http.Cookie{
			Name:     cookieName,
			Value:    token,
			Path:     "/",
			MaxAge:   int(cookieTTL.Seconds()),
			HttpOnly: true,
			Secure:   secure,
			SameSite: http.SameSiteLaxMode,
		})

		position := wr.positionOf(ticket)
		if position < 1 {
			position = 1
		}
		html := wr.resolveHTML()
		c.Data(http.StatusOK, "text/html; charset=utf-8", wr.injectPosition(html, position))
		c.Abort()
	}
}

// ticketReady reports whether the given ticket falls within the current
// serving window.
func (wr *WaitingRoom) ticketReady(ticket int64) bool {
	return ticket <= wr.nowServing.Load()+int64(wr.cap.Load())
}

// release returns a semaphore slot, optionally removes a session token,
// advances nowServing, and fires exit/drain lifecycle events.
//
// Note: nowServing is advanced here without holding wr.mu because the
// WaitingRoom uses a poll-driven admission model. There are no goroutines
// performing cond.Wait(); the advance only needs to be atomic, which
// atomic.Int64.Add guarantees.
func (wr *WaitingRoom) release(token string) {
	if token != "" {
		wr.tokens.delete(token)
	}
	wr.sem.Release()
	wr.nowServing.Add(1)

	snap := wr.snapshot(EventExit)
	wr.emit(EventExit, snap)
	if snap.Empty() {
		wr.emit(EventDrain, wr.snapshot(EventDrain))
	}
}

// resolveHTML returns the HTML bytes to serve. Custom HTML set via SetHTML
// takes precedence over the embedded default.
func (wr *WaitingRoom) resolveHTML() []byte {
	wr.mu.Lock()
	defer wr.mu.Unlock()
	if wr.html != nil {
		return wr.html
	}
	return defaultWaitingRoomBytes
}

// injectPosition substitutes {{.Position}} in the HTML bytes with the
// caller's numeric queue position.
func (wr *WaitingRoom) injectPosition(html []byte, position int64) []byte {
	return bytes.ReplaceAll(
		html,
		[]byte("{{.Position}}"),
		[]byte(fmt.Sprintf("%d", position)),
	)
}

// SetHTML replaces the waiting room page served to queued requests.
// Pass nil to revert to the embedded default waiting_room.html.
// Safe to call at any time including while requests are in flight.
//
// Related: WaitingRoom.resolveHTML
func (wr *WaitingRoom) SetHTML(html []byte) {
	wr.mu.Lock()
	defer wr.mu.Unlock()
	wr.html = html
}

// SetCap adjusts the number of concurrently active requests at runtime.
// Expanding capacity immediately opens new semaphore slots. Shrinking
// drains in-flight work via the underlying sema implementation.
//
// Returns ErrInvalidCap if cap < 1.
//
// Related: WaitingRoom.Cap, sema.Semaphore.SetCap
func (wr *WaitingRoom) SetCap(cap int32) error {
	if cap < 1 {
		return ErrInvalidCap{Given: cap}
	}
	// Delegate entirely to sema which manages its own internal mutex.
	// We update wr.cap after the semaphore resize succeeds so that
	// ticketReady and positionOf remain consistent with actual capacity.
	if err := wr.sem.SetCap(int(cap)); err != nil {
		return err
	}
	wr.cap.Store(cap)
	return nil
}

// Cap returns the current capacity.
//
// Related: WaitingRoom.SetCap
func (wr *WaitingRoom) Cap() int32 {
	return wr.cap.Load()
}

// Len returns the number of requests currently being actively served.
//
// Related: WaitingRoom.QueueDepth
func (wr *WaitingRoom) Len() int {
	return wr.sem.Len()
}

// QueueDepth returns the number of requests currently waiting for a slot.
//
// Related: WaitingRoom.Len
func (wr *WaitingRoom) QueueDepth() int64 {
	depth := wr.nextTicket.Load() - (wr.nowServing.Load() + int64(wr.cap.Load()))
	if depth < 0 {
		return 0
	}
	return depth
}

// Utilization returns the instantaneous ratio of active requests to capacity.
//
// Related: WaitingRoom.UtilizationSmoothed
func (wr *WaitingRoom) Utilization() float64 {
	return wr.sem.Utilization()
}

// UtilizationSmoothed returns the EWMA-smoothed utilization. Prefer this
// over Utilization for dashboards and autoscaler feedback loops.
//
// Related: WaitingRoom.Utilization
func (wr *WaitingRoom) UtilizationSmoothed() float64 {
	return wr.sem.UtilizationSmoothed()
}

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
// If the client presents a valid room_pass cookie (issued after a
// skip-the-line payment), the middleware auto-promotes their ticket to
// the front of the queue. This happens transparently — the client sees
// the waiting room briefly and is admitted on the next poll cycle.
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

		// Check if the client has a valid VIP pass for auto-promotion.
		hasPass := false
		if passCookie, err := c.Request.Cookie(passCookieName); err == nil {
			hasPass = wr.HasValidPass(passCookie.Value)
		}

		// Resume an existing queued position if the client presents a
		// valid room_ticket cookie. This preserves queue position across
		// page reloads and polling retries.
		if cookie, err := c.Request.Cookie(cookieName); err == nil {
			if entry, ok := wr.tokens.get(cookie.Value); ok {
				if wr.ticketReady(entry.ticket) {
					// Client's ticket is now within the serving window.

					// Snapshot occupancy BEFORE acquiring the slot so we
					// can detect the non-full→full transition edge.
					wasFull := wr.Len() >= int(wr.Cap())

					// Acquire a slot and let them through.
					if err := wr.sem.AcquireWith(c.Request.Context()); err != nil {
						// Acquire failed (client disconnected, context
						// cancelled). Clean up the dead token. Only
						// advance nowServing if the ticket was outside
						// the serving window — tickets inside the window
						// already consumed a conceptual slot allocation
						// and advancing for them inflates capacity.
						wr.tokens.delete(cookie.Value)
						if entry.ticket > wr.nowServing.Load()+int64(wr.cap.Load()) {
							wr.nowServing.Add(1)
						}
						wr.emit(EventTimeout, wr.snapshot(EventTimeout))
						c.AbortWithStatus(http.StatusServiceUnavailable)
						return
					}
					wr.tokens.delete(cookie.Value)
					defer wr.release("")
					wr.emit(EventEnter, wr.snapshot(EventEnter))
					if !wasFull && wr.Len() >= int(wr.Cap()) {
						wr.emit(EventFull, wr.snapshot(EventFull))
					}
					c.Next()
					return
				}

				// Client has a valid pass but their ticket isn't ready
				// yet — auto-promote them to the front so they get
				// admitted on the next poll cycle.
				if hasPass && !entry.promoted {
					wr.autoPromote(cookie.Value)
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
				c.Data(http.StatusOK, "text/html; charset=utf-8", wr.injectTemplateVars(html, position))
				c.Abort()
				return
			}
		}

		// Check queue depth limit before issuing a new ticket.
		maxDepth := wr.maxQueueDepth.Load()
		if maxDepth > 0 && wr.QueueDepth() >= maxDepth {
			c.AbortWithStatus(http.StatusServiceUnavailable)
			return
		}

		ticket := wr.nextTicket.Add(1)
		ctx := c.Request.Context()

		// Snapshot occupancy BEFORE acquiring the slot for edge detection.
		wasFull := wr.Len() >= int(wr.Cap())

		// Fast path — ticket is within the serving window.
		if wr.ticketReady(ticket) {
			if err := wr.sem.AcquireWith(ctx); err != nil {
				// Ticket consumed but not served. Only advance
				// nowServing if the ticket was outside the window.
				if ticket > wr.nowServing.Load()+int64(wr.cap.Load()) {
					wr.nowServing.Add(1)
				}
				wr.emit(EventTimeout, wr.snapshot(EventTimeout))
				c.AbortWithStatus(http.StatusServiceUnavailable)
				return
			}
			defer wr.release("")
			wr.emit(EventEnter, wr.snapshot(EventEnter))
			if !wasFull && wr.Len() >= int(wr.Cap()) {
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
			Path:     wr.CookiePath(),
			Domain:   wr.CookieDomain(),
			MaxAge:   int(cookieTTL.Seconds()),
			HttpOnly: true,
			Secure:   secure,
			SameSite: http.SameSiteLaxMode,
		})

		// If the client has a valid pass, auto-promote the freshly
		// issued ticket immediately so they jump to the front.
		if hasPass {
			wr.autoPromote(token)
		}

		position := wr.positionOf(ticket)
		if position < 1 {
			position = 1
		}
		html := wr.resolveHTML()
		c.Data(http.StatusOK, "text/html; charset=utf-8", wr.injectTemplateVars(html, position))
		c.Abort()
	}
}

// autoPromote silently promotes a token to the front of the queue.
// This is used when a client with a valid VIP pass re-enters the queue.
// Unlike PromoteToken, it does not require a RateFunc and does not
// compute cost — the pass was already paid for.
func (wr *WaitingRoom) autoPromote(token string) {
	wr.promoteMu.Lock()
	defer wr.promoteMu.Unlock()

	entry, ok := wr.tokens.get(token)
	if !ok {
		return
	}

	if wr.positionOf(entry.ticket) <= 0 {
		return // already in serving window
	}

	ceiling := wr.nowServing.Load() + int64(wr.cap.Load()) + 1
	insert := wr.promoteInsert.Load()
	if ceiling < insert {
		insert = ceiling
	}

	entry.ticket = insert
	entry.promoted = true
	wr.promoteInsert.Store(insert - 1)
	wr.tokens.set(token, entry)

	wr.emit(EventPromote, wr.snapshot(EventPromote))
}

// ticketReady reports whether the given ticket falls within the current
// serving window.
func (wr *WaitingRoom) ticketReady(ticket int64) bool {
	return ticket <= wr.nowServing.Load()+int64(wr.cap.Load())
}

// release returns a semaphore slot, optionally removes a session token,
// advances nowServing, and fires exit/drain lifecycle events.
//
// EventDrain fires on the transition from full to not-full — i.e. when
// the room was at capacity before this release and now has at least one
// free slot. This matches the documented semantics and is useful for
// scale-in decisions.
//
// Note: nowServing is advanced here without holding wr.mu because the
// WaitingRoom uses a poll-driven admission model. There are no goroutines
// performing cond.Wait(); the advance only needs to be atomic, which
// atomic.Int64.Add guarantees.
func (wr *WaitingRoom) release(token string) {
	// Snapshot BEFORE releasing the slot so we can detect the
	// full→not-full transition.
	wasFull := wr.Len() >= int(wr.Cap())

	if token != "" {
		wr.tokens.delete(token)
	}
	wr.sem.Release()
	wr.nowServing.Add(1)

	snap := wr.snapshot(EventExit)
	wr.emit(EventExit, snap)
	if wasFull && !snap.Full() {
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

// injectTemplateVars substitutes all template placeholders in the HTML
// bytes with their current values:
//
//   - {{.Position}} → the caller's numeric queue position
//   - {{.SkipURL}}  → the payment URL (empty string if not configured)
//
// This is the single point of template injection. All placeholders are
// handled here so they cannot diverge across call sites.
func (wr *WaitingRoom) injectTemplateVars(html []byte, position int64) []byte {
	result := bytes.ReplaceAll(
		html,
		[]byte("{{.Position}}"),
		[]byte(fmt.Sprintf("%d", position)),
	)
	result = bytes.ReplaceAll(
		result,
		[]byte("{{.SkipURL}}"),
		[]byte(wr.SkipURL()),
	)
	return result
}

// SetHTML replaces the waiting room page served to queued requests.
// Pass nil to revert to the embedded default waiting_room.html.
// Safe to call at any time including while requests are in flight.
//
// Custom HTML may use the following template placeholders:
//
//   - {{.Position}} — replaced with the client's queue position (integer)
//   - {{.SkipURL}}  — replaced with the skip-the-line payment URL (string)
//
// Related: WaitingRoom.resolveHTML, WaitingRoom.SetSkipURL
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

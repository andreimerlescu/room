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
// # Cookie dependency
//
// Queue position lives entirely in the room_ticket cookie. A client that
// cannot store it is, from the server's perspective, a new arrival on
// every request — it can never be admitted while a queue exists, and each
// attempt burns a ticket and a token-store entry. Every waiting-room
// render therefore also sets a non-HttpOnly probe cookie so the page can
// detect the condition and stop, rather than reloading forever. See
// setProbeCookie.
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
					filled := wr.enter()
					wr.tokens.delete(cookie.Value)
					defer wr.release("")
					wr.emit(EventEnter, wr.snapshot(EventEnter))
					if filled {
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

				// Refresh the probe alongside the position render. The
				// client demonstrably stores cookies (it just sent one),
				// but the probe has a finite MaxAge and must not expire
				// out from under a still-valid ticket — that would
				// produce a false "cookies disabled" panel.
				wr.setProbeCookie(c, secure)

				html := wr.resolveHTML()
				c.Data(http.StatusOK, "text/html; charset=utf-8", wr.injectTemplateVars(html, position))
				c.Abort()
				return
			}
		}

		// Check queue depth limit before issuing a new ticket.
		//
		// Two independent measures, either of which trips the breaker:
		//
		//   QueueDepth()    — derived from the monotonic ticket counter.
		//                     Includes tickets burned by clients that
		//                     never returned, so it over-reports.
		//   tokens.len()    — live queued clients only.
		//
		// Checking both means a flood of abandoned or cookieless arrivals
		// cannot silently consume the entire budget and 503 real users,
		// while a genuine backlog still trips it on the first measure.
		maxDepth := wr.maxQueueDepth.Load()
		if maxDepth > 0 &&
			(wr.QueueDepth() >= maxDepth || int64(wr.tokens.len()) >= maxDepth) {
			c.AbortWithStatus(http.StatusServiceUnavailable)
			return
		}

		ticket := wr.nextTicket.Add(1)
		ctx := c.Request.Context()

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
			filled := wr.enter()
			defer wr.release("")
			wr.emit(EventEnter, wr.snapshot(EventEnter))
			if filled {
				wr.emit(EventFull, wr.snapshot(EventFull))
			}
			c.Next()
			return
		}

		// Slow path — issue a token, serve the waiting room page, and
		// abort. The client will poll /queue/status and reload when ready.
		token, err := generateToken()
		if err != nil {
			// Ticket consumed but no token issued. Apply the same
			// window guard used elsewhere: advancing nowServing for a
			// within-window ticket inflates the serving window beyond
			// the configured capacity.
			if ticket > wr.nowServing.Load()+int64(wr.cap.Load()) {
				wr.nowServing.Add(1)
			}
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
			MaxAge:   int(wr.TokenTTL().Seconds()),
			HttpOnly: true,
			Secure:   secure,
			SameSite: http.SameSiteLaxMode,
		})
		wr.setProbeCookie(c, secure)

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

// enter records that this request has acquired a semaphore slot and
// reports whether THIS request is the one that took the room from below
// capacity to at capacity.
//
// The return value of atomic.Int32.Add is unique to the caller, so among
// any number of concurrent admissions exactly one observes the counter
// landing on cap. That is what makes EventFull an edge rather than a
// level: the previous implementation snapshotted wr.Len() before
// acquiring and re-read it afterwards, which let every goroutine involved
// in the same crossing observe "was below, now at" and emit EventFull.
// With cap=2 and two simultaneous arrivals that produced two EventFull
// emissions for one transition.
//
// Must be called exactly once per successful acquire, and paired with
// release (which calls exit).
//
// Note on SetCap: capacity is read once here. If SetCap changes capacity
// concurrently with an admission, a transition may be missed or attributed
// to a neighbouring request. The events remain edge-triggered; only their
// exact timing around a resize is approximate.
//
// Related: WaitingRoom.exit, WaitingRoom.release
func (wr *WaitingRoom) enter() bool {
	capacity := wr.cap.Load()
	return wr.occupancy.Add(1) == capacity
}

// exit records that a semaphore slot has been released and reports
// whether THIS release is the one that took the room from at capacity
// back to having a free slot.
//
// The mirror of enter: exactly one concurrent releaser sees the counter
// land on cap-1, so EventDrain fires once per crossing. It does NOT fire
// when occupancy merely falls toward zero from an already non-full state,
// which matches the documented semantics.
//
// Related: WaitingRoom.enter, WaitingRoom.release
func (wr *WaitingRoom) exit() bool {
	capacity := wr.cap.Load()
	return wr.occupancy.Add(-1) == capacity-1
}

// setProbeCookie writes the JS-readable probe cookie. It is deliberately
// NOT HttpOnly: the waiting room page reads it via document.cookie to
// confirm the browser is storing our cookies at all.
//
// Without this, a cookieless client is invisible to itself — room_ticket
// is HttpOnly, so the page has no way to distinguish "I have a queue
// position the server will honour" from "every request I make is a brand
// new arrival at the back of the line". It polls, is told ready=true
// (no cookie means no position to report), reloads, and starts over —
// forever, while burning a ticket and a token-store entry each cycle.
//
// The probe carries a constant value and no session meaning, so exposing
// it to script grants nothing: admission is decided solely by room_ticket.
// It is written with the same Path, Domain, Secure and SameSite attributes
// as the session cookie so that a misconfiguration which blocks one blocks
// both — a probe that survives while room_ticket is rejected would be
// worse than no probe at all.
//
// Related: WaitingRoom.Middleware, WaitingRoom.StatusHandler
func (wr *WaitingRoom) setProbeCookie(c *gin.Context, secure bool) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     probeCookieName,
		Value:    probeCookieValue,
		Path:     wr.CookiePath(),
		Domain:   wr.CookieDomain(),
		MaxAge:   int(wr.TokenTTL().Seconds()),
		HttpOnly: false,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
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
// free slot. The transition is determined by exit, whose atomic decrement
// attributes the crossing to exactly one releaser; comparing occupancy
// readings taken before and after the release would let several
// concurrent releasers claim the same crossing.
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
	drained := wr.exit()
	wr.nowServing.Add(1)

	wr.emit(EventExit, wr.snapshot(EventExit))
	if drained {
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
// Custom pages SHOULD replicate two behaviours from the default page or
// cookieless clients will reload indefinitely without ever being admitted:
//
//  1. Before the first poll, confirm document.cookie contains "room_probe".
//     If it does not, show an error and do not poll.
//  2. Treat a status response containing "cookies_required": true as
//     terminal — show an error and stop polling rather than reloading.
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

// Len returns the number of requests currently being actively served, as
// reported by the underlying semaphore.
//
// Related: WaitingRoom.QueueDepth
func (wr *WaitingRoom) Len() int {
	return wr.sem.Len()
}

// QueueDepth returns the number of requests currently waiting for a slot,
// derived from the monotonic ticket counter.
//
// This measure counts tickets, not clients. A ticket issued to a client
// that never returns (abandoned tab, disabled cookies, crawler) continues
// to contribute until the reaper evicts its token. For a count of clients
// the server can actually still admit, use LiveQueueDepth.
//
// Related: WaitingRoom.Len, WaitingRoom.LiveQueueDepth
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

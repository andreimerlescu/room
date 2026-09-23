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
// A returning client whose ticket is ready CLAIMS its token (removes it
// from the store) before acquiring a semaphore slot. If the reaper or a
// removal claimed it first, the client is treated as a new arrival. This
// guarantees each ticket is accounted for exactly once — see
// WaitingRoom.retire.
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
// Every waiting-room render — first issuance and every reload — sends
// both cookies with a fresh MaxAge; status polls refresh them in between
// (see StatusHandler).
//
// # Observability
//
// Each queued arrival records the SetClientKeyFunc key with its ticket
// and emits EventQueue carrying the issued token, the key, and whether
// the request presented a room_ticket the room did not recognise.
//
// Related: WaitingRoom.RegisterRoutes, WaitingRoom.StatusHandler
func (wr *WaitingRoom) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !wr.checkInitialised(c) {
			return
		}

		secure := wr.cookieSecure(c.Request)

		// Check if the client has a valid VIP pass for auto-promotion.
		hasPass := false
		if passCookie, err := c.Request.Cookie(passCookieName); err == nil {
			hasPass = wr.HasValidPass(passCookie.Value)
		}

		// Resume an existing queued position if the client presents a
		// valid room_ticket cookie. This preserves queue position across
		// page reloads and polling retries.
		cookie, cookieErr := c.Request.Cookie(cookieName)
		hadTicketCookie := cookieErr == nil
		if hadTicketCookie {
			if entry, ok := wr.tokens.get(cookie.Value); ok {
				if wr.ticketReady(entry.ticket) {
					// Client's ticket is now within the serving window.
					// Claim the token BEFORE acquiring: whoever removes
					// it from the store owns the ticket's accounting. If
					// we win, admit either releases (success) or retires
					// (acquire failure) the ticket — exactly once.
					if claimed, won := wr.tokens.take(cookie.Value); won {
						wr.admit(c, claimed.ticket)
						return
					}
					// Lost the claim: the reaper or a removal retired
					// this ticket between get and take. The client no
					// longer has a queue position, so fall through and
					// treat it as a new arrival.
				} else {
					// Client has a valid pass but their ticket isn't
					// ready yet — auto-promote them to the front so they
					// get admitted on the next poll cycle.
					if hasPass && !entry.promoted {
						wr.autoPromote(cookie.Value)
						// Re-read so the page shows the promoted
						// position rather than the stale one.
						if refreshed, ok := wr.tokens.get(cookie.Value); ok {
							entry = refreshed
						}
					}

					// The client just reloaded the waiting page: restart
					// its sliding TTL, mark it seen, restart its
					// cookie-refresh clock (both cookies are about to be
					// re-sent) and record its current pass status.
					wr.tokens.markRendered(cookie.Value, time.Now(), hasPass)

					// Still waiting — serve updated position and abort.
					position := wr.positionOf(entry.ticket)
					if position < 1 {
						position = 1
					}

					// Re-send both cookies with a fresh MaxAge. The
					// client demonstrably stores cookies (it just sent
					// one), but both have a finite MaxAge and must not
					// expire out from under a still-valid ticket — an
					// expired room_ticket makes the next poll report
					// cookies_required, and an expired probe makes the
					// page show a false "cookies disabled" panel.
					wr.setTicketCookie(c, cookie.Value, secure)
					wr.setProbeCookie(c, secure)

					html := wr.resolveHTML()
					c.Data(http.StatusOK, "text/html; charset=utf-8", wr.injectTemplateVars(html, position))
					c.Abort()
					return
				}
			}
		}

		// Check queue depth limit before issuing a new ticket.
		//
		// Two independent measures, and the breaker trips when EITHER
		// reaches the limit — whichever is higher wins, so the breaker is
		// deliberately pessimistic:
		//
		//   QueueDepth()    — ticket arithmetic, net of retired tickets
		//                     pending in the ghost ledger.
		//   tokens.len()    — live queued clients, including ones told
		//                     ready that have not reloaded yet.
		//
		// Abandoned and cookieless arrivals count toward both until they
		// are reaped, so a flood of them can trip the breaker; reclaiming
		// them faster (SetFirstPollGrace, SetReaperInterval) is the lever
		// for that. Clients already holding a live room_ticket were
		// handled above and are never rejected here.
		maxDepth := wr.maxQueueDepth.Load()
		if maxDepth > 0 &&
			(wr.QueueDepth() >= maxDepth || int64(wr.tokens.len()) >= maxDepth) {
			c.AbortWithStatus(http.StatusServiceUnavailable)
			return
		}

		ticket := wr.nextTicket.Add(1)

		// Fast path — ticket is within the serving window. The ticket was
		// issued to this request alone, so admit owns its accounting.
		if wr.ticketReady(ticket) {
			wr.admit(c, ticket)
			return
		}

		// Slow path — issue a token, serve the waiting room page, and
		// abort. The client will poll /queue/status and reload when ready.
		token, err := generateToken()
		if err != nil {
			// Ticket consumed but no token issued: it can never be
			// admitted, so retire it.
			wr.retire(ticket)
			c.AbortWithStatus(http.StatusInternalServerError)
			return
		}

		// Derive the client key outside any lock.
		key := wr.clientKey(c)

		now := time.Now()
		wr.tokens.set(token, ticketEntry{
			ticket:      ticket,
			createdAt:   now,
			issuedAt:    now,
			cookieSetAt: now,
			clientKey:   key,
			hasPass:     hasPass,
		})

		wr.emit(EventQueue, wr.snapshotFor(EventQueue, token, key, hadTicketCookie))

		wr.setTicketCookie(c, token, secure)
		wr.setProbeCookie(c, secure)

		// If the client has a valid pass, auto-promote the freshly
		// issued ticket immediately so they jump to the front.
		if hasPass {
			wr.autoPromote(token)
			if refreshed, ok := wr.tokens.get(token); ok {
				ticket = refreshed.ticket
			}
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

// admit acquires a semaphore slot for a ticket this request owns
// exclusively and runs the rest of the handler chain.
//
// "Owns exclusively" means either the ticket was just issued to this
// request (fast path) or this request won tokenStore.take for it (resume
// path). On acquire failure the ticket can never be admitted and is
// retired; on success, release accounts for it when the handler returns.
// Either way the ticket contributes exactly one nowServing advance.
//
// Related: WaitingRoom.release, WaitingRoom.retire
func (wr *WaitingRoom) admit(c *gin.Context, ticket int64) {
	if err := wr.sem.AcquireWith(c.Request.Context()); err != nil {
		// Client disconnected or its context was cancelled before a
		// slot opened. Retire the ticket so its place in the window is
		// not lost.
		wr.retire(ticket)
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

// cookieSecure reports whether cookies on this response should carry the
// Secure flag: always when SetSecureCookie(true), otherwise only for
// requests that arrived over TLS directly.
func (wr *WaitingRoom) cookieSecure(r *http.Request) bool {
	return wr.secureCookie.Load() || r.TLS != nil
}

// setTicketCookie writes the HttpOnly room_ticket session cookie with a
// fresh MaxAge = TokenTTL. It is used both to issue a new token and to
// re-send an existing one (same value) so the browser keeps it for as
// long as the client keeps waiting.
//
// Its Path, Domain, Secure and SameSite attributes must stay identical to
// setProbeCookie's — see there.
//
// Related: WaitingRoom.setProbeCookie, WaitingRoom.StatusHandler
func (wr *WaitingRoom) setTicketCookie(c *gin.Context, token string, secure bool) {
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
//
// It shares placement, fairness and concurrency behaviour with
// PromoteToken via moveToPosition; a token already in the serving window,
// or one removed concurrently, is left alone. EventPromote fires only if
// the token actually moved, with Snapshot.Token and Snapshot.ClientKey set.
//
// Related: WaitingRoom.PromoteToken, WaitingRoom.moveToPosition
func (wr *WaitingRoom) autoPromote(token string) {
	wr.promoteMu.Lock()
	defer wr.promoteMu.Unlock()

	p, err := wr.moveToPosition(token, 1)
	if err != nil || !p.moved {
		return
	}
	wr.emit(EventPromote, wr.snapshotFor(EventPromote, token, p.entry.clientKey, false))
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
// The advance goes through WaitingRoom.advance so that any retired
// tickets the widened window now reaches are skipped in the same step.
// No mutex or broadcast is needed: admission is poll-driven, so the
// advance only needs to be atomic.
func (wr *WaitingRoom) release(token string) {
	if token != "" {
		wr.tokens.delete(token)
	}
	wr.sem.Release()
	drained := wr.exit()
	wr.advance(1)

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
// Custom pages must also poll /queue/status with a same-origin (or
// credentialed cross-origin) fetch so that the cookie refreshes carried
// on poll responses are stored by the browser.
//
// Related: WaitingRoom.resolveHTML, WaitingRoom.SetSkipURL
func (wr *WaitingRoom) SetHTML(html []byte) {
	wr.mu.Lock()
	defer wr.mu.Unlock()
	wr.html = html
}

// SetCap adjusts the number of concurrently active requests at runtime.
// Expanding capacity immediately opens new semaphore slots and widens the
// serving window; any retired tickets the wider window now reaches are
// skipped at once. Shrinking drains in-flight work via the underlying
// sema implementation.
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
	wr.drainLedger()
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
// derived from the monotonic ticket counter net of retired tickets that
// the serving window has not reached yet (the ghost ledger).
//
// This measure counts tickets, not clients. A ticket issued to a client
// that never returns (abandoned tab, disabled cookies, crawler) continues
// to contribute until its token is reaped or removed; from that moment it
// is subtracted. For a count of clients the server can actually still
// admit, use LiveQueueDepth.
//
// Lock-free.
//
// Related: WaitingRoom.Len, WaitingRoom.LiveQueueDepth
func (wr *WaitingRoom) QueueDepth() int64 {
	depth := wr.nextTicket.Load() -
		(wr.nowServing.Load() + int64(wr.cap.Load())) -
		wr.ledger.len()
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

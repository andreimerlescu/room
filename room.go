package room

import (
	"bytes"
	_ "embed"
	"html"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
)

//go:embed waiting_room.html
var defaultWaitingRoomBytes []byte

var (
	placeholderPosition    = []byte("{{.Position}}")
	placeholderSkipURL     = []byte("{{.SkipURL}}")
	placeholderProbeCookie = []byte("{{.ProbeCookie}}")
)

// Middleware returns the gin.HandlerFunc that enforces the waiting room.
//
// Requests within the current serving window (nowServing + cap) acquire a
// semaphore slot and proceed immediately. Excess requests are issued a queued
// ticket (identified by the ticket cookie, or by an application key when
// [WaitingRoom.SetTicketKeyFunc] matches) and aborted with the waiting page
// or wait response until their ticket enters the serving window.
//
// A returning client claims its token atomically before acquiring a slot so
// each ticket is accounted for exactly once (admitted and released, or retired
// via the ghost ledger). Clients presenting a valid VIP pass (cookie or
// signed header) are auto-promoted to the front of the line while still
// acquiring a local slot within Cap. Clients presenting a verified signed
// lease reserve a slot in the bounded lease pool up to LeaseCeiling, queuing
// at the front of the general line when the pool is full.
func (wr *WaitingRoom) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !wr.checkInitialised(c) {
			return
		}

		secure := wr.cookieSecure(c.Request)

		if key, ok := wr.ticketKey(c); ok {
			if err := validKey(key); err != nil {
				c.AbortWithStatus(http.StatusBadRequest)
				return
			}
			capState, proceed := wr.resolveCapabilities(c, key)
			if !proceed {
				return
			}
			wr.serveKeyed(c, key, capState)
			return
		}

		capState, proceed := wr.resolveCapabilities(c, "")
		if !proceed {
			return
		}
		hasPass := capState.hasPass

		cookie, cookieErr := c.Request.Cookie(wr.TicketCookieName())
		hadTicketCookie := cookieErr == nil
		if hadTicketCookie {
			// A keyed ticket's internal token is ignored in cookies.
			if entry, ok := wr.tokens.get(cookie.Value); ok && entry.key == "" {
				if wr.ticketReady(entry.ticket) {
					if claimed, won := wr.tokens.take(cookie.Value); won {
						wr.admit(c, claimed.ticket)
						return
					}
					// Claim lost to reaper or removal; treat as new arrival.
				} else {
					if hasPass && !entry.promoted {
						wr.autoPromote(cookie.Value)
						if refreshed, ok := wr.tokens.get(cookie.Value); ok {
							entry = refreshed
						}
					}

					wr.tokens.markRendered(cookie.Value, time.Now(), hasPass)

					position := wr.positionOf(entry.ticket)
					if position < 1 {
						position = 1
					}

					wr.setTicketCookie(c.Writer, cookie.Value, secure)
					wr.setProbeCookie(c.Writer, secure)

					if wr.respondCookieWait(c, cookie.Value, position) {
						return
					}
					page := wr.resolveHTML()
					c.Data(http.StatusOK, "text/html; charset=utf-8", wr.injectTemplateVars(page, position))
					c.Abort()
					return
				}
			}
		}

		// Pessimistic circuit breaker: trips when either ticket-counter depth
		// or live token count reaches maxQueueDepth.
		maxDepth := wr.maxQueueDepth.Load()
		if maxDepth > 0 &&
			(wr.QueueDepth() >= maxDepth || int64(wr.tokens.len()) >= maxDepth) {
			c.AbortWithStatus(http.StatusServiceUnavailable)
			return
		}

		ticket := wr.nextTicket.Add(1)

		if wr.ticketReady(ticket) {
			wr.admit(c, ticket)
			return
		}

		token, err := generateToken()
		if err != nil {
			wr.retire(ticket)
			c.AbortWithStatus(http.StatusInternalServerError)
			return
		}

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

		wr.setTicketCookie(c.Writer, token, secure)
		wr.setProbeCookie(c.Writer, secure)

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
		if wr.respondCookieWait(c, token, position) {
			return
		}
		page := wr.resolveHTML()
		c.Data(http.StatusOK, "text/html; charset=utf-8", wr.injectTemplateVars(page, position))
		c.Abort()
	}
}

// admit acquires a semaphore slot for an exclusively owned ticket and runs
// the downstream handler chain. On acquire failure the ticket is retired; on
// success release accounts for it when the handler returns.
func (wr *WaitingRoom) admit(c *gin.Context, ticket int64) {
	if err := wr.sem.AcquireWith(c.Request.Context()); err != nil {
		wr.retire(ticket)
		wr.emit(EventTimeout, wr.snapshot(EventTimeout))
		c.AbortWithStatus(http.StatusServiceUnavailable)
		return
	}
	filled := wr.enter()
	defer wr.release()
	wr.emit(EventEnter, wr.snapshot(EventEnter))
	if filled {
		wr.emit(EventFull, wr.snapshot(EventFull))
	}
	c.Next()
}

// enter increments occupancy and reports whether this admission crossed from
// below capacity to at capacity (edge-triggered EventFull).
func (wr *WaitingRoom) enter() bool {
	capacity := wr.cap.Load()
	return wr.occupancy.Add(1) == capacity
}

// exit decrements occupancy and reports whether this release crossed from
// at capacity back to having a free slot (edge-triggered EventDrain).
func (wr *WaitingRoom) exit() bool {
	capacity := wr.cap.Load()
	return wr.occupancy.Add(-1) == capacity-1
}

func (wr *WaitingRoom) cookieSecure(r *http.Request) bool {
	return wr.secureCookie.Load() || r.TLS != nil
}

// setTicketCookie writes the HttpOnly ticket cookie with MaxAge = TokenTTL.
func (wr *WaitingRoom) setTicketCookie(w http.ResponseWriter, token string, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     wr.TicketCookieName(),
		Value:    token,
		Path:     wr.CookiePath(),
		Domain:   wr.CookieDomain(),
		MaxAge:   int(wr.TokenTTL().Seconds()),
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// setProbeCookie writes the non-HttpOnly probe cookie with the same
// attributes as the ticket cookie so the waiting page can detect
// cookie-blocking browsers before polling.
func (wr *WaitingRoom) setProbeCookie(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     wr.ProbeCookieName(),
		Value:    probeCookieValue,
		Path:     wr.CookiePath(),
		Domain:   wr.CookieDomain(),
		MaxAge:   int(wr.TokenTTL().Seconds()),
		HttpOnly: false,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// autoPromote silently moves a token to position 1 for a valid VIP pass.
func (wr *WaitingRoom) autoPromote(token string) {
	wr.promoteMu.Lock()
	defer wr.promoteMu.Unlock()

	p, err := wr.moveToPosition(token, 1)
	if err != nil || !p.moved {
		return
	}
	wr.emit(EventPromote, wr.snapshotFor(EventPromote, token, p.entry.clientKey, false))
}

func (wr *WaitingRoom) ticketReady(ticket int64) bool {
	return ticket <= wr.nowServing.Load()+int64(wr.cap.Load())
}

// release returns a semaphore slot, advances nowServing (draining any reached
// ghosts), and fires EventExit and edge-triggered EventDrain.
func (wr *WaitingRoom) release() {
	wr.sem.Release()
	drained := wr.exit()
	wr.advance(1)

	wr.emit(EventExit, wr.snapshot(EventExit))
	if drained {
		wr.emit(EventDrain, wr.snapshot(EventDrain))
	}
}

func (wr *WaitingRoom) resolveHTML() []byte {
	wr.mu.Lock()
	defer wr.mu.Unlock()
	if wr.html != nil {
		return wr.html
	}
	return defaultWaitingRoomBytes
}

// injectTemplateVars substitutes {{.Position}}, HTML-escaped {{.SkipURL}},
// and {{.ProbeCookie}} into page bytes.
func (wr *WaitingRoom) injectTemplateVars(page []byte, position int64) []byte {
	result := bytes.ReplaceAll(page, placeholderPosition, strconv.AppendInt(nil, position, 10))
	result = bytes.ReplaceAll(result, placeholderSkipURL, []byte(html.EscapeString(wr.SkipURL())))
	return bytes.ReplaceAll(result, placeholderProbeCookie, []byte(wr.ProbeCookieName()))
}

// SetHTML replaces the waiting room page served to queued requests. Pass nil
// to revert to the embedded default. Safe to call at any time.
//
// Supported placeholders: {{.Position}} (integer) and {{.SkipURL}}
// (HTML-escaped string). Custom pages should verify document.cookie contains
// "room_probe", stop polling on "cookies_required": true, and poll
// /queue/status with credentials so cookie refreshes are stored.
func (wr *WaitingRoom) SetHTML(page []byte) {
	wr.mu.Lock()
	defer wr.mu.Unlock()
	wr.html = page
}

// SetCap adjusts the number of concurrently active requests at runtime.
// Expanding capacity immediately opens new semaphore slots and widens the
// serving window; any retired tickets the wider window now reaches are
// skipped at once. Shrinking preserves all in-flight requests and leases;
// no new slot is granted until occupancy drops below the new capacity, so
// the room never over-admits during a shrink.
//
// Returns [ErrInvalidCap] if cap < 1.
func (wr *WaitingRoom) SetCap(cap int32) error {
	if cap < 1 {
		return ErrInvalidCap{Given: cap}
	}
	if err := wr.sem.SetCap(int(cap)); err != nil {
		return err
	}
	wr.cap.Store(cap)
	wr.drainLedger()
	return nil
}

// Cap returns the current capacity.
func (wr *WaitingRoom) Cap() int32 {
	return wr.cap.Load()
}

// Len returns the number of slots currently occupied in the underlying
// semaphore (active requests plus held leases).
func (wr *WaitingRoom) Len() int {
	return wr.sem.Len()
}

// QueueDepth returns the number of waiting tickets derived from the monotonic
// ticket counter net of pending retired tickets in the ghost ledger. Lock-free.
func (wr *WaitingRoom) QueueDepth() int64 {
	depth := wr.nextTicket.Load() -
		(wr.nowServing.Load() + int64(wr.cap.Load())) -
		wr.ledger.len()
	if depth < 0 {
		return 0
	}
	return depth
}

// Utilization returns the instantaneous ratio of active slots to capacity,
// clamped to [0.0, 1.0].
func (wr *WaitingRoom) Utilization() float64 {
	return wr.sem.Utilization()
}

// UtilizationSmoothed returns the EWMA-smoothed utilization in [0.0, 1.0].
func (wr *WaitingRoom) UtilizationSmoothed() float64 {
	return wr.sem.UtilizationSmoothed()
}

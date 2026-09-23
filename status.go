package room

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// cookieRefreshDivisor sets how often, relative to TokenTTL, a waiting
// client's cookies are re-sent on status polls: at most once per
// TokenTTL/cookieRefreshDivisor. With 3, two consecutive refresh
// responses can be lost before the browser cookie expires.
const cookieRefreshDivisor = 3

// StatusHandler returns a gin.HandlerFunc that serves /queue/status.
// Register it on your router BEFORE the WaitingRoom middleware so that
// polling requests from the waiting room page bypass the queue entirely.
//
// The handler reads the room_ticket cookie set when the client was first
// placed in the waiting room. If the ticket is present but unknown or
// expired, it returns ready=true so the client retries the original
// request and either enters or re-queues cleanly. If the cookie is absent
// entirely, it returns cookies_required=true instead — see below.
//
// A token found expired on poll is retired (so its place in the serving
// window is not lost) and EventEvict fires, exactly as if the reaper had
// found it.
//
// # Keep-alive
//
// Every poll for a live token — including a rate-limited one — refreshes
// the token's sliding TTL and marks it seen, so an actively polling
// client is never reaped. The whole poll is applied under a single lock;
// see tokenStore.poll.
//
// # Cookie refresh
//
// room_ticket and room_probe are issued with MaxAge = TokenTTL, and the
// waiting page never reloads while it polls. To stop the browser from
// discarding them during a long wait, an accepted poll from a client that
// is still waiting re-sends both cookies — same value, same attributes,
// fresh MaxAge — at most once per TokenTTL/3. Rate-limited responses
// never carry Set-Cookie.
//
// # Rate limit
//
// A per-token rate limit prevents clients from hammering this endpoint
// faster than statusPollMinInterval. Polls arriving too quickly receive
// a Retry-After header and a 429 status.
//
// When a RateFunc is configured via SetRateFunc, the response includes
// skip_cost (cost to jump to position 1) and rate_per_pos (current
// per-position rate) so the waiting room page can display pricing.
//
// Related: WaitingRoom.Middleware, WaitingRoom.RegisterRoutes
func (wr *WaitingRoom) StatusHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !wr.checkInitialised(c) {
			return
		}

		cookie, err := c.Request.Cookie(cookieName)
		if err != nil {
			// No room_ticket cookie at all.
			//
			// This previously returned ready=true, reasoning that a client
			// with no queue position should retry the original request.
			// That is correct for a client that never queued — but it is
			// catastrophic for a client that CANNOT store cookies: the
			// waiting room page reads ready=true, reloads, is handed a
			// fresh ticket at the back of the line, and repeats every few
			// seconds indefinitely, never admitted, leaking a token-store
			// entry and a nextTicket increment per cycle.
			//
			// Signal the condition explicitly instead. The page stops
			// polling and surfaces an error.
			//
			// This does not affect the normal admission flow: a client
			// that was just admitted still HAS the cookie (now pointing
			// at a deleted token) and so takes the ready=true path below.
			// Only a client with no cookie at all reaches here, and for
			// such a client ready=true was never actionable anyway.
			c.JSON(http.StatusOK, statusResponse{
				Ready:           false,
				CookiesRequired: true,
			})
			return
		}

		now := time.Now()
		edge := wr.nowServing.Load() + int64(wr.cap.Load())
		res := wr.tokens.poll(
			cookie.Value,
			now,
			edge,
			statusPollMinInterval,
			wr.TokenTTL()/cookieRefreshDivisor,
		)

		switch res.outcome {
		case pollUnknown:
			// Admitted, removed, reaped, or from another process —
			// send the client back to the main handler.
			c.JSON(http.StatusOK, statusResponse{Ready: true})
			return

		case pollExpired:
			// The removed entry is ours alone, so we retire its ticket:
			// without this, an expired ticket discovered here would never
			// advance the window and would permanently occupy a slot.
			wr.retire(res.entry.ticket)
			wr.emit(EventEvict, wr.snapshot(EventEvict))
			c.JSON(http.StatusOK, statusResponse{Ready: true})
			return

		case pollRateLimited:
			// Shed the excess poll. The token was still kept alive by
			// poll, so a fast poller is throttled, not reaped.
			c.Header("Retry-After", "1")
			c.JSON(http.StatusTooManyRequests, statusResponse{
				Ready:    false,
				Position: wr.positionOf(res.entry.ticket),
			})
			return
		}

		// Accepted poll. Re-send cookies first (headers must precede the
		// body) if poll decided they are due.
		if res.refreshCookie {
			secure := wr.cookieSecure(c.Request)
			wr.setTicketCookie(c, cookie.Value, secure)
			wr.setProbeCookie(c, secure)
		}

		position := wr.positionOf(res.entry.ticket)
		if position <= 0 {
			c.JSON(http.StatusOK, statusResponse{Ready: true})
			return
		}

		// Check if the client has a valid VIP pass.
		hasPass := false
		if passCookie, err := c.Request.Cookie(passCookieName); err == nil {
			hasPass = wr.HasValidPass(passCookie.Value)
		}

		// Build the response with optional pricing information.
		resp := statusResponse{
			Ready:       false,
			Position:    position,
			Utilization: wr.sem.UtilizationSmoothed(),
			HasPass:     hasPass,
		}

		// Only show pricing if the client does NOT already have a pass.
		if !hasPass {
			if fn := wr.rateFuncLoad(); fn != nil {
				resp.RatePerPos = fn(wr.QueueDepth())
				if position > 1 {
					resp.SkipCost = float64(position-1) * resp.RatePerPos
				}
			}
		}

		c.JSON(http.StatusOK, resp)
	}
}

// positionOf returns the queue position for a ticket.
//
// A value <= 0 means the ticket is within the serving window and eligible
// for admission; this is exactly the ticketReady condition, so the status
// endpoint and the middleware can never disagree about readiness.
//
// For a waiting ticket the value is the raw distance past the window edge
// minus the retired tickets (ghost ledger entries) ahead of it, clamped to
// a minimum of 1. The subtraction is what makes positions behind a
// removed or abandoned ticket improve immediately, while positions ahead
// of it are unaffected.
//
// This is the single authoritative formula for queue position used by
// StatusHandler, Middleware and the promotion code. Having one
// implementation prevents the call sites from silently diverging.
func (wr *WaitingRoom) positionOf(ticket int64) int64 {
	raw := ticket - wr.nowServing.Load() - int64(wr.cap.Load())
	if raw <= 0 {
		return raw
	}
	pos := raw - wr.ledger.countBelow(ticket)
	if pos < 1 {
		pos = 1
	}
	return pos
}

// RegisterRoutes registers GET /queue/status on the given gin.Engine and
// then attaches the WaitingRoom middleware. It ensures the status endpoint
// always bypasses the queue — if you register routes manually, always add
// StatusHandler before Use(Middleware()).
//
// # Multiple engines
//
// RegisterRoutes may be called on any number of gin.Engines for the same
// WaitingRoom — for example, building a fresh engine per configuration
// "generation" and swapping it in atomically. It is side-effect free with
// respect to the WaitingRoom: it starts no goroutines (the reaper belongs
// to Init), registers no callbacks, and keeps no per-engine state; every
// engine's handlers are closures over the same shared WaitingRoom, so
// in-flight requests on an old engine and new requests on a new one see
// one queue. Calling it twice on the SAME engine panics, because gin
// rejects duplicate route registrations.
//
// # CORS note
//
// If your deployment serves the waiting-room page from a different origin
// than the API (or if any CORS middleware is active), register an OPTIONS
// handler for /queue/status as well so that preflight requests from the
// polling fetch() call succeed:
//
//	r.OPTIONS("/queue/status", func(c *gin.Context) { c.Status(http.StatusNoContent) })
//	r.GET("/queue/status", wr.StatusHandler())
//	r.Use(wr.Middleware())
//
// Note that a cross-origin deployment must also send credentials on the
// polling fetch and allow them in the CORS response, or the room_ticket
// cookie will not accompany the poll and every client will be reported
// as cookieless — and cookie refreshes on poll responses will not be
// stored.
//
// Usage:
//
//	wr := &room.WaitingRoom{}
//	wr.Init(500)
//	wr.RegisterRoutes(r)
//
// Related: WaitingRoom.StatusHandler, WaitingRoom.Middleware
func (wr *WaitingRoom) RegisterRoutes(r *gin.Engine) {
	r.OPTIONS("/queue/status", func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})
	r.GET("/queue/status", wr.StatusHandler())
	r.Use(wr.Middleware())
}

// generateToken returns a cryptographically random hex string suitable
// for use as a waiting room session token.
func generateToken() (string, error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

package room

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// StatusHandler returns a gin.HandlerFunc that serves /queue/status.
// Register it on your router BEFORE the WaitingRoom middleware so that
// polling requests from the waiting room page bypass the queue entirely.
//
// The handler reads the room_ticket cookie set when the client was first
// placed in the waiting room. If the ticket is not found or has expired,
// it returns ready=true so the client retries the original request and
// either enters or re-queues cleanly.
//
// Each successful status poll (where the client is still actively waiting)
// refreshes the token's issuedAt timestamp, preventing the reaper from
// evicting tokens that belong to actively polling clients. This makes the
// effective TTL a sliding window from the last poll rather than a fixed
// window from initial issuance.
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
			// No cookie — client has no queued position; send them back
			// to try the main handler.
			c.JSON(http.StatusOK, statusResponse{Ready: true})
			return
		}

		// Use deleteIfExpired to atomically check and remove in a single
		// write-lock scope, eliminating the TOCTOU window between a
		// separate isExpired check and delete.
		if wr.tokens.deleteIfExpired(cookie.Value) {
			c.JSON(http.StatusOK, statusResponse{Ready: true})
			return
		}

		entry, ok := wr.tokens.get(cookie.Value)
		if !ok {
			// Token was deleted between deleteIfExpired and get — treat
			// as expired/admitted.
			c.JSON(http.StatusOK, statusResponse{Ready: true})
			return
		}

		// Per-token poll rate limiting. If the client is polling faster
		// than statusPollMinInterval, return 429 with a Retry-After
		// header to shed excess load without touching the token store
		// write lock repeatedly.
		if prevPoll, found := wr.tokens.touchLastPoll(cookie.Value); found {
			if !prevPoll.IsZero() && time.Since(prevPoll) < statusPollMinInterval {
				c.Header("Retry-After", "1")
				c.JSON(http.StatusTooManyRequests, statusResponse{
					Ready:    false,
					Position: wr.positionOf(entry.ticket),
				})
				return
			}
		}

		position := wr.positionOf(entry.ticket)
		if position <= 0 {
			c.JSON(http.StatusOK, statusResponse{Ready: true})
			return
		}

		// Client is still actively waiting — refresh the sliding TTL so
		// that the reaper does not evict tokens from polling clients.
		wr.tokens.touchIssuedAt(cookie.Value)

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

// positionOf returns the raw queue position for a ticket. A value <= 0
// means the ticket is within the serving window and eligible for admission.
// Callers that need a display-safe value (minimum 1) should clamp separately.
//
// This is the single authoritative formula for queue position used by both
// StatusHandler and Middleware. Having one implementation prevents the two
// call sites from silently diverging during future edits.
func (wr *WaitingRoom) positionOf(ticket int64) int64 {
	return ticket - wr.nowServing.Load() - int64(wr.cap.Load())
}

// RegisterRoutes registers GET /queue/status on the given gin.Engine and
// then attaches the WaitingRoom middleware. It ensures the status endpoint
// always bypasses the queue — if you register routes manually, always add
// StatusHandler before Use(Middleware()).
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

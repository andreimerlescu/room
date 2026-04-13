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
// Related: WaitingRoom.Middleware, WaitingRoom.RegisterRoutes
func (wr *WaitingRoom) StatusHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !wr.checkInitialised(c) {
			return
		}

		cookie, err := c.Request.Cookie(cookieName)
		if err != nil {
			c.JSON(http.StatusOK, statusResponse{Ready: true})
			return
		}

		entry, ok := wr.tokens.get(cookie.Value)
		if !ok {
			c.JSON(http.StatusOK, statusResponse{Ready: true})
			return
		}

		if wr.tokens.isExpired(cookie.Value) {
			wr.tokens.delete(cookie.Value)
			c.JSON(http.StatusOK, statusResponse{Ready: true})
			return
		}

		position := wr.positionOf(entry.ticket)
		if position <= 0 {
			c.JSON(http.StatusOK, statusResponse{Ready: true})
			return
		}

		c.JSON(http.StatusOK, statusResponse{
			Ready:       false,
			Position:    position,
			Utilization: wr.sem.UtilizationSmoothed(),
		})
	}
}

// positionOf returns the raw queue position for a ticket. A value <= 0
// means the ticket is within the serving window and eligible for admission.
// Callers that need a display-safe value (minimum 1) should clamp separately.
func (wr *WaitingRoom) positionOf(ticket int64) int64 {
	return ticket - wr.nowServing.Load() - int64(wr.cap.Load())
}

// RegisterRoutes registers GET /queue/status on the given gin.Engine and
// then attaches the WaitingRoom middleware. It ensures the status endpoint
// always bypasses the queue — if you register routes manually, always add
// StatusHandler before Use(Middleware()).
//
// Usage:
//
//	wr := &room.WaitingRoom{}
//	wr.Init(500)
//	wr.RegisterRoutes(r)
//
// This is equivalent to:
//
//	r.GET("/queue/status", wr.StatusHandler())
//	r.Use(wr.Middleware())
//
// Related: WaitingRoom.StatusHandler, WaitingRoom.Middleware
func (wr *WaitingRoom) RegisterRoutes(r *gin.Engine) {
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

// issuedAt records when a token was created.
// Used by the reaper to enforce cookieTTL eviction.
func (ts *tokenStore) isExpired(token string) bool {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	entry, ok := ts.entries[token]
	if !ok {
		return true
	}
	return time.Since(entry.issuedAt) > cookieTTL
}

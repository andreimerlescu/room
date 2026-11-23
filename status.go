package room

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// cookieRefreshDivisor controls how often a waiting client's cookies are
// re-sent on status polls (at most once per TokenTTL/3).
const cookieRefreshDivisor = 3

// StatusHandler returns the gin.HandlerFunc serving GET /queue/status.
// Register it before [WaitingRoom.Middleware] so status polls bypass the queue.
//
// Polls carrying no room_ticket cookie receive cookies_required=true so
// cookieless browsers stop polling rather than looping. Live polls refresh the
// token's sliding TTL and re-send cookies at most once per TokenTTL/3; polls
// faster than 1s per token receive 429 Too Many Requests with Retry-After: 1
// while still refreshing the token's TTL.
func (wr *WaitingRoom) StatusHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		wr.serveStatus(c.Writer, c.Request)
	}
}

func (wr *WaitingRoom) serveStatus(w http.ResponseWriter, r *http.Request) {
	if !wr.initialised.Load() {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	cookie, err := r.Cookie(cookieName)
	if err != nil {
		writeJSON(w, http.StatusOK, statusResponse{
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
		writeJSON(w, http.StatusOK, statusResponse{Ready: true})
		return
	case pollExpired:
		wr.retire(res.entry.ticket)
		wr.emit(EventEvict, wr.snapshot(EventEvict))
		writeJSON(w, http.StatusOK, statusResponse{Ready: true})
		return
	case pollRateLimited:
		w.Header().Set("Retry-After", "1")
		writeJSON(w, http.StatusTooManyRequests, statusResponse{
			Ready:    false,
			Position: wr.positionOf(res.entry.ticket),
		})
		return
	}

	if res.refreshCookie {
		secure := wr.cookieSecure(r)
		wr.setTicketCookie(w, cookie.Value, secure)
		wr.setProbeCookie(w, secure)
	}

	position := wr.positionOf(res.entry.ticket)
	if position <= 0 {
		writeJSON(w, http.StatusOK, statusResponse{Ready: true})
		return
	}

	hasPass := false
	if passCookie, err := r.Cookie(passCookieName); err == nil {
		hasPass = wr.HasValidPass(passCookie.Value)
	}

	resp := statusResponse{
		Ready:       false,
		Position:    position,
		Utilization: wr.sem.UtilizationSmoothed(),
		HasPass:     hasPass,
	}
	if !hasPass {
		if fn := wr.rateFuncLoad(); fn != nil {
			resp.RatePerPos = fn(wr.QueueDepth())
			if position > 1 {
				resp.SkipCost = float64(position-1) * resp.RatePerPos
			}
		}
	}

	writeJSON(w, http.StatusOK, resp)
}

// positionOf returns the window-relative queue position for ticket.
// Values <= 0 mean ticket is within the serving window (matches ticketReady).
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

// RegisterRoutes registers OPTIONS /queue/status, GET /queue/status, and
// GET /queue/status/:ticket on r, then attaches [WaitingRoom.Middleware].
// Safe to call across multiple gin.Engines sharing one WaitingRoom (calling
// twice on the same engine panics via gin's duplicate route check).
func (wr *WaitingRoom) RegisterRoutes(r *gin.Engine) {
	r.OPTIONS("/queue/status", func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})
	r.GET("/queue/status", wr.StatusHandler())
	r.GET("/queue/status/:ticket", wr.TicketStatusHandler())
	r.Use(wr.Middleware())
}

func generateToken() (string, error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

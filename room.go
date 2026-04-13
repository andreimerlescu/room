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
// is served and the goroutine blocks until its ticket is called or the
// request context is cancelled.
//
// On exit the slot is released, nowServing advances, and all waiting
// goroutines are broadcast so the next ticket can proceed.
//
// Related: WaitingRoom.RegisterRoutes, WaitingRoom.StatusHandler
func (wr *WaitingRoom) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		ticket := wr.nextTicket.Add(1)
		ctx := c.Request.Context()
		token := ""

		// Fast path — ticket is within the serving window.
		if wr.ticketReady(ticket) {
			if err := wr.sem.AcquireWith(ctx); err != nil {
				c.AbortWithStatus(http.StatusServiceUnavailable)
				return
			}
			defer wr.release(token)
			c.Next()
			return
		}

		// Slow path — serve waiting room and issue a session token.
		if err := wr.serveWaitingRoom(c, ticket); err != nil {
			c.AbortWithStatus(http.StatusInternalServerError)
			return
		}

		if cookie, err := c.Request.Cookie(cookieName); err == nil {
			token = cookie.Value
		}

		// cancelWatcher broadcasts to cond when ctx is cancelled so the
		// blocked cond.Wait() below wakes up and can check ctx.Done().
		waitDone := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
				wr.cond.Broadcast()
			case <-waitDone:
			}
		}()

		// Block until our ticket is called or context is cancelled.
		wr.mu.Lock()
		for !wr.ticketReady(ticket) {
			select {
			case <-ctx.Done():
				wr.mu.Unlock()
				close(waitDone)
				wr.tokens.delete(token)
				c.AbortWithStatus(http.StatusServiceUnavailable)
				return
			default:
			}
			wr.cond.Wait()
		}
		wr.mu.Unlock()
		close(waitDone)

		if err := wr.sem.AcquireWith(ctx); err != nil {
			wr.tokens.delete(token)
			c.AbortWithStatus(http.StatusServiceUnavailable)
			return
		}
		defer wr.release(token)
		c.Next()
	}
}

// ticketReady reports whether the given ticket falls within the current
// serving window.
func (wr *WaitingRoom) ticketReady(ticket int64) bool {
	return ticket <= wr.nowServing.Load()+int64(wr.cap)
}

// release returns a semaphore slot, removes the session token, advances
// nowServing, and broadcasts to all waiting goroutines.
func (wr *WaitingRoom) release(token string) {
	if token != "" {
		wr.tokens.delete(token)
	}
	wr.sem.Release()
	wr.nowServing.Add(1)
	wr.cond.Broadcast()
}

// serveWaitingRoom generates a session token, sets the cookie, and writes
// the waiting room HTML to the response.
func (wr *WaitingRoom) serveWaitingRoom(c *gin.Context, ticket int64) error {
	token, err := generateToken()
	if err != nil {
		return err
	}

	wr.tokens.set(token, ticketEntry{
		ticket:   ticket,
		issuedAt: time.Now(),
	})

	http.SetCookie(c.Writer, &http.Cookie{
		Name:     cookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   int(cookieTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})

	position := ticket - wr.nowServing.Load()
	html := wr.resolveHTML()
	c.Data(http.StatusOK, "text/html; charset=utf-8", wr.injectPosition(html, position))
	return nil
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
// Expanding capacity immediately admits waiting tickets by broadcasting
// to all blocked goroutines so they can recheck ticketReady against the
// new cap value. Shrinking drains in-flight work first.
//
// Returns ErrInvalidCap if cap < 1.
//
// Related: WaitingRoom.Cap, sema.Semaphore.SetCap
func (wr *WaitingRoom) SetCap(cap int32) error {
	if cap < 1 {
		return ErrInvalidCap{Given: cap}
	}
	wr.cap = cap
	if err := wr.sem.SetCap(int(cap)); err != nil {
		return err
	}
	// Wake all waiters so they recheck ticketReady against the new cap.
	wr.cond.Broadcast()
	return nil
}

// Cap returns the current capacity.
//
// Related: WaitingRoom.SetCap
func (wr *WaitingRoom) Cap() int32 {
	return wr.cap
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
	depth := wr.nextTicket.Load() - (wr.nowServing.Load() + int64(wr.cap))
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

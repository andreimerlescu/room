package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/andreimerlescu/room"
	"github.com/gin-gonic/gin"
)

// wr is the WaitingRoom instance. Keeping it package-level lets you call
// wr.SetCap, wr.SetReaperInterval, or wr.On from a config-reload handler
// without restarting the server.
var wr *room.WaitingRoom

// secureCookies mirrors wr.SetSecureCookie so cookies this app sets itself
// (room_pass) use the same Secure rule as the room's own cookies. Set
// SECURE_COOKIES=1 when serving over HTTPS or behind a TLS terminator.
var secureCookies = os.Getenv("SECURE_COOKIES") == "1"

func main() {
	// ── 1. Use gin.New() instead of gin.Default() ─────────────────────────
	//
	// gin.Default() installs gin's own Logger middleware, which buffers
	// output and formats it after the handler returns. That makes it hard
	// to see room events interleaved with request logs in real time.
	// gin.New() gives us a blank engine so we can install our own logger
	// that prints immediately, before and after each request.
	r := gin.New()

	// Trust no proxy headers. c.ClientIP() then reports the real TCP peer
	// instead of a spoofable X-Forwarded-For value. That matters twice in
	// this sample: the room records ClientIP as each ticket's client key,
	// and the /admin endpoints are restricted to loopback by ClientIP.
	// Behind a real reverse proxy, list its addresses here instead.
	if err := r.SetTrustedProxies(nil); err != nil {
		log.Fatalf("SetTrustedProxies: %v", err)
	}

	r.Use(gin.Recovery())  // keep the panic recovery middleware
	r.Use(requestLogger()) // our structured logger — prints on entry AND exit

	// ── 2. Create and initialise the WaitingRoom ─────────────────────────
	//
	// Cap of 5 is deliberately small so that `ab -c 100` fills the room
	// immediately and you can watch the queue build and drain in the logs.
	// In production you would set this to match your actual concurrency budget.
	wr = &room.WaitingRoom{}
	if err := wr.Init(5); err != nil {
		log.Fatalf("room.Init: %v", err)
	}
	defer wr.Stop()

	// ── 3. Configure the WaitingRoom ─────────────────────────────────────
	//
	// Apply settings BEFORE restoring a saved queue (step 5): Import uses
	// the token TTL and the first-poll grace to decide which saved tickets
	// are stale.

	// Leave Secure off for local development so cookies work over plain
	// http://localhost; SECURE_COOKIES=1 turns it on.
	wr.SetSecureCookie(secureCookies)

	// 0 means "use the default" (room.DefaultTokenTTL, 5 minutes). The TTL
	// is a sliding window reset on every poll, so it only governs how long
	// an ABANDONED ticket lingers — waiting visitors never expire.
	if err := wr.SetTokenTTL(0); err != nil {
		log.Fatalf("room.SetTokenTTL: %v", err)
	}

	// Tighten the reaper so ghost tickets from aborted connections are
	// cleaned up quickly during the load test.
	if err := wr.SetReaperInterval(10 * time.Second); err != nil {
		log.Fatalf("room.SetReaperInterval: %v", err)
	}

	// Reclaim tickets whose client never comes back at all — cookieless
	// clients, bots, or tabs closed within seconds — after 30s instead of
	// after the full TTL. Visitors who poll even once are unaffected.
	if err := wr.SetFirstPollGrace(30 * time.Second); err != nil {
		log.Fatalf("room.SetFirstPollGrace: %v", err)
	}

	// Record who each ticket belongs to. The key shows up in the admin
	// view, in event snapshots, and lets /admin/remove drop every ticket
	// from one client in a single call.
	wr.SetClientKeyFunc(func(c *gin.Context) string { return c.ClientIP() })

	// ── 3a. Configure skip-the-line pricing ──────────────────────────────
	//
	// SetRateFunc defines the per-position cost. Here we use a flat rate
	// of $2.50 per position. In production you might use surge pricing:
	//
	//   wr.SetRateFunc(func(depth int64) float64 {
	//       return 1.00 + float64(depth)*0.05  // base $1 + 5¢ per queued request
	//   })
	//
	// SetSkipURL tells the waiting room page where the "Pay to skip"
	// button should navigate. This must be registered BEFORE
	// RegisterRoutes so it bypasses the waiting room.
	//
	// SetPassDuration configures how long a paid skip-the-line pass
	// remains valid. During this window, if the client is evicted,
	// times out, or refreshes, they are auto-promoted to the front
	// without paying again. Set to 0 to disable (single-use promotions).
	//
	// Operator promotions (/admin/promote below) use AdminPromote, which
	// does not depend on any of this.
	wr.SetRateFunc(func(depth int64) float64 { return 2.50 })
	wr.SetSkipURL("/queue/purchase")
	if err := wr.SetPassDuration(90 * time.Minute); err != nil {
		log.Fatalf("room.SetPassDuration: %v", err)
	}

	// ── 4. Lifecycle callbacks ────────────────────────────────────────────
	//
	// These callbacks are what you will see in the terminal during ab.
	// Each line is prefixed with a tag so you can grep for it:
	//
	//   grep '\[ FULL'    — moments the room hit capacity
	//   grep '\[ QUEUE'   — every request that had to wait
	//   grep '\[ ENTER'   — every admission into active service
	//   grep '\[ EXIT'    — every slot release
	//   grep '\[ DRAIN'   — moments the room dropped below capacity
	//   grep '\[ EVICT'   — abandoned tickets reclaimed by the reaper
	//   grep '\[ TIMEOUT' — requests whose context was cancelled mid-queue
	//   grep '\[ PROMOTE' — a queued client paid (or was moved) to skip the line
	//   grep '\[ REMOVE'  — an operator removed queued tickets
	//
	// Snapshot.Token is the visitor's bearer credential for their place in
	// line. These handlers log Snapshot.ClientKey and never the token.

	wr.On(room.EventFull, func(s room.Snapshot) {
		roomLog("FULL   ", fmt.Sprintf(
			"capacity reached  occupancy=%d/%d  queue=%d  util=%.0f%%",
			s.Occupancy, s.Capacity, s.QueueDepth,
			pct(s.Occupancy, s.Capacity),
		))
	})

	wr.On(room.EventDrain, func(s room.Snapshot) {
		roomLog("DRAIN  ", fmt.Sprintf(
			"room no longer full  occupancy=%d/%d  queue=%d",
			s.Occupancy, s.Capacity, s.QueueDepth,
		))
	})

	wr.On(room.EventQueue, func(s room.Snapshot) {
		// StaleTicket distinguishes a returning visitor (carried a
		// room_ticket the room no longer knows — typically already
		// admitted once) from a brand-new arrival with no cookie at all.
		// Many "new" arrivals from one client means it is discarding
		// cookies.
		arrival := "new"
		if s.StaleTicket {
			arrival = "returning"
		}
		roomLog("QUEUE  ", fmt.Sprintf(
			"request queued  depth=%d  client=%s  arrival=%s  occupancy=%d/%d  util=%.0f%%",
			s.QueueDepth, s.ClientKey, arrival, s.Occupancy, s.Capacity,
			pct(s.Occupancy, s.Capacity),
		))
	})

	wr.On(room.EventEnter, func(s room.Snapshot) {
		roomLog("ENTER  ", fmt.Sprintf(
			"slot acquired  occupancy=%d/%d  queue=%d  util=%.0f%%",
			s.Occupancy, s.Capacity, s.QueueDepth,
			pct(s.Occupancy, s.Capacity),
		))
	})

	wr.On(room.EventExit, func(s room.Snapshot) {
		roomLog("EXIT   ", fmt.Sprintf(
			"slot released  occupancy=%d/%d  queue=%d  util=%.0f%%",
			s.Occupancy, s.Capacity, s.QueueDepth,
			pct(s.Occupancy, s.Capacity),
		))
	})

	wr.On(room.EventEvict, func(s room.Snapshot) {
		roomLog("EVICT  ", fmt.Sprintf(
			"abandoned ticket(s) reclaimed  queue=%d  live=%d  occupancy=%d/%d",
			s.QueueDepth, wr.LiveQueueDepth(), s.Occupancy, s.Capacity,
		))
	})

	wr.On(room.EventTimeout, func(s room.Snapshot) {
		roomLog("TIMEOUT", fmt.Sprintf(
			"context cancelled before admission  occupancy=%d/%d  queue=%d",
			s.Occupancy, s.Capacity, s.QueueDepth,
		))
	})

	wr.On(room.EventPromote, func(s room.Snapshot) {
		roomLog("PROMOTE", fmt.Sprintf(
			"client moved forward  client=%s  occupancy=%d/%d  queue=%d",
			s.ClientKey, s.Occupancy, s.Capacity, s.QueueDepth,
		))
	})

	wr.On(room.EventRemove, func(s room.Snapshot) {
		// RemoveToken carries the ticket's client; RemoveTokensFunc fires
		// once per batch with no token.
		who := s.ClientKey
		if s.Token == "" {
			who = "batch"
		}
		roomLog("REMOVE ", fmt.Sprintf(
			"ticket(s) removed  client=%s  queue=%d  live=%d",
			who, s.QueueDepth, wr.LiveQueueDepth(),
		))
	})

	// ── 5. Restore the queue saved by the previous run ───────────────────
	//
	// Must run after the settings above and before the server accepts
	// traffic. Import refuses a room that has already issued tickets.
	stateFile := envOr("ROOM_STATE_FILE",
		filepath.Join(os.TempDir(), "room-basic-web-app-queue.json"))
	restoreQueue(stateFile)

	// ── 6. Routes that must bypass the waiting room ──────────────────────
	//
	// Everything registered BEFORE RegisterRoutes is not gated.

	// Skip-the-line payment flow — queued clients must be able to reach it.
	//
	// GET /queue/purchase shows the "confirm payment" page. In production
	// it would create a Stripe Checkout session and redirect.
	r.GET("/queue/purchase", handlePurchasePage)

	// POST /queue/purchase/confirm processes the payment and promotes. In
	// production this is your Stripe webhook endpoint that verifies the
	// payment signature before promoting.
	r.POST("/queue/purchase/confirm", handlePurchaseConfirm)

	// Operator endpoints — loopback only in this sample. They address
	// queued visitors by position or client key and never expose tokens.
	admin := r.Group("/admin", localOnly())
	admin.GET("/queue", adminQueue)      // the line, front first
	admin.POST("/promote", adminPromote) // {"position": N, "to": 1}
	admin.POST("/remove", adminRemove)   // {"position": N} or {"client_key": "..."}
	admin.POST("/cap", adminCap)         // {"cap": N}

	// ── 7. Register the WaitingRoom routes ───────────────────────────────
	//
	// It installs, in order:
	//   OPTIONS /queue/status  — CORS preflight
	//   GET     /queue/status  — polling endpoint for the waiting-room page
	//   r.Use(wr.Middleware()) — gates every route registered after this
	wr.RegisterRoutes(r)

	// ── 8. Application routes (all gated by the waiting room) ────────────

	r.GET("/", homePage)
	r.GET("/about", aboutPage)
	r.GET("/pricing", pricingPage)
	r.GET("/contact", contactPage)

	// ── 9. Serve, then shut down gracefully and save the queue ───────────

	srv := &http.Server{
		Addr:    ":8080",
		Handler: r,
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		log.Printf("[ INFO  ] listening on http://localhost:8080  cap=%d  rate=$%.2f/pos  pass=%s  ttl=%s  first_poll_grace=%s",
			wr.Cap(), 2.50, wr.PassDuration(), wr.TokenTTL(), wr.FirstPollGrace())
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("ListenAndServe: %v", err)
		}
	}()

	<-quit
	log.Println("[ INFO  ] shutdown signal received — draining in-flight requests...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("[ ERROR ] server forced to shut down: %v", err)
	}

	// No new requests can arrive now, so the export is a complete picture
	// of the line. Visitors keep polling through the restart (the waiting
	// page retries failed polls) and resume their place when we're back.
	saveQueue(stateFile)

	log.Println("[ INFO  ] server exited cleanly")
}

// ── Queue persistence ─────────────────────────────────────────────────────────

// restoreQueue imports a queue saved by saveQueue, then deletes the file.
// A missing or unreadable file just means starting with an empty line.
func restoreQueue(path string) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		log.Printf("[ INFO  ] no saved queue at %s — starting with an empty line", path)
		return
	}
	if err != nil {
		log.Printf("[ WARN  ] cannot open saved queue %s: %v — starting with an empty line", path, err)
		return
	}

	stats, err := wr.Import(f)
	f.Close()

	// Delete whether or not the import worked: the file holds every queued
	// visitor's bearer token, and a bad file should not be retried forever.
	if rmErr := os.Remove(path); rmErr != nil {
		log.Printf("[ WARN  ] could not delete saved queue %s: %v", path, rmErr)
	}

	if err != nil {
		log.Printf("[ WARN  ] saved queue not restored: %v — starting with an empty line", err)
		return
	}
	log.Printf("[ INFO  ] queue restored  tickets=%d  dropped_stale=%d  passes=%d  passes_expired=%d",
		stats.Restored, stats.DroppedStale, stats.PassesRestored, stats.PassesExpired)
}

// saveQueue exports the line atomically: write to a 0600 temp file, fsync,
// then rename over the target, so a crash mid-write never leaves a
// truncated file for the next start to import.
func saveQueue(path string) {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		log.Printf("[ ERROR ] queue not saved: %v", err)
		return
	}
	if err := wr.Export(f); err != nil {
		f.Close()
		os.Remove(tmp)
		log.Printf("[ ERROR ] queue not saved: %v", err)
		return
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		log.Printf("[ ERROR ] queue not saved: fsync: %v", err)
		return
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		log.Printf("[ ERROR ] queue not saved: close: %v", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		log.Printf("[ ERROR ] queue not saved: rename: %v", err)
		return
	}
	log.Printf("[ INFO  ] queue saved  tickets=%d  file=%s", wr.LiveQueueDepth(), path)
}

// ── Admin endpoints ───────────────────────────────────────────────────────────
//
// These are a minimal operator console. They are registered before the
// waiting room (so an operator is never queued) and restricted to loopback.
// In production, put them behind real authentication.

// localOnly rejects any request whose peer is not a loopback address.
func localOnly() gin.HandlerFunc {
	return func(c *gin.Context) {
		ip := net.ParseIP(c.ClientIP())
		if ip == nil || !ip.IsLoopback() {
			c.AbortWithStatusJSON(http.StatusForbidden,
				gin.H{"error": "admin endpoints are loopback-only in this sample"})
			return
		}
		c.Next()
	}
}

// adminQueue returns the front of the line. Tokens are deliberately
// omitted — they are bearer credentials — so visitors are addressed by
// position or client key instead.
//
//	curl -s localhost:8080/admin/queue?limit=10 | jq
func adminQueue(c *gin.Context) {
	limit := 50
	if v, err := strconv.Atoi(c.DefaultQuery("limit", "50")); err == nil && v > 0 && v <= 1000 {
		limit = v
	}

	now := time.Now()
	tickets := make([]gin.H, 0, limit)
	for _, t := range wr.Queue(limit) {
		row := gin.H{
			"position":    t.Position, // <= 0: told ready, not yet reloaded
			"client_key":  t.ClientKey,
			"waiting_for": now.Sub(t.IssuedAt).Round(time.Second).String(),
			"seen":        t.Seen,
			"promoted":    t.Promoted,
			"has_pass":    t.HasPass,
		}
		if !t.LastPoll.IsZero() {
			row["last_poll_ago"] = now.Sub(t.LastPoll).Round(time.Second).String()
		}
		tickets = append(tickets, row)
	}

	c.JSON(http.StatusOK, gin.H{
		"cap":              wr.Cap(),
		"occupancy":        wr.Len(),
		"queue_depth":      wr.QueueDepth(),
		"live_queue_depth": wr.LiveQueueDepth(),
		"token_ttl":        wr.TokenTTL().String(),
		"first_poll_grace": wr.FirstPollGrace().String(),
		"tickets":          tickets,
	})
}

// adminPromote moves the visitor at a given position forward — free, with
// no RateFunc involved.
//
//	curl -s -X POST localhost:8080/admin/promote -d '{"position": 7, "to": 1}' | jq
func adminPromote(c *gin.Context) {
	var body struct {
		Position int64 `json:"position"`
		To       int64 `json:"to"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Position < 1 {
		c.JSON(http.StatusBadRequest, gin.H{"error": `send {"position": N} with N >= 1, optionally "to": M`})
		return
	}
	if body.To == 0 {
		body.To = 1
	}

	t, ok := findByPosition(body.Position)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("nobody is waiting at position %d", body.Position)})
		return
	}
	if err := wr.AdminPromote(t.Token, body.To); err != nil {
		c.JSON(adminErrorStatus(err), gin.H{"error": err.Error()})
		return
	}

	resp := gin.H{"from": body.Position, "client_key": t.ClientKey}
	if after, ok := wr.Ticket(t.Token); ok {
		resp["now"] = after.Position
	}
	c.JSON(http.StatusOK, resp)
}

// adminRemove drops one visitor by position, or every ticket from one
// client key. Removed visitors re-enter at the back on their next page
// load unless you also block them upstream.
//
//	curl -s -X POST localhost:8080/admin/remove -d '{"position": 3}' | jq
//	curl -s -X POST localhost:8080/admin/remove -d '{"client_key": "127.0.0.1"}' | jq
func adminRemove(c *gin.Context) {
	var body struct {
		Position  int64  `json:"position"`
		ClientKey string `json:"client_key"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": `send {"position": N} or {"client_key": "..."}`})
		return
	}

	switch {
	case body.ClientKey != "":
		n := wr.RemoveTokensFunc(func(t room.TicketInfo) bool {
			return t.ClientKey == body.ClientKey
		})
		c.JSON(http.StatusOK, gin.H{"removed": n, "client_key": body.ClientKey})

	case body.Position >= 1:
		t, ok := findByPosition(body.Position)
		if !ok {
			c.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("nobody is waiting at position %d", body.Position)})
			return
		}
		if err := wr.RemoveToken(t.Token); err != nil {
			c.JSON(adminErrorStatus(err), gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"removed": 1, "position": body.Position, "client_key": t.ClientKey})

	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": `send {"position": N} or {"client_key": "..."}`})
	}
}

// adminCap changes capacity at runtime.
//
//	curl -s -X POST localhost:8080/admin/cap -d '{"cap": 10}' | jq
func adminCap(c *gin.Context) {
	var body struct {
		Cap int32 `json:"cap"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := wr.SetCap(body.Cap); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"cap":         wr.Cap(),
		"occupancy":   wr.Len(),
		"queue_depth": wr.QueueDepth(),
		"utilization": fmt.Sprintf("%.0f%%", wr.Utilization()*100),
	})
}

// findByPosition returns the waiting visitor currently at position pos.
// Queue lists tickets that are already inside the serving window first
// (position <= 0), so it looks past up to twice the capacity of them.
func findByPosition(pos int64) (room.TicketInfo, bool) {
	for _, t := range wr.Queue(int(pos) + 2*int(wr.Cap())) {
		if t.Position == pos {
			return t, true
		}
	}
	return room.TicketInfo{}, false
}

// adminErrorStatus maps room errors to HTTP status codes.
func adminErrorStatus(err error) int {
	switch err.(type) {
	case room.ErrTokenNotFound, room.ErrAlreadyAdmitted:
		return http.StatusConflict // gone or already being admitted
	case room.ErrInvalidTargetPosition:
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

// ── Skip-the-line payment handlers ────────────────────────────────────────────
//
// These simulate a payment flow for the demo. In production, replace
// handlePurchasePage with a Stripe Checkout redirect and
// handlePurchaseConfirm with your Stripe webhook handler.

// handlePurchasePage shows a confirmation page with the current cost.
// The room_ticket cookie identifies which queued client is paying.
//
// Production equivalent: create a Stripe Checkout session with the
// token as client_reference_id, then redirect to session.URL.
func handlePurchasePage(c *gin.Context) {
	cookie, err := c.Request.Cookie("room_ticket")
	if err != nil || cookie.Value == "" {
		c.Data(http.StatusBadRequest, "text/html; charset=utf-8", page(
			"Error",
			`<h1>No queue ticket found</h1>
			<p>You need to be in the waiting room to skip the line.</p>
			<a href="/">← Back to site</a>`,
		))
		return
	}

	token := cookie.Value

	// Check if the client already has a valid pass — no need to pay again.
	if passCookie, err := c.Request.Cookie("room_pass"); err == nil {
		if wr.HasValidPass(passCookie.Value) {
			c.Data(http.StatusOK, "text/html; charset=utf-8", page(
				"VIP pass active",
				fmt.Sprintf(
					`<h1>You already have a VIP pass</h1>
					<p>Your pass is still active (expires in %s). You'll be
					automatically moved to the front — no additional payment needed.</p>
					<p>Head back and you'll be admitted shortly.</p>
					<a href="/">← Back to site</a>`,
					wr.PassDuration().Round(time.Minute)),
			))
			return
		}
	}

	// Get the current cost to jump to position 1.
	cost, err := wr.QuoteCost(token, 1)
	if err != nil {
		var msg string
		switch err.(type) {
		case room.ErrTokenNotFound:
			msg = "Your queue ticket has expired or was already used."
		case room.ErrAlreadyAdmitted:
			msg = "You're already being admitted — no need to pay!"
		case room.ErrPromotionDisabled:
			msg = "Skip-the-line is not available right now."
		default:
			msg = "Something went wrong: " + err.Error()
		}
		c.Data(http.StatusOK, "text/html; charset=utf-8", page("Skip the line", fmt.Sprintf(
			`<h1>Skip the line</h1>
			<p>%s</p>
			<a href="/">← Back to site</a>`, msg,
		)))
		return
	}

	if cost <= 0 {
		c.Data(http.StatusOK, "text/html; charset=utf-8", page("Skip the line",
			`<h1>You're next!</h1>
			<p>You're already at the front of the line — no payment needed.</p>
			<p>Head back and you'll be admitted momentarily.</p>
			<a href="/">← Back to site</a>`,
		))
		return
	}

	// Render the payment confirmation page.
	// In production this would be a Stripe Checkout redirect instead.
	c.Data(http.StatusOK, "text/html; charset=utf-8", purchasePage(cost, wr.PassDuration()))
}

// handlePurchaseConfirm processes the "payment" and promotes the token.
//
// Production equivalent: this is your Stripe webhook handler. It would:
//  1. Verify the Stripe signature (stripe.ConstructEvent)
//  2. Extract client_reference_id from the checkout session
//  3. Call wr.PromoteTokenToFront(token)
//  4. Set the room_pass cookie from result.PassToken
//  5. Return 200 to Stripe
//
// For this demo we skip signature verification and just promote.
func handlePurchaseConfirm(c *gin.Context) {
	cookie, err := c.Request.Cookie("room_ticket")
	if err != nil || cookie.Value == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no room_ticket cookie"})
		return
	}

	token := cookie.Value

	result, err := wr.PromoteTokenToFront(token)
	if err != nil {
		log.Printf("[ SKIP  ] promotion failed: %v", err)
		c.Data(http.StatusOK, "text/html; charset=utf-8", page("Payment failed", fmt.Sprintf(
			`<h1>Something went wrong</h1>
			<p>%s</p>
			<p>You haven't been charged. Head back to the waiting room and try again.</p>
			<a href="/">← Back to site</a>`, err.Error(),
		)))
		return
	}

	log.Printf("[ SKIP  ] client=%s promoted to front  cost=$%.2f  pass=%v",
		c.ClientIP(), result.Cost, result.PassToken != "")

	// Set the VIP pass cookie if a pass was issued. This cookie persists
	// across queue entries so the client is auto-promoted for the
	// configured pass duration without paying again.
	//
	// Secure follows the same rule as the room's own cookies: on when
	// configured, or when this request arrived over TLS directly. Marking
	// it Secure over plain HTTP would make some browsers drop the pass.
	if result.PassToken != "" {
		http.SetCookie(c.Writer, &http.Cookie{
			Name:     "room_pass",
			Value:    result.PassToken,
			Path:     wr.CookiePath(),
			Domain:   wr.CookieDomain(),
			MaxAge:   int(wr.PassDuration().Seconds()),
			HttpOnly: true,
			Secure:   secureCookies || c.Request.TLS != nil,
			SameSite: http.SameSiteLaxMode,
		})
	}

	// Redirect back to the site. The next poll (or page load) will see
	// ready=true and admit the client immediately.
	passMsg := ""
	if result.PassToken != "" {
		passMsg = fmt.Sprintf(
			`<p>Your VIP pass is valid for <strong>%s</strong> — if you
			re-enter the queue during that time, you'll be automatically
			moved to the front at no extra cost.</p>`,
			wr.PassDuration().Round(time.Minute))
	}

	c.Data(http.StatusOK, "text/html; charset=utf-8", page("Payment confirmed",
		fmt.Sprintf(
			`<h1>Payment confirmed — $%.2f</h1>
			<p>You've been moved to the front of the line!</p>
			%s
			<p>Redirecting you now...</p>
			<script>setTimeout(function(){ window.location.href = "/"; }, 2000);</script>
			<noscript><a href="/">← Click here to continue</a></noscript>`,
			result.Cost, passMsg,
		),
	))
}

// ── Page handlers ─────────────────────────────────────────────────────────────
//
// Each handler sleeps for a realistic duration so that concurrent requests
// actually hold their semaphore slots long enough for the room to fill up.
// Without the sleep, handlers return in microseconds and you will never see
// the waiting room trigger, even at -c 100.

const simulatedLatency = 500 * time.Millisecond

func homePage(c *gin.Context) {
	time.Sleep(simulatedLatency)
	c.Data(http.StatusOK, "text/html; charset=utf-8", page(
		"Home",
		`<h1>Welcome</h1>
		<p>This server admits at most <strong>5 concurrent requests</strong>.</p>
		<p>
		  Run <code>ab -t 60 -n 1000 -c 100 http://localhost:8080/about</code>
		  in a second terminal and watch this terminal for room events.
		</p>
		<p>
		  When you land in the waiting room, you'll see a
		  <strong>"Skip the line"</strong> option — click it to test the
		  payment flow at <strong>$2.50/position</strong>. Your VIP pass
		  lasts <strong>90 minutes</strong>.
		</p>
		<h2>Operator console (localhost only)</h2>
		<ul>
		  <li>See the line: <code>curl -s localhost:8080/admin/queue | jq</code></li>
		  <li>Move position 7 to the front: <code>curl -s -X POST localhost:8080/admin/promote -d '{"position":7}'</code></li>
		  <li>Remove position 3: <code>curl -s -X POST localhost:8080/admin/remove -d '{"position":3}'</code></li>
		  <li>Change capacity: <code>curl -s -X POST localhost:8080/admin/cap -d '{"cap":10}'</code></li>
		</ul>
		<p>
		  Stop the server with Ctrl-C while people are queued and start it
		  again: the line is saved on shutdown and restored on startup, and
		  queued visitors keep their place.
		</p>
		<nav>
		  <a href="/about">About</a> ·
		  <a href="/pricing">Pricing</a> ·
		  <a href="/contact">Contact</a>
		</nav>`,
	))
}

func aboutPage(c *gin.Context) {
	time.Sleep(simulatedLatency)
	c.Data(http.StatusOK, "text/html; charset=utf-8", page(
		"About",
		`<h1>About Us</h1>
		<p>
		  We use <strong>room</strong> — a FIFO waiting room middleware for
		  Go + Gin — to keep this service stable under sudden load spikes.
		  Instead of dropping excess requests with a 429, callers wait their
		  turn and are admitted in the order they arrived.
		</p>
		<p>
		  High-priority customers can <strong>skip the line</strong> by paying
		  a per-position fee. The VIP pass lasts 90 minutes — re-enter the
		  queue anytime during that window and you'll be auto-promoted for free.
		</p>
		<a href="/">← Home</a>`,
	))
}

func pricingPage(c *gin.Context) {
	time.Sleep(simulatedLatency)
	c.Data(http.StatusOK, "text/html; charset=utf-8", page(
		"Pricing",
		`<h1>Pricing</h1>
		<table>
		  <thead><tr><th>Tier</th><th>Requests / day</th><th>Queue priority</th></tr></thead>
		  <tbody>
		    <tr><td>Free</td><td>100</td><td>Standard (FIFO)</td></tr>
		    <tr><td>Pro</td><td>Unlimited</td><td>Standard (FIFO)</td></tr>
		    <tr><td>Skip the line</td><td>—</td><td>$2.50/pos + 90 min VIP pass</td></tr>
		  </tbody>
		</table>
		<a href="/">← Home</a>`,
	))
}

func contactPage(c *gin.Context) {
	time.Sleep(simulatedLatency)
	c.Data(http.StatusOK, "text/html; charset=utf-8", page(
		"Contact",
		`<h1>Contact</h1>
		<p>Email us at <a href="mailto:hello@example.com">hello@example.com</a></p>
		<a href="/">← Home</a>`,
	))
}

// ── Middleware ────────────────────────────────────────────────────────────────

// requestLogger returns a gin middleware that prints a line when the request
// arrives and another when it completes. Printing on arrival makes it
// immediately visible which requests are being held by the waiting room
// versus which are actively executing their handler.
func requestLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()

		// Skip logging the status-polling endpoint — it fires every 3 s per
		// queued client and would bury the room events in noise.
		if c.Request.URL.Path == "/queue/status" {
			c.Next()
			return
		}

		log.Printf("[ REQ   ] --> %s %s  remote=%s",
			c.Request.Method, c.Request.URL.Path, c.ClientIP())

		c.Next()

		log.Printf("[ REQ   ] <-- %s %s  status=%d  latency=%s",
			c.Request.Method, c.Request.URL.Path,
			c.Writer.Status(), time.Since(start).Round(time.Millisecond))
	}
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// roomLog prints a room event line with a consistent format so all room
// events sort together when the output is piped through sort or grep.
func roomLog(tag, msg string) {
	log.Printf("[ %s ] %s", tag, msg)
}

// pct converts an occupancy/capacity pair to a percentage, guarding
// against division by zero if capacity is somehow zero.
func pct(occupancy, capacity int) float64 {
	if capacity == 0 {
		return 0
	}
	return float64(occupancy) / float64(capacity) * 100
}

// envOr returns the environment variable's value, or def if it is unset.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// page wraps a body fragment in a complete, styled HTML document.
func page(title, body string) []byte {
	return []byte(`<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>` + title + ` — Basic Web App</title>
  <style>
    *, *::before, *::after { box-sizing: border-box; margin: 0; padding: 0; }
    body  { font-family: system-ui, sans-serif; max-width: 700px;
            margin: 4rem auto; padding: 0 1.5rem; color: #1a1a1a;
            line-height: 1.6; }
    h1    { margin-bottom: 1rem; }
    h2    { margin: 1.5rem 0 .5rem; font-size: 1.1rem; }
    p     { margin-bottom: 1rem; }
    ul    { margin: 0 0 1rem 1.25rem; }
    code  { background: #f0f0f0; padding: .1em .4em; border-radius: 3px; }
    nav   { margin-top: 2rem; }
    a     { color: #6c8ef5; }
    table { border-collapse: collapse; width: 100%; margin-bottom: 1rem; }
    th, td { border: 1px solid #ddd; padding: .5rem 1rem; text-align: left; }
    th    { background: #f5f5f5; }
  </style>
</head>
<body>` + body + `</body>
</html>`)
}

// purchasePage renders the skip-the-line payment confirmation page.
// This is the demo equivalent of a Stripe Checkout page. It shows the
// cost and a "Confirm payment" button that POSTs to /queue/purchase/confirm.
func purchasePage(cost float64, passDur time.Duration) []byte {
	passNote := ""
	if passDur > 0 {
		passNote = fmt.Sprintf(
			`<div class="price-detail" style="margin-top: 0.5rem;">
			Includes a <strong>%s VIP pass</strong> — re-enter the queue
			anytime during that window and skip for free.</div>`,
			passDur.Round(time.Minute))
	}

	return []byte(fmt.Sprintf(`<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>Skip the line — Basic Web App</title>
  <style>
    *, *::before, *::after { box-sizing: border-box; margin: 0; padding: 0; }
    body {
      font-family: system-ui, sans-serif;
      min-height: 100vh;
      display: flex;
      align-items: center;
      justify-content: center;
      background: #0f1117;
      color: #e2e8f0;
      padding: 1.5rem;
    }
    .card {
      background: #1a1d27;
      border: 1px solid #2a2d3a;
      border-radius: 12px;
      padding: 2.5rem 3rem;
      max-width: 420px;
      width: 100%%;
      text-align: center;
    }
    .icon { font-size: 2.5rem; margin-bottom: 1.25rem; }
    h1 {
      font-size: 1.4rem;
      font-weight: 600;
      margin-bottom: 0.5rem;
      background: linear-gradient(135deg, #6c8ef5, #a78bfa);
      -webkit-background-clip: text;
      -webkit-text-fill-color: transparent;
      background-clip: text;
    }
    .subtitle {
      color: #64748b;
      font-size: 0.9rem;
      margin-bottom: 2rem;
      line-height: 1.5;
    }
    .price-block {
      background: #0f1117;
      border: 1px solid #2a2d3a;
      border-radius: 12px;
      padding: 1.25rem;
      margin-bottom: 2rem;
    }
    .price-label {
      font-size: 0.75rem;
      text-transform: uppercase;
      letter-spacing: 0.08em;
      color: #64748b;
      margin-bottom: 0.4rem;
    }
    .price-amount {
      font-size: 2.5rem;
      font-weight: 700;
      line-height: 1;
      background: linear-gradient(135deg, #6c8ef5, #a78bfa);
      -webkit-background-clip: text;
      -webkit-text-fill-color: transparent;
      background-clip: text;
    }
    .price-detail {
      font-size: 0.8rem;
      color: #64748b;
      margin-top: 0.3rem;
    }
    .price-detail strong {
      color: #34d399;
    }
    .btn-pay {
      background: linear-gradient(135deg, #6c8ef5, #a78bfa);
      color: #fff;
      border: none;
      border-radius: 8px;
      padding: 0.75rem 2rem;
      font-size: 1rem;
      font-weight: 600;
      cursor: pointer;
      width: 100%%;
      margin-bottom: 1rem;
      transition: opacity 0.2s;
    }
    .btn-pay:hover { opacity: 0.9; }
    .btn-pay:active { opacity: 0.8; transform: scale(0.99); }
    .btn-pay:disabled { opacity: 0.5; cursor: not-allowed; }
    .back-link {
      display: block;
      color: #64748b;
      font-size: 0.8rem;
      text-decoration: none;
    }
    .back-link:hover { color: #e2e8f0; }
    .disclaimer {
      font-size: 0.7rem;
      color: #475569;
      margin-top: 1.5rem;
      line-height: 1.4;
    }
  </style>
</head>
<body>
  <div class="card">
    <div class="icon">⚡</div>
    <h1>Skip the line</h1>
    <p class="subtitle">
      Jump to the front of the queue instantly.<br>
      You'll be admitted on your next page load.
    </p>

    <div class="price-block">
      <div class="price-label">Total cost</div>
      <div class="price-amount">$%.2f</div>
      <div class="price-detail">$2.50 per position · one-time payment</div>
      %s
    </div>

    <form method="POST" action="/queue/purchase/confirm" id="pay-form">
      <button type="submit" class="btn-pay" id="pay-btn">
        Confirm payment — $%.2f
      </button>
    </form>

    <a href="/" class="back-link">← Back to the waiting room</a>

    <p class="disclaimer">
      Demo mode — no real payment is processed.<br>
      In production this page would be a Stripe Checkout session.
    </p>
  </div>

  <script>
    var form = document.getElementById("pay-form");
    var btn = document.getElementById("pay-btn");
    form.addEventListener("submit", function() {
      btn.disabled = true;
      btn.textContent = "Processing...";
    });
  </script>
</body>
</html>`, cost, passNote, cost))
}

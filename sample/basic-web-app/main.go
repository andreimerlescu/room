package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/andreimerlescu/room"
	"github.com/gin-gonic/gin"
)

// wr is the WaitingRoom instance. Keeping it package-level lets you call
// wr.SetCap, wr.SetReaperInterval, or wr.On from a config-reload handler
// without restarting the server.
var wr *room.WaitingRoom

func main() {
	// ── 1. Use gin.New() instead of gin.Default() ─────────────────────────
	//
	// gin.Default() installs gin's own Logger middleware, which buffers
	// output and formats it after the handler returns. That makes it hard
	// to see room events interleaved with request logs in real time.
	// gin.New() gives us a blank engine so we can install our own logger
	// that prints immediately, before and after each request.
	r := gin.New()
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

	// Leave SetSecureCookie at its default (false) for local development
	// so the cookie works over plain http://localhost.
	// Call wr.SetSecureCookie(true) in production behind TLS.

	// Tighten the reaper so ghost tickets from ab's aborted connections
	// are cleaned up quickly during the load test.
	if err := wr.SetReaperInterval(10 * time.Second); err != nil {
		log.Fatalf("room.SetReaperInterval: %v", err)
	}

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
	wr.SetRateFunc(func(depth int64) float64 { return 2.50 })
	wr.SetSkipURL("/queue/purchase")

	// ── 4. Lifecycle callbacks ────────────────────────────────────────────
	//
	// These callbacks are what you will see in the terminal during ab.
	// Each line is prefixed with a tag so you can grep for it:
	//
	//   grep '\[FULL\]'   — moments the room hit capacity
	//   grep '\[QUEUE\]'  — every request that had to wait
	//   grep '\[ENTER\]'  — every admission into active service
	//   grep '\[EXIT\]'   — every slot release
	//   grep '\[DRAIN\]'  — moments the room dropped below capacity
	//   grep '\[EVICT\]'  — abandoned ghost tickets removed by the reaper
	//   grep '\[TIMEOUT\]'— requests whose context was cancelled mid-queue
	//   grep '\[PROMOTE\]'— a queued client paid to skip the line

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
		roomLog("QUEUE  ", fmt.Sprintf(
			"request queued  depth=%d  occupancy=%d/%d  util=%.0f%%",
			s.QueueDepth, s.Occupancy, s.Capacity,
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
			"ghost ticket removed  queue=%d  occupancy=%d/%d",
			s.QueueDepth, s.Occupancy, s.Capacity,
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
			"client paid to skip  occupancy=%d/%d  queue=%d",
			s.Occupancy, s.Capacity, s.QueueDepth,
		))
	})

	// ── 5. Register skip-the-line routes BEFORE the waiting room ─────────
	//
	// These routes must bypass the waiting room so that queued clients
	// can access the payment flow. Register them before RegisterRoutes.
	//
	// In production you would replace the GET /queue/purchase page with
	// a handler that creates a Stripe Checkout session and redirects,
	// and POST /queue/purchase/confirm with a Stripe webhook handler
	// that verifies the payment event before calling PromoteTokenToFront.

	// GET /queue/purchase — shows the "confirm payment" page.
	// In production: creates a Stripe Checkout session and redirects.
	r.GET("/queue/purchase", handlePurchasePage)

	// POST /queue/purchase/confirm — processes the payment and promotes.
	// In production: this is your Stripe webhook endpoint that verifies
	// the payment signature before promoting.
	r.POST("/queue/purchase/confirm", handlePurchaseConfirm)

	// ── 6. Register the WaitingRoom routes ───────────────────────────────
	//
	// RegisterRoutes must come AFTER the payment routes (so they bypass
	// the queue) and BEFORE your application routes (so they are gated).
	// It installs, in order:
	//   OPTIONS /queue/status  — CORS preflight
	//   GET     /queue/status  — polling endpoint for the waiting-room page
	//   r.Use(wr.Middleware()) — gates every route registered after this
	wr.RegisterRoutes(r)

	// ── 7. Application routes (all gated by the waiting room) ────────────

	r.GET("/", homePage)
	r.GET("/about", aboutPage)
	r.GET("/pricing", pricingPage)
	r.GET("/contact", contactPage)

	// ── 8. Graceful shutdown ──────────────────────────────────────────────

	srv := &http.Server{
		Addr:    ":8080",
		Handler: r,
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		log.Printf("[ INFO  ] listening on http://localhost:8080  cap=%d  rate=$%.2f/pos  skip_url=%s",
			wr.Cap(), 2.50, wr.SkipURL())
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
	log.Println("[ INFO  ] server exited cleanly")
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
	c.Data(http.StatusOK, "text/html; charset=utf-8", purchasePage(cost))
}

// handlePurchaseConfirm processes the "payment" and promotes the token.
//
// Production equivalent: this is your Stripe webhook handler. It would:
//  1. Verify the Stripe signature (stripe.ConstructEvent)
//  2. Extract client_reference_id from the checkout session
//  3. Call wr.PromoteTokenToFront(token)
//  4. Return 200 to Stripe
//
// For this demo we skip signature verification and just promote.
func handlePurchaseConfirm(c *gin.Context) {
	cookie, err := c.Request.Cookie("room_ticket")
	if err != nil || cookie.Value == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no room_ticket cookie"})
		return
	}

	token := cookie.Value

	cost, err := wr.PromoteTokenToFront(token)
	if err != nil {
		log.Printf("[ SKIP  ] promotion failed for token=%.8s...: %v", token, err)
		c.Data(http.StatusOK, "text/html; charset=utf-8", page("Payment failed", fmt.Sprintf(
			`<h1>Something went wrong</h1>
			<p>%s</p>
			<p>You haven't been charged. Head back to the waiting room and try again.</p>
			<a href="/">← Back to site</a>`, err.Error(),
		)))
		return
	}

	log.Printf("[ SKIP  ] token=%.8s... promoted to front  cost=$%.2f", token, cost)

	// Redirect back to the site. The next poll (or page load) will
	// see ready=true and admit the client immediately.
	c.Data(http.StatusOK, "text/html; charset=utf-8", page("Payment confirmed",
		fmt.Sprintf(
			`<h1>Payment confirmed — $%.2f</h1>
			<p>You've been moved to the front of the line!</p>
			<p>Redirecting you now...</p>
			<script>setTimeout(function(){ window.location.href = "/"; }, 1500);</script>
			<noscript><a href="/">← Click here to continue</a></noscript>`, cost,
		),
	))
}

// ── Page handlers ─────────────────────────────────────────────────────────────
//
// Each handler sleeps for a realistic duration so that concurrent ab requests
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
		  payment flow at <strong>$2.50/position</strong>.
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
		  a per-position fee. The price updates in real time as the queue moves.
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
		    <tr><td>Skip the line</td><td>—</td><td>Pay per position ($2.50/pos)</td></tr>
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
    p     { margin-bottom: 1rem; }
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
func purchasePage(cost float64) []byte {
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
    .btn-pay:disabled {
      opacity: 0.5;
      cursor: not-allowed;
    }
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
</html>`, cost, cost))
}

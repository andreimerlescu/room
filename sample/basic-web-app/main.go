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

	// ── 5. Register the WaitingRoom routes ───────────────────────────────
	//
	// RegisterRoutes must come BEFORE your application routes.
	// It installs, in order:
	//   OPTIONS /queue/status  — CORS preflight
	//   GET     /queue/status  — polling endpoint for the waiting-room page
	//   r.Use(wr.Middleware()) — gates every route registered after this
	wr.RegisterRoutes(r)

	// ── 6. Application routes (all gated by the waiting room) ────────────

	r.GET("/", homePage)
	r.GET("/about", aboutPage)
	r.GET("/pricing", pricingPage)
	r.GET("/contact", contactPage)

	// ── 7. Graceful shutdown ──────────────────────────────────────────────

	srv := &http.Server{
		Addr:    ":8080",
		Handler: r,
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		log.Printf("[ INFO  ] listening on http://localhost:8080  cap=%d", wr.Cap())
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
		    <tr><td>Free</td><td>100</td><td>Standard</td></tr>
		    <tr><td>Pro</td><td>Unlimited</td><td>Standard</td></tr>
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

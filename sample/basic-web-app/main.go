package main

import (
	"context"
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
	// ── 1. Create the router ─────────────────────────────────────────────
	r := gin.Default()

	// ── 2. Create and initialise the WaitingRoom ─────────────────────────
	//
	// Cap of 10 means at most 10 requests are actively served at once.
	// The 11th request sees the waiting room and is admitted automatically
	// when a slot opens — no refresh required.
	wr = &room.WaitingRoom{}
	if err := wr.Init(10); err != nil {
		log.Fatalf("room.Init: %v", err)
	}
	defer wr.Stop() // clean up the background reaper goroutine on exit

	// ── 3. Configure the WaitingRoom ─────────────────────────────────────

	// In production, behind Cloudflare / nginx / AWS ALB, the Go process
	// receives plain HTTP even though the user is on HTTPS. Set this so
	// the session cookie carries the Secure flag.
	wr.SetSecureCookie(true)

	// Tighten the reaper so ghost tickets are evicted every 30 s during
	// a high-traffic event rather than the default 5 m.
	if err := wr.SetReaperInterval(30 * time.Second); err != nil {
		log.Fatalf("room.SetReaperInterval: %v", err)
	}

	// ── 4. Lifecycle callbacks ────────────────────────────────────────────
	//
	// Callbacks are fired asynchronously in their own goroutines, so a
	// slow handler (e.g. one that calls an external API) never stalls the
	// request path. Register them before calling RegisterRoutes.

	// Fired when every slot is occupied and the next request will queue.
	wr.On(room.EventFull, func(s room.Snapshot) {
		log.Printf("[room] FULL  occupancy=%d/%d queue=%d",
			s.Occupancy, s.Capacity, s.QueueDepth)
	})

	// Fired when the room drops from full back to having a free slot.
	wr.On(room.EventDrain, func(s room.Snapshot) {
		log.Printf("[room] DRAIN occupancy=%d/%d", s.Occupancy, s.Capacity)
	})

	// Fired every time a request joins the waiting room queue.
	wr.On(room.EventQueue, func(s room.Snapshot) {
		log.Printf("[room] QUEUE depth=%d utilization=%.0f%%",
			s.QueueDepth, float64(s.Occupancy)/float64(s.Capacity)*100)
	})

	// Fired every time a request is admitted into active service.
	wr.On(room.EventEnter, func(s room.Snapshot) {
		log.Printf("[room] ENTER occupancy=%d/%d", s.Occupancy, s.Capacity)
	})

	// Fired every time a request completes and releases its slot.
	wr.On(room.EventExit, func(s room.Snapshot) {
		log.Printf("[room] EXIT  occupancy=%d/%d", s.Occupancy, s.Capacity)
	})

	// Fired when the reaper evicts a ghost ticket (client disappeared).
	wr.On(room.EventEvict, func(s room.Snapshot) {
		log.Printf("[room] EVICT queue=%d", s.QueueDepth)
	})

	// Fired when a queued request's context is cancelled before admission.
	wr.On(room.EventTimeout, func(s room.Snapshot) {
		log.Printf("[room] TIMEOUT occupancy=%d/%d", s.Occupancy, s.Capacity)
	})

	// ── 5. Register the WaitingRoom routes ───────────────────────────────
	//
	// RegisterRoutes does three things in the correct order:
	//   a) OPTIONS /queue/status  — handles CORS preflight
	//   b) GET     /queue/status  — the polling endpoint the waiting-room
	//                               page calls every 3 s
	//   c) r.Use(wr.Middleware()) — gates every subsequent route
	//
	// Routes registered BEFORE this call bypass the gate entirely — useful
	// for health checks, readiness probes, and metrics scrapers that must
	// always succeed regardless of application load.
	wr.RegisterRoutes(r)

	// ── 6. Application routes ─────────────────────────────────────────────
	//
	// Every handler below is protected by the waiting room. If more than
	// 10 requests are simultaneously active, the 11th caller sees the
	// waiting-room page until a slot opens — automatically, no refresh.

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
		log.Println("listening on http://localhost:8080")
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("ListenAndServe: %v", err)
		}
	}()

	<-quit
	log.Println("shutdown signal received — draining in-flight requests...")

	// Give active requests up to 15 s to complete before forcing exit.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("server forced to shut down: %v", err)
	}
	log.Println("server exited cleanly")
}

// ── Page handlers ─────────────────────────────────────────────────────────────
//
// Each handler returns a self-contained HTML page so the sample runs with
// no external template files. In a real application you would use
// html/template with embed.FS, or a front-end build step instead.

func homePage(c *gin.Context) {
	c.Data(http.StatusOK, "text/html; charset=utf-8", page(
		"Home",
		`<h1>Welcome</h1>
		<p>This is the home page of the basic-web-app sample.</p>
		<p>
		  This server admits at most <strong>10 concurrent requests</strong>.
		  Open this page in many tabs simultaneously and some will see the
		  waiting room — they will be admitted automatically when a slot opens.
		</p>
		<nav>
		  <a href="/about">About</a> ·
		  <a href="/pricing">Pricing</a> ·
		  <a href="/contact">Contact</a>
		</nav>`,
	))
}

func aboutPage(c *gin.Context) {
	c.Data(http.StatusOK, "text/html; charset=utf-8", page(
		"About",
		`<h1>About Us</h1>
		<p>
		  We use <strong>room</strong> — a FIFO waiting room middleware for
		  Go + Gin — to keep this service stable under sudden load spikes.
		  Instead of dropping requests with a 429, callers wait their turn
		  and are admitted in the order they arrived.
		</p>
		<a href="/">← Home</a>`,
	))
}

func pricingPage(c *gin.Context) {
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
	c.Data(http.StatusOK, "text/html; charset=utf-8", page(
		"Contact",
		`<h1>Contact</h1>
		<p>Email us at <a href="mailto:hello@example.com">hello@example.com</a></p>
		<a href="/">← Home</a>`,
	))
}

// ── Helpers ───────────────────────────────────────────────────────────────────

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

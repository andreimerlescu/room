# basic-web-app — room middleware tutorial

This sample walks you through adding a FIFO waiting room to a four-page
Gin web application from scratch. By the end you will have a running server
that admits at most N concurrent requests, queues the rest, and admits them
automatically in arrival order — with no client-side refresh required.

---

## Prerequisites

| Tool | Minimum version |
|---|---|
| Go | 1.22 |
| git | any |

---

## Step 1 — Create the module

```bash
mkdir basic-web-app && cd basic-web-app
go mod init github.com/your-username/basic-web-app
```

Fetch the two dependencies the sample uses:

```bash
go get github.com/andreimerlescu/room
go get github.com/gin-gonic/gin
```

---

## Step 2 — Create `main.go` with a plain Gin server

Start with the simplest possible Gin application — four routes, no waiting
room yet:

```go
package main

import (
    "net/http"

    "github.com/gin-gonic/gin"
)

func main() {
    r := gin.Default()

    r.GET("/",        func(c *gin.Context) { c.String(http.StatusOK, "Home")    })
    r.GET("/about",   func(c *gin.Context) { c.String(http.StatusOK, "About")   })
    r.GET("/pricing", func(c *gin.Context) { c.String(http.StatusOK, "Pricing") })
    r.GET("/contact", func(c *gin.Context) { c.String(http.StatusOK, "Contact") })

    r.Run(":8080")
}
```

Run it and confirm all four pages respond:

```bash
go run main.go &
curl http://localhost:8080/
curl http://localhost:8080/about
curl http://localhost:8080/pricing
curl http://localhost:8080/contact
```

---

## Step 3 — Declare a package-level WaitingRoom

Add a package-level variable. Keeping the `*room.WaitingRoom` at package
scope means you can call `wr.SetCap` or `wr.On` from a config-reload
handler later without restarting the server.

```go
import "github.com/andreimerlescu/room"

var wr *room.WaitingRoom
```

---

## Step 4 — Initialise the WaitingRoom

Inside `main()`, before creating any routes, initialise the WaitingRoom with
your chosen capacity. The capacity is the maximum number of requests that are
**actively being served** at any one moment. Requests beyond that limit see
the waiting room page.

```go
wr = &room.WaitingRoom{}
if err := wr.Init(10); err != nil {
    log.Fatalf("room.Init: %v", err)
}
defer wr.Stop() // stops the background reaper goroutine on exit
```

> **Choosing a capacity.** Start with the number of goroutines your slowest
> handler can tolerate simultaneously without degrading latency — typically
> the size of your database connection pool or your downstream service's
> rate limit. You can change it at runtime with `wr.SetCap`.

---

## Step 5 — Configure the WaitingRoom (optional but recommended)

### 5a — Cookie security

By default the waiting-room session cookie is issued **without** the `Secure`
flag so that `http://localhost` works during development. In any deployment
that sits behind a TLS-terminating proxy (Cloudflare, nginx, AWS ALB) the Go
process receives plain HTTP even though users are on HTTPS, so you must opt
in explicitly:

```go
wr.SetSecureCookie(true)
```

Leave this line out during local development. Add it before deploying to
any environment reachable over HTTPS.

### 5b — Reaper interval

The reaper is a background goroutine that evicts tokens from clients that
disappeared mid-queue (closed the tab, lost their connection). The default
interval is 5 minutes. For high-traffic events where ghost tickets could
stall the queue, tighten it:

```go
if err := wr.SetReaperInterval(30 * time.Second); err != nil {
    log.Fatalf("room.SetReaperInterval: %v", err)
}
```

---

## Step 6 — Register lifecycle callbacks (optional)

Callbacks let your application react to capacity events in real time. They
run asynchronously in their own goroutines, so a slow callback never stalls
the request path.

Register them **before** calling `RegisterRoutes`:

```go
// Fired when every slot is occupied — good time to scale out.
wr.On(room.EventFull, func(s room.Snapshot) {
    log.Printf("[room] FULL  occupancy=%d/%d queue=%d",
        s.Occupancy, s.Capacity, s.QueueDepth)
})

// Fired when the room drops from full back to having a free slot.
wr.On(room.EventDrain, func(s room.Snapshot) {
    log.Printf("[room] DRAIN occupancy=%d/%d", s.Occupancy, s.Capacity)
})

// Fired every time a request joins the queue.
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

// Fired when the reaper removes an abandoned token.
wr.On(room.EventEvict, func(s room.Snapshot) {
    log.Printf("[room] EVICT queue=%d", s.QueueDepth)
})

// Fired when a queued request's context is cancelled before admission.
wr.On(room.EventTimeout, func(s room.Snapshot) {
    log.Printf("[room] TIMEOUT occupancy=%d/%d", s.Occupancy, s.Capacity)
})
```

The `room.Snapshot` delivered to every callback is a point-in-time copy of
the room's state — safe to read after the callback returns.

---

## Step 7 — Register the WaitingRoom routes

This is the single most important ordering constraint in the whole setup:
call `wr.RegisterRoutes(r)` **after** any routes that must bypass the gate
(health checks, metrics, etc.) and **before** any routes that should be
protected by it.

```go
// Routes registered before this line bypass the waiting room entirely.
// Example: r.GET("/healthz", healthHandler)

wr.RegisterRoutes(r)

// Routes registered after this line are protected by the waiting room.
r.GET("/",        homePage)
r.GET("/about",   aboutPage)
r.GET("/pricing", pricingPage)
r.GET("/contact", contactPage)
```

`RegisterRoutes` does three things internally, in this order:

| Step | What it registers | Why |
|---|---|---|
| 1 | `OPTIONS /queue/status` | Handles CORS preflight from the polling `fetch()` |
| 2 | `GET /queue/status` | The JSON endpoint the waiting-room page polls every 3 s |
| 3 | `r.Use(wr.Middleware())` | Gates every route registered after this call |

> **Do not** call `r.Use(wr.Middleware())` manually if you are using
> `RegisterRoutes`. The two are mutually exclusive — `RegisterRoutes` calls
> `r.Use` for you, in the correct position relative to `/queue/status`.

---

## Step 8 — Add graceful shutdown

When the process receives `SIGINT` or `SIGTERM`, give active requests time
to finish before the server closes. This pairs naturally with the waiting
room because in-flight requests that are admitted through the gate must be
allowed to complete cleanly.

```go
import (
    "context"
    "net/http"
    "os"
    "os/signal"
    "syscall"
    "time"
)

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

shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
defer cancel()

if err := srv.Shutdown(shutdownCtx); err != nil {
    log.Printf("server forced to shut down: %v", err)
}
log.Println("server exited cleanly")
```

The `defer wr.Stop()` from Step 4 runs after `srv.Shutdown` returns, which
stops the reaper goroutine and leaves nothing running after `main` exits.

---

## Step 9 — Run it

```bash
go run main.go
```

Open `http://localhost:8080` in your browser. You will see the home page.

### Simulating the waiting room

The easiest way to trigger the waiting room locally is to temporarily lower
the capacity and flood the server with slow requests.

**Terminal 1 — start the server with cap=2:**

Edit `wr.Init(10)` → `wr.Init(2)`, then:

```bash
go run main.go
```

**Terminal 2 — send 10 slow concurrent requests:**

```bash
# requires: go install github.com/rakyll/hey@latest
hey -n 10 -c 10 -q 1 http://localhost:8080/
```

Or with plain `curl` in a loop:

```bash
for i in $(seq 1 10); do
  curl -s http://localhost:8080/ &
done
wait
```

**Terminal 3 — watch the server logs:**

You will see `[room] FULL` when both slots are occupied, `[room] QUEUE`
for each request that lands in the waiting room, and `[room] DRAIN` when
the last active slot is released.

Open `http://localhost:8080/` in a browser tab while the flood is running
and you will see the waiting-room page counting down your position.

---

## Step 10 — Runtime capacity adjustment

You can change the capacity without restarting the server. Because `wr` is
a package-level variable, any handler or goroutine can call `wr.SetCap`:

```go
// In a config-reload handler or an admin endpoint:
func adminSetCap(c *gin.Context) {
    var body struct{ Cap int32 `json:"cap"` }
    if err := c.ShouldBindJSON(&body); err != nil {
        c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
        return
    }
    if err := wr.SetCap(body.Cap); err != nil {
        c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
        return
    }
    c.JSON(http.StatusOK, gin.H{"cap": wr.Cap()})
}
```

Expanding capacity immediately admits waiting clients. Shrinking capacity
drains the semaphore down to the new limit — existing in-flight requests
complete normally.

---

## Complete file layout

```
sample/basic-web-app/
├── main.go      ← the result of this tutorial
├── README.md    ← this file
└── go.mod       ← created by go mod init
```

`go.sum` is generated automatically the first time you run `go mod tidy` or
`go run main.go`.

---

## What the waiting room does, in plain terms

```
Browser                  room middleware              Your handler
   │                          │                            │
   │── GET /pricing ──────────▶                            │
   │                    slot available?                    │
   │                    yes → acquire slot ────────────────▶
   │                                              handler runs
   │                                              slot released ◀──────────┐
   │◀─────────────────────────────── 200 OK ──────────────│               │
   │                                                                       │
   │── GET /pricing ──────────▶                            │               │
   │                    slot available?                    │               │
   │                    no → issue token                   │               │
   │◀── 200 waiting-room HTML ─│                           │               │
   │                           │                           │               │
   │── GET /queue/status ──────▶ position=3, ready=false   │               │
   │◀── {ready:false,pos:3} ───│                           │               │
   │      ... 3 s ...          │                           │               │
   │── GET /queue/status ──────▶ slot opened ──────────────────────────────┘
   │◀── {ready:true} ──────────│
   │      reload               │
   │── GET /pricing ──────────▶ acquire slot ──────────────▶
   │◀─────────────────────────────── 200 OK ───────────────│
```

Key properties:

- **FIFO**: requests are admitted in ticket order — first in, first out.
- **No server-side goroutines**: the middleware is stateless per request
  beyond the token store lookup; there are no goroutines blocking on behalf
  of waiting clients.
- **Automatic admission**: the browser reloads automatically when its
  ticket becomes ready — the user sees the page appear without pressing F5.
- **Ghost cleanup**: if a waiting client closes their tab, the reaper evicts
  their ticket after the TTL, advancing the queue for everyone behind them.

---

## Common mistakes

### Registering routes before `RegisterRoutes`

```go
// ✗ Wrong — /about is not gated
r.GET("/about", aboutPage)
wr.RegisterRoutes(r)
r.GET("/", homePage) // ✓ gated
```

```go
// ✓ Correct — all four pages are gated
wr.RegisterRoutes(r)
r.GET("/", homePage)
r.GET("/about", aboutPage)
r.GET("/pricing", pricingPage)
r.GET("/contact", contactPage)
```

### Forgetting `defer wr.Stop()`

Without `wr.Stop()`, the reaper goroutine outlives the `http.Server`. In a
long-running process this is harmless (it exits when `main` returns), but in
tests that construct and discard `WaitingRoom` instances it will leak
goroutines and trigger the race detector.

### Setting `Secure: true` cookies on plain HTTP

If you call `wr.SetSecureCookie(true)` and run the server on plain
`http://localhost`, browsers will silently drop the cookie. The waiting-room
page will be served but the client will never re-present the token, so it
will get a new ticket on every reload and appear to never be admitted.

Only call `wr.SetSecureCookie(true)` in environments where every request
reaches the Go process via HTTPS — or via a proxy that terminates TLS and
forwards over HTTP on a private network.

---

## License

Apache 2.0 — see the root [`LICENSE`](../../LICENSE) file.
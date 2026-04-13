# room — FIFO Waiting Room for Go + Gin

> Drop-in waiting room middleware for [gin](https://github.com/gin-gonic/gin)
> web applications. Built on [sema](https://github.com/andreimerlescu/sema).

[![Go Reference](https://pkg.go.dev/badge/github.com/andreimerlescu/room.svg)](https://pkg.go.dev/github.com/andreimerlescu/room)
[![Go Report Card](https://goreportcard.com/badge/github.com/andreimerlescu/room)](https://goreportcard.com/report/github.com/andreimerlescu/room)
[![CI](https://github.com/andreimerlescu/room/actions/workflows/go.yml/badge.svg)](https://github.com/andreimerlescu/room/actions/workflows/go.yml)
[![Apache 2.0 License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)

When your Go service hits capacity, don't drop requests — queue them.

`room` is a single-import middleware that sits in front of your Gin handlers
and turns excess traffic into an orderly waiting room. Every request gets a
ticket. Clients that can't be served immediately see a live-updating queue
page with their position. As slots open, they're admitted automatically in
FIFO order. Your handlers never know the difference — they see normal
requests arriving at the rate you chose.

```go
wr := &room.WaitingRoom{}
wr.Init(500)
defer wr.Stop()
wr.RegisterRoutes(r)
// That's it. Request 501 sees the waiting room.
```

---

## See it in action — 30 seconds

```bash
cd sample/basic-web-app
bash test.sh
```

The test script builds the server, launches 30 concurrent clients, and
prints a live dashboard while the waiting room queues and admits them.
Open `http://localhost:8080/` in your browser while it runs to see your
position tick down in real time.

```
  [ 15s] sent:120  served:42   queued:78   err:0   active:83  ~2 req/s [wave 4]
```

```
╔══════════════════════════════════════════════════╗
║   Results                                       ║
╠══════════════════════════════════════════════════╣
║  Total sent:               191                  ║
║  Served (200):             191                  ║
║  Queued (waited):          187                  ║
║  Errors:                     0                  ║
║  Throughput:               3 req/s              ║
╠══════════════════════════════════════════════════╣
║  FULL transitions:           7                  ║
║  DRAIN transitions:          7                  ║
║  QUEUE events:             187                  ║
╚══════════════════════════════════════════════════╝

✓ Waiting room activated — 187 requests queued.
```

No configuration files, no external dependencies, no infrastructure.
One `go get`, one `Init`, one `RegisterRoutes`.

---

## Why room?

When your application is at capacity you have three choices:

| Strategy | What happens | UX |
|---|---|---|
| **Drop (429)** | Reject the request | User sees an error, retries blindly, amplifies the spike |
| **Queue blindly** | Buffer with no ordering | No position awareness, no ETA, users refresh and make it worse |
| **Waiting room** | Issue a ticket, show position, admit in order | User waits calmly, knows their place, gets in automatically |

`room` does the third. It gives you FIFO ordering, live position tracking,
a polished waiting-room page, lifecycle callbacks for autoscaling, a reaper
that cleans up abandoned clients, configurable cookie security, runtime
capacity adjustment, and a max-queue-depth circuit breaker — all behind a
single middleware call.

---

## Installation

```
go get github.com/andreimerlescu/room
```

Requires **Go 1.22+** and [gin](https://github.com/gin-gonic/gin).

---

## Quick start

```go
package main

import (
    "log"

    "github.com/andreimerlescu/room"
    "github.com/gin-gonic/gin"
)

func main() {
    r := gin.Default()

    wr := &room.WaitingRoom{}
    if err := wr.Init(500); err != nil {
        log.Fatal(err)
    }
    defer wr.Stop()

    // Registers GET /queue/status and attaches the middleware.
    // Every route registered AFTER this line is gated.
    wr.RegisterRoutes(r)

    r.GET("/", func(c *gin.Context) {
        c.String(200, "You're in!")
    })

    r.Run(":8080")
}
```

The 501st concurrent request sees the waiting room. The moment a slot
opens, the next client in line is admitted automatically — no refresh
required.

---

## What your users see

When a request can't be served immediately, `room` responds with a
self-contained HTML page that polls `/queue/status` every 3 seconds and
updates the position in place:

- **Queue position** — a large, visible number that ticks down
- **Auto-admit** — the page reloads automatically when `ready=true`
- **No refresh needed** — the status text updates live
- **Dark theme** — clean, modern design that works on mobile
- **Accessible** — uses `aria-live` regions for screen readers

Replace the default page with your own via `wr.SetHTML(myHTML)`. The only
contract is `{{.Position}}` for the queue number and a `fetch("/queue/status")`
poll loop in your JavaScript.

---

## Lifecycle callbacks

`room` exposes a full event system. Register handlers with `On` and react
to capacity changes in real time — without polling, without a sidecar,
without coupling business logic to the middleware.

```go
// Scale out when the room fills up.
wr.On(room.EventFull, func(s room.Snapshot) {
    log.Printf("room full (%d/%d)", s.Occupancy, s.Capacity)
    go provisionHost()
})

// Scale back in when pressure drops.
wr.On(room.EventDrain, func(s room.Snapshot) {
    go deregisterHost()
})

// Observe every admission and completion.
wr.On(room.EventEnter, func(s room.Snapshot) { metrics.Inc("room.enter") })
wr.On(room.EventExit,  func(s room.Snapshot) { metrics.Inc("room.exit") })

// React to queuing, abandoned tickets, and timeouts.
wr.On(room.EventQueue,   func(s room.Snapshot) { metrics.Inc("room.queue") })
wr.On(room.EventEvict,   func(s room.Snapshot) { metrics.Inc("room.evict") })
wr.On(room.EventTimeout, func(s room.Snapshot) { metrics.Inc("room.timeout") })
```

Every handler receives a **Snapshot** — a point-in-time copy of the room's
state at the moment the event fired:

```go
type Snapshot struct {
    Event      Event
    Occupancy  int    // slots in use
    Capacity   int    // maximum slots
    QueueDepth int64  // requests waiting
}

func (s Snapshot) Full()  bool  // Occupancy >= Capacity
func (s Snapshot) Empty() bool  // Occupancy == 0
```

Handlers run asynchronously in their own goroutines — a slow callback
never stalls the request path. Remove handlers at any time with `wr.Off(event)`.

### Events at a glance

| Event | Fires when | Use case |
|---|---|---|
| `EventEnter` | Request acquires a slot | Throughput metrics |
| `EventExit` | Request completes, slot released | Latency tracking |
| `EventFull` | Room transitions to full (edge, not every admission) | Scale-out trigger |
| `EventDrain` | Room transitions from full to available (edge) | Scale-in signal |
| `EventQueue` | Request issued a waiting-room ticket | Queue depth alerting |
| `EventEvict` | Reaper removes an abandoned token | Ghost ticket monitoring |
| `EventTimeout` | Request context cancelled before admission | Client timeout tracking |

`EventFull` and `EventDrain` fire only on the **transition edge** — not
on every admission while full. This means your autoscaler callback fires
once when you need it, not 10,000 times during a traffic spike.

---

## Full control

```go
wr := &room.WaitingRoom{}
if err := wr.Init(500); err != nil {
    log.Fatal(err)
}
defer wr.Stop()

// Custom waiting room page.
html, _ := os.ReadFile("my_waiting_room.html")
wr.SetHTML(html)

// Production cookie security.
wr.SetSecureCookie(true)
wr.SetCookiePath("/app")
wr.SetCookieDomain(".example.com")

// Queue depth circuit breaker — reject with 503 beyond this depth.
wr.SetMaxQueueDepth(10000)

// Tighten the reaper for a high-traffic event.
wr.SetReaperInterval(15 * time.Second)

// Register lifecycle hooks before traffic arrives.
wr.On(room.EventFull, func(s room.Snapshot) {
    go provisionHost()
})

wr.RegisterRoutes(r)
r.Run(":8080")
```

---

## Runtime capacity adjustment

Change capacity without restarting the server:

```go
r.POST("/admin/cap", func(c *gin.Context) {
    var body struct{ Cap int32 `json:"cap"` }
    c.ShouldBindJSON(&body)
    wr.SetCap(body.Cap)
    c.JSON(200, gin.H{
        "cap":         wr.Cap(),
        "occupancy":   wr.Len(),
        "queue_depth": wr.QueueDepth(),
        "utilization": wr.UtilizationSmoothed(),
    })
})
```

```bash
# Double capacity — queued requests rush into the new slots
curl -X POST localhost:8080/admin/cap -d '{"cap":1000}'
```

`SetCap` takes effect immediately. Expanding capacity opens new semaphore
slots and queued requests start being admitted on their next poll. Shrinking
drains gracefully — in-flight requests finish normally.

---

## Config reload

```go
func onConfigReload(cfg Config) {
    wr.SetCap(int32(cfg.MaxConcurrent))
    wr.SetReaperInterval(cfg.ReaperInterval)
    wr.SetMaxQueueDepth(cfg.MaxQueueDepth)
}
```

Every setter is safe to call while traffic is flowing.

---

## How it works

```
            ┌─────────────────────────────────────────────────────┐
            │                  Incoming Request                   │
            └─────────────┬───────────────────────────────────────┘
                          │
                          ▼
                 ┌────────────────┐
                 │  Issue Ticket  │  nextTicket.Add(1)
                 └───────┬────────┘
                         │
                         ▼
              ┌──────────────────────┐     YES    ┌──────────────┐
              │  ticket ≤ nowServing │ ──────────▶ │ Acquire Slot │
              │      + cap?         │             │  (fast path) │
              └──────────┬──────────┘             └──────┬───────┘
                         │ NO                            │
                         ▼                               ▼
              ┌──────────────────────┐          ┌────────────────┐
              │  Serve Waiting Room  │          │   Run Handler  │
              │  + Issue Cookie      │          │   defer Release│
              └──────────┬──────────┘          └────────────────┘
                         │
                         ▼
              ┌──────────────────────┐
              │  Client Polls        │  GET /queue/status
              │  /queue/status       │  every 3s + jitter
              └──────────┬──────────┘
                         │ ready=true
                         ▼
              ┌──────────────────────┐
              │  Client Reloads      │  Browser auto-redirects
              │  → Fast Path         │  with cookie → admitted
              └──────────────────────┘
```

| Layer | Responsibility |
|---|---|
| **Ticket counter** | Monotonically increasing position on arrival |
| **Serving window** | `nowServing + cap` determines who gets in |
| **Semaphore** | Enforces concurrent slot limit via [sema](https://github.com/andreimerlescu/sema) |
| **Token store** | Maps session cookies to tickets for poll-based admission |
| **Reaper** | Evicts abandoned tokens, advances the queue past ghost tickets |
| **Callbacks** | Fires lifecycle events for autoscaling and observability |

---

## Security considerations

| Concern | How room handles it |
|---|---|
| **Cookie theft / replay** | Tokens are 128-bit cryptographically random hex strings. `HttpOnly` flag prevents XSS reads. Call `SetSecureCookie(true)` in production for the `Secure` flag. |
| **Queue flooding** | `SetMaxQueueDepth(n)` rejects new arrivals with 503 when the queue exceeds `n`, preventing unbounded memory growth. |
| **Poll abuse** | Per-token rate limiting on `/queue/status` — polls faster than 1/second receive 429 with `Retry-After`. |
| **Ghost tickets** | The reaper runs on a configurable interval (default 5m), evicts expired tokens, and advances `nowServing` so the queue doesn't stall behind abandoned clients. |
| **Cookie scoping** | `SetCookiePath` and `SetCookieDomain` let you restrict cookie visibility in multi-app deployments. `SameSite=Lax` is set by default. |
| **Capacity enforcement** | The `nowServing` window guard prevents the serving window from inflating beyond `cap` even under adversarial client disconnection patterns. |

---

## API reference

```go
// ── Construction ──────────────────────────────────────────

// Simple — one line, panics on invalid cap.
room.NewWaitingRoom(r *gin.Engine, cap int32) gin.HandlerFunc

// Full control — error handling, lifecycle management.
wr := &room.WaitingRoom{}
wr.Init(cap int32) error
wr.Stop()

// ── Routing ───────────────────────────────────────────────

wr.RegisterRoutes(r *gin.Engine)     // recommended: registers status + middleware
wr.Middleware() gin.HandlerFunc      // manual: just the middleware
wr.StatusHandler() gin.HandlerFunc   // manual: just the status endpoint

// ── Configuration (safe to call at any time) ──────────────

wr.SetCap(cap int32) error
wr.SetHTML(html []byte)
wr.SetReaperInterval(d time.Duration) error
wr.SetSecureCookie(secure bool)
wr.SetMaxQueueDepth(max int64) error
wr.SetCookiePath(path string)
wr.SetCookieDomain(domain string)

// ── Introspection ─────────────────────────────────────────

wr.Cap() int32
wr.Len() int
wr.QueueDepth() int64
wr.Utilization() float64
wr.UtilizationSmoothed() float64
wr.ReaperInterval() time.Duration
wr.MaxQueueDepth() int64
wr.CookiePath() string
wr.CookieDomain() string

// ── Lifecycle callbacks ───────────────────────────────────

wr.On(event room.Event, fn room.CallbackFunc)
wr.Off(event room.Event)
```

---

## Testing

The test suite includes unit tests, race-detector tests, fuzz tests, and
benchmarks:

```bash
make all    # vet, test, race, fuzz (30s), bench
```

```
BenchmarkFastPath-28             429842    2751 ns/op    5318 B/op    13 allocs/op
BenchmarkQueueDepth-28       1000000000    0.64 ns/op       0 B/op     0 allocs/op
BenchmarkUtilization-28      1000000000    0.88 ns/op       0 B/op     0 allocs/op
```

The fast path (request admitted immediately) completes in under 3μs
including cookie handling and semaphore acquisition. `QueueDepth` and
`UtilizationSmoothed` are sub-nanosecond — safe to call from hot
dashboards and autoscaler feedback loops.

---

## Sample app

The [`sample/basic-web-app`](sample/basic-web-app/) directory contains a
complete tutorial that walks through every feature:

```bash
cd sample/basic-web-app
bash test.sh                    # automated load test with live dashboard
```

Or run manually:

```bash
cd sample/basic-web-app
go run .                        # Terminal 1: starts server on :8080
open http://localhost:8080      # Browser: see the waiting room live
ab -c 100 -n 1000 localhost:8080/about   # Terminal 2: generate load
```

The tutorial covers capacity tuning, lifecycle callbacks, log filtering,
runtime capacity adjustment, custom HTML, and common mistakes.

\[ [Read the full tutorial →](sample/basic-web-app/README.md) \]

---

## License

Apache 2.0 — see [LICENSE](LICENSE).

---

*Built on [sema](https://github.com/andreimerlescu/sema) by
[Andrei Merlescu](https://github.com/andreimerlescu). FIFO ordering, live
position tracking, edge-triggered lifecycle callbacks, a reaper that keeps
ghost tickets from stalling your queue, and a circuit breaker that protects
your memory when the queue gets too deep.*
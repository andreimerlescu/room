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

High-value clients can **skip the line** by paying a per-position fee. The
price updates in real time as the queue moves. Paid clients receive a
**time-limited VIP pass** that auto-promotes them on re-entry — no second
payment required for the configured window.

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
position tick down in real time — and click "Skip the line" to test the
payment flow.

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
capacity adjustment, a max-queue-depth circuit breaker, skip-the-line
payments with real-time pricing, and time-limited VIP passes — all behind
a single middleware call.

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
- **Skip the line** — optional payment card with live pricing (appears only when configured)
- **VIP pass badge** — shown when the client has an active pass (auto-promoting)
- **Dark theme** — clean, modern design that works on mobile
- **Accessible** — uses `aria-live` regions for screen readers

Replace the default page with your own via `wr.SetHTML(myHTML)`. The
template contract is:

| Placeholder | Replaced with |
|---|---|
| `{{.Position}}` | Client's queue position (integer) |
| `{{.SkipURL}}` | Payment page URL (empty string if not configured) |

Your JavaScript should poll `fetch("/queue/status")` and react to these
JSON fields: `ready`, `position`, `skip_cost`, `rate_per_pos`, `has_pass`.

---

## Skip the line — paid queue jumping

`room` has built-in support for letting clients pay to skip the queue. The
pricing model is simple: the cost to jump is `distance × rate`, where
distance is how many positions the client wants to skip and rate is a
per-position price you define.

### How it works

1. You configure a `RateFunc` (pricing) and a `SkipURL` (payment page)
2. The waiting room page shows a "Skip the line" card with the live price
3. The client clicks "Pay to skip" and is sent to your payment handler
4. After payment verification, you call `wr.PromoteTokenToFront(token)`
5. The client's next poll returns `ready=true` and they're admitted

The price updates on every poll — if the queue shrinks while they're
deciding, the price drops in real time.

### Basic setup

```go
wr := &room.WaitingRoom{}
wr.Init(500)
defer wr.Stop()

// $2.50 per position. Position 40 → front costs $97.50.
wr.SetRateFunc(func(depth int64) float64 { return 2.50 })

// Where the "Pay to skip" button sends the client.
wr.SetSkipURL("/queue/purchase")

// Register your payment handler BEFORE RegisterRoutes so it
// bypasses the waiting room.
r.GET("/queue/purchase", handlePurchasePage)
r.POST("/queue/purchase/confirm", handlePurchaseConfirm)

wr.RegisterRoutes(r)
```

### Surge pricing

The `RateFunc` receives the current queue depth, so you can implement
dynamic pricing:

```go
// Base $1.00 + 5¢ per person in the queue.
// Queue of 100 → $6.00/position. Queue of 10 → $1.50/position.
wr.SetRateFunc(func(depth int64) float64 {
    return 1.00 + float64(depth)*0.05
})
```

### Quoting the price

Use `QuoteCost` to show the client what they'll pay before they commit.
It's read-only — no state changes:

```go
cost, err := wr.QuoteCost(token, 1) // cost to jump to position 1
if err != nil {
    // ErrTokenNotFound, ErrAlreadyAdmitted, ErrPromotionDisabled, etc.
}
```

### Processing the payment

After your payment provider confirms the charge:

```go
result, err := wr.PromoteTokenToFront(token)
if err != nil { ... }

log.Printf("promoted for $%.2f", result.Cost)

// Set the VIP pass cookie if a pass was issued.
if result.PassToken != "" {
    http.SetCookie(w, &http.Cookie{
        Name:     "room_pass",
        Value:    result.PassToken,
        Path:     wr.CookiePath(),
        MaxAge:   int(wr.PassDuration().Seconds()),
        HttpOnly: true,
        Secure:   true,
    })
}
```

`PromoteToken` also accepts an intermediate target position if you want
to offer partial skips at a lower price:

```go
// Jump from position 50 to position 10 instead of position 1.
result, err := wr.PromoteToken(token, 10)
```

### Stripe integration pattern

```go
// Before RegisterRoutes — bypasses the waiting room.
r.GET("/queue/purchase", func(c *gin.Context) {
    cookie, _ := c.Request.Cookie("room_ticket")
    cost, _ := wr.QuoteCost(cookie.Value, 1)

    session, _ := stripe.CheckoutSessions.New(&stripe.CheckoutSessionParams{
        LineItems: []*stripe.CheckoutSessionLineItemParams{{
            PriceData: &stripe.CheckoutSessionLineItemPriceDataParams{
                Currency:   stripe.String("usd"),
                UnitAmount: stripe.Int64(int64(cost * 100)),
                ProductData: &stripe.CheckoutSessionLineItemPriceDataProductDataParams{
                    Name: stripe.String("Skip the line"),
                },
            },
            Quantity: stripe.Int64(1),
        }},
        Mode:               stripe.String("payment"),
        ClientReferenceID:  stripe.String(cookie.Value),
        SuccessURL:         stripe.String("https://example.com/"),
        CancelURL:          stripe.String("https://example.com/"),
    })
    c.Redirect(http.StatusSeeOther, session.URL)
})

// Stripe webhook.
r.POST("/stripe/webhook", func(c *gin.Context) {
    event := verifyStripeSignature(c)
    session := event.Data.Object
    token := session["client_reference_id"].(string)

    result, err := wr.PromoteTokenToFront(token)
    if err != nil { c.Status(400); return }

    // Pass cookie is set on the client's next page load via the
    // success URL handler — Stripe webhooks can't set cookies.
    storePassForClient(token, result.PassToken)

    c.Status(200)
})
```

---

## VIP passes — durable skip-the-line access

By default, a promotion is single-use: the client is moved to the front
once, and if they re-enter the queue later, they wait like everyone else.

**VIP passes** change this. When configured, every promotion automatically
issues a time-limited pass. If the client is evicted, times out, refreshes,
or re-enters the queue during the pass window, they are silently
auto-promoted to the front — no second payment required.

### Enabling passes

```go
// Pay once, skip for 90 minutes.
wr.SetPassDuration(90 * time.Minute)
```

That's it. `PromoteToken` and `PromoteTokenToFront` now return a
`PassToken` in their result, and the middleware checks for the `room_pass`
cookie on every request.

### How it works under the hood

1. Client pays and you call `PromoteTokenToFront` → result includes `PassToken`
2. You set `PassToken` as the `room_pass` cookie (alongside `room_ticket`)
3. Client is admitted, browses, eventually their slot is released
4. Client returns → new ticket is issued → middleware sees `room_pass` cookie
5. Middleware calls `autoPromote` → client jumps to front automatically
6. Pass expires after the configured duration → client returns to standard FIFO

### Pass lifecycle

| Event | What happens |
|---|---|
| Payment confirmed | `PromoteTokenToFront` returns `PassToken`; you set the `room_pass` cookie |
| Client admitted | Normal flow — pass is not consumed, just checked |
| Client re-enters queue | Middleware sees valid `room_pass`, calls `autoPromote` |
| Pass expires | `room_pass` cookie expires; `passStore` reaper cleans up server-side |
| Client returns after expiry | Standard FIFO — must pay again to skip |

### Configuration

```go
// Enable 90-minute passes.
wr.SetPassDuration(90 * time.Minute)

// Valid range: 1 minute – 24 hours. 0 disables passes (default).
wr.SetPassDuration(0) // back to single-use promotions

// Check programmatically.
if wr.PassDuration() > 0 {
    log.Printf("passes enabled: %s", wr.PassDuration())
}

// Check a specific pass token.
if wr.HasValidPass(passToken) {
    log.Println("client has active VIP pass")
}
```

### The status endpoint and passes

When a client has a valid pass, the `/queue/status` response changes:

```json
{
  "ready": false,
  "position": 3,
  "has_pass": true
}
```

Note that `skip_cost` and `rate_per_pos` are omitted — the client already
paid. The default waiting room page reacts to `has_pass` by showing a
"VIP pass active" badge instead of the purchase offer.

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

// Track skip-the-line revenue.
wr.On(room.EventPromote, func(s room.Snapshot) {
    metrics.Inc("room.promote")
    log.Printf("queue jump — depth now %d", s.QueueDepth)
})
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
| `EventPromote` | Client promoted via payment or VIP pass | Revenue tracking, fairness monitoring |

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

// Skip-the-line with surge pricing and 2-hour VIP passes.
wr.SetRateFunc(func(depth int64) float64 {
    return 1.00 + float64(depth)*0.05
})
wr.SetSkipURL("/queue/purchase")
wr.SetPassDuration(2 * time.Hour)

// Register lifecycle hooks before traffic arrives.
wr.On(room.EventFull, func(s room.Snapshot) {
    go provisionHost()
})
wr.On(room.EventPromote, func(s room.Snapshot) {
    metrics.Inc("room.skip_revenue")
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
    wr.SetPassDuration(cfg.PassDuration)
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
              │  Has room_pass?      │──YES──▶ autoPromote
              │  (VIP pass cookie)   │         (jump to front)
              └──────────┬──────────┘
                         │ NO
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
| **Pass store** | Maps VIP pass cookies to expiry times for auto-promotion |
| **Reaper** | Evicts abandoned tokens and expired passes, advances the queue past ghost tickets |
| **Callbacks** | Fires lifecycle events for autoscaling and observability |
| **Promoter** | Serialized ticket reassignment for skip-the-line with unique ticket guarantees |

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
| **Promotion serialization** | `PromoteToken` acquires a dedicated mutex and uses a monotonic insert counter to guarantee unique ticket assignment even under concurrent promotions. |
| **Pass expiry** | VIP passes have a server-side expiry check (not just cookie MaxAge). The reaper sweeps expired passes on every cycle. Expired `room_pass` cookies are ignored. |

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
wr.SetRateFunc(fn room.RateFunc)
wr.SetSkipURL(url string)
wr.SetPassDuration(d time.Duration) error

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
wr.SkipURL() string
wr.PassDuration() time.Duration
wr.HasValidPass(passToken string) bool

// ── Skip the line ─────────────────────────────────────────

wr.QuoteCost(token string, targetPosition int64) (float64, error)
wr.PromoteToken(token string, targetPosition int64) (room.PromoteResult, error)
wr.PromoteTokenToFront(token string) (room.PromoteResult, error)
wr.GrantPass() string

// ── Lifecycle callbacks ───────────────────────────────────

wr.On(event room.Event, fn room.CallbackFunc)
wr.Off(event room.Event)
```

### PromoteResult

```go
type PromoteResult struct {
    Cost      float64  // price computed at promotion time
    PassToken string   // VIP pass token (empty if passes disabled)
}
```

### RateFunc

```go
// Receives current queue depth, returns per-position cost.
type RateFunc func(queueDepth int64) float64
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
BenchmarkQuoteCost-28        ...
BenchmarkPromoteToken-28     ...
BenchmarkGrantPass-28        ...
BenchmarkHasValidPass-28     ...
```

The fast path (request admitted immediately) completes in under 3μs
including cookie handling and semaphore acquisition. `QueueDepth` and
`UtilizationSmoothed` are sub-nanosecond — safe to call from hot
dashboards and autoscaler feedback loops. `QuoteCost` and `HasValidPass`
are lock-free reads suitable for high-frequency polling.

---

## Sample app

The [`sample/basic-web-app`](sample/basic-web-app/) directory contains a
complete tutorial that walks through every feature including skip-the-line
payments and VIP passes:

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

The sample configures `$2.50/position` flat pricing with 90-minute VIP
passes. When you land in the waiting room, click "Skip the line" to see
the payment confirmation page, complete the (simulated) payment, and
get admitted immediately. Refresh the page — you'll be auto-promoted
thanks to your VIP pass.

The tutorial covers capacity tuning, lifecycle callbacks, log filtering,
runtime capacity adjustment, skip-the-line pricing, VIP pass configuration,
custom HTML, and common mistakes.

\[ [Read the full tutorial →](sample/basic-web-app/README.md) \]

---

## License

Apache 2.0 — see [LICENSE](LICENSE).

---

*Built on [sema](https://github.com/andreimerlescu/sema) by
[Andrei Merlescu](https://github.com/andreimerlescu). FIFO ordering, live
position tracking, edge-triggered lifecycle callbacks, skip-the-line
payments with real-time surge pricing, time-limited VIP passes with
auto-promotion, a reaper that keeps ghost tickets from stalling your queue,
and a circuit breaker that protects your memory when the queue gets too deep.*
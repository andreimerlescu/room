# room — FIFO Waiting Room for Go + Gin

> Drop-in waiting room middleware for [gin](https://github.com/gin-gonic/gin)
> web applications. Built on [sema](https://github.com/andreimerlescu/sema).

[![Go Reference](https://pkg.go.dev/badge/github.com/andreimerlescu/room.svg)](https://pkg.go.dev/github.com/andreimerlescu/room)
[![Go Report Card](https://goreportcard.com/badge/github.com/andreimerlescu/room)](https://goreportcard.com/report/github.com/andreimerlescu/room)
[![Apache 2.0 License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)

---

## Why room?

When your application is at capacity, you have three choices: drop the
request (429), queue it blindly (no ordering guarantee), or admit it
through a proper waiting room with FIFO ordering, position awareness,
and a live status page.

`room` does the third. It sits in front of your gin handlers as middleware,
issues every arriving request a ticket and admits them in ticket order as
slots open, though clients that become eligible simultaneously may be served
in any order among themselves. Clients that must wait see a clean waiting
room page that updates their position automatically — no refresh required.

And when the room fills up, your application finds out immediately — via
lifecycle callbacks — so it can provision new capacity, open a new host,
or update a load balancer before the queue grows.

---

## Installation

```
go get github.com/andreimerlescu/room
```

Requires **Go 1.22+**.

---

## Quick start

```go
r := gin.Default()

wr := &room.WaitingRoom{}
if err := wr.Init(500); err != nil {
    log.Fatal(err)
}
defer wr.Stop()

// Registers GET /queue/status and attaches the middleware.
wr.RegisterRoutes(r)
r.Run(":8080")
```

That's it. The 501st concurrent request sees the waiting room. The 500th
slot to free up admits them automatically.

---

## Lifecycle callbacks

`room` exposes a full lifecycle event system. Register handlers with `On`
and react to capacity changes in real time — without polling, without
a sidecar, without coupling your business logic to the middleware internals.

```go
// Scale out when the room fills up.
wr.On(room.EventFull, func(s room.Snapshot) {
    log.Printf("room full (%d/%d) — provisioning new host", s.Occupancy, s.Capacity)
    go provisionHost()
})

// Scale back in when the room drains.
wr.On(room.EventDrain, func(s room.Snapshot) {
    log.Printf("room drained — deregistering spare host")
    go deregisterHost()
})

// Observe every admission.
wr.On(room.EventEnter, func(s room.Snapshot) {
    metrics.Increment("room.enter")
})

// Observe every completion.
wr.On(room.EventExit, func(s room.Snapshot) {
    metrics.Increment("room.exit")
})

// React to clients being queued.
wr.On(room.EventQueue, func(s room.Snapshot) {
    log.Printf("request queued — depth now %d", s.QueueDepth)
})

// React to abandoned tickets being reaped.
wr.On(room.EventEvict, func(s room.Snapshot) {
    metrics.Increment("room.evict")
})

// React to context cancellations before admission.
wr.On(room.EventTimeout, func(s room.Snapshot) {
    metrics.Increment("room.timeout")
})
```

Every handler receives a `Snapshot` — a point-in-time copy of the room's
state at the moment the event fired:

```go
type Snapshot struct {
    Event      Event  // which lifecycle event fired
    Occupancy  int    // slots in use right now
    Capacity   int    // maximum concurrent slots
    QueueDepth int64  // requests currently waiting
}

func (s Snapshot) Full()  bool // Occupancy >= Capacity
func (s Snapshot) Empty() bool // Occupancy == 0
```

Handlers are invoked asynchronously — each in its own goroutine — so a
slow callback never stalls the request hot path. Remove all handlers for
an event at any time with `Off`:

```go
wr.Off(room.EventFull)
```

### Events at a glance

| Event | Fires when |
|---|---|
| `EventEnter` | A request acquires a slot and enters active service |
| `EventExit` | A request completes and releases its slot |
| `EventFull` | The room reaches capacity after an admission |
| `EventDrain` | The room transitions from full back to available |
| `EventQueue` | An arriving request is issued a waiting room ticket |
| `EventEvict` | The reaper removes an expired token from the queue |
| `EventTimeout` | A request's context is cancelled before admission |

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

// Tighten the reaper for a high-traffic event.
wr.SetReaperInterval(15 * time.Second)

// Register lifecycle hooks before traffic arrives.
wr.On(room.EventFull, func(s room.Snapshot) {
    go provisionHost()
})

// Registers GET /queue/status and attaches the middleware.
wr.RegisterRoutes(r)

r.Run(":8080")
```

---

## Config reload

```go
func onConfigReload(cfg Config) {
    wr.SetCap(int32(cfg.MaxConcurrent))
    wr.SetReaperInterval(cfg.ReaperInterval)
}
```

---

## How it works

| Layer | Responsibility |
|---|---|
| Ticket counter | Assigns each request a monotonically increasing position on arrival |
| FIFO gate | Blocks requests whose ticket is outside the serving window |
| sema | Manages how many requests are actively being served |
| Token store | Maps session cookies to tickets for `/queue/status` polling |
| Reaper | Evicts ghost tickets from clients that disconnected mid-queue |
| Callbacks | Fires lifecycle events so your app can react to capacity changes |

---

## API

```go
// Simple path.
room.NewWaitingRoom(r *gin.Engine, cap int32) gin.HandlerFunc

// Full control path.
wr.Init(cap int32) error
wr.Stop()
wr.Middleware() gin.HandlerFunc
wr.RegisterRoutes(r *gin.Engine)
wr.StatusHandler() gin.HandlerFunc
wr.SetHTML(html []byte)
wr.SetCap(cap int32) error
wr.SetReaperInterval(d time.Duration) error
wr.Cap() int32
wr.Len() int
wr.QueueDepth() int64
wr.Utilization() float64
wr.UtilizationSmoothed() float64
wr.ReaperInterval() time.Duration

// Lifecycle callbacks.
wr.On(event room.Event, fn room.CallbackFunc)
wr.Off(event room.Event)
```

---

## License

Apache 2.0 © [Andrei Merlescu](https://github.com/andreimerlescu)

---

*Built on [sema](https://github.com/andreimerlescu/sema). FIFO ordering,
live position tracking, a reaper that keeps ghost tickets from stalling
your queue, and lifecycle callbacks so your application can respond to
capacity events the moment they happen.*
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
issues every arriving request a ticket, and admits them strictly in order
as slots open. Clients that must wait see a clean waiting room page that
updates their position automatically — no refresh required.

---

## Installation

```
go get github.com/andreimerlescu/room
```

Requires **Go 1.21+**.

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

---

## API

```go
// Simple path.
room.NewWaitingRoom(cap int32) gin.HandlerFunc

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
```

---

## License

Apache 2.0 © [Andrei Merlescu](https://github.com/andreimerlescu)

---

*Built on [sema](https://github.com/andreimerlescu/sema). FIFO ordering,
live position tracking, and a reaper that keeps ghost tickets from stalling
your queue.*

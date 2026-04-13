# basic-web-app — room middleware tutorial

This sample walks you through adding a FIFO waiting room to a four-page
Gin web application from scratch. By the end you will have a running server
that admits at most N concurrent requests, queues the rest, and admits them
automatically in arrival order — with no client-side refresh required.

---

## Prerequisites

| Tool | Minimum version | Install |
|---|---|---|
| Go | 1.22 | https://go.dev/dl |
| Apache Bench | any | `apt install apache2-utils` / `brew install httpd` |

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

## Step 2 — Why `gin.New()` instead of `gin.Default()`

`gin.Default()` installs gin's own `Logger` middleware, which buffers each
log line and prints it **after** the handler returns. During a load test that
means you see nothing until the request is already complete — room events
and request logs appear out of order and the queue activity is invisible.

`gin.New()` gives you a blank engine. We install two middlewares manually:

```go
r := gin.New()
r.Use(gin.Recovery())  // keep the panic recovery middleware
r.Use(requestLogger()) // our logger — prints on entry AND exit
```

The custom `requestLogger` middleware in this sample prints a line the moment
a request arrives (`-->`) and another when it completes (`<--`). That means
you can see which requests are being held by the waiting room versus which
are executing their handler, in real time, as the load test runs.

---

## Step 3 — Initialise the WaitingRoom with a small capacity

```go
wr = &room.WaitingRoom{}
if err := wr.Init(5); err != nil {
    log.Fatalf("room.Init: %v", err)
}
defer wr.Stop()
```

A cap of **5** is deliberately small so that `ab -c 100` fills the room
immediately and you can watch the queue build and drain in the terminal.
In production you would set this to match your actual concurrency budget
— typically the size of your database connection pool or the rate limit of
your slowest downstream dependency.

---

## Step 4 — Add simulated latency to every handler

This is the most important step for making the waiting room visible during
a load test. Without it, handlers return in microseconds and the room never
fills up even at `-c 100` because requests complete faster than they arrive.

```go
const simulatedLatency = 500 * time.Millisecond

func aboutPage(c *gin.Context) {
    time.Sleep(simulatedLatency) // holds the semaphore slot for 500 ms
    c.Data(http.StatusOK, "text/html; charset=utf-8", page("About", body))
}
```

With `cap=5` and each request taking 500 ms, the server can process at most
10 requests per second. At `ab -c 100` you are sending 100 concurrent
requests, so roughly 95 of them will be queued immediately and admitted one
by one as slots open.

---

## Step 5 — Register lifecycle callbacks

Callbacks are what you will actually see in the terminal during the load
test. They fire asynchronously in their own goroutines — a slow callback
never stalls the request path.

```go
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
```

The `roomLog` helper prefixes every line with a fixed-width tag so you can
filter the output with `grep`:

```bash
go run main.go 2>&1 | grep '\[QUEUE\]'   # only queuing events
go run main.go 2>&1 | grep '\[FULL\]'    # only full-capacity events
go run main.go 2>&1 | grep -v '\[REQ\]'  # room events only, no request logs
```

---

## Step 6 — Register routes in the correct order

```go
// Routes registered before RegisterRoutes bypass the waiting room.
// Use this for health checks and readiness probes that must always succeed.
// r.GET("/healthz", func(c *gin.Context) { c.Status(http.StatusOK) })

wr.RegisterRoutes(r)

// Routes registered after RegisterRoutes are gated by the waiting room.
r.GET("/",        homePage)
r.GET("/about",   aboutPage)
r.GET("/pricing", pricingPage)
r.GET("/contact", contactPage)
```

---

## Step 7 — Run the server

**Terminal 1:**

```bash
go run main.go
```

You should see:

```
[ INFO  ] listening on http://localhost:8080  cap=5
```

---

## Step 8 — Run the load test

**Terminal 2:**

```bash
ab -t 60 -n 1000 -c 100 http://localhost:8080/about
```

| Flag | Meaning |
|---|---|
| `-t 60` | run for 60 seconds |
| `-n 1000` | send at most 1000 total requests |
| `-c 100` | maintain 100 concurrent connections |

---

## Step 9 — Read the logs

Switch back to Terminal 1. You will see output like this:

```
[ INFO  ] listening on http://localhost:8080  cap=5
[ REQ   ] --> GET /about  remote=127.0.0.1
[ REQ   ] --> GET /about  remote=127.0.0.1
[ REQ   ] --> GET /about  remote=127.0.0.1
[ REQ   ] --> GET /about  remote=127.0.0.1
[ REQ   ] --> GET /about  remote=127.0.0.1
[ ENTER   ] slot acquired  occupancy=1/5  queue=0  util=20%
[ ENTER   ] slot acquired  occupancy=2/5  queue=0  util=40%
[ ENTER   ] slot acquired  occupancy=3/5  queue=0  util=60%
[ ENTER   ] slot acquired  occupancy=4/5  queue=0  util=80%
[ ENTER   ] slot acquired  occupancy=5/5  queue=0  util=100%
[ FULL    ] capacity reached  occupancy=5/5  queue=0  util=100%
[ QUEUE   ] request queued  depth=1   occupancy=5/5  util=100%
[ QUEUE   ] request queued  depth=2   occupancy=5/5  util=100%
[ QUEUE   ] request queued  depth=3   occupancy=5/5  util=100%
...
[ EXIT    ] slot released  occupancy=4/5  queue=95  util=80%
[ DRAIN   ] room no longer full  occupancy=4/5  queue=95
[ ENTER   ] slot acquired  occupancy=5/5  queue=94  util=100%
[ FULL    ] capacity reached  occupancy=5/5  queue=94  util=100%
[ REQ   ] <-- GET /about  status=200  latency=500ms
[ REQ   ] --> GET /about  remote=127.0.0.1
[ EXIT    ] slot released  occupancy=4/5  queue=93  util=80%
...
```

Here is what each tag means in the context of this load test:

| Tag | What you are seeing |
|---|---|
| `[ REQ   ] -->` | A new HTTP connection arrived at the server |
| `[ ENTER   ]` | The request passed through the waiting room and is now running its handler |
| `[ FULL    ]` | All 5 slots are occupied — the next request will queue |
| `[ QUEUE   ]` | A request landed in the waiting room; `depth=N` is its position |
| `[ EXIT    ]` | A handler finished and released its slot |
| `[ DRAIN   ]` | The room dropped below full capacity — queued requests can now enter |
| `[ REQ   ] <--` | The HTTP response was sent; `latency` includes waiting room time |
| `[ EVICT   ]` | The reaper cleaned up a ghost ticket (ab closed a connection mid-wait) |
| `[ TIMEOUT ]` | A queued request's context was cancelled before it was admitted |

### What the queue depth column tells you

The `queue=N` value in `QUEUE` events shows how many requests are waiting
behind the one that just joined. Watch it climb during the flood and fall
as handlers complete and admit the next waiter. When `queue=0` appears in
`EXIT` events, the backlog has cleared.

### What a healthy load test looks like

- `FULL` fires once at the start and only fires again after a `DRAIN`.
- `DRAIN` fires every time the occupancy drops below 5 and a queued request
  enters.
- `QUEUE` depth climbs quickly at the start then stays roughly stable or
  trends downward as ab's concurrency saturates.
- `TIMEOUT` events appear only if ab's connection timeout is shorter than
  the time a request spends waiting in the queue. Increase `-t` on ab or
  add `-s 60` (socket timeout) to reduce spurious timeouts.
- `EVICT` events appear only after the reaper runs (every 10 s in this
  sample). Each eviction means a client disappeared mid-queue — normal
  during ab runs since ab recycles connections aggressively.

---

## Step 10 — Read the ab report

When ab finishes it prints a summary. With `cap=5` and 500 ms handlers the
numbers will look roughly like this:

```
Concurrency Level:      100
Time taken for tests:   60.012 seconds
Complete requests:      1000
Failed requests:        0
Requests per second:    16.66 [#/sec] (mean)
Time per request:       6001.2 [ms] (mean)
Time per request:       60.01 [ms] (mean, across all concurrent requests)
```

The mean time per request of ~6 s reflects queuing time: a request that
arrives when the queue is 10 deep waits 10 × 500 ms before its handler
runs. That is the waiting room working as designed — absorbing burst traffic
instead of dropping it or crashing the downstream.

To increase throughput, call `wr.SetCap` with a higher value and re-run ab:

```bash
# in a third terminal while the server is running
curl -s http://localhost:8080/  # confirm server is up, then edit main.go
# or wire up an admin endpoint as shown in the runtime-adjustment section
```

---

## Grepping the logs for specific events

```bash
# Room events only — filter out per-request noise
go run main.go 2>&1 | grep -v '\[ REQ'

# Only queueing events — see the queue depth grow
go run main.go 2>&1 | grep '\[ QUEUE'

# Only full-capacity moments
go run main.go 2>&1 | grep '\[ FULL'

# Count how many requests were queued
go run main.go 2>&1 | grep -c '\[ QUEUE'

# Watch the queue depth trend
go run main.go 2>&1 | grep '\[ QUEUE' | awk '{print $6}'
```

---

## Runtime capacity adjustment

Because `wr` is a package-level variable you can change capacity without
restarting the server. Wire up an admin endpoint:

```go
// Register this BEFORE wr.RegisterRoutes so it bypasses the waiting room.
r.POST("/admin/cap", func(c *gin.Context) {
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
})
```

While ab is running in Terminal 2, change the cap in Terminal 3:

```bash
# Double capacity — queued requests will immediately start being admitted
curl -s -X POST http://localhost:8080/admin/cap \
     -H 'Content-Type: application/json' \
     -d '{"cap": 10}' | jq

# Halve it again
curl -s -X POST http://localhost:8080/admin/cap \
     -H 'Content-Type: application/json' \
     -d '{"cap": 5}' | jq
```

Watch the server logs — you will see a burst of `ENTER` events as queued
requests rush into the newly opened slots when you expand, and then `FULL`
almost immediately as the new capacity fills.

---

## Common mistakes

### Handlers return too fast — the room never fills up

```go
// ✗ Wrong — returns in microseconds, room stays at occupancy=1
func aboutPage(c *gin.Context) {
    c.String(http.StatusOK, "About")
}

// ✓ Correct for testing — holds the slot long enough to observe queuing
func aboutPage(c *gin.Context) {
    time.Sleep(500 * time.Millisecond)
    c.String(http.StatusOK, "About")
}
```

In production you do not need `time.Sleep` — real database queries,
template rendering, and downstream API calls provide the natural latency
that holds slots open.

### Using `gin.Default()` — room events are buried in buffered output

```go
// ✗ gin's Logger buffers and only prints after the handler returns.
//   Room events appear out of order; the queue activity is invisible.
r := gin.Default()

// ✓ Build the logger yourself so it prints on arrival, not completion.
r := gin.New()
r.Use(gin.Recovery())
r.Use(requestLogger())
```

### Registering application routes before `RegisterRoutes`

```go
// ✗ /about is not gated — it bypasses the waiting room entirely
r.GET("/about", aboutPage)
wr.RegisterRoutes(r)

// ✓ All four pages are protected
wr.RegisterRoutes(r)
r.GET("/about", aboutPage)
```

### Forgetting `defer wr.Stop()`

Without it the reaper goroutine outlives the `http.Server`. In tests that
construct and discard `WaitingRoom` instances it leaks goroutines and
triggers the race detector.

---

## File layout

```
sample/basic-web-app/
├── main.go      ← the result of this tutorial
├── README.md    ← this file
└── go.mod
```

---

## Testing It Yourself

This allows you to connect to [localhost:8080](http://localhost:8080/about) and see 
yourself in the waiting room, then get entered. Hit refresh, you're back in the room.

It's easy to do: 

```bash
chmod +x test.sh
./test.sh
```

Then [connect to localhost](https://localhost:8080/about) and see it for yourself!

```log
╭─andrei@Andreis-Mac-Studio ~/work/personal/room/sample/basic-web-app ‹main› 
╰─$ ./test.sh             

╔══════════════════════════════════════════════════╗
║   room — Waiting Room Load Test                 ║
╚══════════════════════════════════════════════════╝

  target:       http://localhost:8080/about
  concurrency:  30 simultaneous clients
  duration:     30s
  ramp delay:   50ms between client launches

Building server...
Starting server...
✓ Server is up at http://localhost:8080 (PID 68079)

──────────────────────────────────────────────────────────────────────
  Open http://localhost:8080/ in your browser to see the waiting room.
──────────────────────────────────────────────────────────────────────

  Server log: tail -f /var/folders/0m/y8d29v892039ldkgqkxbfvvh0000gn/T/tmp.9v9lhtoiY9/server.log

▶ Starting load test...

  [ 29s] sent:191  served:61   queued:125  err:0   active:130 ~2 req/s [wave 7]   

⏳ Draining in-flight requests (up to 30s)...

  [ 59s] sent:191  served:131  queued:55   err:0   active:60  ~2 req/s [draining]   

Server lifecycle events:
──────────────────────────────────────────────────────────────────────
2026/04/13 15:24:05 [ ENTER   ] slot acquired  occupancy=1/5  queue=0  util=20%
2026/04/13 15:24:05 [ EXIT    ] slot released  occupancy=0/5  queue=0  util=0%
2026/04/13 15:24:07 [ ENTER   ] slot acquired  occupancy=1/5  queue=0  util=20%
2026/04/13 15:24:08 [ ENTER   ] slot acquired  occupancy=2/5  queue=0  util=40%
2026/04/13 15:24:08 [ ENTER   ] slot acquired  occupancy=3/5  queue=0  util=60%
2026/04/13 15:24:08 [ ENTER   ] slot acquired  occupancy=4/5  queue=0  util=80%
2026/04/13 15:24:08 [ FULL    ] capacity reached  occupancy=5/5  queue=0  util=100%
2026/04/13 15:24:08 [ ENTER   ] slot acquired  occupancy=5/5  queue=0  util=100%
2026/04/13 15:24:08 [ QUEUE   ] request queued  depth=1  occupancy=5/5  util=100%
2026/04/13 15:24:08 [ QUEUE   ] request queued  depth=2  occupancy=5/5  util=100%
2026/04/13 15:24:08 [ QUEUE   ] request queued  depth=3  occupancy=5/5  util=100%
2026/04/13 15:24:08 [ QUEUE   ] request queued  depth=4  occupancy=5/5  util=100%
2026/04/13 15:24:08 [ EXIT    ] slot released  occupancy=4/5  queue=3  util=80%
2026/04/13 15:24:08 [ DRAIN   ] room no longer full  occupancy=4/5  queue=3
2026/04/13 15:24:08 [ QUEUE   ] request queued  depth=4  occupancy=4/5  util=80%
2026/04/13 15:24:08 [ EXIT    ] slot released  occupancy=3/5  queue=3  util=60%
2026/04/13 15:24:08 [ QUEUE   ] request queued  depth=4  occupancy=3/5  util=60%
2026/04/13 15:24:08 [ QUEUE   ] request queued  depth=5  occupancy=3/5  util=60%
2026/04/13 15:24:08 [ EXIT    ] slot released  occupancy=2/5  queue=4  util=40%
2026/04/13 15:24:08 [ QUEUE   ] request queued  depth=5  occupancy=2/5  util=40%
2026/04/13 15:24:08 [ EXIT    ] slot released  occupancy=1/5  queue=4  util=20%
2026/04/13 15:24:08 [ QUEUE   ] request queued  depth=5  occupancy=1/5  util=20%
2026/04/13 15:24:08 [ EXIT    ] slot released  occupancy=0/5  queue=4  util=0%
2026/04/13 15:24:08 [ QUEUE   ] request queued  depth=5  occupancy=0/5  util=0%
2026/04/13 15:24:08 [ QUEUE   ] request queued  depth=6  occupancy=0/5  util=0%
2026/04/13 15:24:08 [ QUEUE   ] request queued  depth=7  occupancy=0/5  util=0%
2026/04/13 15:24:08 [ QUEUE   ] request queued  depth=8  occupancy=0/5  util=0%
2026/04/13 15:24:08 [ QUEUE   ] request queued  depth=9  occupancy=0/5  util=0%
2026/04/13 15:24:09 [ QUEUE   ] request queued  depth=10  occupancy=0/5  util=0%
2026/04/13 15:24:09 [ QUEUE   ] request queued  depth=11  occupancy=0/5  util=0%
  (no lifecycle events captured)
^C
Stopping server (PID 68079)...
```

---

## License

Apache 2.0 — see the root [`LICENSE`](../../LICENSE) file.
```
# room — FIFO Waiting Room for Go + Gin

> Drop-in waiting room middleware for [gin](https://github.com/gin-gonic/gin)
> (plus a `net/http` status handler). Built on [sema](https://github.com/andreimerlescu/sema).

[![Go Reference](https://pkg.go.dev/badge/github.com/andreimerlescu/room.svg)](https://pkg.go.dev/github.com/andreimerlescu/room)
[![CI](https://github.com/andreimerlescu/room/actions/workflows/go.yml/badge.svg)](https://github.com/andreimerlescu/room/actions/workflows/go.yml)
[![Apache 2.0 License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)
![Unique](https://counthis.app/github?repository=andreimerlescu/room&label=Unique) ![Visitors](https://counthis.app/g/qyjy5vwjtjgbjfzy)

When your Go service hits capacity, don't drop requests — queue them.

`room` is a single-import middleware that sits in front of your Gin handlers
and turns excess traffic into an orderly waiting room. Every request gets a
ticket. Clients that can't be served immediately see a live-updating queue
page (or JSON wait response for API/agent clients) with their position. As
slots open, they are admitted automatically in ticket order.

- **Exactly-once ticket accounting** via a ghost ledger so abandoned clients never stall the line
- **Zero-over-admission `SetCap` shrinking** preserving in-flight requests and held leases
- **`O(log N)` indexed queue & parallel `RLock` polling** with sub-microsecond hot paths at 100k queued clients
- **Keyed tickets & leases** for API clients and AI agents without cookies, plus a `net/http` status handler
- **Paid skip-the-line, VIP passes, operator promotions, and ranking**
- **Restart persistence** (`Export`/`Import`) and **async lifecycle callbacks**

```go
wr := &room.WaitingRoom{}
wr.Init(500)
defer wr.Stop()
wr.RegisterRoutes(r)
// Request 501 sees the waiting room.
```

---

## Installation

```sh
go get github.com/andreimerlescu/room@v1.8.0
```

Requires [gin](https://github.com/gin-gonic/gin) and the Go version declared in
`go.mod`. Full API documentation is on
[pkg.go.dev](https://pkg.go.dev/github.com/andreimerlescu/room); release notes
and upgrade guides live in [CHANGELOG.md](CHANGELOG.md).

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

	// Registers OPTIONS/GET /queue/status, GET /queue/status/:ticket, and attaches the middleware.
	// Every route registered AFTER this line is gated.
	wr.RegisterRoutes(r)

	r.GET("/", func(c *gin.Context) {
		c.String(200, "You're in!")
	})

	r.Run(":8080")
}
```

To see a live dashboard demo in 30 seconds, run `cd sample/basic-web-app && bash test.sh`
(see [`sample/basic-web-app/README.md`](sample/basic-web-app/README.md)).

---

## Waiting room page & custom HTML

<img src="/waiting_room.jpg" alt="waiting_room.html preview" width="369px" />

Queued browsers receive an embedded HTML page that polls `/queue/status` every
3 seconds (plus jitter), updates position in place, refreshes session cookies
automatically during arbitrarily long waits, detects cookie-blocking browsers
via `room_probe`, and reloads to enter once `ready: true`.

Replace the page at any time with `wr.SetHTML(myHTML)` (pass `nil` to revert).
Supported placeholders:

| Placeholder | Replaced with |
|---|---|
| `{{.Position}}` | Client's queue position (integer) |
| `{{.SkipURL}}` | Payment URL (HTML-escaped; empty if unset) |
| `{{.ProbeCookie}}` | Probe cookie name (`room_probe`, or `<prefix>_probe` with `SetCookiePrefix`) |

Custom pages should check `document.cookie` for `{{.ProbeCookie}}` (or `room_probe`) before polling,
poll `/queue/status` with credentials, reload when `ready: true`, and stop
polling if `cookies_required: true`.

---

## Tuning for live events

```go
wr.SetFirstPollGrace(30 * time.Second) // reclaim clients that never come back
wr.SetReaperInterval(30 * time.Second) // eviction cadence
wr.SetMaxQueueDepth(20000)             // 503 new arrivals beyond this
wr.SetSecureCookie(true)               // behind TLS / TLS-terminating proxy
```

| Setting | Default | What it controls |
|---|---|---|
| `SetTokenTTL` | `5m` (`DefaultTokenTTL`) | Sliding-window lifetime for abandoned tokens (reset on every poll/reload). `0` restores default. |
| `SetFirstPollGrace` | off (`0`) | Reclaims tickets whose clients never return after issuance (cookieless clients, bots, instant abandons). |
| `SetReaperInterval` | `5m` (`DefaultReaperInterval`) | Eviction pass cadence (shortened automatically while `FirstPollGrace` is smaller). `0` restores default. |
| `SetMaxQueueDepth` | unlimited (`0`) | Circuit breaker for new arrivals; trips when either `QueueDepth` or `LiveQueueDepth` reaches the limit. |

Every setter (`SetCap`, `SetTokenTTL`, `SetReaperInterval`, `SetFirstPollGrace`,
`SetMaxQueueDepth`, `SetPassDuration`, `SetHTML`, `SetSkipURL`, `SetRateFunc`,
`SetClientKeyFunc`, `SetSecureCookie`, `SetCookiePath`, `SetCookieDomain`,
`SetCookiePrefix`, `SetScope`, `SetService`, `SetSigner`, `SetVerifier`,
`SetSigningKey`, `SetVerifyKeys`, `SetEd25519VerifyKeys`, `SetSignedMaxTTL`,
`SetLeaseFraction`, `SetClockSkew`, `SetTicketKeyFunc`, `SetTokenExtractor`,
`SetWaitJSON`, `SetWaitResponder`, `SetStatusBaseURL`, `SetLeaseTTL`) is safe to
call concurrently while serving traffic. Functional options may also be passed
directly to `Init(cap, opts...)`. Call `wr.Validate()` (also run automatically
by `RegisterRoutes`, which panics on misconfiguration) before serving. Only
`Init` and `Import` must run before serving.

### Changing capacity

```go
wr.SetCap(1000) // opens slots immediately; queued clients admit on next poll
wr.SetCap(200)  // zero-over-admission graceful shrink
```

Growing takes effect immediately. Shrinking is **graceful with zero
over-admission**: all in-flight requests and held leases are preserved, `Len()`
stays synchronized with active occupancy, `Utilization()` and
`UtilizationSmoothed()` stay clamped to `[0.0, 1.0]`, and no new slot is
granted until occupancy drops below the new capacity (when `EventDrain` fires
once).

---

## Managing the line

```go
// Inspect the first 100 clients in O(log N + limit) without blocking status polls.
for _, t := range wr.Queue(100) {
	fmt.Println(t.Place, t.Position, t.ClientKey, t.Seen, t.Rank)
}

// Tag queued tickets with a client IP/ID (capped at 256 bytes).
wr.SetClientKeyFunc(func(c *gin.Context) string { return c.ClientIP() })

// Ban or kick waiting clients immediately (frees their place at once).
_ = wr.RemoveToken(token)
n := wr.RemoveTokensFunc(func(t room.TicketInfo) bool {
	return bannedRange.Contains(net.ParseIP(t.ClientKey))
})

// Move a waiting client forward without pricing, or insert strictly by priority rank.
_ = wr.AdminPromote(token, 1)
moved, _ := wr.SetTicketRank(token, 2)
```

- **Promotions (`PromoteToken`, `AdminPromote`)** place a ticket at the target
  position (at worst tying with earlier promotions to the same target).
- **Ranking (`SetTicketRank`)** inserts strictly ahead of all lower ranks in
  arrival order within each rank in `O(log N)`, shifting intervening tickets
  and pending ghosts cleanly; lowering a rank records it without moving back.

---

## Skip the line & VIP passes

```go
wr.SetRateFunc(func(depth int64) float64 { return 1.00 + float64(depth)*0.05 })
wr.SetSkipURL("/queue/purchase")
wr.SetPassDuration(15 * time.Minute) // optional: auto-promote on re-entry (<= SignedMaxTTL when signing)

cost, err := wr.QuoteCost(token, 1)
res, err := wr.PromoteTokenToFront(token)
if res.PassToken != "" {
	http.SetCookie(w, &http.Cookie{
		Name: wr.PassCookieName(), Value: res.PassToken, Path: wr.CookiePath(),
		MaxAge: int(wr.PassDuration().Seconds()), HttpOnly: true, Secure: true,
	})
}
```

Register payment routes **before** `wr.RegisterRoutes(r)` so checkout endpoints
bypass the waiting room.

---

## API clients, AI agents & leases

### Keyed tickets through the middleware

```go
wr.SetTicketKeyFunc(func(c *gin.Context) (string, bool) {
	return tokenIDFrom(c.GetHeader("Authorization")) // ok=false falls back to cookies
})
```

Keyed requests receive no cookies and get a `503 Service Unavailable` JSON wait
response (`Retry-After: 3`, `waiting`, `position`, `queue_depth`, `status_url`,
`retry_after`). `status_url` points to a read-only public status page
(`GET /queue/status/{id}`) that never refreshes TTL and cannot claim admission.
Set `wr.SetWaitJSON(true)` to send JSON to non-HTML cookie clients (`fetch`,
`curl`), or `wr.SetWaitResponder` for full control.

### Direct line & lease APIs (with or without gin)

```go
// Application-managed admission:
ticketID, place, err := wr.Enter(key) // O(log N), never admits
place, ok := wr.Position(ticketID)    // O(log N), read-only
err = wr.Leave(key)

// Room-managed leases (slot held across requests until Release or LeaseTTL):
ok, place, err := wr.Acquire(key)
err = wr.Release(key)
wr.SetLeaseTTL(4 * time.Hour)

// Standard net/http status handler (OPTIONS/GET /queue/status and GET /queue/status/{id}):
mux.Handle("/wait/", http.StripPrefix("/wait", wr.Handler()))
wr.SetStatusBaseURL("/wait/queue/status/")
```

Leases share the room's capacity and line FIFO with normal requests; with
`SetTicketKeyFunc`, a lease holder's requests pass through without taking a
second slot. **Per-replica lease ceiling:** when a `Signer` or `Verifier` is
configured, held leases (local and remote signed) are bounded to `25%` of `cap`
by default (`SetLeaseFraction`, work-conserving, clamped to at least 1 for
`cap >= 2`, with overflow queuing at position 1 of the general line);
unconfigured single-process rooms keep `1.0` (`100%` of `cap`), and calling
`SetLeaseFraction(1.0)` restores that compatibility ceiling in signed mode.

### One line per process, verifiable capabilities across replicas

`room` stays strictly single-process and zero-dependency: `cap` is a per-replica
origin-protection budget. Operators who want a single global line per service
front it with a gateway or sidecar running `room`.

When serving multiple replicas without a shared store, VIP passes and leases
can be verified locally with cryptographic envelopes (`kid`, `kind`, `scope`,
`key`, `iat`, `exp`) instead of in-memory pass lookups — with zero coordination
on the hot path:

- **Ed25519 split mode (recommended):** Your checkout/control-plane service
  holds the private key via `room.NewTokenIssuer(scope, ed25519Signer)` and
  mints short-lived passes (`MintPass`) or leases (`MintLease`). Serving
  replicas hold only public keys (`WithEd25519VerifyKeys` / `SetEd25519VerifyKeys`)
  so a compromised replica can verify tokens (including rotated `kid`s) but
  cannot mint them (minting fails closed with `ErrNoSigner`).
- **HMAC symmetric mode:** Call `SetSigningKey` / `SetVerifyKeys` (or pass
  `WithSigningKey` / `WithVerifyKeys` to `Init`) for simple single-secret setups.
  When neither `Signer` nor `Verifier` is configured, `room` keeps the local
  in-memory pass store and `Export`/`Import` unchanged.
- **Per-service isolation:** `SetScope` binds tokens to one service (required
  whenever a `Signer` or `Verifier` is set — `Validate()` and `RegisterRoutes`
  fail closed if empty). `SetCookiePrefix("billing")` namespaces browser cookies
  (`billing_ticket`, `billing_pass`, `billing_probe`) independently from scope,
  or `SetService("billing")` sets both.
- **Short-lived signed TTLs:** Signed token lifetimes are capped by
  `SetSignedMaxTTL` (default `15m`, hard ceiling `MaxSignedTokenTTL = 1h`,
  zero rejected). If a `Signer` is configured and `PassDuration > SignedMaxTTL`,
  `Validate()` and `RegisterRoutes` fail at startup naming both durations.
- **Per-replica cap protection & bounded lease pool:** Verified passes promote
  to position 1 of the replica's line and still acquire a local slot within
  `cap`. Local and remote signed leases share a work-conserving ceiling
  (`SetLeaseFraction`, defaulting to `0.25` when signing is active, `1.0`
  otherwise for v1.x compatibility; clamped to at least 1 for `cap >= 2` when
  positive). Unused lease slots remain usable by ordinary requests, and when
  the lease ceiling is full, lease holders queue at position 1 of the general
  line rather than bypassing `cap` or being rejected.
- **Identity vs. capability & rejection events:** `TicketKeyFunc` identifies
  the client; `SetTokenExtractor` (default `X-Room-Pass` / `X-Room-Lease`
  headers with pass-cookie fallback) extracts capabilities. Browser cookie
  failures fall back to normal queuing; explicit API headers fall back on
  expiry and return `401`/`403` on tampering, wrong scope, wrong kind, or key
  mismatch. Every rejection emits `EventTokenRejected` with `Snapshot.Reason`
  and `Snapshot.TokenTransport`.

```go
// Issuer (checkout / control plane — holds private key):
signer, _ := room.NewEd25519Signer("k2", ed25519PrivKey)
issuer, _ := room.NewTokenIssuer("billing", signer)
passTok, _  := issuer.MintPass("", 15*time.Minute)
leaseTok, _ := issuer.MintLease("agent-42", 10*time.Minute)

// Serving replica (verify-only — holds only public keys, incl. rotated k1):
wr := &room.WaitingRoom{}
if err := wr.Init(500,
	room.WithService("billing"),
	room.WithEd25519VerifyKeys(map[string]ed25519.PublicKey{"k1": oldPub, "k2": newPub}),
	room.WithLeaseFraction(0.25),
); err != nil {
	log.Fatal(err)
}
```

---

## Lifecycle callbacks

```go
wr.On(room.EventFull,  func(s room.Snapshot) { go provisionHost() })
wr.On(room.EventDrain, func(s room.Snapshot) { go deregisterHost() })
wr.On(room.EventQueue, func(s room.Snapshot) { metrics.Inc("room.queue") })
```

Events: `EventEnter`, `EventExit`, `EventFull` (edge), `EventDrain` (edge),
`EventQueue`, `EventEvict`, `EventTimeout`, `EventPromote`, `EventRemove`,
`EventRank`, `EventLease`, `EventRelease`, `EventLeaseExpire`,
`EventTokenRejected`. Handlers run
asynchronously in their own goroutines; keep high-frequency handlers
non-blocking.

---

## Surviving restarts

Stop accepting traffic (`srv.Shutdown`), `wr.Export(f)` to a `0600` file
(contains bearer tokens), then on startup call `Init` + settings first,
`wr.Import(f)`, delete the file, and finally call `RegisterRoutes`:

```go
// Startup: apply TTL/grace before Import so staleness is judged accurately.
wr.Init(cap)
wr.SetTokenTTL(ttl)
wr.SetFirstPollGrace(30 * time.Second)
if f, err := os.Open(path); err == nil {
	stats, err := wr.Import(f)
	f.Close()
	os.Remove(path)
	log.Printf("restored=%d dropped=%d passes=%d leases=%d err=%v",
		stats.Restored, stats.DroppedStale, stats.PassesRestored, stats.LeasesRestored, err)
}
wr.RegisterRoutes(r)
```

Downtime is shifted forward so waiting clients and leases are not charged for
restart time; VIP passes keep their real wall-clock expiry.

---

## Long-lived connections (WebSockets, SSE)

Requests hold a slot until their handler returns, so register WebSocket or SSE
routes **before** `wr.RegisterRoutes(r)` (or dispatch around the middleware)
and apply a separate connection limit to them.

---

## How it works

```
arrival ──▶ ticket = nextTicket+1 ──▶ ticket ≤ nowServing+cap ? ──yes──▶ acquire slot ──▶ handler ──▶ release: nowServing+1
                                           │ no
                                           ▼
                        token + cookies, waiting page ◀── poll /queue/status (RLock + per-node lock, cookie refresh)
                                           │ ready
                                           ▼
                        reload with cookie ──▶ claim token ──▶ acquire slot ──▶ handler
```

| Layer | Role & Complexity |
|---|---|
| **Ticket counter & window** | Monotonic `nextTicket` vs `nowServing + cap` (lock-free atomics) |
| **Semaphore** | Hard concurrency ceiling via [sema](https://github.com/andreimerlescu/sema) with non-draining shrink |
| **Token store** | Maps + randomized order-statistic treap (`O(log N)` place/rank/promote, `O(log N + limit)` `Queue`) + `RLock` parallel polls |
| **Ghost ledger** | Order-statistic treap of retired tickets skipped when the window reaches them (`O(log G)` add/consume/count/shift) |
| **Reaper** | Typed intrusive `issuedAt` min-heaps (`O(1)` idle peek, `O(k log N)` bounded eviction batches) + low-water post-spike compaction |

---

## Security considerations

| Concern | Mitigation |
|---|---|
| **Cookie theft / replay** | 128-bit `crypto/rand` tokens, `HttpOnly`, `SameSite=Lax`. Call `SetSecureCookie(true)` behind TLS terminators. |
| **Bearer credentials** | `Snapshot.Token`, `TicketInfo.Token`, and `Export` files carry bearer tokens — never log them; write exports with `0600`. |
| **Queue & poll abuse** | `SetMaxQueueDepth` circuit breaker, `SetFirstPollGrace` ghost reclamation, and 1 req/s per-token/ID rate limiting (`429`). |
| **Keyed isolation** | Keyed tokens are internal and rejected if presented in cookies; `/queue/status/{id}` is read-only and never extends TTL. |
| **HTML safety** | `{{.SkipURL}}` is HTML-escaped on injection; `/queue/status/{id}` uses `html/template` and `Cache-Control: no-store`. |

---

## Testing & benchmarks

```sh
make all    # go vet, test, -race, 30s fuzz, bench
```

Measured on Apple M4 Pro (`-14`) with `go test -bench=. -benchmem ./...`:

| Benchmark | Time / op | Memory / allocs |
|---|---|---|
| `BenchmarkFastPath-14` | `1818 ns/op` | `5318 B / 13 allocs` |
| `BenchmarkStatusPoll_Accepted-14` (full Gin stack) | `957.1 ns/op` | `6928 B / 29 allocs` |
| `BenchmarkTokenStore_PollSingleLock-14` (14-way parallel) | `148.9 ns/op` | `0 B / 0 allocs` |
| `BenchmarkStatusPoll_Parallel_100k-14` (100k queued) | `147.3 ns/op` | `0 B / 0 allocs` |
| `BenchmarkEnter_ExistingKey_100k-14` (100k queued) | `150.9 ns/op` | `0 B / 0 allocs` |
| `BenchmarkView_TicketStatus_100k-14` (100k queued) | `151.3 ns/op` | `0 B / 0 allocs` |
| `BenchmarkSetTicketRank_100k-14` (100k queued) | `346.5 ns/op` | `0 B / 0 allocs` |
| `BenchmarkReap_NoExpired_100k-14` (100k queued) | `101.9 ns/op` | `0 B / 0 allocs` |
| `BenchmarkQueue_100of100k-14` (100 of 100k queued) | `7.2 µs/op` | `40 KB / 2 allocs` |
| `BenchmarkQueueDepth-14` | `0.98 ns/op` | `0 B / 0 allocs` |
| `BenchmarkUtilizationSmoothed-14` | `0.78 ns/op` | `0 B / 0 allocs` |

---

## License

Apache 2.0 — see [LICENSE](LICENSE).

*Built on [sema](https://github.com/andreimerlescu/sema) by [Andrei Merlescu](https://github.com/andreimerlescu).*

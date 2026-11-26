# Changelog

All notable changes to `github.com/andreimerlescu/room` are documented here.
This project follows semantic versioning; `v1.x` releases maintain full
exported API, cookie, HTTP endpoint, and callback backward compatibility.

## v1.9.0

- **Ready-claim deadline:** Added `SetReadyGrace` / `WithReadyGrace` /
  `ReadyGrace` (`5s–10m`, `0` disabled by default, `ErrReadyGrace`,
  `TicketInfo.ReadyAt`) to prevent serving-window head-of-line deadlocks by
  evicting unclaimed ready tickets. Applies to cookie tickets and
  `Middleware()`-queued keyed tickets while skipping programmatic
  `Enter()`/`Acquire()` tickets, and clamps background reaper cadence to
  `max(1s, readyGrace/2)`. Unconditionally freezes `issuedAt` once a ticket
  enters the serving window so continuous `/queue/status` polling without
  reloading can never pin a slot forever (even when `ReadyGrace` is disabled).
- **Idle-poll TTL:** Added `SetIdlePollTTL` / `WithIdlePollTTL` /
  `IdlePollTTL` (`10s–24h`, `0` disabled by default, `ErrIdlePollTTL`) to cap
  silent time for queued tickets independently from `TokenTTL` using the
  existing `O(k log N)` `ttlHeap` with automatic reaper cadence clamping, while
  `TokenTTL` continues governing cookie `MaxAge` and refresh cadence.
- **Rate-limited synchronous stale-reap before `503` / `ErrQueueFull`:** Before
  tripping `maxQueueDepth` in cookie `Middleware()`, keyed `Middleware()`,
  `Enter()`, and `Acquire()`, runs at most one bounded `reapBatch()` per
  `250ms` (single-flight CAS) and re-evaluates depth so stale/expired tickets
  cannot cause spurious `503` lockouts between reaper ticks.
- **Queue-full responder & rejection observability:** Added `SetFullResponder`
  / `WithFullResponder`, `FullResponder`, and `EventReject`. HTTP
  `maxQueueDepth` refusals set `Cache-Control: no-store` and `Retry-After`
  (default `3`s if unset), fire `EventReject` (also fired on `Enter`/`Acquire`
  `ErrQueueFull`), and invoke `FullResponder(c, snap)` outside all locks (or
  fall back to `503`).
- **Queue clearing & safe snapshot import:** Added `ClearQueue` and
  `ClearStale` for atomic bulk queue clearing and idle-ticket sweeping
  (emitting `EventRemove` once when tickets are removed). `Export`/`Import` now
  persist and downtime-shift `ready_at` and drop imported tickets exceeding
  `IdlePollTTL` or `ReadyGrace` (counted in `ImportStats.DroppedStale`).

## v1.8.0

**Ed25519 split mode for multi-replica deployments:** `room` stays single-process
and zero-dependency — `cap` remains a per-replica origin-protection budget, and
operators who want a single shared line front a service with a gateway or
sidecar. Across multiple replicas, VIP passes and leases no longer depend on an
in-memory store or a shared Redis/gossip cluster: your checkout or control-plane
service holds the Ed25519 private key in a standalone `TokenIssuer`
(`NewTokenIssuer`, `NewEd25519Signer`, `MintPass`, `MintLease`) and mints
short-lived `rm1` envelopes (`kid`, `kind`, `scope`, `key`, `iat`, `exp`), while
serving replicas hold only public verification keys (`SetEd25519VerifyKeys` /
`WithEd25519VerifyKeys`, indexed by `kid` for zero-downtime key rotation). A
compromised serving replica can verify tokens locally in pure crypto and cannot
mint them — any minting attempt on a verify-only replica fails closed with
`ErrNoSigner`.

- **Pluggable `Signer`/`Verifier` & symmetric fallback:** Added `Signer` and
  `Verifier` interfaces with built-in stdlib Ed25519 and HMAC-SHA256
  (`SetSigningKey`, `SetVerifyKeys`, `NewHMACSigner`, `NewHMACVerifier`).
  Verifier resolution happens dynamically at `Validate()`/verify time so
  `SetSigner` and `SetVerifier` are completely order-independent (`ErrNoVerifier`
  when a custom signer cannot derive one). When neither is configured, `room`
  keeps the local `passStore` and `Export`/`Import` unchanged with zero hot-path
  overhead (`0 B / 0 allocs` on in-memory poll and lease-renew paths).
- **Per-replica cap protection & work-conserving bounded lease pool:** Verified
  passes promote to position 1 of the replica's line and still acquire a local
  slot within `cap`. Local and remote signed leases draw from a work-conserving
  ceiling (`SetLeaseFraction`, `LeaseCeiling`) carved out of `cap` — defaulting
  to `0.25` when a `Signer`/`Verifier` is configured (clamped to `>= 1` for
  `cap >= 2` when positive) and `1.0` otherwise for full `v1.x` compatibility
  (also configurable via `SetLeaseFraction(1.0)`). Overflowing lease holders
  queue at the front of the general line rather than bypassing `cap` or being
  rejected.
- **Short-lived signed TTL cap (`1h` hard ceiling) & startup `Validate()`:**
  Signed tokens enforce `SetSignedMaxTTL` (`DefaultSignedMaxTTL = 15m`, hard
  ceiling `MaxSignedTokenTTL = 1h`, zero rejected via `ErrInvalidTokenTTL`
  distinct from `ErrSignedTokenTTLTooLong`). Added `Init(cap, opts...)`
  functional options and `Validate() error` (also run by `RegisterRoutes`, which
  panics naming both values when `PassDuration > SignedMaxTTL` with a signer
  configured, or when `Scope` is empty).
- **Per-service isolation & cookie prefixing:** Separated token security
  `SetScope` from browser cookie namespacing `SetCookiePrefix` (`<prefix>_ticket`,
  `<prefix>_pass`, `<prefix>_probe`, `{{.ProbeCookie}}` injected in
  `waiting_room.html`), with `SetService` configuring both.
- **Identity vs. capability & rejection observability:** Added
  `SetTokenExtractor` (default `X-Room-Pass` / `X-Room-Lease` with pass-cookie
  fallback) separate from `TicketKeyFunc`, split-by-transport failure handling
  (cookie failures and expired API headers fall back to the normal queue;
  tampered, wrong-scope, wrong-kind, or key-mismatched API headers abort with
  `401`/`403`), and `EventTokenRejected` (`Snapshot.Reason`,
  `Snapshot.TokenTransport`).

## v1.7.0

- **`O(log N)` indexed token store and ghost ledger:** Backed `tokenStore` and
  `ghostLedger` with internal randomized order-statistic treaps (`math/rand/v2`)
  featuring lazy range-add and subtree `maxRank`, eliminating all `O(N)`
  full-map scans across `Enter`, `Acquire`, `Position`, `TicketByKey`,
  `GET /queue/status/{id}`, `SetTicketRank`, `Queue(limit)` (`O(log N + limit)`),
  and `Export` (`O(N)` pre-sorted) while keeping dense ticket semantics exact.
- **Parallel read-locked polling:** `GET /queue/status`,
  `GET /queue/status/{id}`, and repeat keyed calls (`Enter`, `Acquire`,
  middleware) now run concurrently under `RLock` + a per-node leaf mutex
  instead of serializing on a global write lock.
- **Lazy-heap reaping & post-spike compaction:** `reapBatch` inspects only
  due entries via typed intrusive `issuedAt`-keyed min-heaps (`O(1)` when
  nothing is expired, `O(k log N)` bounded batches otherwise) with instant
  runtime `SetTokenTTL` / `SetFirstPollGrace` updates and automatic low-water
  map/heap compaction once spikes drain.
- **Stdlib alignment & hardening:** Replaced internal `atomic.Value` fields
  with `atomic.Pointer[T]` (making zero-value `CookiePath`, `CookieDomain`,
  and `SkipURL` panic-safe), HTML-escaped `{{.SkipURL}}` in
  `injectTemplateVars`, added `OPTIONS /queue/status` parity to
  `NewWaitingRoom`, and marked `NewWaitingRoomFromStruct` deprecated in favor
  of `wr.Middleware()`.

## v1.6.0

- **Zero-over-admission `SetCap` shrink:** Upgraded to `sema v1.2.0` so
  shrinking capacity below current occupancy preserves all in-flight requests
  and held leases without draining the semaphore, keeps `Len()` aligned with
  `Held()` and active occupancy, clamps `Utilization()` /
  `UtilizationSmoothed()` to `[0.0, 1.0]`, and blocks new admissions until
  occupancy drops below the new capacity (firing `EventDrain` once).
- **Lease restore into smaller capacity:** `Import` preserves semaphore
  occupancy when restoring more leases than the new room's capacity, keeping
  `Len()` and `Held()` aligned from startup.

## v1.5.0

- **Keyed tickets & leases:** Added `SetTicketKeyFunc`, `Enter`, `Position`,
  `Leave`, `TicketByKey`, `Acquire`, `Release`, `Held`, `HasLease`,
  `SetLeaseTTL`, `LeaseTTL`, `SetWaitJSON`, `WaitJSON`, `SetWaitResponder`,
  `WaitResponder`, `SetStatusBaseURL`, `StatusBaseURL`, `TicketStatusHandler`,
  `Handler` (`net/http`), `MaxKeyLen`, and `ErrInvalidKey`, `ErrQueueFull`,
  `ErrLeaseHeld`, `ErrNoLease`, `ErrLeaseTTL`.
- **New route:** `RegisterRoutes` and `NewWaitingRoom` register
  `GET /queue/status/:ticket`.
- **Clean reaper shutdown:** `Stop` now waits for the background reaper
  goroutine to exit.
- **Export format v2:** Exports taken while leases are held write version 2
  (refused by older rooms rather than dropping leases); `Import` accepts
  versions 1 and 2.
- **New fields & events:** `ImportStats.LeasesRestored`,
  `TicketInfo.Key`/`ID`/`Place`, `Snapshot.Key`, and `EventLease`,
  `EventRelease`, `EventLeaseExpire`.

## v1.4.0

- **Ranked tickets:** Added `SetTicketRank`, `TicketInfo.Rank`, `EventRank`,
  and `ErrInvalidRank`. Ranked tickets wait ahead of lower ranks in arrival
  order within each rank and survive `Export`/`Import`.

## v1.3.0 (Upgrading from v1.2.1)

- **Queue freezes fixed:** Abandoned tickets already inside the serving window
  are now retired through the ghost ledger rather than leaking capacity.
- **Long waits fixed:** `room_ticket` and `room_probe` are refreshed on status
  polls (at most once per `TokenTTL/3`) and on waiting-page reloads.
- **Rate-limited pollers stay alive:** 429 polls refresh the token's sliding
  TTL.
- **Promotion fairness & race fixes:** Promotions land at the exact target
  position accounting for ghosts; later payers never jump ahead of earlier ones
  to the same target, and concurrent removals cannot be resurrected.
- **Defaults & new APIs:** `SetTokenTTL(0)` and `SetReaperInterval(0)` restore
  defaults (`DefaultTokenTTL`, `DefaultReaperInterval`). Added `RemoveToken`,
  `RemoveTokensFunc`, `Queue`, `Ticket`, `TicketInfo`, `SetClientKeyFunc`,
  `ClientKeyFunc`, `AdminPromote`, `SetFirstPollGrace`, `FirstPollGrace`,
  `Export`, `Import`, `ImportStats`, `EventRemove`, `Snapshot.Token`,
  `Snapshot.ClientKey`, `Snapshot.StaleTicket`, and `ErrFirstPollGrace`,
  `ErrImportNotEmpty`, `ErrImportFormat`.

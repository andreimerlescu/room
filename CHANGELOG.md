# Changelog

All notable changes to `github.com/andreimerlescu/room` are documented here.
This project follows semantic versioning; `v1.x` releases maintain full
exported API, cookie, HTTP endpoint, and callback backward compatibility.

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

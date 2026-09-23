package room

import "time"

// Exported defaults. Passing 0 to the corresponding setter restores these
// values, so callers can express "use the default" without hard-coding
// them.
const (
	// DefaultTokenTTL is the default sliding-window lifetime of a queued
	// client's token. SetTokenTTL(0) restores it.
	//
	// See defaultTokenTTL for the reasoning behind the value.
	DefaultTokenTTL = 5 * time.Minute

	// DefaultReaperInterval is the default interval between eviction
	// passes. SetReaperInterval(0) restores it.
	DefaultReaperInterval = 5 * time.Minute
)

const (
	// cookieName is the HTTP-only session cookie issued to queued clients.
	cookieName = "room_ticket"

	// passCookieName is the HTTP-only cookie that holds the VIP pass
	// token. This cookie outlives individual queue tickets — it persists
	// for the configured pass duration so that clients who paid to skip
	// are auto-promoted on re-entry without paying again.
	passCookieName = "room_pass"

	// probeCookieName is a deliberately NON-HttpOnly cookie set alongside
	// room_ticket on every waiting-room render. It carries no secret and
	// no state — its only purpose is to be readable from JavaScript via
	// document.cookie so the waiting room page can determine whether the
	// browser is actually storing our cookies.
	//
	// Without this probe, a client with cookies disabled cannot detect its
	// own condition: room_ticket is HttpOnly and therefore invisible to
	// JS, so the page polls /queue/status, receives ready=true (no cookie
	// means no queue position), reloads, is issued a fresh ticket at the
	// back of the line, and repeats indefinitely — never admitted, while
	// leaking a token-store entry and a nextTicket increment every cycle.
	//
	// The probe is safe to expose: it is a constant value with no session
	// meaning. An attacker forging it gains nothing, since admission is
	// decided entirely by room_ticket.
	probeCookieName = "room_probe"

	// probeCookieValue is the constant value written to probeCookieName.
	probeCookieValue = "1"

	// defaultTokenTTL is the default lifetime of a queued client's token.
	//
	// This is a SLIDING window: every poll and every waiting-page render
	// resets it, so an actively waiting client refreshes its token roughly
	// every 3 seconds and is never reaped regardless of how long it waits.
	// The TTL therefore only needs to cover a small multiple of the poll
	// interval, not the expected total wait.
	//
	// It was formerly 30 minutes, which meant every abandoned or cookieless
	// client's token occupied the store for half an hour, inflating
	// QueueDepth (and therefore displayed positions, surge pricing, and the
	// max-queue-depth breaker) with load that no longer exists. Five minutes
	// preserves the "close the laptop for a moment" case while bounding
	// ghost residency at roughly 1/6 of the previous worst case.
	//
	// Tune with SetTokenTTL. Exported as DefaultTokenTTL.
	defaultTokenTTL = DefaultTokenTTL

	// cookieTTL is retained as the package-internal default token lifetime
	// for backwards compatibility with existing call sites and tests.
	//
	// Deprecated: the effective TTL is now per-WaitingRoom and runtime
	// configurable. Read WaitingRoom.TokenTTL() instead of this constant.
	cookieTTL = defaultTokenTTL

	// tokenTTLMin is the minimum value accepted by SetTokenTTL. Values
	// below this risk reaping clients that are polling normally.
	tokenTTLMin = 30 * time.Second

	// tokenTTLMax is the maximum value accepted by SetTokenTTL.
	tokenTTLMax = 24 * time.Hour

	// tokenBytes is the number of random bytes in a ticket token.
	// 16 bytes = 128 bits of entropy.
	tokenBytes = 16

	// reaperInterval is the default interval between eviction passes.
	// Exported as DefaultReaperInterval.
	reaperInterval = DefaultReaperInterval

	// reaperMinInterval is the minimum value accepted by SetReaperInterval.
	reaperMinInterval = 5 * time.Second

	// reaperMaxInterval is the maximum value accepted by SetReaperInterval.
	reaperMaxInterval = 24 * time.Hour

	// reaperBatchSize is the maximum tokens evicted per single scan pass
	// within a reap cycle. The reaper loops until a scan evicts fewer than
	// this many, so all expired tokens are cleared in a single reap() call
	// regardless of total volume.
	reaperBatchSize = 1000

	// defaultFirstPollGrace is the default first-poll grace: 0, disabled.
	// Existing deployments see no change unless they opt in.
	defaultFirstPollGrace = 0

	// firstPollGraceMin is the minimum non-zero value accepted by
	// SetFirstPollGrace. The default page first polls 3–3.5s after it
	// loads; anything much shorter would reap real visitors on slow
	// connections before their first poll lands.
	firstPollGraceMin = 10 * time.Second

	// firstPollGraceMax is the maximum value accepted by SetFirstPollGrace.
	// Values at or above TokenTTL are accepted but have no effect, since
	// the TTL reaps the token first.
	firstPollGraceMax = 24 * time.Hour

	// secureCookieDefault is the default value for the Secure cookie flag.
	// Set to false so that plain-HTTP local development works out of the box.
	// Production deployments behind TLS or a TLS-terminating proxy should
	// call SetSecureCookie(true) or rely on SetSecureCookieFromRequest.
	secureCookieDefault = false

	// defaultMaxQueueDepth is the default maximum number of requests that
	// may be waiting in the queue simultaneously. Zero means unlimited
	// (no cap on queue depth). When non-zero, requests arriving after the
	// queue is full receive a 503 immediately.
	defaultMaxQueueDepth int64 = 0

	// statusPollMinInterval is the minimum time between successive
	// /queue/status polls for a single token. Polls arriving faster
	// than this receive a cached response with a Retry-After header.
	statusPollMinInterval = 1 * time.Second

	// defaultPassDuration is the default lifetime of a VIP pass issued
	// by PromoteTokenToFront / GrantPass. Zero means passes are disabled
	// (single-use promotion only). Call SetPassDuration to enable.
	defaultPassDuration = 0

	// passMinDuration is the minimum value accepted by SetPassDuration.
	passMinDuration = 1 * time.Minute

	// passMaxDuration is the maximum value accepted by SetPassDuration.
	passMaxDuration = 24 * time.Hour
)

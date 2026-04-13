package room

import "time"

const (
	// cookieName is the HTTP-only session cookie issued to queued clients.
	cookieName = "room_ticket"

	// passCookieName is the HTTP-only cookie that holds the VIP pass
	// token. This cookie outlives individual queue tickets — it persists
	// for the configured pass duration so that clients who paid to skip
	// are auto-promoted on re-entry without paying again.
	passCookieName = "room_pass"

	// cookieTTL is how long a queued client's token remains valid.
	// If a client disappears before being admitted, their token is
	// evicted by the reaper after this duration.
	cookieTTL = 30 * time.Minute

	// tokenBytes is the number of random bytes in a ticket token.
	// 16 bytes = 128 bits of entropy.
	tokenBytes = 16

	// reaperInterval is the default interval between eviction passes.
	reaperInterval = 5 * time.Minute

	// reaperMinInterval is the minimum value accepted by SetReaperInterval.
	reaperMinInterval = 5 * time.Second

	// reaperMaxInterval is the maximum value accepted by SetReaperInterval.
	reaperMaxInterval = 24 * time.Hour

	// reaperBatchSize is the maximum tokens evicted per single scan pass
	// within a reap cycle. The reaper loops until a scan evicts fewer than
	// this many, so all expired tokens are cleared in a single reap() call
	// regardless of total volume.
	reaperBatchSize = 1000

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

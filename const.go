package room

import "time"

const (
	// cookieName is the HTTP-only session cookie issued to queued clients.
	cookieName = "room_ticket"

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

	// reaperBatchSize is the maximum tokens evicted per reap pass.
	reaperBatchSize = 1000
)

package room

import "time"

// Exported defaults. Passing 0 to SetTokenTTL or SetReaperInterval restores
// these values.
const (
	// DefaultTokenTTL is the default sliding-window lifetime of an abandoned
	// queued token. Active pollers reset the window on every poll or reload.
	DefaultTokenTTL = 5 * time.Minute

	// DefaultReaperInterval is the default cadence between background
	// eviction passes.
	DefaultReaperInterval = 5 * time.Minute

	// MaxKeyLen is the longest application key accepted by Enter, Acquire,
	// Leave, Release, and SetTicketKeyFunc, in bytes.
	MaxKeyLen = 256
)

const (
	cookieName       = "room_ticket"
	passCookieName   = "room_pass"
	probeCookieName  = "room_probe"
	probeCookieValue = "1"

	tokenBytes = 16 // 128 bits of entropy

	tokenTTLMin = 30 * time.Second
	tokenTTLMax = 24 * time.Hour

	reaperMinInterval = 5 * time.Second
	reaperMaxInterval = 24 * time.Hour
	reaperBatchSize   = 1000

	firstPollGraceMin = 10 * time.Second
	firstPollGraceMax = 24 * time.Hour

	statusPollMinInterval = 1 * time.Second

	passMinDuration = 1 * time.Minute
	passMaxDuration = 24 * time.Hour

	leaseTTLMin = time.Minute
	leaseTTLMax = 30 * 24 * time.Hour

	waitRetryAfter       = 3 * time.Second
	defaultStatusBaseURL = "/queue/status/"
)

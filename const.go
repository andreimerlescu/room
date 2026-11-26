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

	// DefaultSignedMaxTTL is the default maximum lifetime of a signed pass or
	// lease token. Kept short because signed tokens have no revocation store.
	DefaultSignedMaxTTL = 15 * time.Minute

	// MaxSignedTokenTTL is the hard upper bound for SignedMaxTTL and any
	// signed token's lifetime. Longer lifetimes cannot be configured.
	MaxSignedTokenTTL = 1 * time.Hour

	// DefaultLeaseFraction is the default fraction of capacity that held
	// leases (local and remote signed) may occupy when a Signer or Verifier is
	// configured. When neither is configured, the default remains 1.0 for v1.x
	// single-process compatibility.
	DefaultLeaseFraction = 0.25

	// DefaultClockSkew is the default allowed clock skew when validating a
	// signed token's issued-at (iat) timestamp.
	DefaultClockSkew = 30 * time.Second

	// DefaultPassHeader is the HTTP header read by the default capability
	// extractor for signed VIP pass tokens.
	DefaultPassHeader = "X-Room-Pass"

	// DefaultLeaseHeader is the HTTP header read by the default capability
	// extractor for signed lease tokens.
	DefaultLeaseHeader = "X-Room-Lease"
)

const (
	defaultCookiePrefix = "room"
	cookieName          = "room_ticket"
	passCookieName      = "room_pass"
	probeCookieName     = "room_probe"
	probeCookieValue    = "1"

	tokenBytes = 16 // 128 bits of entropy

	tokenTTLMin = 30 * time.Second
	tokenTTLMax = 24 * time.Hour

	reaperMinInterval = 5 * time.Second
	reaperMaxInterval = 24 * time.Hour
	reaperBatchSize   = 1000

	firstPollGraceMin = 10 * time.Second
	firstPollGraceMax = 24 * time.Hour

	readyGraceMin         = 5 * time.Second
	readyGraceMax         = 10 * time.Minute
	readyGraceReaperFloor = 1 * time.Second

	idlePollTTLMin = 10 * time.Second
	idlePollTTLMax = 24 * time.Hour

	forceReapMinInterval = 250 * time.Millisecond

	statusPollMinInterval = 1 * time.Second

	passMinDuration = 1 * time.Minute
	passMaxDuration = 24 * time.Hour

	leaseTTLMin = time.Minute
	leaseTTLMax = 30 * 24 * time.Hour

	signedMinTTL        = 1 * time.Second
	maxClockSkewCeiling = 5 * time.Minute
	maxCookiePrefixLen  = 64
	maxScopeLen         = 256
	maxSignedTokenLen   = 2048

	unconfiguredLeaseFraction = 1.0
	defaultHMACKeyID          = "default"

	waitRetryAfter       = 3 * time.Second
	defaultStatusBaseURL = "/queue/status/"
)

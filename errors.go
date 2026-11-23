package room

import (
	"fmt"
	"time"
)

// ErrReaperInterval is returned by SetReaperInterval when the provided
// duration falls outside [reaperMinInterval, reaperMaxInterval].
type ErrReaperInterval struct {
	Given time.Duration
	Min   time.Duration
	Max   time.Duration
}

func (e ErrReaperInterval) Error() string {
	return fmt.Sprintf(
		"room: reaper interval %s out of range [%s, %s]",
		e.Given, e.Min, e.Max,
	)
}

// ErrInvalidCap is returned by Init or SetCap when cap is less than 1.
type ErrInvalidCap struct {
	Given int32
}

func (e ErrInvalidCap) Error() string {
	return fmt.Sprintf("room: invalid capacity %d: must be >= 1", e.Given)
}

// ErrNotInitialised is returned by methods that require Init when called
// on a WaitingRoom that has not been initialised (for example RemoveToken).
type ErrNotInitialised struct{}

func (e ErrNotInitialised) Error() string {
	return "room: WaitingRoom not initialised — call Init before use"
}

// ErrInvalidMaxQueueDepth is returned by SetMaxQueueDepth when the
// value is negative.
type ErrInvalidMaxQueueDepth struct {
	Given int64
}

func (e ErrInvalidMaxQueueDepth) Error() string {
	return fmt.Sprintf("room: invalid max queue depth %d: must be >= 0", e.Given)
}

// ErrPromotionDisabled is returned when PromoteToken or QuoteCost is
// called without a RateFunc configured via SetRateFunc.
type ErrPromotionDisabled struct{}

func (e ErrPromotionDisabled) Error() string {
	return "room: promotions disabled — call SetRateFunc first"
}

// ErrTokenNotFound is returned when the token does not exist in the
// token store (expired, already admitted, removed, or never issued).
type ErrTokenNotFound struct{}

func (e ErrTokenNotFound) Error() string {
	return "room: token not found"
}

// ErrAlreadyAdmitted is returned when a promotion is attempted on a
// token that is already within the serving window.
type ErrAlreadyAdmitted struct{}

func (e ErrAlreadyAdmitted) Error() string {
	return "room: token already within serving window"
}

// ErrInvalidTargetPosition is returned when targetPosition < 1.
type ErrInvalidTargetPosition struct {
	Given int64
}

func (e ErrInvalidTargetPosition) Error() string {
	return fmt.Sprintf("room: invalid target position %d: must be >= 1", e.Given)
}

// ErrPassDuration is returned by SetPassDuration when the provided
// duration falls outside [passMinDuration, passMaxDuration].
type ErrPassDuration struct {
	Given time.Duration
	Min   time.Duration
	Max   time.Duration
}

func (e ErrPassDuration) Error() string {
	return fmt.Sprintf(
		"room: pass duration %s out of range [%s, %s]",
		e.Given, e.Min, e.Max,
	)
}

// ErrTokenTTL is returned by SetTokenTTL when the provided duration
// falls outside [tokenTTLMin, tokenTTLMax].
type ErrTokenTTL struct {
	Given time.Duration
	Min   time.Duration
	Max   time.Duration
}

func (e ErrTokenTTL) Error() string {
	return fmt.Sprintf(
		"room: token TTL %s out of range [%s, %s]",
		e.Given, e.Min, e.Max,
	)
}

// ErrFirstPollGrace is returned by SetFirstPollGrace when a non-zero
// duration falls outside [firstPollGraceMin, firstPollGraceMax].
type ErrFirstPollGrace struct {
	Given time.Duration
	Min   time.Duration
	Max   time.Duration
}

func (e ErrFirstPollGrace) Error() string {
	return fmt.Sprintf(
		"room: first-poll grace %s out of range [%s, %s] (0 disables)",
		e.Given, e.Min, e.Max,
	)
}

// ErrImportNotEmpty is returned by Import when the WaitingRoom has already
// issued a ticket or holds a pass. Import must run on a freshly
// initialised room, before it serves traffic.
type ErrImportNotEmpty struct{}

func (e ErrImportNotEmpty) Error() string {
	return "room: import requires a freshly initialised WaitingRoom that has not served traffic"
}

// ErrImportFormat is returned by Import when the input cannot be decoded
// or fails validation. The WaitingRoom is left unchanged.
type ErrImportFormat struct {
	Reason string
}

func (e ErrImportFormat) Error() string {
	return "room: invalid import data: " + e.Reason
}

// ErrInvalidRank is returned by SetTicketRank when rank is negative.
type ErrInvalidRank struct {
	Given int
}

func (e ErrInvalidRank) Error() string {
	return fmt.Sprintf("room: invalid rank %d: must be >= 0", e.Given)
}

// ErrInvalidKey is returned when an application key is empty or longer
// than MaxKeyLen bytes. Keys are identities, so room rejects an over-long
// key rather than truncating it into another key's place.
type ErrInvalidKey struct {
	Len int
}

func (e ErrInvalidKey) Error() string {
	return fmt.Sprintf("room: invalid key of %d bytes: must be 1–%d bytes", e.Len, MaxKeyLen)
}

// ErrQueueFull is returned by Enter and Acquire when SetMaxQueueDepth is
// reached and the key is not already in line.
type ErrQueueFull struct {
	Max int64
}

func (e ErrQueueFull) Error() string {
	return fmt.Sprintf("room: queue is full (max depth %d)", e.Max)
}

// ErrLeaseHeld is returned by Enter for a key that holds a lease: a key
// with a slot is not in line.
type ErrLeaseHeld struct{}

func (e ErrLeaseHeld) Error() string {
	return "room: key holds a lease and is not in line"
}

// ErrNoLease is returned by Release for a key that holds no lease.
type ErrNoLease struct{}

func (e ErrNoLease) Error() string {
	return "room: key holds no lease"
}

// ErrLeaseTTL is returned by SetLeaseTTL when a non-zero duration falls
// outside [Min, Max].
type ErrLeaseTTL struct {
	Given time.Duration
	Min   time.Duration
	Max   time.Duration
}

func (e ErrLeaseTTL) Error() string {
	return fmt.Sprintf(
		"room: lease TTL %s out of range [%s, %s] (0 disables)",
		e.Given, e.Min, e.Max,
	)
}

// ErrMissingScope is returned by Validate, NewTokenIssuer, and mint/verify
// operations when a Signer or Verifier is configured without a non-empty scope.
type ErrMissingScope struct{}

func (e ErrMissingScope) Error() string {
	return "room: missing required Scope when Signer or Verifier is configured"
}

// ErrNoSigner is returned when minting a signed token without a configured
// Signer (for example on a verify-only replica).
type ErrNoSigner struct{}

func (e ErrNoSigner) Error() string {
	return "room: no Signer configured; cannot mint verifiable tokens on a verify-only replica"
}

// ErrNoVerifier is returned by Validate when a Signer is configured without an
// explicit Verifier and cannot derive one from its own key.
type ErrNoVerifier struct{}

func (e ErrNoVerifier) Error() string {
	return "room: no Verifier configured and Signer cannot derive one"
}

// ErrInvalidTokenTTL is returned when a signed token TTL or SignedMaxTTL is
// zero, negative, or below the minimum allowed duration.
type ErrInvalidTokenTTL struct {
	Given time.Duration
	Min   time.Duration
}

func (e ErrInvalidTokenTTL) Error() string {
	if e.Given <= 0 {
		return fmt.Sprintf("room: signed token TTL %s must be positive (minimum %s; zero does not mean infinite)", e.Given, e.Min)
	}
	return fmt.Sprintf("room: signed token TTL %s is below minimum %s", e.Given, e.Min)
}

// ErrSignedTokenTTLTooLong is returned when a signed token TTL or SignedMaxTTL
// exceeds its configured maximum (or the hard 1h ceiling).
type ErrSignedTokenTTLTooLong struct {
	Given time.Duration
	Max   time.Duration
}

func (e ErrSignedTokenTTLTooLong) Error() string {
	return fmt.Sprintf("room: signed token TTL %s exceeds maximum %s", e.Given, e.Max)
}

// ErrPassDurationExceedsSignedMaxTTL is returned by Validate (and RegisterRoutes)
// when a Signer is configured and PassDuration exceeds SignedMaxTTL.
type ErrPassDurationExceedsSignedMaxTTL struct {
	PassDuration time.Duration
	SignedMaxTTL time.Duration
}

func (e ErrPassDurationExceedsSignedMaxTTL) Error() string {
	return fmt.Sprintf(
		"room: PassDuration %s exceeds SignedMaxTTL %s with a signer configured",
		e.PassDuration, e.SignedMaxTTL,
	)
}

// ErrInvalidLeaseFraction is returned by SetLeaseFraction when the fraction
// falls outside [0.0, 1.0] or is NaN.
type ErrInvalidLeaseFraction struct {
	Given float64
}

func (e ErrInvalidLeaseFraction) Error() string {
	return fmt.Sprintf("room: invalid lease fraction %v: must be in [0.0, 1.0]", e.Given)
}

// ErrInvalidCookiePrefix is returned by SetCookiePrefix or SetService when a
// cookie prefix contains invalid characters or exceeds the length limit.
type ErrInvalidCookiePrefix struct {
	Prefix string
	Reason string
}

func (e ErrInvalidCookiePrefix) Error() string {
	return fmt.Sprintf("room: invalid cookie prefix %q: %s", e.Prefix, e.Reason)
}

// ErrInvalidClockSkew is returned by SetClockSkew when the duration falls
// outside [0, maxClockSkewCeiling].
type ErrInvalidClockSkew struct {
	Given time.Duration
	Max   time.Duration
}

func (e ErrInvalidClockSkew) Error() string {
	return fmt.Sprintf("room: invalid clock skew %s: must be in [0, %s]", e.Given, e.Max)
}

// ErrInvalidSigningKey is returned when constructing a Signer or Verifier with
// malformed, empty, or invalid key material.
type ErrInvalidSigningKey struct {
	Reason string
}

func (e ErrInvalidSigningKey) Error() string {
	return "room: invalid signing key: " + e.Reason
}

// ErrTokenVerification describes why a signed pass or lease token failed
// cryptographic or policy verification.
type ErrTokenVerification struct {
	Reason string
	Cause  error
}

func (e ErrTokenVerification) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("room: token verification failed (%s): %v", e.Reason, e.Cause)
	}
	return fmt.Sprintf("room: token verification failed (%s)", e.Reason)
}

func (e ErrTokenVerification) Unwrap() error {
	return e.Cause
}

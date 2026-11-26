package room

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"math"
	"net/http"
	"time"

	"github.com/andreimerlescu/sema"
	"github.com/gin-gonic/gin"
)

// Option configures a WaitingRoom during [WaitingRoom.Init].
type Option func(*WaitingRoom) error

// WithScope sets the service scope bound into and verified on signed tokens.
func WithScope(scope string) Option {
	return func(wr *WaitingRoom) error {
		wr.SetScope(scope)
		return nil
	}
}

// WithCookiePrefix sets the per-service prefix used for queue cookies
// (<prefix>_ticket, <prefix>_pass, <prefix>_probe).
func WithCookiePrefix(prefix string) Option {
	return func(wr *WaitingRoom) error {
		return wr.SetCookiePrefix(prefix)
	}
}

// WithService sets both Scope and CookiePrefix to name.
func WithService(name string) Option {
	return func(wr *WaitingRoom) error {
		return wr.SetService(name)
	}
}

// WithSigner configures the Signer used to mint verifiable tokens.
func WithSigner(s Signer) Option {
	return func(wr *WaitingRoom) error {
		wr.SetSigner(s)
		return nil
	}
}

// WithVerifier configures the Verifier used to check verifiable tokens.
func WithVerifier(v Verifier) Option {
	return func(wr *WaitingRoom) error {
		wr.SetVerifier(v)
		return nil
	}
}

// WithSigningKey configures an HMAC-SHA256 Signer with keyID and secret.
func WithSigningKey(keyID string, secret []byte) Option {
	return func(wr *WaitingRoom) error {
		s, err := NewHMACSigner(keyID, secret)
		if err != nil {
			return err
		}
		wr.SetSigner(s)
		return nil
	}
}

// WithVerifyKeys configures an HMAC-SHA256 Verifier with a rotation key map.
func WithVerifyKeys(keys map[string][]byte) Option {
	return func(wr *WaitingRoom) error {
		return wr.SetVerifyKeys(keys)
	}
}

// WithEd25519Signer configures an Ed25519 Signer with keyID and private key.
func WithEd25519Signer(keyID string, priv ed25519.PrivateKey) Option {
	return func(wr *WaitingRoom) error {
		s, err := NewEd25519Signer(keyID, priv)
		if err != nil {
			return err
		}
		wr.SetSigner(s)
		return nil
	}
}

// WithEd25519VerifyKeys configures an Ed25519 Verifier with a public key rotation map.
func WithEd25519VerifyKeys(keys map[string]ed25519.PublicKey) Option {
	return func(wr *WaitingRoom) error {
		return wr.SetEd25519VerifyKeys(keys)
	}
}

// WithLeaseFraction sets the work-conserving fraction of capacity leases may occupy.
func WithLeaseFraction(frac float64) Option {
	return func(wr *WaitingRoom) error {
		return wr.SetLeaseFraction(frac)
	}
}

// WithSignedMaxTTL sets the maximum allowed lifetime for signed pass and lease tokens.
func WithSignedMaxTTL(d time.Duration) Option {
	return func(wr *WaitingRoom) error {
		return wr.SetSignedMaxTTL(d)
	}
}

// WithPassDuration configures the VIP pass lifetime during Init.
func WithPassDuration(d time.Duration) Option {
	return func(wr *WaitingRoom) error {
		return wr.SetPassDuration(d)
	}
}

// WithLeaseTTL configures the local lease idle TTL during Init.
func WithLeaseTTL(d time.Duration) Option {
	return func(wr *WaitingRoom) error {
		return wr.SetLeaseTTL(d)
	}
}

// WithClockSkew configures the allowed clock skew on signed token iat claims.
func WithClockSkew(d time.Duration) Option {
	return func(wr *WaitingRoom) error {
		return wr.SetClockSkew(d)
	}
}

// WithTokenExtractor sets the capability token extractor during Init.
func WithTokenExtractor(fn TokenExtractor) Option {
	return func(wr *WaitingRoom) error {
		wr.SetTokenExtractor(fn)
		return nil
	}
}

// WithReadyGrace configures the ready-claim deadline during Init.
func WithReadyGrace(d time.Duration) Option {
	return func(wr *WaitingRoom) error {
		return wr.SetReadyGrace(d)
	}
}

// WithIdlePollTTL configures the independent idle-poll TTL during Init.
func WithIdlePollTTL(d time.Duration) Option {
	return func(wr *WaitingRoom) error {
		return wr.SetIdlePollTTL(d)
	}
}

// WithFullResponder sets the custom response writer for maxQueueDepth
// rejections during Init.
func WithFullResponder(fn FullResponder) Option {
	return func(wr *WaitingRoom) error {
		wr.SetFullResponder(fn)
		return nil
	}
}

// NewWaitingRoom constructs an initialised WaitingRoom, registers the status
// routes on r, and returns its middleware. It panics if cap < 1.
//
// Because the caller receives no *WaitingRoom handle, its background reaper
// goroutine lives for the process lifetime. For explicit shutdown or runtime
// configuration, construct a WaitingRoom manually with [WaitingRoom.Init],
// [WaitingRoom.RegisterRoutes], and [WaitingRoom.Stop].
func NewWaitingRoom(r *gin.Engine, cap int32) gin.HandlerFunc {
	wr := &WaitingRoom{}
	if err := wr.Init(cap); err != nil {
		panic(fmt.Sprintf("room.NewWaitingRoom: %v", err))
	}
	r.OPTIONS("/queue/status", func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})
	r.GET("/queue/status", wr.StatusHandler())
	r.GET("/queue/status/:ticket", wr.TicketStatusHandler())
	return wr.Middleware()
}

// NewWaitingRoomFromStruct returns wr.Middleware(), panicking if wr is nil.
//
// Deprecated: call wr.Middleware() directly.
func NewWaitingRoomFromStruct(wr *WaitingRoom) gin.HandlerFunc {
	if wr == nil {
		panic("room.NewWaitingRoomFromStruct: nil WaitingRoom")
	}
	return wr.Middleware()
}

// Init initialises the WaitingRoom with capacity cap (>= 1), applies any
// functional options, validates configuration when options are supplied, and
// starts the background reaper goroutine. Call Init once before serving
// traffic; use [WaitingRoom.SetCap] and other setters for runtime
// reconfiguration, and [WaitingRoom.Validate] (also run by
// [WaitingRoom.RegisterRoutes]) after standalone setters.
//
// Returns [ErrInvalidCap] if cap < 1, or any error from options/validation.
func (wr *WaitingRoom) Init(cap int32, opts ...Option) error {
	if cap < 1 {
		return ErrInvalidCap{Given: cap}
	}

	wr.haltReaper()
	wr.initialised.Store(false)

	wr.cap.Store(cap)
	wr.sem = sema.Must(int(cap))
	wr.tokens = newTokenStore()
	wr.tokens.setTTL(DefaultTokenTTL)
	wr.passes = newPassStore()
	wr.ledger = newGhostLedger()
	wr.reaperRestart = make(chan struct{}, 1)
	wr.nowServing.Store(0)
	wr.nextTicket.Store(0)
	wr.occupancy.Store(0)
	wr.reaperInterval.Store(int64(DefaultReaperInterval))
	wr.firstPollGrace.Store(0)
	wr.readyGrace.Store(0)
	wr.idlePollTTL.Store(0)
	wr.lastForceReap.Store(0)
	wr.secureCookie.Store(false)
	wr.maxQueueDepth.Store(0)
	wr.SetCookiePath("/")
	wr.SetCookieDomain("")
	wr.rateFunc.Store(nil)
	wr.SetSkipURL("")
	wr.passDuration.Store(0)
	wr.callbacks = newCallbackRegistry()
	wr.waitJSON.Store(false)
	wr.SetStatusBaseURL(defaultStatusBaseURL)
	wr.leaseTTL.Store(0)
	wr.leaseMu.Lock()
	wr.leases = make(map[string]leaseEntry)
	wr.leaseMu.Unlock()

	wr.SetScope("")
	_ = wr.SetCookiePrefix("")
	wr.signer.Store(nil)
	wr.verifier.Store(nil)
	wr.signedMaxTTL.Store(int64(DefaultSignedMaxTTL))
	wr.clockSkew.Store(int64(DefaultClockSkew))
	wr.leaseFractionBits.Store(math.Float64bits(unconfiguredLeaseFraction))
	wr.leaseFractionSet.Store(false)

	for _, opt := range opts {
		if opt == nil {
			continue
		}
		if err := opt(wr); err != nil {
			return err
		}
	}
	if len(opts) > 0 {
		if err := wr.validateConfig(); err != nil {
			return err
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	wr.stopReaper = cancel
	wr.startReaper(ctx)

	wr.initialised.Store(true)
	return nil
}

// Stop cancels the background reaper goroutine and waits for it to exit.
// Calling Stop more than once is harmless.
func (wr *WaitingRoom) Stop() {
	wr.haltReaper()
}

// SetSecureCookie forces the Secure flag on waiting-room cookies when true.
// When false (the default), Secure is set only on requests arriving directly
// over TLS (r.TLS != nil). Call SetSecureCookie(true) behind TLS-terminating
// proxies. Safe to call at any time.
func (wr *WaitingRoom) SetSecureCookie(secure bool) {
	wr.secureCookie.Store(secure)
}

// SetTokenTTL sets the sliding-window lifetime of an abandoned queued token.
// Passing 0 restores [DefaultTokenTTL]; otherwise valid values are 30s–24h
// (returns [ErrTokenTTL] outside that range). Safe to call at any time.
func (wr *WaitingRoom) SetTokenTTL(d time.Duration) error {
	if d == 0 {
		d = DefaultTokenTTL
	}
	if d < tokenTTLMin || d > tokenTTLMax {
		return ErrTokenTTL{Given: d, Min: tokenTTLMin, Max: tokenTTLMax}
	}
	wr.tokens.setTTL(d)
	return nil
}

// TokenTTL returns the current sliding-window token lifetime.
func (wr *WaitingRoom) TokenTTL() time.Duration {
	return wr.tokens.ttl()
}

// SetMaxQueueDepth sets the maximum waiting queue depth before new arrivals
// receive 503 Service Unavailable. Zero disables the limit (default); negative
// values return [ErrInvalidMaxQueueDepth]. Safe to call at any time.
func (wr *WaitingRoom) SetMaxQueueDepth(max int64) error {
	if max < 0 {
		return ErrInvalidMaxQueueDepth{Given: max}
	}
	wr.maxQueueDepth.Store(max)
	return nil
}

// MaxQueueDepth returns the current maximum queue depth (0 means unlimited).
func (wr *WaitingRoom) MaxQueueDepth() int64 {
	return wr.maxQueueDepth.Load()
}

// LiveQueueDepth returns the number of live tokens currently in the store
// (including clients told ready that have not reloaded yet).
func (wr *WaitingRoom) LiveQueueDepth() int64 {
	return int64(wr.tokens.len())
}

// SetCookiePath sets the Path attribute for waiting-room cookies (empty
// restores "/"). Safe to call at any time.
func (wr *WaitingRoom) SetCookiePath(path string) {
	if path == "" {
		path = "/"
	}
	wr.cookiePath.Store(&path)
}

// CookiePath returns the current cookie Path setting ("/" if unset).
func (wr *WaitingRoom) CookiePath() string {
	if p := wr.cookiePath.Load(); p != nil && *p != "" {
		return *p
	}
	return "/"
}

// SetCookieDomain sets the Domain attribute for waiting-room cookies.
// Safe to call at any time.
func (wr *WaitingRoom) SetCookieDomain(domain string) {
	wr.cookieDomain.Store(&domain)
}

// CookieDomain returns the current cookie Domain setting (empty if unset).
func (wr *WaitingRoom) CookieDomain() string {
	if p := wr.cookieDomain.Load(); p != nil {
		return *p
	}
	return ""
}

// SetSkipURL sets the payment URL for the "Pay to skip" button on the waiting
// room page. Both [WaitingRoom.SetRateFunc] and SetSkipURL must be configured
// for the offer to appear. Safe to call at any time.
func (wr *WaitingRoom) SetSkipURL(url string) {
	wr.skipURL.Store(&url)
}

// SkipURL returns the current skip-the-line payment URL (empty if unset).
func (wr *WaitingRoom) SkipURL() string {
	if p := wr.skipURL.Load(); p != nil {
		return *p
	}
	return ""
}

// SetPassDuration sets the lifetime of VIP passes issued by [WaitingRoom.GrantPass]
// and [WaitingRoom.PromoteToken]. Zero disables passes (default); non-zero
// values must lie in 1m–24h (otherwise returns [ErrPassDuration]). Safe to
// call at any time.
func (wr *WaitingRoom) SetPassDuration(d time.Duration) error {
	if d == 0 {
		wr.passDuration.Store(0)
		return nil
	}
	if d < passMinDuration || d > passMaxDuration {
		return ErrPassDuration{Given: d, Min: passMinDuration, Max: passMaxDuration}
	}
	wr.passDuration.Store(int64(d))
	return nil
}

// PassDuration returns the current VIP pass lifetime (0 means disabled).
func (wr *WaitingRoom) PassDuration() time.Duration {
	return time.Duration(wr.passDuration.Load())
}

// GrantPass creates a time-limited VIP pass token, or returns "" if passes
// are disabled or minting fails. Set the returned token as the pass cookie
// ([WaitingRoom.PassCookieName], default "room_pass") on the response.
//
// When neither Signer nor Verifier is configured, GrantPass stores an opaque
// random token in the local pass store. When signed mode is active, it mints a
// signed pass envelope; use [WaitingRoom.GrantPassE] to inspect errors such as
// [ErrNoSigner], [ErrMissingScope], or [ErrPassDurationExceedsSignedMaxTTL].
func (wr *WaitingRoom) GrantPass() string {
	tok, err := wr.GrantPassE()
	if err != nil {
		return ""
	}
	return tok
}

// GrantPassE creates a time-limited VIP pass token and returns any error.
// Returns ("", nil) when PassDuration is 0 (passes disabled).
//
// When neither Signer nor Verifier is configured, it stores a random bearer
// token in the local pass store. When a Signer or Verifier is configured, it
// requires a Signer (returning [ErrNoSigner] on verify-only replicas), a
// non-empty Scope ([ErrMissingScope]), and PassDuration <= SignedMaxTTL
// ([ErrPassDurationExceedsSignedMaxTTL]).
func (wr *WaitingRoom) GrantPassE() (string, error) {
	if !wr.initialised.Load() {
		return "", ErrNotInitialised{}
	}
	d := wr.PassDuration()
	if d == 0 {
		return "", nil
	}
	if !wr.signingEnabled() {
		token, err := generateToken()
		if err != nil {
			return "", err
		}
		wr.passes.set(token, passEntry{expiresAt: wr.now().Add(d)})
		return token, nil
	}
	s := wr.signerLoad()
	if s == nil {
		return "", ErrNoSigner{}
	}
	scope := wr.Scope()
	if scope == "" {
		return "", ErrMissingScope{}
	}
	maxTTL := wr.SignedMaxTTL()
	if d > maxTTL {
		return "", ErrPassDurationExceedsSignedMaxTTL{PassDuration: d, SignedMaxTTL: maxTTL}
	}
	return mintEnvelope(s, scope, TokenKindPass, "", wr.now(), d, maxTTL)
}

// MintPass mints a signed VIP pass envelope for key (empty for an anonymous
// browser cookie pass, or an application key for a key-bound API pass) with
// explicit lifetime ttl. Always requires a configured Signer and non-empty
// Scope; rejects ttl <= 0 with [ErrInvalidTokenTTL] and ttl > SignedMaxTTL
// with [ErrSignedTokenTTLTooLong].
func (wr *WaitingRoom) MintPass(key string, ttl time.Duration) (string, error) {
	if !wr.initialised.Load() {
		return "", ErrNotInitialised{}
	}
	return mintEnvelope(wr.signerLoad(), wr.Scope(), TokenKindPass, key, wr.now(), ttl, wr.SignedMaxTTL())
}

// MintLease mints a signed lease envelope bound to key with explicit lifetime
// ttl. Always requires a configured Signer, non-empty Scope, valid non-empty
// key, and 1s <= ttl <= SignedMaxTTL (zero never means infinite).
func (wr *WaitingRoom) MintLease(key string, ttl time.Duration) (string, error) {
	if !wr.initialised.Load() {
		return "", ErrNotInitialised{}
	}
	return mintEnvelope(wr.signerLoad(), wr.Scope(), TokenKindLease, key, wr.now(), ttl, wr.SignedMaxTTL())
}

// HasValidPass reports whether passToken is valid and unexpired. When a Signer
// or Verifier is configured it checks cryptographic signature, scope, kind,
// and expiry; otherwise it checks the local pass store.
func (wr *WaitingRoom) HasValidPass(passToken string) bool {
	if passToken == "" || !wr.initialised.Load() {
		return false
	}
	if !wr.signingEnabled() {
		_, ok := wr.passes.get(passToken)
		return ok
	}
	_, _, err := wr.verifySignedToken(passToken, TokenKindPass, "")
	return err == nil
}

// VerifyToken verifies a signed pass or lease token against this room's
// resolved Verifier, Scope, expectedKind, expectedKey, ClockSkew, and current
// time. Fails closed if Scope is empty or no Verifier is available.
func (wr *WaitingRoom) VerifyToken(token string, expectedKind TokenKind, expectedKey string) (TokenClaims, error) {
	if !wr.initialised.Load() {
		return TokenClaims{}, ErrNotInitialised{}
	}
	claims, _, err := wr.verifySignedToken(token, expectedKind, expectedKey)
	return claims, err
}

func (wr *WaitingRoom) verifySignedToken(raw string, expectedKind TokenKind, expectedKey string) (TokenClaims, string, error) {
	scope := wr.Scope()
	if scope == "" {
		return TokenClaims{}, RejectReasonMissingScope, ErrMissingScope{}
	}
	v, err := wr.resolveVerifier()
	if err != nil || v == nil {
		return TokenClaims{}, RejectReasonNoVerifier, ErrNoVerifier{}
	}
	return verifyEnvelope(v, scope, expectedKind, expectedKey, raw, wr.now(), wr.ClockSkew())
}

// SetScope sets the service scope bound into and verified on signed tokens.
// Scope is a security boundary distinct from cookie naming; a non-empty scope
// is required whenever a Signer or Verifier is configured. Safe at any time.
func (wr *WaitingRoom) SetScope(scope string) {
	wr.scope.Store(&scope)
}

// Scope returns the configured service scope (empty if unset).
func (wr *WaitingRoom) Scope() string {
	if p := wr.scope.Load(); p != nil {
		return *p
	}
	return ""
}

// SetCookiePrefix sets the per-service prefix used for browser queue cookies
// (<prefix>_ticket, <prefix>_pass, <prefix>_probe). Empty string restores the
// default "room" prefix (room_ticket, room_pass, room_probe). Only ASCII
// letters, digits, '_' and '-' are permitted (max 64 bytes). Safe at any time.
func (wr *WaitingRoom) SetCookiePrefix(prefix string) error {
	if prefix == "" {
		prefix = defaultCookiePrefix
	}
	if err := validateCookiePrefix(prefix); err != nil {
		return err
	}
	ticket := prefix + "_ticket"
	pass := prefix + "_pass"
	probe := prefix + "_probe"
	wr.cookiePrefix.Store(&prefix)
	wr.ticketCookie.Store(&ticket)
	wr.passCookie.Store(&pass)
	wr.probeCookie.Store(&probe)
	return nil
}

func validateCookiePrefix(prefix string) error {
	if len(prefix) > maxCookiePrefixLen {
		return ErrInvalidCookiePrefix{Prefix: prefix, Reason: fmt.Sprintf("exceeds %d bytes", maxCookiePrefixLen)}
	}
	for i := 0; i < len(prefix); i++ {
		c := prefix[i]
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-'
		if !ok {
			return ErrInvalidCookiePrefix{Prefix: prefix, Reason: fmt.Sprintf("invalid character %q (allowed: A-Z, a-z, 0-9, _, -)", c)}
		}
	}
	return nil
}

// CookiePrefix returns the active cookie prefix ("room" by default).
func (wr *WaitingRoom) CookiePrefix() string {
	if p := wr.cookiePrefix.Load(); p != nil && *p != "" {
		return *p
	}
	return defaultCookiePrefix
}

// TicketCookieName returns the ticket cookie name ("room_ticket" by default).
func (wr *WaitingRoom) TicketCookieName() string {
	if p := wr.ticketCookie.Load(); p != nil && *p != "" {
		return *p
	}
	return cookieName
}

// PassCookieName returns the VIP pass cookie name ("room_pass" by default).
func (wr *WaitingRoom) PassCookieName() string {
	if p := wr.passCookie.Load(); p != nil && *p != "" {
		return *p
	}
	return passCookieName
}

// ProbeCookieName returns the probe cookie name ("room_probe" by default).
func (wr *WaitingRoom) ProbeCookieName() string {
	if p := wr.probeCookie.Load(); p != nil && *p != "" {
		return *p
	}
	return probeCookieName
}

// SetService is a convenience setter that configures both Scope and
// CookiePrefix to name. name must be non-empty and a valid cookie prefix.
func (wr *WaitingRoom) SetService(name string) error {
	if name == "" {
		return ErrInvalidCookiePrefix{Prefix: "", Reason: "service name must not be empty"}
	}
	if err := wr.SetCookiePrefix(name); err != nil {
		return err
	}
	wr.SetScope(name)
	return nil
}

// SetSigner configures the Signer used to mint verifiable tokens. Pass nil to
// clear. SetSigner does not mutate the configured Verifier; verifier resolution
// happens dynamically at Validate and verification time so SetSigner and
// SetVerifier are completely order-independent. Safe at any time.
func (wr *WaitingRoom) SetSigner(s Signer) {
	if s == nil {
		wr.signer.Store(nil)
		return
	}
	wr.signer.Store(&s)
}

func (wr *WaitingRoom) signerLoad() Signer {
	if p := wr.signer.Load(); p != nil {
		return *p
	}
	return nil
}

// SetVerifier configures the Verifier used to check verifiable tokens. Pass
// nil to clear (falling back to a Signer-derived verifier if Signer is set).
// An explicit Verifier always takes precedence over a Signer-derived verifier
// regardless of setter call order. Safe at any time.
func (wr *WaitingRoom) SetVerifier(v Verifier) {
	if v == nil {
		wr.verifier.Store(nil)
		return
	}
	wr.verifier.Store(&v)
}

func (wr *WaitingRoom) verifierLoad() Verifier {
	if p := wr.verifier.Load(); p != nil {
		return *p
	}
	return nil
}

// SetSigningKey configures an HMAC-SHA256 Signer using secret (under key ID
// "default"). Does not mutate the explicit Verifier. For multi-replica
// deployments prefer Ed25519 via [NewEd25519Signer] and [SetEd25519VerifyKeys].
func (wr *WaitingRoom) SetSigningKey(secret []byte) error {
	s, err := NewHMACSigner(defaultHMACKeyID, secret)
	if err != nil {
		return err
	}
	wr.SetSigner(s)
	return nil
}

// SetVerifyKeys configures an HMAC-SHA256 Verifier with a rotation key map
// (keyID -> secret).
func (wr *WaitingRoom) SetVerifyKeys(keys map[string][]byte) error {
	v, err := NewHMACVerifier(keys)
	if err != nil {
		return err
	}
	wr.SetVerifier(v)
	return nil
}

// SetEd25519VerifyKeys configures an Ed25519 Verifier with a public key
// rotation map (keyID -> public key), ideal for verify-only serving replicas.
func (wr *WaitingRoom) SetEd25519VerifyKeys(keys map[string]ed25519.PublicKey) error {
	v, err := NewEd25519Verifier(keys)
	if err != nil {
		return err
	}
	wr.SetVerifier(v)
	return nil
}

// SetSignedMaxTTL sets the maximum lifetime allowed for signed pass and lease
// tokens. Must be in [1s, MaxSignedTokenTTL (1h)]; zero is rejected with
// [ErrInvalidTokenTTL] and values > 1h are rejected with
// [ErrSignedTokenTTLTooLong]. Safe at any time.
func (wr *WaitingRoom) SetSignedMaxTTL(d time.Duration) error {
	if err := validateSignedMaxTTL(d); err != nil {
		return err
	}
	wr.signedMaxTTL.Store(int64(d))
	return nil
}

// SignedMaxTTL returns the maximum lifetime allowed for signed tokens
// (DefaultSignedMaxTTL = 15m by default).
func (wr *WaitingRoom) SignedMaxTTL() time.Duration {
	if d := time.Duration(wr.signedMaxTTL.Load()); d > 0 {
		return d
	}
	return DefaultSignedMaxTTL
}

// SetClockSkew sets the tolerated clock skew when checking a signed token's
// iat claim (default DefaultClockSkew = 30s, range [0, 5m]). Safe at any time.
func (wr *WaitingRoom) SetClockSkew(d time.Duration) error {
	if d < 0 || d > maxClockSkewCeiling {
		return ErrInvalidClockSkew{Given: d, Max: maxClockSkewCeiling}
	}
	wr.clockSkew.Store(int64(d))
	return nil
}

// ClockSkew returns the configured clock skew tolerance for signed tokens.
func (wr *WaitingRoom) ClockSkew() time.Duration {
	return time.Duration(wr.clockSkew.Load())
}

// SetLeaseFraction sets the work-conserving fraction of capacity [0.0, 1.0]
// that held leases (local and remote signed) may occupy. When unset, defaults
// to DefaultLeaseFraction (0.25) whenever a Signer or Verifier is configured,
// and 1.0 otherwise for v1.x single-process compatibility. Safe at any time.
func (wr *WaitingRoom) SetLeaseFraction(frac float64) error {
	if math.IsNaN(frac) || math.IsInf(frac, 0) || frac < 0 || frac > 1 {
		return ErrInvalidLeaseFraction{Given: frac}
	}
	wr.leaseFractionBits.Store(math.Float64bits(frac))
	wr.leaseFractionSet.Store(true)
	return nil
}

// LeaseFraction returns the effective lease capacity fraction.
func (wr *WaitingRoom) LeaseFraction() float64 {
	if wr.leaseFractionSet.Load() {
		return math.Float64frombits(wr.leaseFractionBits.Load())
	}
	if wr.signingEnabled() {
		return DefaultLeaseFraction
	}
	return unconfiguredLeaseFraction
}

func (wr *WaitingRoom) signingEnabled() bool {
	return wr.signerLoad() != nil || wr.verifierLoad() != nil
}

// resolveVerifier returns the effective Verifier without mutating state.
// Explicit SetVerifier always wins over a Signer-derived verifier regardless
// of setter call order.
func (wr *WaitingRoom) resolveVerifier() (Verifier, error) {
	if v := wr.verifierLoad(); v != nil {
		return v, nil
	}
	if s := wr.signerLoad(); s != nil {
		if v := deriveVerifierFromSigner(s); v != nil {
			return v, nil
		}
		return nil, ErrNoVerifier{}
	}
	return nil, nil
}

// Validate checks that the WaitingRoom has been initialised and its signing,
// scope, verifier, and TTL settings are self-consistent. Called automatically
// by [WaitingRoom.RegisterRoutes] (which panics with the error on failure) and
// by [WaitingRoom.Init] when functional options are supplied.
func (wr *WaitingRoom) Validate() error {
	if !wr.initialised.Load() {
		return ErrNotInitialised{}
	}
	return wr.validateConfig()
}

func (wr *WaitingRoom) validateConfig() error {
	s := wr.signerLoad()
	v := wr.verifierLoad()
	if s == nil && v == nil {
		return nil
	}
	scope := wr.Scope()
	if scope == "" {
		return ErrMissingScope{}
	}
	if len(scope) > maxScopeLen {
		return ErrInvalidSigningKey{Reason: fmt.Sprintf("scope exceeds %d bytes", maxScopeLen)}
	}
	if _, err := wr.resolveVerifier(); err != nil {
		return err
	}
	if s != nil {
		if err := validateKeyID(s.KeyID()); err != nil {
			return err
		}
		if passDur, maxTTL := wr.PassDuration(), wr.SignedMaxTTL(); passDur > 0 && passDur > maxTTL {
			return ErrPassDurationExceedsSignedMaxTTL{
				PassDuration: passDur,
				SignedMaxTTL: maxTTL,
			}
		}
	}
	return nil
}

func (wr *WaitingRoom) now() time.Time {
	if wr.nowFn != nil {
		return wr.nowFn()
	}
	return time.Now()
}

// checkInitialised aborts with 500 and returns false if Init was not called or
// signing configuration is invalid (fail closed).
func (wr *WaitingRoom) checkInitialised(c *gin.Context) bool {
	if !wr.initialised.Load() {
		c.AbortWithStatus(http.StatusInternalServerError)
		return false
	}
	if err := wr.validateConfig(); err != nil {
		c.AbortWithStatus(http.StatusInternalServerError)
		return false
	}
	return true
}

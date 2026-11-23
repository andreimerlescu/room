package room

import (
	"bytes"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// TokenKind identifies the capability carried by a signed envelope.
// Kind is load-bearing: a pass cannot be replayed as a lease or vice versa.
type TokenKind string

const (
	// TokenKindPass grants front-of-line promotion within a replica's local
	// capacity without bypassing its semaphore.
	TokenKindPass TokenKind = "pass"

	// TokenKindLease grants a slot in a replica's bounded lease pool on first
	// arrival, queuing at the front of the general line if the pool is full.
	TokenKindLease TokenKind = "lease"
)

// Rejection reasons reported in Snapshot.Reason on EventTokenRejected and in
// ErrTokenVerification.Reason.
const (
	RejectReasonMissingScope     = "missing_scope"
	RejectReasonNoVerifier       = "no_verifier"
	RejectReasonMalformed        = "malformed"
	RejectReasonUnknownKey       = "unknown_key"
	RejectReasonInvalidSignature = "invalid_signature"
	RejectReasonExpired          = "expired"
	RejectReasonFutureIssued     = "future_issued"
	RejectReasonWrongScope       = "wrong_scope"
	RejectReasonWrongKind        = "wrong_kind"
	RejectReasonKeyMismatch      = "key_mismatch"
)

// TokenTransport values reported in Snapshot.TokenTransport on
// EventTokenRejected.
const (
	TokenTransportCookie = "cookie"
	TokenTransportHeader = "header"
)

const tokenWirePrefix = "rm1"

var (
	errUnknownKeyID     = errors.New("unknown key id")
	errInvalidSignature = errors.New("invalid signature")
)

// TokenClaims is the authenticated payload inside a signed pass or lease token.
type TokenClaims struct {
	KeyID     string    `json:"kid"`
	Kind      TokenKind `json:"kind"`
	Scope     string    `json:"scope"`
	Key       string    `json:"key,omitempty"`
	IssuedAt  int64     `json:"iat"`
	ExpiresAt int64     `json:"exp"`
}

// Signer signs canonical token envelope inputs with an identified key.
// Implementations must be safe for concurrent use.
type Signer interface {
	KeyID() string
	Sign(payload []byte) ([]byte, error)
}

// Verifier checks canonical token envelope signatures by key ID.
// Implementations must be safe for concurrent use.
type Verifier interface {
	Verify(keyID string, payload, sig []byte) error
}

// verifierDeriver lets built-in Signers derive a matching Verifier at Validate
// or verify time without mutating WaitingRoom state during SetSigner.
type verifierDeriver interface {
	deriveVerifier() Verifier
}

func deriveVerifierFromSigner(s Signer) Verifier {
	if s == nil {
		return nil
	}
	if d, ok := s.(verifierDeriver); ok {
		return d.deriveVerifier()
	}
	if v, ok := s.(Verifier); ok {
		return v
	}
	return nil
}

// Ed25519Signer signs verifiable tokens with an Ed25519 private key.
// Recommended for multi-replica deployments where only the issuer holds the
// private key and serving replicas verify with public keys via Ed25519Verifier.
type Ed25519Signer struct {
	keyID   string
	priv    ed25519.PrivateKey
	derived *Ed25519Verifier
}

// NewEd25519Signer constructs an Ed25519Signer for keyID and priv.
func NewEd25519Signer(keyID string, priv ed25519.PrivateKey) (*Ed25519Signer, error) {
	if err := validateKeyID(keyID); err != nil {
		return nil, err
	}
	if len(priv) != ed25519.PrivateKeySize {
		return nil, ErrInvalidSigningKey{Reason: fmt.Sprintf("ed25519 private key must be %d bytes, got %d", ed25519.PrivateKeySize, len(priv))}
	}
	cp := make(ed25519.PrivateKey, ed25519.PrivateKeySize)
	copy(cp, priv)
	pub := cp.Public().(ed25519.PublicKey)
	derived, err := NewEd25519Verifier(map[string]ed25519.PublicKey{keyID: pub})
	if err != nil {
		return nil, err
	}
	return &Ed25519Signer{keyID: keyID, priv: cp, derived: derived}, nil
}

// KeyID returns the signer's key identifier embedded in minted envelopes.
func (s *Ed25519Signer) KeyID() string { return s.keyID }

// Sign signs payload with the Ed25519 private key.
func (s *Ed25519Signer) Sign(payload []byte) ([]byte, error) {
	return ed25519.Sign(s.priv, payload), nil
}

// PublicKey returns a copy of the signer's Ed25519 public key.
func (s *Ed25519Signer) PublicKey() ed25519.PublicKey {
	pub := s.priv.Public().(ed25519.PublicKey)
	cp := make(ed25519.PublicKey, ed25519.PublicKeySize)
	copy(cp, pub)
	return cp
}

func (s *Ed25519Signer) deriveVerifier() Verifier {
	return s.derived
}

// Ed25519Verifier verifies Ed25519-signed tokens against a set of public keys
// indexed by key ID (supporting zero-downtime key rotation).
type Ed25519Verifier struct {
	keys map[string]ed25519.PublicKey
}

// NewEd25519Verifier constructs an Ed25519Verifier from keyID -> public key.
func NewEd25519Verifier(keys map[string]ed25519.PublicKey) (*Ed25519Verifier, error) {
	if len(keys) == 0 {
		return nil, ErrInvalidSigningKey{Reason: "at least one verification key is required"}
	}
	cp := make(map[string]ed25519.PublicKey, len(keys))
	for kid, pub := range keys {
		if err := validateKeyID(kid); err != nil {
			return nil, err
		}
		if len(pub) != ed25519.PublicKeySize {
			return nil, ErrInvalidSigningKey{Reason: fmt.Sprintf("ed25519 public key %q must be %d bytes, got %d", kid, ed25519.PublicKeySize, len(pub))}
		}
		k := make(ed25519.PublicKey, ed25519.PublicKeySize)
		copy(k, pub)
		cp[kid] = k
	}
	return &Ed25519Verifier{keys: cp}, nil
}

// Verify checks sig against payload using the public key identified by keyID.
func (v *Ed25519Verifier) Verify(keyID string, payload, sig []byte) error {
	pub, ok := v.keys[keyID]
	if !ok {
		return errUnknownKeyID
	}
	if len(sig) != ed25519.SignatureSize || !ed25519.Verify(pub, payload, sig) {
		return errInvalidSignature
	}
	return nil
}

// HMACSigner signs verifiable tokens with HMAC-SHA256 using a shared secret.
// Simple symmetric default; prefer Ed25519Signer + Ed25519Verifier across
// replicas so verify-only hosts cannot mint tokens.
type HMACSigner struct {
	keyID   string
	secret  []byte
	derived *HMACVerifier
}

// NewHMACSigner constructs an HMACSigner for keyID and secret (recommended
// 32+ random bytes).
func NewHMACSigner(keyID string, secret []byte) (*HMACSigner, error) {
	if err := validateKeyID(keyID); err != nil {
		return nil, err
	}
	if len(secret) == 0 {
		return nil, ErrInvalidSigningKey{Reason: "HMAC secret must not be empty"}
	}
	cp := make([]byte, len(secret))
	copy(cp, secret)
	derived, err := NewHMACVerifier(map[string][]byte{keyID: cp})
	if err != nil {
		return nil, err
	}
	return &HMACSigner{keyID: keyID, secret: cp, derived: derived}, nil
}

// KeyID returns the signer's key identifier embedded in minted envelopes.
func (s *HMACSigner) KeyID() string { return s.keyID }

// Sign returns the HMAC-SHA256 tag over payload.
func (s *HMACSigner) Sign(payload []byte) ([]byte, error) {
	mac := hmac.New(sha256.New, s.secret)
	_, _ = mac.Write(payload)
	return mac.Sum(nil), nil
}

func (s *HMACSigner) deriveVerifier() Verifier {
	return s.derived
}

// HMACVerifier verifies HMAC-SHA256 tokens against one or more secrets indexed
// by key ID (supporting zero-downtime key rotation).
type HMACVerifier struct {
	keys map[string][]byte
}

// NewHMACVerifier constructs an HMACVerifier from keyID -> shared secret.
func NewHMACVerifier(keys map[string][]byte) (*HMACVerifier, error) {
	if len(keys) == 0 {
		return nil, ErrInvalidSigningKey{Reason: "at least one verification key is required"}
	}
	cp := make(map[string][]byte, len(keys))
	for kid, secret := range keys {
		if err := validateKeyID(kid); err != nil {
			return nil, err
		}
		if len(secret) == 0 {
			return nil, ErrInvalidSigningKey{Reason: fmt.Sprintf("HMAC secret for key %q must not be empty", kid)}
		}
		k := make([]byte, len(secret))
		copy(k, secret)
		cp[kid] = k
	}
	return &HMACVerifier{keys: cp}, nil
}

// Verify checks sig against payload using the HMAC secret identified by keyID.
func (v *HMACVerifier) Verify(keyID string, payload, sig []byte) error {
	secret, ok := v.keys[keyID]
	if !ok {
		return errUnknownKeyID
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(payload)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return errInvalidSignature
	}
	return nil
}

func validateKeyID(kid string) error {
	if kid == "" {
		return ErrInvalidSigningKey{Reason: "key ID must not be empty"}
	}
	if len(kid) > 64 {
		return ErrInvalidSigningKey{Reason: "key ID exceeds 64 bytes"}
	}
	return nil
}

func validateSignedTTL(ttl, maxTTL time.Duration) error {
	if ttl < signedMinTTL {
		return ErrInvalidTokenTTL{Given: ttl, Min: signedMinTTL}
	}
	if ttl > maxTTL {
		return ErrSignedTokenTTLTooLong{Given: ttl, Max: maxTTL}
	}
	return nil
}

func validateSignedMaxTTL(d time.Duration) error {
	if d < signedMinTTL {
		return ErrInvalidTokenTTL{Given: d, Min: signedMinTTL}
	}
	if d > MaxSignedTokenTTL {
		return ErrSignedTokenTTLTooLong{Given: d, Max: MaxSignedTokenTTL}
	}
	return nil
}

func mintEnvelope(signer Signer, scope string, kind TokenKind, key string, now time.Time, ttl, maxTTL time.Duration) (string, error) {
	if signer == nil {
		return "", ErrNoSigner{}
	}
	if scope == "" {
		return "", ErrMissingScope{}
	}
	if err := validateKeyID(signer.KeyID()); err != nil {
		return "", err
	}
	if err := validateSignedTTL(ttl, maxTTL); err != nil {
		return "", err
	}
	if kind == TokenKindLease {
		if err := validKey(key); err != nil {
			return "", err
		}
	} else if key != "" {
		if err := validKey(key); err != nil {
			return "", err
		}
	}

	iat := now.Unix()
	exp := now.Add(ttl).Unix()
	if exp <= iat {
		exp = iat + 1
	}

	claims := TokenClaims{
		KeyID:     signer.KeyID(),
		Kind:      kind,
		Scope:     scope,
		Key:       key,
		IssuedAt:  iat,
		ExpiresAt: exp,
	}
	rawJSON, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("room: marshal token claims: %w", err)
	}
	payloadB64 := base64.RawURLEncoding.EncodeToString(rawJSON)
	signingInput := tokenWirePrefix + "." + payloadB64

	sig, err := signer.Sign([]byte(signingInput))
	if err != nil {
		return "", fmt.Errorf("room: sign token: %w", err)
	}
	if len(sig) == 0 {
		return "", ErrInvalidSigningKey{Reason: "signer produced empty signature"}
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// verifyEnvelope authenticates raw and checks scope, kind, key binding, clock
// skew, and expiry in security-first order:
//  1. Config preconditions (non-empty scope, non-nil verifier)
//  2. Envelope framing & JSON decode
//  3. Cryptographic signature verification (so forged tokens never masquerade as expired)
//  4. Scope, kind, and key binding (so cross-tenant/replay bugs are loud even if expired)
//  5. Clock skew (iat) and expiry (exp)
func verifyEnvelope(v Verifier, expectedScope string, expectedKind TokenKind, expectedKey string, raw string, now time.Time, maxSkew time.Duration) (TokenClaims, string, error) {
	if expectedScope == "" {
		return TokenClaims{}, RejectReasonMissingScope, ErrMissingScope{}
	}
	if v == nil {
		return TokenClaims{}, RejectReasonNoVerifier, ErrNoVerifier{}
	}
	if raw == "" || len(raw) > maxSignedTokenLen {
		return TokenClaims{}, RejectReasonMalformed, ErrTokenVerification{Reason: RejectReasonMalformed}
	}

	firstDot := strings.IndexByte(raw, '.')
	if firstDot <= 0 {
		return TokenClaims{}, RejectReasonMalformed, ErrTokenVerification{Reason: RejectReasonMalformed}
	}
	prefix := raw[:firstDot]
	if prefix != tokenWirePrefix {
		return TokenClaims{}, RejectReasonMalformed, ErrTokenVerification{Reason: RejectReasonMalformed}
	}
	rest := raw[firstDot+1:]
	secondDot := strings.IndexByte(rest, '.')
	if secondDot <= 0 || secondDot == len(rest)-1 {
		return TokenClaims{}, RejectReasonMalformed, ErrTokenVerification{Reason: RejectReasonMalformed}
	}
	payloadB64 := rest[:secondDot]
	sigB64 := rest[secondDot+1:]
	if strings.IndexByte(sigB64, '.') >= 0 {
		return TokenClaims{}, RejectReasonMalformed, ErrTokenVerification{Reason: RejectReasonMalformed}
	}

	payloadJSON, err := base64.RawURLEncoding.Strict().DecodeString(payloadB64)
	if err != nil {
		return TokenClaims{}, RejectReasonMalformed, ErrTokenVerification{Reason: RejectReasonMalformed, Cause: err}
	}
	sig, err := base64.RawURLEncoding.Strict().DecodeString(sigB64)
	if err != nil || len(sig) == 0 {
		return TokenClaims{}, RejectReasonMalformed, ErrTokenVerification{Reason: RejectReasonMalformed, Cause: err}
	}

	var claims TokenClaims
	dec := json.NewDecoder(bytes.NewReader(payloadJSON))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&claims); err != nil {
		return TokenClaims{}, RejectReasonMalformed, ErrTokenVerification{Reason: RejectReasonMalformed, Cause: err}
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return TokenClaims{}, RejectReasonMalformed, ErrTokenVerification{Reason: RejectReasonMalformed}
	}
	if claims.KeyID == "" || claims.Scope == "" || claims.IssuedAt <= 0 || claims.ExpiresAt <= claims.IssuedAt {
		return TokenClaims{}, RejectReasonMalformed, ErrTokenVerification{Reason: RejectReasonMalformed}
	}
	if claims.Kind != TokenKindPass && claims.Kind != TokenKindLease {
		return TokenClaims{}, RejectReasonMalformed, ErrTokenVerification{Reason: RejectReasonMalformed}
	}

	signingInput := raw[:firstDot+1+secondDot]
	if err := v.Verify(claims.KeyID, []byte(signingInput), sig); err != nil {
		if errors.Is(err, errUnknownKeyID) {
			return TokenClaims{}, RejectReasonUnknownKey, ErrTokenVerification{Reason: RejectReasonUnknownKey, Cause: err}
		}
		return TokenClaims{}, RejectReasonInvalidSignature, ErrTokenVerification{Reason: RejectReasonInvalidSignature, Cause: err}
	}

	if claims.Scope != expectedScope {
		return TokenClaims{}, RejectReasonWrongScope, ErrTokenVerification{Reason: RejectReasonWrongScope}
	}
	if claims.Kind != expectedKind {
		return TokenClaims{}, RejectReasonWrongKind, ErrTokenVerification{Reason: RejectReasonWrongKind}
	}
	switch expectedKind {
	case TokenKindLease:
		if claims.Key == "" {
			return TokenClaims{}, RejectReasonMalformed, ErrTokenVerification{Reason: RejectReasonMalformed}
		}
		if claims.Key != expectedKey {
			return TokenClaims{}, RejectReasonKeyMismatch, ErrTokenVerification{Reason: RejectReasonKeyMismatch}
		}
	case TokenKindPass:
		if claims.Key != "" && claims.Key != expectedKey {
			return TokenClaims{}, RejectReasonKeyMismatch, ErrTokenVerification{Reason: RejectReasonKeyMismatch}
		}
	}

	skewSecs := int64(maxSkew / time.Second)
	if claims.IssuedAt > now.Unix()+skewSecs {
		return TokenClaims{}, RejectReasonFutureIssued, ErrTokenVerification{Reason: RejectReasonFutureIssued}
	}
	if now.Unix() >= claims.ExpiresAt {
		return TokenClaims{}, RejectReasonExpired, ErrTokenVerification{Reason: RejectReasonExpired}
	}

	return claims, "", nil
}

// TokenIssuer mints short-lived signed pass and lease envelopes without
// running a WaitingRoom. Intended for split Ed25519 deployments where a
// checkout or control-plane service holds the private key and serving replicas
// hold only public verification keys.
type TokenIssuer struct {
	scope  string
	signer Signer
	maxTTL time.Duration
	nowFn  func() time.Time
}

// IssuerOption configures NewTokenIssuer.
type IssuerOption func(*TokenIssuer) error

// WithIssuerMaxTTL sets the maximum lifetime permitted by MintPass and
// MintLease (default DefaultSignedMaxTTL, hard ceiling MaxSignedTokenTTL = 1h).
func WithIssuerMaxTTL(d time.Duration) IssuerOption {
	return func(ti *TokenIssuer) error {
		if err := validateSignedMaxTTL(d); err != nil {
			return err
		}
		ti.maxTTL = d
		return nil
	}
}

// NewTokenIssuer constructs a standalone issuer bound to scope and signer.
// Returns ErrMissingScope if scope is empty, ErrNoSigner if signer is nil, or
// an error from any IssuerOption.
func NewTokenIssuer(scope string, signer Signer, opts ...IssuerOption) (*TokenIssuer, error) {
	if scope == "" {
		return nil, ErrMissingScope{}
	}
	if len(scope) > maxScopeLen {
		return nil, ErrInvalidSigningKey{Reason: fmt.Sprintf("scope exceeds %d bytes", maxScopeLen)}
	}
	if signer == nil {
		return nil, ErrNoSigner{}
	}
	if err := validateKeyID(signer.KeyID()); err != nil {
		return nil, err
	}
	ti := &TokenIssuer{
		scope:  scope,
		signer: signer,
		maxTTL: DefaultSignedMaxTTL,
		nowFn:  time.Now,
	}
	for _, opt := range opts {
		if opt == nil {
			continue
		}
		if err := opt(ti); err != nil {
			return nil, err
		}
	}
	return ti, nil
}

// Scope returns the issuer's bound service scope.
func (ti *TokenIssuer) Scope() string { return ti.scope }

// MaxTTL returns the maximum token lifetime enforced by this issuer.
func (ti *TokenIssuer) MaxTTL() time.Duration { return ti.maxTTL }

// MintPass mints a signed VIP pass envelope for key (empty for an anonymous
// browser cookie pass, or a client key for a key-bound API pass).
// Rejects ttl <= 0 with ErrInvalidTokenTTL and ttl > MaxTTL with
// ErrSignedTokenTTLTooLong.
func (ti *TokenIssuer) MintPass(key string, ttl time.Duration) (string, error) {
	return mintEnvelope(ti.signer, ti.scope, TokenKindPass, key, ti.nowFn(), ttl, ti.maxTTL)
}

// MintLease mints a signed lease envelope bound to key.
// Rejects empty/invalid keys with ErrInvalidKey, ttl <= 0 with
// ErrInvalidTokenTTL (zero never means infinite), and ttl > MaxTTL with
// ErrSignedTokenTTLTooLong.
func (ti *TokenIssuer) MintLease(key string, ttl time.Duration) (string, error) {
	return mintEnvelope(ti.signer, ti.scope, TokenKindLease, key, ti.nowFn(), ttl, ti.maxTTL)
}

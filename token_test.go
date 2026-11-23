package room

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// helpers

func mustEd25519KeyPair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	return pub, priv
}

type customOnlySigner struct {
	kid string
}

func (s customOnlySigner) KeyID() string                { return s.kid }
func (s customOnlySigner) Sign(p []byte) ([]byte, error) { return append([]byte("sig:"), p...), nil }

type customOnlyVerifier struct {
	kid string
}

func (v customOnlyVerifier) Verify(keyID string, payload, sig []byte) error {
	if keyID != v.kid {
		return errUnknownKeyID
	}
	want := append([]byte("sig:"), payload...)
	if string(sig) != string(want) {
		return errInvalidSignature
	}
	return nil
}

// 1. Order-independence of SetSigner and SetVerifier (resolved at Validate/verify time)

func TestSetSignerVerifier_OrderIndependence_Ed25519(t *testing.T) {
	t.Parallel()
	oldPub, oldPriv := mustEd25519KeyPair(t)
	_, newPriv := mustEd25519KeyPair(t)

	oldSigner, err := NewEd25519Signer("k-old", oldPriv)
	if err != nil {
		t.Fatal(err)
	}
	newSigner, err := NewEd25519Signer("k-new", newPriv)
	if err != nil {
		t.Fatal(err)
	}
	oldVerifier, err := NewEd25519Verifier(map[string]ed25519.PublicKey{"k-old": oldPub})
	if err != nil {
		t.Fatal(err)
	}

	oldIssuer, err := NewTokenIssuer("billing", oldSigner)
	if err != nil {
		t.Fatal(err)
	}
	newIssuer, err := NewTokenIssuer("billing", newSigner)
	if err != nil {
		t.Fatal(err)
	}
	oldTok, err := oldIssuer.MintPass("", 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	newTok, err := newIssuer.MintPass("", 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	// Order A: SetSigner(new) then SetVerifier(oldPub)
	wrA := newTestWR(t, 4)
	wrA.SetScope("billing")
	wrA.SetSigner(newSigner)
	wrA.SetVerifier(oldVerifier)

	// Order B: SetVerifier(oldPub) then SetSigner(new)
	wrB := newTestWR(t, 4)
	wrB.SetScope("billing")
	wrB.SetVerifier(oldVerifier)
	wrB.SetSigner(newSigner)

	for name, wr := range map[string]*WaitingRoom{"SignerThenVerifier": wrA, "VerifierThenSigner": wrB} {
		if err := wr.Validate(); err != nil {
			t.Fatalf("%s: Validate: %v", name, err)
		}
		if _, err := wr.VerifyToken(oldTok, TokenKindPass, ""); err != nil {
			t.Errorf("%s: expected explicit oldVerifier to accept oldTok, got %v", name, err)
		}
		if _, err := wr.VerifyToken(newTok, TokenKindPass, ""); err == nil {
			t.Errorf("%s: expected explicit oldVerifier to reject newTok (proving explicit SetVerifier won regardless of order)", name)
		}
	}
}

func TestSetSignerVerifier_OrderIndependence_HMAC(t *testing.T) {
	t.Parallel()
	oldSecret := []byte("old-shared-secret-value-0001")
	newSecret := []byte("new-shared-secret-value-0002")

	oldSigner, err := NewHMACSigner("default", oldSecret)
	if err != nil {
		t.Fatal(err)
	}
	oldIssuer, err := NewTokenIssuer("orders", oldSigner)
	if err != nil {
		t.Fatal(err)
	}
	oldTok, err := oldIssuer.MintPass("", 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	newSigner, err := NewHMACSigner("default", newSecret)
	if err != nil {
		t.Fatal(err)
	}
	newIssuer, err := NewTokenIssuer("orders", newSigner)
	if err != nil {
		t.Fatal(err)
	}
	newTok, err := newIssuer.MintPass("", 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	wrA := newTestWR(t, 4)
	wrA.SetScope("orders")
	if err := wrA.SetSigningKey(newSecret); err != nil {
		t.Fatal(err)
	}
	if err := wrA.SetVerifyKeys(map[string][]byte{"default": oldSecret}); err != nil {
		t.Fatal(err)
	}

	wrB := newTestWR(t, 4)
	wrB.SetScope("orders")
	if err := wrB.SetVerifyKeys(map[string][]byte{"default": oldSecret}); err != nil {
		t.Fatal(err)
	}
	if err := wrB.SetSigningKey(newSecret); err != nil {
		t.Fatal(err)
	}

	for name, wr := range map[string]*WaitingRoom{"SigningKeyThenVerifyKeys": wrA, "VerifyKeysThenSigningKey": wrB} {
		if _, err := wr.VerifyToken(oldTok, TokenKindPass, ""); err != nil {
			t.Errorf("%s: oldTok rejected: %v", name, err)
		}
		if _, err := wr.VerifyToken(newTok, TokenKindPass, ""); err == nil {
			t.Errorf("%s: newTok unexpectedly accepted", name)
		}
	}
}

func TestValidate_SignerDerivedVerifierAndErrNoVerifier(t *testing.T) {
	t.Parallel()
	_, priv := mustEd25519KeyPair(t)
	edSigner, err := NewEd25519Signer("k1", priv)
	if err != nil {
		t.Fatal(err)
	}

	// Built-in Ed25519Signer derives verifier automatically when no explicit verifier is set.
	wr := newTestWR(t, 4)
	wr.SetScope("svc")
	wr.SetSigner(edSigner)
	if err := wr.Validate(); err != nil {
		t.Fatalf("Ed25519Signer single-setter Validate: %v", err)
	}
	tok, err := wr.MintPass("", 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !wr.HasValidPass(tok) {
		t.Error("expected derived verifier to verify token minted by Ed25519Signer")
	}

	// Custom Signer that cannot derive a Verifier must fail Validate with ErrNoVerifier.
	wrCustom := newTestWR(t, 4)
	wrCustom.SetScope("svc")
	wrCustom.SetSigner(customOnlySigner{kid: "c1"})
	if err := wrCustom.Validate(); !errors.As(err, &ErrNoVerifier{}) {
		t.Fatalf("custom signer without verifier: got %v, want ErrNoVerifier", err)
	}
	wrCustom.SetVerifier(customOnlyVerifier{kid: "c1"})
	if err := wrCustom.Validate(); err != nil {
		t.Fatalf("custom signer with explicit verifier: %v", err)
	}
}

// 2. Startup validation, Init options, RegisterRoutes panic, and SignedMaxTTL 1h hard cap

func TestValidate_AndRegisterRoutesPanic_NamesBothValues(t *testing.T) {
	t.Parallel()
	_, priv := mustEd25519KeyPair(t)
	signer, err := NewEd25519Signer("k1", priv)
	if err != nil {
		t.Fatal(err)
	}

	// Missing scope
	t.Run("missing scope", func(t *testing.T) {
		wr := newTestWR(t, 4)
		wr.SetSigner(signer)
		err := wr.Validate()
		if _, ok := err.(ErrMissingScope); !ok {
			t.Fatalf("Validate() = %T (%v), want ErrMissingScope", err, err)
		}
		var panicked any
		func() {
			defer func() { panicked = recover() }()
			wr.RegisterRoutes(gin.New())
		}()
		msg := fmt.Sprint(panicked)
		if panicked == nil || !strings.Contains(msg, "Scope") {
			t.Fatalf("RegisterRoutes panic = %q, want message naming Scope", msg)
		}
	})

	// PassDuration > SignedMaxTTL names both durations in Validate and RegisterRoutes panic
	t.Run("PassDuration exceeds SignedMaxTTL", func(t *testing.T) {
		wr := newTestWR(t, 4)
		wr.SetScope("billing")
		wr.SetSigner(signer)
		if err := wr.SetPassDuration(90 * time.Minute); err != nil {
			t.Fatal(err)
		}
		err := wr.Validate()
		var mismatch ErrPassDurationExceedsSignedMaxTTL
		if !errors.As(err, &mismatch) {
			t.Fatalf("Validate() = %T (%v), want ErrPassDurationExceedsSignedMaxTTL", err, err)
		}
		if mismatch.PassDuration != 90*time.Minute || mismatch.SignedMaxTTL != DefaultSignedMaxTTL {
			t.Errorf("mismatch fields = %+v, want 90m / 15m", mismatch)
		}
		if !strings.Contains(err.Error(), (90 * time.Minute).String()) || !strings.Contains(err.Error(), DefaultSignedMaxTTL.String()) {
			t.Errorf("error message %q does not name both values", err.Error())
		}

		var panicked any
		func() {
			defer func() { panicked = recover() }()
			wr.RegisterRoutes(gin.New())
		}()
		msg := fmt.Sprint(panicked)
		if panicked == nil || !strings.Contains(msg, (90*time.Minute).String()) || !strings.Contains(msg, DefaultSignedMaxTTL.String()) {
			t.Fatalf("RegisterRoutes panic = %q, want both PassDuration and SignedMaxTTL named", msg)
		}

		// Runtime fail closed even if Middleware was wired directly without RegisterRoutes.
		r := gin.New()
		r.Use(wr.Middleware())
		r.GET("/", func(c *gin.Context) { c.Status(http.StatusOK) })
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
		if w.Code != http.StatusInternalServerError {
			t.Errorf("misconfigured Middleware status = %d, want 500 fail-closed", w.Code)
		}
	})

	// Raising SignedMaxTTL up to 1h and aligning PassDuration succeeds; >1h is impossible.
	t.Run("SignedMaxTTL hard cap 1h", func(t *testing.T) {
		wr := newTestWR(t, 4)
		if err := wr.SetSignedMaxTTL(0); !errors.As(err, &ErrInvalidTokenTTL{}) {
			t.Errorf("SetSignedMaxTTL(0) = %v, want ErrInvalidTokenTTL", err)
		}
		if err := wr.SetSignedMaxTTL(-time.Minute); !errors.As(err, &ErrInvalidTokenTTL{}) {
			t.Errorf("SetSignedMaxTTL(-1m) = %v, want ErrInvalidTokenTTL", err)
		}
		if err := wr.SetSignedMaxTTL(61 * time.Minute); !errors.As(err, &ErrSignedTokenTTLTooLong{}) {
			t.Errorf("SetSignedMaxTTL(61m) = %v, want ErrSignedTokenTTLTooLong", err)
		}
		if err := wr.SetSignedMaxTTL(24 * time.Hour); !errors.As(err, &ErrSignedTokenTTLTooLong{}) {
			t.Errorf("SetSignedMaxTTL(24h) = %v, want ErrSignedTokenTTLTooLong", err)
		}
		if err := wr.SetSignedMaxTTL(MaxSignedTokenTTL); err != nil {
			t.Fatalf("SetSignedMaxTTL(1h): %v", err)
		}
		wr.SetScope("billing")
		wr.SetSigner(signer)
		if err := wr.SetPassDuration(60 * time.Minute); err != nil {
			t.Fatal(err)
		}
		if err := wr.Validate(); err != nil {
			t.Fatalf("Validate with 60m PassDuration and 1h SignedMaxTTL: %v", err)
		}
	})
}

func TestInit_FunctionalOptionsValidateImmediately(t *testing.T) {
	t.Parallel()
	pub, priv := mustEd25519KeyPair(t)

	// Missing scope fails inside Init without leaking reaper or marking initialised.
	var wr WaitingRoom
	err := wr.Init(4, WithEd25519VerifyKeys(map[string]ed25519.PublicKey{"k1": pub}))
	if _, ok := err.(ErrMissingScope); !ok {
		t.Fatalf("Init without WithScope: got %T (%v), want ErrMissingScope", err, err)
	}
	if wr.initialised.Load() {
		t.Error("failed Init must leave initialised=false")
	}

	// PassDuration > SignedMaxTTL fails inside Init.
	err = wr.Init(4,
		WithService("billing"),
		WithEd25519Signer("k1", priv),
		WithPassDuration(30*time.Minute),
	)
	if _, ok := err.(ErrPassDurationExceedsSignedMaxTTL); !ok {
		t.Fatalf("Init with PassDuration 30m > default 15m: got %T (%v)", err, err)
	}

	// Valid options succeed.
	if err := wr.Init(4,
		WithService("billing"),
		WithEd25519VerifyKeys(map[string]ed25519.PublicKey{"k1": pub}),
		WithLeaseFraction(0.5),
		WithClockSkew(10*time.Second),
	); err != nil {
		t.Fatalf("valid Init with options: %v", err)
	}
	defer wr.Stop()
	if wr.Scope() != "billing" || wr.CookiePrefix() != "billing" || wr.LeaseFraction() != 0.5 || wr.ClockSkew() != 10*time.Second {
		t.Errorf("options not applied: scope=%q prefix=%q frac=%v skew=%s", wr.Scope(), wr.CookiePrefix(), wr.LeaseFraction(), wr.ClockSkew())
	}
}

// 3. TokenIssuer & Ed25519 split mode (verify-only replicas cannot mint)

func TestTokenIssuer_BoundsAndSplitEd25519Replicas(t *testing.T) {
	t.Parallel()
	pub1, priv1 := mustEd25519KeyPair(t)
	pub2, priv2 := mustEd25519KeyPair(t)

	s1, err := NewEd25519Signer("k1", priv1)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := NewEd25519Signer("k2", priv2)
	if err != nil {
		t.Fatal(err)
	}

	// Constructor & TTL validation.
	if _, err := NewTokenIssuer("", s1); !errors.As(err, &ErrMissingScope{}) {
		t.Errorf("empty scope: got %v, want ErrMissingScope", err)
	}
	if _, err := NewTokenIssuer("svc", nil); !errors.As(err, &ErrNoSigner{}) {
		t.Errorf("nil signer: got %v, want ErrNoSigner", err)
	}
	if _, err := NewTokenIssuer("svc", s1, WithIssuerMaxTTL(2*time.Hour)); !errors.As(err, &ErrSignedTokenTTLTooLong{}) {
		t.Errorf("issuer maxTTL > 1h: got %v, want ErrSignedTokenTTLTooLong", err)
	}

	issuer1, err := NewTokenIssuer("checkout", s1)
	if err != nil {
		t.Fatal(err)
	}
	issuer2, err := NewTokenIssuer("checkout", s2)
	if err != nil {
		t.Fatal(err)
	}

	// Separate errors for zero/negative TTL vs too-long TTL.
	if _, err := issuer1.MintLease("agent-1", 0); !errors.As(err, &ErrInvalidTokenTTL{}) {
		t.Errorf("MintLease(ttl=0): got %T (%v), want ErrInvalidTokenTTL", err, err)
	}
	if _, err := issuer1.MintPass("", 0); !errors.As(err, &ErrInvalidTokenTTL{}) {
		t.Errorf("MintPass(ttl=0): got %T (%v), want ErrInvalidTokenTTL", err, err)
	}
	if _, err := issuer1.MintLease("agent-1", 20*time.Minute); !errors.As(err, &ErrSignedTokenTTLTooLong{}) {
		t.Errorf("MintLease(ttl=20m > 15m): got %T (%v), want ErrSignedTokenTTLTooLong", err, err)
	}
	if _, err := issuer1.MintLease("", 5*time.Minute); !errors.As(err, &ErrInvalidKey{}) {
		t.Errorf("MintLease(empty key): got %T (%v), want ErrInvalidKey", err, err)
	}

	// Two verify-only replicas holding rotated public keys k1 + k2, NO private key.
	pubSet := map[string]ed25519.PublicKey{"k1": pub1, "k2": pub2}
	newReplica := func() (*WaitingRoom, *gin.Engine, chan struct{}, chan struct{}) {
		wr := &WaitingRoom{}
		if err := wr.Init(1,
			WithScope("checkout"),
			WithEd25519VerifyKeys(pubSet),
			WithPassDuration(10*time.Minute),
		); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(wr.Stop)
		wr.SetRateFunc(func(depth int64) float64 { return 1.0 })
		serving := make(chan struct{}, 2)
		release := make(chan struct{})
		r := newTestRouter(wr, serving, release)
		return wr, r, serving, release
	}

	r1WR, r1, s1Ch, rel1 := newReplica()
	defer close(rel1)
	r2WR, r2, s2Ch, rel2 := newReplica()
	defer close(rel2)

	// Verify-only replica must fail closed on all minting paths without mutating queue.
	if tok := r1WR.GrantPass(); tok != "" {
		t.Errorf("verify-only GrantPass() = %q, want empty", tok)
	}
	if _, err := r1WR.GrantPassE(); !errors.As(err, &ErrNoSigner{}) {
		t.Errorf("verify-only GrantPassE() = %v, want ErrNoSigner", err)
	}
	if _, err := r1WR.MintPass("", 5*time.Minute); !errors.As(err, &ErrNoSigner{}) {
		t.Errorf("verify-only MintPass() = %v, want ErrNoSigner", err)
	}
	if _, err := r1WR.MintLease("a", 5*time.Minute); !errors.As(err, &ErrNoSigner{}) {
		t.Errorf("verify-only MintLease() = %v, want ErrNoSigner", err)
	}

	// Fill both replicas and queue normal clients ahead.
	fillOneSlot(t, r1, s1Ch)
	fillOneSlot(t, r2, s2Ch)
	_, q1 := serveWithCookie(r1, "")
	_, q2 := serveWithCookie(r1, "")
	if q1 == "" || q2 == "" {
		t.Fatal("expected normal requests to queue on r1")
	}
	// PromoteToken on verify-only replica fails with ErrNoSigner and does not move q2!
	if _, err := r1WR.PromoteToken(q2, 1); !errors.As(err, &ErrNoSigner{}) {
		t.Fatalf("PromoteToken on verify-only replica: got %v, want ErrNoSigner", err)
	}
	if ti, _ := r1WR.Ticket(q2); ti.Position != 2 {
		t.Errorf("q2 moved to position %d after failed PromoteToken, want 2", ti.Position)
	}

	// Token minted with k1 works on Replica 1; token minted with k2 works on Replica 2.
	// Both promote to position 1 AND stay gated by local cap (never bypass cap=1).
	passK1, err := issuer1.MintPass("", 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	passK2, err := issuer2.MintPass("", 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	// Queue normal waiter on r2 first.
	_, r2Normal := serveWithCookie(r2, "")
	if r2Normal == "" {
		t.Fatal("expected normal waiter on r2")
	}

	// VIP with k1 hits r1 while full -> queued at position 1, cap not exceeded.
	req1 := httptest.NewRequest(http.MethodGet, "/", nil)
	req1.AddCookie(&http.Cookie{Name: passCookieName, Value: passK1})
	w1 := httptest.NewRecorder()
	r1.ServeHTTP(w1, req1)
	if r1WR.Len() != 1 {
		t.Errorf("r1 Len() = %d, want 1 (signed pass must never bypass cap)", r1WR.Len())
	}
	var vip1Tok string
	for _, c := range w1.Result().Cookies() {
		if c.Name == cookieName {
			vip1Tok = c.Value
		}
	}
	if ti, ok := r1WR.Ticket(vip1Tok); !ok || ti.Position != 1 || !ti.Promoted {
		t.Errorf("r1 VIP ticket = %+v ok=%v, want position 1 promoted", ti, ok)
	}
	// Status endpoint also recognizes the signed VIP pass.
	statusReq := httptest.NewRequest(http.MethodGet, "/queue/status", nil)
	statusReq.AddCookie(&http.Cookie{Name: cookieName, Value: vip1Tok})
	statusReq.AddCookie(&http.Cookie{Name: passCookieName, Value: passK1})
	sw := httptest.NewRecorder()
	r1.ServeHTTP(sw, statusReq)
	var sResp statusResponse
	if err := json.Unmarshal(sw.Body.Bytes(), &sResp); err != nil || !sResp.HasPass || sResp.Position != 1 {
		t.Errorf("r1 /queue/status with signed pass = %+v (err=%v), want HasPass=true Position=1", sResp, err)
	}

	// VIP with rotated k2 hits r2 while full -> queued at position 1 ahead of r2Normal.
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2.AddCookie(&http.Cookie{Name: passCookieName, Value: passK2})
	w2 := httptest.NewRecorder()
	r2.ServeHTTP(w2, req2)
	if r2WR.Len() != 1 {
		t.Errorf("r2 Len() = %d, want 1", r2WR.Len())
	}
	var vip2Tok string
	for _, c := range w2.Result().Cookies() {
		if c.Name == cookieName {
			vip2Tok = c.Value
		}
	}
	if ti, ok := r2WR.Ticket(vip2Tok); !ok || ti.Position != 1 {
		t.Errorf("r2 VIP ticket = %+v ok=%v, want position 1", ti, ok)
	}
}

// 4. Split-by-transport rejection handling & EventTokenRejected observability

func TestTokenRejection_SplitByTransportAndEvents(t *testing.T) {
	t.Parallel()
	pub, priv := mustEd25519KeyPair(t)
	otherPub, otherPriv := mustEd25519KeyPair(t)
	_ = otherPub

	signer, _ := NewEd25519Signer("k1", priv)
	otherSigner, _ := NewEd25519Signer("k-unknown", otherPriv)

	baseTime := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	var clockMu sync.Mutex
	curTime := baseTime
	nowFn := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return curTime
	}

	issuerGood, _ := NewTokenIssuer("svc-a", signer)
	issuerGood.nowFn = nowFn
	issuerWrongScope, _ := NewTokenIssuer("svc-b", signer)
	issuerWrongScope.nowFn = nowFn
	issuerUnknownKey, _ := NewTokenIssuer("svc-a", otherSigner)
	issuerUnknownKey.nowFn = nowFn

	wr := &WaitingRoom{}
	if err := wr.Init(1,
		WithScope("svc-a"),
		WithEd25519VerifyKeys(map[string]ed25519.PublicKey{"k1": pub}),
	); err != nil {
		t.Fatal(err)
	}
	defer wr.Stop()
	wr.nowFn = nowFn

	rejected := collectEvents(wr, EventTokenRejected)
	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	defer close(release)
	r := keyedRouter(wr, serving, release)
	fillOneSlot(t, r, serving)

	// Mint test tokens at baseTime.
	wrongScopePass, _ := issuerWrongScope.MintPass("", 10*time.Minute)
	leaseForAlice, _ := issuerGood.MintLease("alice", 10*time.Minute)
	passForAlice, _ := issuerGood.MintPass("alice", 10*time.Minute)
	unknownKeyPass, _ := issuerUnknownKey.MintPass("", 10*time.Minute)
	shortPass, _ := issuerGood.MintPass("alice", 5*time.Second)
	shortLease, _ := issuerGood.MintLease("alice", 5*time.Second)

	// Future-dated token beyond default 30s clock skew.
	issuerFuture, _ := NewTokenIssuer("svc-a", signer)
	issuerFuture.nowFn = func() time.Time { return baseTime.Add(5 * time.Minute) }
	futurePass, _ := issuerFuture.MintPass("alice", 5*time.Minute)

	// Tampered signature: mutate an interior signature byte so base64 decodes
	// to a genuinely altered 64-byte signature.
	idx := len(passForAlice) - 10
	mut := byte('A')
	if passForAlice[idx] == 'A' {
		mut = 'B'
	}
	tampered := passForAlice[:idx] + string(mut) + passForAlice[idx+1:]

	// (a) Browser cookie failures fall back to normal queue (200 HTML waiting room) and emit EventTokenRejected.
	cookieCases := []struct {
		name       string
		cookieVal  string
		wantReason string
	}{
		{"malformed", "not-a-token", RejectReasonMalformed},
		{"tampered", tampered, RejectReasonInvalidSignature},
		{"unknown_key", unknownKeyPass, RejectReasonUnknownKey},
		{"wrong_scope", wrongScopePass, RejectReasonWrongScope},
		{"wrong_kind_lease_in_pass_cookie", leaseForAlice, RejectReasonWrongKind},
		{"key_bound_pass_in_anon_cookie", passForAlice, RejectReasonKeyMismatch},
	}
	for _, tc := range cookieCases {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.AddCookie(&http.Cookie{Name: passCookieName, Value: tc.cookieVal})
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "You're in the queue") {
			t.Errorf("cookie %s: got status %d, want 200 waiting page fallback", tc.name, w.Code)
		}
		ev := nextSnapshot(t, rejected)
		if ev.Reason != tc.wantReason || ev.TokenTransport != TokenTransportCookie {
			t.Errorf("cookie %s event: reason=%q transport=%q, want %q / %q", tc.name, ev.Reason, ev.TokenTransport, tc.wantReason, TokenTransportCookie)
		}
	}

	// (b) Explicit API header failures (non-expired) abort with 401/403 and emit EventTokenRejected.
	headerCases := []struct {
		name       string
		key        string
		headers    map[string]string
		wantStatus int
		wantReason string
	}{
		{"malformed pass", "alice", map[string]string{DefaultPassHeader: "garbage"}, http.StatusUnauthorized, RejectReasonMalformed},
		{"tampered pass", "alice", map[string]string{DefaultPassHeader: tampered}, http.StatusUnauthorized, RejectReasonInvalidSignature},
		{"unknown kid", "alice", map[string]string{DefaultPassHeader: unknownKeyPass}, http.StatusUnauthorized, RejectReasonUnknownKey},
		{"future iat", "alice", map[string]string{DefaultPassHeader: futurePass}, http.StatusUnauthorized, RejectReasonFutureIssued},
		{"wrong scope pass", "alice", map[string]string{DefaultPassHeader: wrongScopePass}, http.StatusForbidden, RejectReasonWrongScope},
		{"pass replayed as lease", "alice", map[string]string{DefaultLeaseHeader: passForAlice}, http.StatusForbidden, RejectReasonWrongKind},
		{"lease replayed as pass", "alice", map[string]string{DefaultPassHeader: leaseForAlice}, http.StatusForbidden, RejectReasonWrongKind},
		{"pass key mismatch", "bob", map[string]string{DefaultPassHeader: passForAlice}, http.StatusForbidden, RejectReasonKeyMismatch},
		{"lease key mismatch", "bob", map[string]string{DefaultLeaseHeader: leaseForAlice}, http.StatusForbidden, RejectReasonKeyMismatch},
		{"lease without identity key", "", map[string]string{DefaultLeaseHeader: leaseForAlice}, http.StatusForbidden, RejectReasonKeyMismatch},
	}
	for _, tc := range headerCases {
		w := keyedGet(r, tc.key, tc.headers)
		if w.Code != tc.wantStatus {
			t.Errorf("header %s: status=%d, want %d", tc.name, w.Code, tc.wantStatus)
		}
		ev := nextSnapshot(t, rejected)
		if ev.Reason != tc.wantReason || ev.TokenTransport != TokenTransportHeader {
			t.Errorf("header %s event: reason=%q transport=%q, want %q / %q", tc.name, ev.Reason, ev.TokenTransport, tc.wantReason, TokenTransportHeader)
		}
	}

	// (c) Forged token with past exp must still report invalid_signature (401), never expired!
	expiredPayload, _ := json.Marshal(TokenClaims{
		KeyID: "k1", Kind: TokenKindPass, Scope: "svc-a", Key: "alice",
		IssuedAt: baseTime.Add(-2 * time.Hour).Unix(), ExpiresAt: baseTime.Add(-time.Hour).Unix(),
	})
	forgedExpired := tokenWirePrefix + "." + base64.RawURLEncoding.EncodeToString(expiredPayload) + "." + base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.SignatureSize))
	w := keyedGet(r, "alice", map[string]string{DefaultPassHeader: forgedExpired})
	if w.Code != http.StatusUnauthorized {
		t.Errorf("forged expired token: status=%d, want 401", w.Code)
	}
	if ev := nextSnapshot(t, rejected); ev.Reason != RejectReasonInvalidSignature {
		t.Errorf("forged expired token reason=%q, want %q", ev.Reason, RejectReasonInvalidSignature)
	}

	// (d) Advance clock so shortPass, shortLease, and wrongScopePass expire.
	// Wrong-scope expired token must STILL be loud (403 wrong_scope), whereas genuinely expired
	// in-scope API header tokens fall back to the normal queue (503) + emit expired event.
	clockMu.Lock()
	curTime = baseTime.Add(30 * time.Minute)
	clockMu.Unlock()

	w = keyedGet(r, "alice", map[string]string{DefaultPassHeader: wrongScopePass})
	if w.Code != http.StatusForbidden {
		t.Errorf("expired wrong-scope header: status=%d, want 403", w.Code)
	}
	if ev := nextSnapshot(t, rejected); ev.Reason != RejectReasonWrongScope {
		t.Errorf("expired wrong-scope reason=%q, want %q", ev.Reason, RejectReasonWrongScope)
	}

	w = keyedGet(r, "alice", map[string]string{DefaultPassHeader: shortPass})
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expired pass header while full: status=%d, want 503 queue fallback", w.Code)
	}
	if ev := nextSnapshot(t, rejected); ev.Reason != RejectReasonExpired || ev.TokenTransport != TokenTransportHeader {
		t.Errorf("expired pass header event = %+v, want expired/header", ev)
	}

	w = keyedGet(r, "alice", map[string]string{DefaultLeaseHeader: shortLease})
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expired lease header while full: status=%d, want 503 queue fallback", w.Code)
	}
	if ev := nextSnapshot(t, rejected); ev.Reason != RejectReasonExpired || ev.TokenTransport != TokenTransportHeader {
		t.Errorf("expired lease header event = %+v, want expired/header", ev)
	}
}

func TestCustomTokenExtractor_WithCookieFallback(t *testing.T) {
	t.Parallel()
	pub, priv := mustEd25519KeyPair(t)
	signer, _ := NewEd25519Signer("k1", priv)
	issuer, _ := NewTokenIssuer("api", signer)

	wr := &WaitingRoom{}
	if err := wr.Init(1,
		WithScope("api"),
		WithEd25519VerifyKeys(map[string]ed25519.PublicKey{"k1": pub}),
		WithTokenExtractor(func(c *gin.Context) (string, string) {
			return c.GetHeader("X-Custom-VIP"), c.GetHeader("X-Custom-Lease")
		}),
	); err != nil {
		t.Fatal(err)
	}
	defer wr.Stop()

	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	defer close(release)
	r := keyedRouter(wr, serving, release)
	fillOneSlot(t, r, serving)

	// Queue two normal users first.
	keyedGet(r, "regular-1", nil)
	keyedGet(r, "regular-2", nil)

	// Keyed client uses custom VIP header -> auto-promoted to front (Position=1, ahead of regular-2).
	vipTok, _ := issuer.MintPass("vip-agent", 5*time.Minute)
	w := keyedGet(r, "vip-agent", map[string]string{"X-Custom-VIP": vipTok})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("custom VIP header while full: %d", w.Code)
	}
	vipTI, ok := wr.TicketByKey("vip-agent")
	reg2TI, ok2 := wr.TicketByKey("regular-2")
	if !ok || !ok2 || vipTI.Position != 1 || !vipTI.Promoted || vipTI.Place >= reg2TI.Place {
		t.Errorf("custom VIP ticket = %+v (regular-2=%+v), want Position=1 Promoted=true ahead of regular-2", vipTI, reg2TI)
	}

	// Browser still falls back to room_pass cookie when custom extractor returns "".
	cookiePass, _ := issuer.MintPass("", 5*time.Minute)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: passCookieName, Value: cookiePass})
	bw := httptest.NewRecorder()
	r.ServeHTTP(bw, req)
	var bTok string
	for _, c := range bw.Result().Cookies() {
		if c.Name == cookieName {
			bTok = c.Value
		}
	}
	if ti, ok := wr.Ticket(bTok); !ok || !ti.Promoted || ti.Position != 1 {
		t.Errorf("cookie fallback with custom extractor: %+v ok=%v, want promoted at pos 1", ti, ok)
	}
}

// 5. Work-conserving bounded lease pool & remote signed lease lifecycle

func TestLeaseCeiling_DefaultsAndSmallCapClamp(t *testing.T) {
	t.Parallel()
	pub, _ := mustEd25519KeyPair(t)

	// Unconfigured room: default fraction is 1.0 (full v1.x compat).
	wrPlain := newTestWR(t, 5)
	if wrPlain.LeaseFraction() != 1.0 || wrPlain.LeaseCeiling() != 5 {
		t.Errorf("unconfigured: frac=%v ceiling=%d, want 1.0 / 5", wrPlain.LeaseFraction(), wrPlain.LeaseCeiling())
	}

	// Signed room: default fraction is 0.25 with cap>=2 clamp to >=1.
	cases := []struct {
		cap  int32
		frac float64 // < 0 means leave default
		want int32
	}{
		{100, -1, 25},
		{10, -1, 2},
		{4, -1, 1},
		{3, -1, 1}, // floor(0.75)=0 -> clamped to 1 because cap>=2 && frac>0
		{2, -1, 1}, // floor(0.50)=0 -> clamped to 1 because cap>=2 && frac>0
		{1, -1, 0}, // cap=1 with 0.25 stays 0 unless explicitly 1.0
		{1, 1.0, 1},
		{10, 0.0, 0}, // explicit 0 disables leases
		{10, 1.0, 10},
	}
	for _, tc := range cases {
		wr := &WaitingRoom{}
		opts := []Option{
			WithScope("svc"),
			WithEd25519VerifyKeys(map[string]ed25519.PublicKey{"k1": pub}),
		}
		if tc.frac >= 0 {
			opts = append(opts, WithLeaseFraction(tc.frac))
		}
		if err := wr.Init(tc.cap, opts...); err != nil {
			t.Fatalf("cap=%d frac=%v: %v", tc.cap, tc.frac, err)
		}
		if got := wr.LeaseCeiling(); got != tc.want {
			t.Errorf("cap=%d frac=%v: LeaseCeiling()=%d, want %d", tc.cap, tc.frac, got, tc.want)
		}
		wr.Stop()
	}

	// Invalid fractions rejected.
	wr := newTestWR(t, 4)
	for _, bad := range []float64{-0.1, 1.01, math.NaN(), math.Inf(1), math.Inf(-1)} {
		if err := wr.SetLeaseFraction(bad); !errors.As(err, &ErrInvalidLeaseFraction{}) {
			t.Errorf("SetLeaseFraction(%v) = %v, want ErrInvalidLeaseFraction", bad, err)
		}
	}
}

func TestLeasePool_WorkConservingAndOverflowQueuesAtFront(t *testing.T) {
	t.Parallel()
	pub, priv := mustEd25519KeyPair(t)
	signer, _ := NewEd25519Signer("k1", priv)
	issuer, _ := NewTokenIssuer("compute", signer)

	// cap=4, default frac=0.25 -> LeaseCeiling=1, total cap=4.
	wr := &WaitingRoom{}
	if err := wr.Init(4,
		WithScope("compute"),
		WithEd25519VerifyKeys(map[string]ed25519.PublicKey{"k1": pub}),
	); err != nil {
		t.Fatal(err)
	}
	defer wr.Stop()

	serving := make(chan struct{}, 8)
	release := make(chan struct{})
	r := keyedRouter(wr, serving, release)

	// 1. Work-conserving: with 0 leases held, all 4 slots can serve normal HTTP requests.
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := keyedGet(r, fmt.Sprintf("http-%d", i), nil)
			if w.Code != http.StatusOK {
				t.Errorf("normal HTTP %d: got %d, want 200", i, w.Code)
			}
		}()
		<-serving
	}
	if wr.Len() != 4 || wr.Held() != 0 {
		t.Fatalf("work-conserving: Len=%d Held=%d, want 4 / 0", wr.Len(), wr.Held())
	}
	close(release)
	wg.Wait()
	waitForNowServing(t, wr, 4, 2*time.Second)

	// 2. Remote signed lease 1 arrives while idle -> reserves the 1 allowed lease slot.
	lease1, _ := issuer.MintLease("lease-1", 10*time.Minute)
	lease2, _ := issuer.MintLease("lease-2", 10*time.Minute)

	serving2 := make(chan struct{}, 8)
	release2 := make(chan struct{})
	r2 := gin.New()
	wr.SetTicketKeyFunc(func(c *gin.Context) (string, bool) {
		k := c.GetHeader("X-Key")
		return k, k != ""
	})
	wr.RegisterRoutes(r2)
	r2.GET("/", func(c *gin.Context) {
		if c.GetHeader("X-Hold") == "1" {
			serving2 <- struct{}{}
			<-release2
		}
		c.Status(http.StatusOK)
	})

	if w := keyedGet(r2, "lease-1", map[string]string{DefaultLeaseHeader: lease1}); w.Code != http.StatusOK {
		t.Fatalf("remote lease-1 first arrival: %d, want 200", w.Code)
	}
	if wr.Held() != 1 || wr.Len() != 1 || !wr.HasLease("lease-1") {
		t.Fatalf("after lease-1: Held=%d Len=%d HasLease=%v, want 1/1/true", wr.Held(), wr.Len(), wr.HasLease("lease-1"))
	}

	// Repeat request from lease-1 passes without taking a second slot.
	if w := keyedGet(r2, "lease-1", map[string]string{DefaultLeaseHeader: lease1}); w.Code != http.StatusOK {
		t.Fatalf("remote lease-1 repeat: %d", w.Code)
	}
	if wr.Held() != 1 || wr.Len() != 1 {
		t.Fatalf("after lease-1 repeat: Held=%d Len=%d, want 1/1", wr.Held(), wr.Len())
	}

	// 3. Lease ceiling (1) is now full, but 3 general slots remain open.
	// Remote lease-2 runs as a transient HTTP request (never raising Held above 1).
	if w := keyedGet(r2, "lease-2", map[string]string{DefaultLeaseHeader: lease2}); w.Code != http.StatusOK {
		t.Fatalf("lease-2 with open general slots: %d, want 200", w.Code)
	}
	if wr.Held() != 1 || wr.Len() != 1 || wr.HasLease("lease-2") {
		t.Fatalf("lease-2 must not exceed ceiling: Held=%d Len=%d HasLease(lease-2)=%v", wr.Held(), wr.Len(), wr.HasLease("lease-2"))
	}

	// 4. Fill remaining 3 general slots so Len == 4 == cap, queue two regular callers,
	// then send remote lease-2 -> must queue at position 1 of the general line (no bypass, no reject).
	var wg2 sync.WaitGroup
	for i := range 3 {
		wg2.Add(1)
		go func() {
			defer wg2.Done()
			keyedGet(r2, fmt.Sprintf("busy-%d", i), map[string]string{"X-Hold": "1"})
		}()
		<-serving2
	}
	if wr.Len() != 4 || wr.Held() != 1 {
		t.Fatalf("at full cap: Len=%d Held=%d, want 4/1", wr.Len(), wr.Held())
	}

	if w := keyedGet(r2, "normal-a", nil); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("normal-a: %d", w.Code)
	}
	if w := keyedGet(r2, "normal-b", nil); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("normal-b: %d", w.Code)
	}

	wOverflow := keyedGet(r2, "lease-2", map[string]string{DefaultLeaseHeader: lease2})
	if wOverflow.Code != http.StatusServiceUnavailable {
		t.Fatalf("overflow lease-2 while full: status=%d, want 503 queued at front", wOverflow.Code)
	}
	lease2TI, ok := wr.TicketByKey("lease-2")
	normalBTI, ok2 := wr.TicketByKey("normal-b")
	if !ok || !ok2 || lease2TI.Position != 1 || !lease2TI.Promoted || lease2TI.Place >= normalBTI.Place {
		t.Errorf("overflow lease-2 = %+v (normal-b=%+v), want Position=1 Promoted=true ahead of normal-b", lease2TI, normalBTI)
	}
	if wr.Len() != 4 || wr.Held() != 1 {
		t.Errorf("overflow lease-2 must never push host past cap: Len=%d Held=%d", wr.Len(), wr.Held())
	}

	// Release the 3 busy HTTP handlers -> lease-2 admits cleanly as a transient request.
	close(release2)
	wg2.Wait()

	if w := keyedGet(r2, "lease-2", map[string]string{DefaultLeaseHeader: lease2}); w.Code != http.StatusOK {
		t.Fatalf("lease-2 after drain: status=%d, want 200", w.Code)
	}
	if wr.Held() != 1 || wr.Len() != 1 {
		t.Errorf("after lease-2 admitted: Held=%d Len=%d, want 1/1", wr.Held(), wr.Len())
	}

	// Clean up remaining waiters & check exact ticket accounting.
	_ = wr.Leave("normal-a")
	_ = wr.Leave("normal-b")
	assertAccounting(t, wr)
	assertIndexes(t, wr.tokens)
}

func TestRemoteSignedLease_ExpiresWithoutLocalLeaseTTL(t *testing.T) {
	t.Parallel()
	pub, priv := mustEd25519KeyPair(t)
	signer, _ := NewEd25519Signer("k1", priv)

	base := time.Date(2026, 11, 23, 14, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	cur := base
	nowFn := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return cur
	}

	issuer, _ := NewTokenIssuer("agent-pool", signer)
	issuer.nowFn = nowFn

	// Local LeaseTTL stays 0, yet remote signed lease carries a 2-minute exp and must expire.
	wr := &WaitingRoom{}
	if err := wr.Init(4,
		WithScope("agent-pool"),
		WithEd25519VerifyKeys(map[string]ed25519.PublicKey{"k1": pub}),
	); err != nil {
		t.Fatal(err)
	}
	defer wr.Stop()
	wr.nowFn = nowFn

	expiredEvents := collectEvents(wr, EventLeaseExpire)
	r := keyedRouter(wr, nil, nil)

	tok, err := issuer.MintLease("agent-x", 2*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if w := keyedGet(r, "agent-x", map[string]string{DefaultLeaseHeader: tok}); w.Code != http.StatusOK {
		t.Fatalf("initial reserve: %d", w.Code)
	}
	if wr.Held() != 1 || wr.Len() != 1 {
		t.Fatalf("Held=%d Len=%d, want 1/1", wr.Held(), wr.Len())
	}

	// Advance clock past signed expiry and run reaper.
	mu.Lock()
	cur = base.Add(3 * time.Minute)
	mu.Unlock()
	wr.reap()

	if wr.Held() != 0 || wr.Len() != 0 || wr.HasLease("agent-x") {
		t.Fatalf("expired remote lease survived reap: Held=%d Len=%d", wr.Held(), wr.Len())
	}
	if ev := nextSnapshot(t, expiredEvents); ev.Key != "agent-x" {
		t.Errorf("EventLeaseExpire Key=%q, want agent-x", ev.Key)
	}
	assertAccounting(t, wr)
}

func TestSetCap_ResizeRecomputesLeaseCeiling(t *testing.T) {
	t.Parallel()
	pub, _ := mustEd25519KeyPair(t)
	wr := &WaitingRoom{}
	if err := wr.Init(20,
		WithScope("svc"),
		WithEd25519VerifyKeys(map[string]ed25519.PublicKey{"k1": pub}),
	); err != nil {
		t.Fatal(err)
	}
	defer wr.Stop()

	// cap=20, frac=0.25 -> ceiling=5
	for i := range 5 {
		if ok, _, err := wr.Acquire(fmt.Sprintf("l%d", i)); err != nil || !ok {
			t.Fatalf("Acquire l%d: ok=%v err=%v", i, ok, err)
		}
	}
	// 6th lease hits ceiling=5 even though cap=20!
	if ok, pos, err := wr.Acquire("l5"); err != nil || ok || pos != 1 {
		t.Fatalf("Acquire l5 past ceiling: ok=%v pos=%d err=%v, want false/1/nil", ok, pos, err)
	}

	// Shrink cap to 8 -> ceiling=2; existing 5 leases are preserved, no new leases granted until Held < 2.
	if err := wr.SetCap(8); err != nil {
		t.Fatal(err)
	}
	if wr.LeaseCeiling() != 2 || wr.Held() != 5 {
		t.Fatalf("after shrink: ceiling=%d Held=%d, want 2/5", wr.LeaseCeiling(), wr.Held())
	}
	for i := range 3 { // 5 -> 2 held (still at ceiling 2)
		if err := wr.Release(fmt.Sprintf("l%d", i)); err != nil {
			t.Fatal(err)
		}
		if ok, _, _ := wr.Acquire("l5"); ok {
			t.Fatalf("l5 granted with Held=%d and ceiling=2", wr.Held())
		}
	}
	// Release one more (2 -> 1 < ceiling 2) -> l5 can now acquire.
	if err := wr.Release("l3"); err != nil {
		t.Fatal(err)
	}
	if ok, _, err := wr.Acquire("l5"); err != nil || !ok {
		t.Fatalf("l5 refused with Held=1 and ceiling=2: ok=%v err=%v", ok, err)
	}
	assertAccounting(t, wr)
}

// 6. Per-service cookie prefixing & multi-service isolation

func TestCookiePrefix_IsolatesTwoServicesOnOneHost(t *testing.T) {
	t.Parallel()
	pub, priv := mustEd25519KeyPair(t)
	signer, _ := NewEd25519Signer("k1", priv)
	issuerA, _ := NewTokenIssuer("svc_a", signer)

	// Invalid prefixes rejected.
	wrBad := newTestWR(t, 1)
	for _, bad := range []string{"bad name", "a;b", "a=b", "a/b", strings.Repeat("x", maxCookiePrefixLen+1)} {
		if err := wrBad.SetCookiePrefix(bad); !errors.As(err, &ErrInvalidCookiePrefix{}) {
			t.Errorf("SetCookiePrefix(%q) = %v, want ErrInvalidCookiePrefix", bad, err)
		}
	}
	if err := wrBad.SetService(""); !errors.As(err, &ErrInvalidCookiePrefix{}) {
		t.Errorf("SetService(\"\") = %v, want ErrInvalidCookiePrefix", err)
	}

	wrA := &WaitingRoom{}
	if err := wrA.Init(1, WithService("svc_a"), WithEd25519VerifyKeys(map[string]ed25519.PublicKey{"k1": pub})); err != nil {
		t.Fatal(err)
	}
	defer wrA.Stop()
	wrB := &WaitingRoom{}
	if err := wrB.Init(1, WithService("svc_b"), WithEd25519VerifyKeys(map[string]ed25519.PublicKey{"k1": pub})); err != nil {
		t.Fatal(err)
	}
	defer wrB.Stop()

	if wrA.TicketCookieName() != "svc_a_ticket" || wrA.PassCookieName() != "svc_a_pass" || wrA.ProbeCookieName() != "svc_a_probe" {
		t.Fatalf("unexpected svc_a cookie names: %q %q %q", wrA.TicketCookieName(), wrA.PassCookieName(), wrA.ProbeCookieName())
	}

	servingA := make(chan struct{}, 1)
	releaseA := make(chan struct{})
	defer close(releaseA)
	rA := newTestRouter(wrA, servingA, releaseA)
	fillOneSlot(t, rA, servingA)

	servingB := make(chan struct{}, 1)
	releaseB := make(chan struct{})
	defer close(releaseB)
	rB := newTestRouter(wrB, servingB, releaseB)
	fillOneSlot(t, rB, servingB)

	// Request queued on svc_a sets svc_a_ticket & svc_a_probe and renders svc_a_probe in HTML.
	wA := httptest.NewRecorder()
	rA.ServeHTTP(wA, httptest.NewRequest(http.MethodGet, "/", nil))
	var tokA, probeA string
	for _, c := range wA.Result().Cookies() {
		if c.Name == "svc_a_ticket" {
			tokA = c.Value
		}
		if c.Name == "svc_a_probe" {
			probeA = c.Value
		}
	}
	if tokA == "" || probeA != probeCookieValue {
		t.Fatalf("svc_a cookies: ticket=%q probe=%q", tokA, probeA)
	}
	if !strings.Contains(wA.Body.String(), `const PROBE_COOKIE       = "svc_a_probe";`) {
		t.Error("waiting_room.html did not inject svc_a_probe")
	}

	// Status poll on svc_a works with svc_a_ticket and ignores svc_b_ticket.
	pollReq := httptest.NewRequest(http.MethodGet, "/queue/status", nil)
	pollReq.AddCookie(&http.Cookie{Name: "svc_a_ticket", Value: tokA})
	pw := httptest.NewRecorder()
	rA.ServeHTTP(pw, pollReq)
	var pResp statusResponse
	if err := json.Unmarshal(pw.Body.Bytes(), &pResp); err != nil || pResp.Position != 1 || pResp.CookiesRequired {
		t.Errorf("svc_a status poll: %+v err=%v", pResp, err)
	}

	// Presenting svc_a's ticket & signed pass to svc_b does not interfere with svc_b.
	passA, _ := issuerA.MintPass("", 10*time.Minute)
	reqB := httptest.NewRequest(http.MethodGet, "/", nil)
	reqB.AddCookie(&http.Cookie{Name: "svc_a_ticket", Value: tokA})
	reqB.AddCookie(&http.Cookie{Name: "svc_a_pass", Value: passA})
	wB := httptest.NewRecorder()
	rB.ServeHTTP(wB, reqB)
	var tokB string
	for _, c := range wB.Result().Cookies() {
		if c.Name == "svc_b_ticket" {
			tokB = c.Value
		}
	}
	if tokB == "" {
		t.Fatal("svc_b did not issue its own svc_b_ticket")
	}
	if ti, _ := wrB.Ticket(tokB); ti.Promoted {
		t.Error("svc_a pass must not promote a ticket on svc_b")
	}
}

// 7. Concurrent race & accounting stress test across signed passes, signed leases, and normal traffic

func TestSignedCapabilities_ConcurrentCapAndAccountingInvariant(t *testing.T) {
	t.Parallel()
	const capVal = 8
	pub, priv := mustEd25519KeyPair(t)
	signer, _ := NewEd25519Signer("k1", priv)
	issuer, _ := NewTokenIssuer("stress", signer)

	wr := &WaitingRoom{}
	if err := wr.Init(capVal,
		WithScope("stress"),
		WithEd25519VerifyKeys(map[string]ed25519.PublicKey{"k1": pub}),
		WithLeaseFraction(0.25), // LeaseCeiling == 2
	); err != nil {
		t.Fatal(err)
	}
	defer wr.Stop()

	var (
		inFlight  atomic.Int32
		peakTotal atomic.Int32
		peakHeld  atomic.Int32
	)
	wr.SetTicketKeyFunc(func(c *gin.Context) (string, bool) {
		k := c.GetHeader("X-Key")
		return k, k != ""
	})
	r := gin.New()
	wr.RegisterRoutes(r)
	r.GET("/", func(c *gin.Context) {
		cur := inFlight.Add(1)
		defer inFlight.Add(-1)
		// Snapshot Held + active check (note: lease holders don't take a second sem slot,
		// so total sem occupancy wr.Len() must never exceed capVal).
		if l := int32(wr.Len()); l > peakTotal.Load() {
			for {
				old := peakTotal.Load()
				if l <= old || peakTotal.CompareAndSwap(old, l) {
					break
				}
			}
		}
		if h := wr.Held(); h > peakHeld.Load() {
			for {
				old := peakHeld.Load()
				if h <= old || peakHeld.CompareAndSwap(old, h) {
					break
				}
			}
		}
		_ = cur
		time.Sleep(time.Millisecond)
		c.Status(http.StatusOK)
	})

	leaseTokens := make([]string, 10)
	passTokens := make([]string, 10)
	for i := range 10 {
		k := fmt.Sprintf("agent-%d", i)
		leaseTokens[i], _ = issuer.MintLease(k, 5*time.Minute)
		passTokens[i], _ = issuer.MintPass(k, 5*time.Minute)
	}

	var wg sync.WaitGroup
	for i := range 80 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			id := idx % 10
			key := fmt.Sprintf("agent-%d", id)
			switch idx % 4 {
			case 0:
				keyedGet(r, key, map[string]string{DefaultLeaseHeader: leaseTokens[id]})
			case 1:
				keyedGet(r, key, map[string]string{DefaultPassHeader: passTokens[id]})
			case 2:
				keyedGet(r, fmt.Sprintf("plain-%d", idx), nil)
			case 3:
				_ = wr.Release(key)
				wr.reap()
			}
		}(i)
	}
	wg.Wait()

	if peakTotal.Load() > capVal {
		t.Errorf("peak Len() = %d exceeded cap %d", peakTotal.Load(), capVal)
	}
	if peakHeld.Load() > wr.LeaseCeiling() {
		t.Errorf("peak Held() = %d exceeded LeaseCeiling %d", peakHeld.Load(), wr.LeaseCeiling())
	}
	if wr.Len() != int(wr.Held()) {
		t.Errorf("after drain: Len()=%d != Held()=%d", wr.Len(), wr.Held())
	}
	assertAccounting(t, wr)
	assertIndexes(t, wr.tokens)
}

func TestZeroValue_NewGettersDoNotPanic(t *testing.T) {
	t.Parallel()
	var wr WaitingRoom
	if wr.Scope() != "" {
		t.Errorf("zero Scope() = %q", wr.Scope())
	}
	if wr.CookiePrefix() != "room" || wr.TicketCookieName() != "room_ticket" || wr.PassCookieName() != "room_pass" || wr.ProbeCookieName() != "room_probe" {
		t.Errorf("zero cookie getters unexpected")
	}
	if wr.SignedMaxTTL() != DefaultSignedMaxTTL {
		t.Errorf("zero SignedMaxTTL() = %s", wr.SignedMaxTTL())
	}
	if wr.LeaseFraction() != 1.0 || wr.LeaseCeiling() != 0 {
		t.Errorf("zero LeaseFraction/Ceiling = %v / %d", wr.LeaseFraction(), wr.LeaseCeiling())
	}
	if wr.ClockSkew() != 0 {
		t.Errorf("zero ClockSkew() = %s", wr.ClockSkew())
	}
	if wr.HasValidPass("anything") {
		t.Error("zero HasValidPass should be false")
	}
	if err := wr.Validate(); !errors.As(err, &ErrNotInitialised{}) {
		t.Errorf("zero Validate() = %v, want ErrNotInitialised", err)
	}
}

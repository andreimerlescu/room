package room

import (
	"encoding/json"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// WaitResponder writes the response for a queued request in place of the
// default waiting-room page or JSON wait response. For keyed tickets t.Token
// is empty. Runs outside all room locks; the middleware aborts after fn returns.
type WaitResponder func(c *gin.Context, t TicketInfo)

// FullResponder writes the response when maxQueueDepth rejects an HTTP arrival
// in place of the default 503 Service Unavailable response. Runs outside all
// room locks; the middleware aborts after fn returns.
type FullResponder func(c *gin.Context, snap Snapshot)

// TokenExtractor extracts signed capability tokens (VIP pass and/or lease)
// from an incoming request. Identity remains the job of [TicketKeyFunc];
// TokenExtractor answers what capability the client has already been granted.
// Either returned string may be empty. When unset, Middleware reads
// [DefaultPassHeader] ("X-Room-Pass") and [DefaultLeaseHeader] ("X-Room-Lease")
// and falls back to the configured pass cookie when passToken is empty.
type TokenExtractor func(c *gin.Context) (passToken, leaseToken string)

// SetTokenExtractor overrides how signed pass and lease tokens are extracted
// from HTTP requests. Pass nil to restore the default header + cookie
// extractor. Not reset by [WaitingRoom.Init]. Safe at any time.
func (wr *WaitingRoom) SetTokenExtractor(fn TokenExtractor) {
	if fn == nil {
		wr.tokenExtractor.Store(nil)
		return
	}
	wr.tokenExtractor.Store(&fn)
}

func (wr *WaitingRoom) tokenExtractorLoad() TokenExtractor {
	if p := wr.tokenExtractor.Load(); p != nil {
		return *p
	}
	return nil
}

// SetWaitResponder overrides the queued response for both cookie and keyed
// requests. Pass nil to restore defaults. Not reset by [WaitingRoom.Init].
func (wr *WaitingRoom) SetWaitResponder(fn WaitResponder) {
	if fn == nil {
		wr.waitResponder.Store(nil)
		return
	}
	wr.waitResponder.Store(&fn)
}

func (wr *WaitingRoom) waitResponderLoad() WaitResponder {
	if p := wr.waitResponder.Load(); p != nil {
		return *p
	}
	return nil
}

// SetFullResponder overrides the rejection response written when maxQueueDepth
// rejects an HTTP arrival (cookie or keyed). Pass nil to restore the default
// 503 Service Unavailable response. Not reset by [WaitingRoom.Init]. Safe at
// any time.
func (wr *WaitingRoom) SetFullResponder(fn FullResponder) {
	if fn == nil {
		wr.fullResponder.Store(nil)
		return
	}
	wr.fullResponder.Store(&fn)
}

func (wr *WaitingRoom) fullResponderLoad() FullResponder {
	if p := wr.fullResponder.Load(); p != nil {
		return *p
	}
	return nil
}

// SetWaitJSON opts cookie requests that do not accept text/html into the JSON
// wait response (503, Retry-After). Off by default; reset to false by Init.
func (wr *WaitingRoom) SetWaitJSON(on bool) {
	wr.waitJSON.Store(on)
}

// WaitJSON reports whether SetWaitJSON is enabled.
func (wr *WaitingRoom) WaitJSON() bool {
	return wr.waitJSON.Load()
}

// SetStatusBaseURL sets the prefix prepended to a keyed ticket's public ID in
// status_url (empty restores "/queue/status/"). Reset by Init.
func (wr *WaitingRoom) SetStatusBaseURL(base string) {
	if base == "" {
		base = defaultStatusBaseURL
	}
	wr.statusBaseURL.Store(&base)
}

// StatusBaseURL returns the current status_url prefix.
func (wr *WaitingRoom) StatusBaseURL() string {
	if p := wr.statusBaseURL.Load(); p != nil && *p != "" {
		return *p
	}
	return defaultStatusBaseURL
}

func acceptsHTML(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept"), ",") {
		mt, params, err := mime.ParseMediaType(strings.TrimSpace(part))
		if err != nil || (mt != "text/html" && mt != "application/xhtml+xml") {
			continue
		}
		if q, ok := params["q"]; ok {
			if f, err := strconv.ParseFloat(q, 64); err == nil && f <= 0 {
				continue
			}
		}
		return true
	}
	return false
}

type waitResponse struct {
	Waiting    bool   `json:"waiting"`
	Position   int64  `json:"position"`
	QueueDepth int64  `json:"queue_depth"`
	StatusURL  string `json:"status_url"`
	RetryAfter int    `json:"retry_after"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "room: encoding response", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_, _ = w.Write(b)
}

func (wr *WaitingRoom) writeWaitJSON(w http.ResponseWriter, position int64, statusURL string) {
	secs := int(waitRetryAfter / time.Second)
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusServiceUnavailable, waitResponse{
		Waiting:    true,
		Position:   position,
		QueueDepth: wr.LiveQueueDepth(),
		StatusURL:  statusURL,
		RetryAfter: secs,
	})
}

func (wr *WaitingRoom) respondCookieWait(c *gin.Context, token string, position int64) bool {
	if fn := wr.waitResponderLoad(); fn != nil {
		e, ok := wr.tokens.get(token)
		ti := wr.ticketInfo(token, e)
		if ok {
			ti.Place, _ = wr.tokens.placeOfToken(token)
		}
		fn(c, ti)
		c.Abort()
		return true
	}
	if wr.waitJSON.Load() && !acceptsHTML(c.Request) {
		wr.writeWaitJSON(c.Writer, position, "/queue/status")
		c.Abort()
		return true
	}
	return false
}

func (wr *WaitingRoom) respondKeyedWait(c *gin.Context, token string, e ticketEntry, place int64) {
	if fn := wr.waitResponderLoad(); fn != nil {
		ti := wr.ticketInfo(token, e)
		ti.Token = ""
		ti.Place = place
		fn(c, ti)
		c.Abort()
		return
	}
	wr.writeWaitJSON(c.Writer, place, wr.StatusBaseURL()+e.id)
	c.Abort()
}

func (wr *WaitingRoom) rejectFull(c *gin.Context, key string) {
	c.Header("Cache-Control", "no-store")
	if c.Writer.Header().Get("Retry-After") == "" {
		c.Header("Retry-After", strconv.Itoa(int(waitRetryAfter/time.Second)))
	}
	var snap Snapshot
	if key != "" {
		snap = wr.snapshotForKey(EventReject, key)
	} else {
		snap = wr.snapshotFor(EventReject, "", wr.clientKey(c), false)
	}
	wr.emit(EventReject, snap)
	if fn := wr.fullResponderLoad(); fn != nil {
		fn(c, snap)
		c.Abort()
		return
	}
	c.AbortWithStatus(http.StatusServiceUnavailable)
}

type capabilityState struct {
	hasPass  bool
	hasLease bool
	leaseExp time.Time
}

func rejectionHTTPStatus(reason string) int {
	switch reason {
	case RejectReasonWrongScope, RejectReasonWrongKind, RejectReasonKeyMismatch, RejectReasonMissingScope:
		return http.StatusForbidden
	default:
		return http.StatusUnauthorized
	}
}

// resolveCapabilities extracts and verifies pass and lease tokens for the
// request. Browser cookie failures emit EventTokenRejected and fall back to
// ordinary queuing. Explicit header tokens abort with 401/403 on tampering,
// wrong scope, wrong kind, or key mismatch, and fall back to the queue on
// expiry (still emitting EventTokenRejected).
func (wr *WaitingRoom) resolveCapabilities(c *gin.Context, reqKey string) (capabilityState, bool) {
	var passTok, leaseTok string
	passTransport := TokenTransportHeader

	if fn := wr.tokenExtractorLoad(); fn != nil {
		passTok, leaseTok = fn(c)
	} else {
		passTok = c.GetHeader(DefaultPassHeader)
		leaseTok = c.GetHeader(DefaultLeaseHeader)
	}
	if passTok == "" {
		if pc, err := c.Request.Cookie(wr.PassCookieName()); err == nil && pc.Value != "" {
			passTok = pc.Value
			passTransport = TokenTransportCookie
		}
	}

	var state capabilityState
	signedMode := wr.signingEnabled()

	if leaseTok != "" {
		if !signedMode {
			wr.emit(EventTokenRejected, wr.snapshotForRejection(RejectReasonNoVerifier, TokenTransportHeader, reqKey, wr.clientKey(c)))
			c.AbortWithStatus(http.StatusUnauthorized)
			return capabilityState{}, false
		}
		claims, reason, err := wr.verifySignedToken(leaseTok, TokenKindLease, reqKey)
		if err == nil {
			state.hasLease = true
			state.leaseExp = time.Unix(claims.ExpiresAt, 0)
		} else {
			wr.emit(EventTokenRejected, wr.snapshotForRejection(reason, TokenTransportHeader, reqKey, wr.clientKey(c)))
			if reason != RejectReasonExpired {
				c.AbortWithStatus(rejectionHTTPStatus(reason))
				return capabilityState{}, false
			}
		}
	}

	if passTok != "" {
		if !signedMode {
			if passTransport == TokenTransportHeader {
				wr.emit(EventTokenRejected, wr.snapshotForRejection(RejectReasonNoVerifier, TokenTransportHeader, reqKey, wr.clientKey(c)))
				c.AbortWithStatus(http.StatusUnauthorized)
				return capabilityState{}, false
			}
			if _, ok, reason := wr.passes.lookup(passTok); ok {
				state.hasPass = true
			} else {
				wr.emit(EventTokenRejected, wr.snapshotForRejection(reason, TokenTransportCookie, reqKey, wr.clientKey(c)))
			}
		} else {
			_, reason, err := wr.verifySignedToken(passTok, TokenKindPass, reqKey)
			if err == nil {
				state.hasPass = true
			} else {
				wr.emit(EventTokenRejected, wr.snapshotForRejection(reason, passTransport, reqKey, wr.clientKey(c)))
				if passTransport == TokenTransportHeader && reason != RejectReasonExpired {
					c.AbortWithStatus(rejectionHTTPStatus(reason))
					return capabilityState{}, false
				}
			}
		}
	}

	return state, true
}

type keyedAction uint8

const (
	keyedPass keyedAction = iota
	keyedLeaseReserved
	keyedAdmit
	keyedWait
	keyedFull
	keyedError
)

func (wr *WaitingRoom) serveKeyed(c *gin.Context, key string, capState capabilityState) {
	act, ticket, k, filled := wr.decideKeyed(key, wr.clientKey(c), wr.now(), capState)
	switch act {
	case keyedPass:
		c.Next()
		return
	case keyedLeaseReserved:
		wr.emit(EventEnter, wr.snapshot(EventEnter))
		if filled {
			wr.emit(EventFull, wr.snapshot(EventFull))
		}
		wr.emit(EventLease, wr.snapshotForKey(EventLease, key))
		c.Next()
		return
	case keyedAdmit:
		wr.admit(c, ticket)
		return
	case keyedFull:
		wr.rejectFull(c, key)
		return
	case keyedError:
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}

	if (capState.hasPass || capState.hasLease) && !k.entry.promoted {
		wr.autoPromote(k.token)
		if e, ok := wr.tokens.get(k.token); ok {
			k.entry = e
		}
		if p, ok := wr.tokens.placeOfToken(k.token); ok {
			k.place = p
		}
	}
	if k.created {
		wr.emit(EventQueue, wr.snapshotForKey(EventQueue, key))
	}
	wr.respondKeyedWait(c, k.token, k.entry, k.place)
}

func (wr *WaitingRoom) decideKeyed(key, clientKey string, now time.Time, capState capabilityState) (keyedAction, int64, keyedEntry, bool) {
	ttl := time.Duration(wr.leaseTTL.Load())

	// Fast path: check existing local lease under RLock.
	wr.leaseMu.RLock()
	l, held := wr.leases[key]
	if held && !l.isExpired(now, ttl) {
		needExtend := capState.hasLease && !l.expiresAt.IsZero() && capState.leaseExp.After(l.expiresAt)
		wr.leaseMu.RUnlock()
		if needExtend {
			wr.leaseMu.Lock()
			if cur, still := wr.leases[key]; still && !cur.isExpired(now, ttl) {
				if !cur.expiresAt.IsZero() && capState.leaseExp.After(cur.expiresAt) {
					cur.expiresAt = capState.leaseExp
					cur.renewedAt = now
					wr.leases[key] = cur
				}
			}
			wr.leaseMu.Unlock()
		}
		return keyedPass, 0, keyedEntry{}, false
	}
	wr.leaseMu.RUnlock()

	if held {
		// Lazily reclaim an expired lease before re-evaluating.
		var expired bool
		wr.leaseMu.Lock()
		if cur, still := wr.leases[key]; still && cur.isExpired(now, ttl) {
			delete(wr.leases, key)
			expired = true
		}
		wr.leaseMu.Unlock()
		if expired {
			wr.release()
			wr.emit(EventLeaseExpire, wr.snapshotForKey(EventLeaseExpire, key))
		}
	}

	if capState.hasLease {
		return wr.decideSignedLease(key, clientKey, now, ttl, capState.leaseExp)
	}

	wr.leaseMu.RLock()
	defer wr.leaseMu.RUnlock()

	if cur, still := wr.leases[key]; still && !cur.isExpired(now, ttl) {
		return keyedPass, 0, keyedEntry{}, false
	}

	if token, e, ok := wr.tokens.lookupKey(key); ok {
		if wr.ticketReady(e.ticket) {
			if claimed, won := wr.tokens.take(token); won {
				return keyedAdmit, claimed.ticket, keyedEntry{}, false
			}
		} else if k := wr.tokens.enterKey(key, "", ticketEntry{}, nil, now); k.token != "" {
			return keyedWait, 0, k, false
		}
	}

	if _, full := wr.queueFullWithReap(now); full {
		return keyedFull, 0, keyedEntry{}, false
	}

	ticket := wr.nextTicket.Add(1)
	if wr.ticketReady(ticket) {
		return keyedAdmit, ticket, keyedEntry{}, false
	}

	token, fresh, err := wr.newKeyedEntry(key, now)
	if err != nil {
		wr.retire(ticket)
		return keyedError, 0, keyedEntry{}, false
	}
	fresh.clientKey = clientKey
	k := wr.tokens.enterKey(key, token, fresh, func() int64 { return ticket }, now)
	if !k.created {
		wr.retire(ticket)
	}
	return keyedWait, 0, k, false
}

// decideSignedLease handles a verified remote signed lease on this replica.
// It reserves a slot in the shared work-conserving lease pool (up to
// LeaseCeiling and Cap) on first arrival; when the lease pool or room is full,
// the caller falls through to the general line (auto-promoted to position 1
// in serveKeyed) rather than bypassing Cap or being rejected.
func (wr *WaitingRoom) decideSignedLease(key, clientKey string, now time.Time, ttl time.Duration, leaseExp time.Time) (keyedAction, int64, keyedEntry, bool) {
	wr.leaseMu.Lock()
	defer wr.leaseMu.Unlock()

	if cur, held := wr.leases[key]; held && !cur.isExpired(now, ttl) {
		if !cur.expiresAt.IsZero() && leaseExp.After(cur.expiresAt) {
			cur.expiresAt = leaseExp
			cur.renewedAt = now
			wr.leases[key] = cur
		}
		return keyedPass, 0, keyedEntry{}, false
	}

	ceiling := wr.leaseCeilingFor(wr.cap.Load())

	if token, e, ok := wr.tokens.lookupKey(key); ok {
		if wr.ticketReady(e.ticket) {
			if int32(len(wr.leases)) < ceiling && wr.sem.TryAcquire() {
				if claimed, won := wr.tokens.take(token); won {
					filled := wr.enter()
					wr.leases[key] = leaseEntry{
						ticket:     claimed.ticket,
						acquiredAt: now,
						renewedAt:  now,
						expiresAt:  leaseExp,
					}
					return keyedLeaseReserved, 0, keyedEntry{}, filled
				}
				_ = wr.sem.Release()
			}
			if claimed, won := wr.tokens.take(token); won {
				return keyedAdmit, claimed.ticket, keyedEntry{}, false
			}
		} else if k := wr.tokens.enterKey(key, "", ticketEntry{}, nil, now); k.token != "" {
			return keyedWait, 0, k, false
		}
	}

	// First arrival on this replica: reserve immediately if under lease
	// ceiling, no waiting line ahead, and a semaphore slot is open.
	if int32(len(wr.leases)) < ceiling && wr.tokens.len() == 0 && wr.sem.TryAcquire() {
		ticket := wr.nextTicket.Add(1)
		if wr.ticketReady(ticket) {
			filled := wr.enter()
			wr.leases[key] = leaseEntry{
				ticket:     ticket,
				acquiredAt: now,
				renewedAt:  now,
				expiresAt:  leaseExp,
			}
			return keyedLeaseReserved, 0, keyedEntry{}, filled
		}
		_ = wr.sem.Release()
		token, fresh, err := wr.newKeyedEntry(key, now)
		if err != nil {
			wr.retire(ticket)
			return keyedError, 0, keyedEntry{}, false
		}
		fresh.clientKey = clientKey
		k := wr.tokens.enterKey(key, token, fresh, func() int64 { return ticket }, now)
		if !k.created {
			wr.retire(ticket)
		}
		return keyedWait, 0, k, false
	}

	if _, full := wr.queueFullWithReap(now); full {
		return keyedFull, 0, keyedEntry{}, false
	}

	ticket := wr.nextTicket.Add(1)
	if wr.ticketReady(ticket) {
		return keyedAdmit, ticket, keyedEntry{}, false
	}

	token, fresh, err := wr.newKeyedEntry(key, now)
	if err != nil {
		wr.retire(ticket)
		return keyedError, 0, keyedEntry{}, false
	}
	fresh.clientKey = clientKey
	k := wr.tokens.enterKey(key, token, fresh, func() int64 { return ticket }, now)
	if !k.created {
		wr.retire(ticket)
	}
	return keyedWait, 0, k, false
}

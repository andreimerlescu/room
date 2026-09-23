package room

// RateFunc returns the per-position cost given the current queue depth.
// Implementations can return a flat rate, a curve, or surge pricing.
//
// Examples:
//
//	// Flat: $1 per position
//	func(depth int64) float64 { return 1.00 }
//
//	// Surge: price increases with queue depth
//	func(depth int64) float64 { return 0.50 + float64(depth)*0.01 }
type RateFunc func(queueDepth int64) float64

// SetRateFunc sets the per-position pricing function used by QuoteCost
// and PromoteToken. If nil, paid promotions are disabled and PromoteToken
// returns ErrPromotionDisabled. Operator promotions via AdminPromote do
// not depend on it.
//
// Safe to call at any time including while requests are in flight.
func (wr *WaitingRoom) SetRateFunc(fn RateFunc) {
	if fn == nil {
		wr.rateFunc.Store((*rateFuncHolder)(nil))
		return
	}
	wr.rateFunc.Store(&rateFuncHolder{fn: fn})
}

// rateFuncHolder wraps a RateFunc so that atomic.Value always stores the
// same concrete type. atomic.Value panics if Store is called with a
// different concrete type than a previous Store, so we cannot alternate
// between a nil interface and a concrete function value. Wrapping in a
// pointer-to-struct avoids this: we always store *rateFuncHolder (which
// may itself be nil).
type rateFuncHolder struct {
	fn RateFunc
}

// rateFuncLoad returns the current RateFunc or nil if unset.
func (wr *WaitingRoom) rateFuncLoad() RateFunc {
	v := wr.rateFunc.Load()
	if v == nil {
		return nil
	}
	h, ok := v.(*rateFuncHolder)
	if !ok || h == nil {
		return nil
	}
	return h.fn
}

// PromoteResult is returned by PromoteToken and PromoteTokenToFront.
// It contains the computed cost and, when pass duration is configured,
// a pass token that should be set as the room_pass cookie on the
// client's response.
type PromoteResult struct {
	// Cost is the price computed at promotion time based on the
	// current RateFunc and queue position.
	Cost float64

	// PassToken is the VIP pass token to set as the room_pass cookie.
	// Empty if SetPassDuration was not called or is 0.
	PassToken string
}

// QuoteCost returns the cost for a queued token to jump to targetPosition.
// targetPosition=1 means next to be admitted. Returns the cost without
// modifying any state — use this to display pricing on the waiting room page.
//
// Returns ErrNotInitialised on an uninitialised WaitingRoom.
// Returns ErrPromotionDisabled if no RateFunc is set.
// Returns ErrTokenNotFound if the token does not exist in the queue.
// Returns ErrAlreadyAdmitted if the token is already within the serving window.
// Returns ErrInvalidTargetPosition if targetPosition < 1.
//
// Related: PromoteToken, SetRateFunc
func (wr *WaitingRoom) QuoteCost(token string, targetPosition int64) (float64, error) {
	if !wr.initialised.Load() {
		return 0, ErrNotInitialised{}
	}
	if targetPosition < 1 {
		return 0, ErrInvalidTargetPosition{Given: targetPosition}
	}

	fn := wr.rateFuncLoad()
	if fn == nil {
		return 0, ErrPromotionDisabled{}
	}

	entry, ok := wr.tokens.get(token)
	if !ok {
		return 0, ErrTokenNotFound{}
	}

	currentPosition := wr.positionOf(entry.ticket)
	if currentPosition <= 0 {
		return 0, ErrAlreadyAdmitted{}
	}

	if targetPosition >= currentPosition {
		return 0, nil // already at or ahead of target
	}

	distance := currentPosition - targetPosition
	rate := fn(wr.QueueDepth())
	return float64(distance) * rate, nil
}

// PromoteToken moves a queued token to targetPosition in the queue.
// targetPosition=1 means next to be admitted.
//
// The caller is responsible for payment verification before calling this.
// PromoteToken does not handle payments — it only reassigns the ticket.
//
// When SetPassDuration has been configured with a non-zero duration, the
// returned PromoteResult includes a PassToken. The caller must set this
// as the room_pass cookie on the HTTP response so that the client is
// auto-promoted on subsequent queue entries without paying again.
//
// # Placement and fairness
//
// The token is placed at the ticket number that currently sits at
// targetPosition, accounting for retired tickets ahead of it, so the
// client sees exactly that position. Promotions are serialized, and the
// serving window only moves forward, so a later promotion to the same
// target never lands ahead of an earlier one — at most it ties with it.
// Tied clients become eligible together; the semaphore still bounds real
// concurrency.
//
// The client's former ticket number is left in the sequence as a vacancy
// that offsets the shared number, so admission stays exact in aggregate.
// Until the window passes the vacancy, clients between the new and old
// positions may see a position one lower than their true place, and one
// extra client may be eligible at a time (it waits briefly for a slot).
//
// If the token is removed, reaped or admitted concurrently, the promotion
// fails with ErrTokenNotFound rather than resurrecting it.
//
// Returns ErrNotInitialised on an uninitialised WaitingRoom.
// Returns ErrPromotionDisabled if no RateFunc is set.
// Returns ErrTokenNotFound if the token does not exist.
// Returns ErrAlreadyAdmitted if already within the serving window.
// Returns ErrInvalidTargetPosition if targetPosition < 1.
//
// Fires EventPromote with Snapshot.Token and Snapshot.ClientKey set.
//
// Related: QuoteCost, PromoteTokenToFront, AdminPromote, SetRateFunc, SetPassDuration
func (wr *WaitingRoom) PromoteToken(token string, targetPosition int64) (PromoteResult, error) {
	if !wr.initialised.Load() {
		return PromoteResult{}, ErrNotInitialised{}
	}
	if targetPosition < 1 {
		return PromoteResult{}, ErrInvalidTargetPosition{Given: targetPosition}
	}

	fn := wr.rateFuncLoad()
	if fn == nil {
		return PromoteResult{}, ErrPromotionDisabled{}
	}

	wr.promoteMu.Lock()
	defer wr.promoteMu.Unlock()

	// Price against the depth at promotion time. Moving a ticket does not
	// change QueueDepth, so reading it before the move is equivalent.
	rate := fn(wr.QueueDepth())

	p, err := wr.moveToPosition(token, targetPosition)
	if err != nil {
		return PromoteResult{}, err
	}
	if !p.moved {
		return PromoteResult{}, nil // no-op, already ahead
	}

	wr.emit(EventPromote, wr.snapshotFor(EventPromote, token, p.entry.clientKey, false))

	// Grant a time-limited VIP pass if pass duration is configured.
	return PromoteResult{
		Cost:      float64(p.distance) * rate,
		PassToken: wr.GrantPass(),
	}, nil
}

// PromoteTokenToFront is a convenience wrapper that promotes a token
// to position 1 (next to be admitted).
//
// Related: PromoteToken, QuoteCost
func (wr *WaitingRoom) PromoteTokenToFront(token string) (PromoteResult, error) {
	return wr.PromoteToken(token, 1)
}

// AdminPromote moves a queued token to targetPosition on an operator's
// authority. targetPosition=1 means next to be admitted.
//
// It is the pricing-free counterpart of PromoteToken: it does not need a
// RateFunc, computes no cost, and grants no VIP pass (a pass is a cookie,
// and an administrative request has no way to deliver it to the
// visitor's browser). Placement, fairness and concurrency behave exactly
// as documented on PromoteToken, and EventPromote fires with
// Snapshot.Token and Snapshot.ClientKey set.
//
// Enabling operator promotion this way does not enable paid promotion:
// without a RateFunc, QuoteCost and PromoteToken still return
// ErrPromotionDisabled.
//
// A token already at or ahead of targetPosition is left unchanged and nil
// is returned.
//
// Returns ErrNotInitialised on an uninitialised WaitingRoom.
// Returns ErrInvalidTargetPosition if targetPosition < 1.
// Returns ErrTokenNotFound if the token does not exist (or was removed,
// reaped or admitted concurrently).
// Returns ErrAlreadyAdmitted if already within the serving window.
//
// Related: PromoteToken, EventPromote
func (wr *WaitingRoom) AdminPromote(token string, targetPosition int64) error {
	if !wr.initialised.Load() {
		return ErrNotInitialised{}
	}
	if targetPosition < 1 {
		return ErrInvalidTargetPosition{Given: targetPosition}
	}

	wr.promoteMu.Lock()
	defer wr.promoteMu.Unlock()

	p, err := wr.moveToPosition(token, targetPosition)
	if err != nil {
		return err
	}
	if p.moved {
		wr.emit(EventPromote, wr.snapshotFor(EventPromote, token, p.entry.clientKey, false))
	}
	return nil
}

// promotion describes the outcome of moveToPosition.
type promotion struct {
	// moved is false when the token was already at or ahead of the target.
	moved bool

	// distance is the number of positions jumped (0 when !moved).
	distance int64

	// entry is the token's state after the move (or before, if !moved).
	entry ticketEntry
}

// moveToPosition is the shared core of PromoteToken, AdminPromote and
// autoPromote. The caller must hold wr.promoteMu so that concurrent
// promotions compute their targets against a consistent sequence.
//
// The ticket is rewritten only if the token still exists at write time;
// a token removed, reaped or admitted between the read and the write is
// reported as ErrTokenNotFound rather than silently re-inserted, which
// would account for its ticket twice.
func (wr *WaitingRoom) moveToPosition(token string, target int64) (promotion, error) {
	entry, ok := wr.tokens.get(token)
	if !ok {
		return promotion{}, ErrTokenNotFound{}
	}

	current := wr.positionOf(entry.ticket)
	if current <= 0 {
		return promotion{}, ErrAlreadyAdmitted{}
	}
	if target >= current {
		return promotion{entry: entry}, nil
	}

	newTicket := wr.ticketForPosition(target)

	updated, ok := wr.tokens.updateIfPresent(token, func(e *ticketEntry) {
		e.ticket = newTicket
		e.promoted = true
	})
	if !ok {
		return promotion{}, ErrTokenNotFound{}
	}

	return promotion{moved: true, distance: current - target, entry: updated}, nil
}

// ticketForPosition returns the ticket number at which a client would see
// exactly the given position (>= 1), taking retired tickets ahead of it
// into account.
//
// It solves n = edge + position + countBelow(n) — the inverse of
// positionOf — by fixed-point iteration. countBelow is non-decreasing in
// n, so n only grows, and it is bounded by edge + position + ledger size,
// so the loop terminates; in the common case (no ghosts) it runs once.
func (wr *WaitingRoom) ticketForPosition(position int64) int64 {
	edge := wr.nowServing.Load() + int64(wr.cap.Load())
	n := edge + position
	for {
		next := edge + position + wr.ledger.countBelow(n)
		if next == n {
			return n
		}
		n = next
	}
}

// updateIfPresent applies fn to the token's entry under the write lock
// only if the token still exists, and returns the updated entry. It never
// inserts.
func (ts *tokenStore) updateIfPresent(token string, fn func(*ticketEntry)) (ticketEntry, bool) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	e, ok := ts.entries[token]
	if !ok {
		return ticketEntry{}, false
	}
	fn(&e)
	ts.entries[token] = e
	return e, true
}

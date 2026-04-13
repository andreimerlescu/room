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
// and PromoteToken. If nil, promotions are disabled and PromoteToken
// returns ErrPromotionDisabled.
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

// QuoteCost returns the cost for a queued token to jump to targetPosition.
// targetPosition=1 means next to be admitted. Returns the cost without
// modifying any state — use this to display pricing on the waiting room page.
//
// Returns ErrPromotionDisabled if no RateFunc is set.
// Returns ErrTokenNotFound if the token does not exist in the queue.
// Returns ErrAlreadyAdmitted if the token is already within the serving window.
// Returns ErrInvalidTargetPosition if targetPosition < 1.
//
// Related: PromoteToken, SetRateFunc
func (wr *WaitingRoom) QuoteCost(token string, targetPosition int64) (float64, error) {
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
// Returns the cost that was computed at promotion time. The caller should
// compare this against the amount actually charged to detect race conditions
// where the queue moved between QuoteCost and PromoteToken.
//
// Promotion is serialized: only one PromoteToken call executes at a time
// to prevent two promotions from claiming the same ticket slot. Each
// successive promotion to the same target position is placed one position
// behind the previous promotee, ensuring unique ticket assignment.
//
// Returns ErrPromotionDisabled if no RateFunc is set.
// Returns ErrTokenNotFound if the token does not exist.
// Returns ErrAlreadyAdmitted if already within the serving window.
// Returns ErrInvalidTargetPosition if targetPosition < 1.
//
// Related: QuoteCost, PromoteTokenToFront, SetRateFunc
func (wr *WaitingRoom) PromoteToken(token string, targetPosition int64) (float64, error) {
	if targetPosition < 1 {
		return 0, ErrInvalidTargetPosition{Given: targetPosition}
	}

	fn := wr.rateFuncLoad()
	if fn == nil {
		return 0, ErrPromotionDisabled{}
	}

	// Serialize promotions so two concurrent callers cannot claim
	// the same synthetic ticket number.
	wr.promoteMu.Lock()
	defer wr.promoteMu.Unlock()

	entry, ok := wr.tokens.get(token)
	if !ok {
		return 0, ErrTokenNotFound{}
	}

	currentPosition := wr.positionOf(entry.ticket)
	if currentPosition <= 0 {
		return 0, ErrAlreadyAdmitted{}
	}

	if targetPosition >= currentPosition {
		return 0, nil // no-op, already ahead
	}

	distance := currentPosition - targetPosition
	rate := fn(wr.QueueDepth())
	cost := float64(distance) * rate

	// Compute the new ticket number. We use promoteInsert to guarantee
	// uniqueness: it tracks the lowest ticket number that the next
	// promotion is allowed to claim. Each promotion takes the minimum
	// of the natural ceiling (where the target position would land in
	// the ticket space) and promoteInsert, then decrements promoteInsert
	// so the next caller gets a strictly lower value.
	//
	// promoteInsert is initialised to math.MaxInt64, so the first
	// promotion always uses the natural ceiling. Subsequent promotions
	// use whichever is lower — the ceiling or the running counter —
	// preventing collisions even when multiple tokens target the same
	// position.
	ceiling := wr.nowServing.Load() + int64(wr.cap.Load()) + targetPosition
	insert := wr.promoteInsert.Load()
	if ceiling < insert {
		insert = ceiling
	}

	newTicket := insert
	wr.promoteInsert.Store(insert - 1)

	// Update the entry with the new ticket.
	entry.ticket = newTicket
	entry.promoted = true
	wr.tokens.set(token, entry)

	wr.emit(EventPromote, wr.snapshot(EventPromote))

	return cost, nil
}

// PromoteTokenToFront is a convenience wrapper that promotes a token
// to position 1 (next to be admitted).
//
// Related: PromoteToken, QuoteCost
func (wr *WaitingRoom) PromoteTokenToFront(token string) (float64, error) {
	return wr.PromoteToken(token, 1)
}

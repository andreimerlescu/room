package room

// RateFunc returns the per-position skip-the-line cost given current queue depth.
type RateFunc func(queueDepth int64) float64

// SetRateFunc configures the per-position pricing function used by
// [WaitingRoom.QuoteCost] and [WaitingRoom.PromoteToken]. Pass nil to disable
// paid promotions ([WaitingRoom.AdminPromote] is unaffected). Safe at any time.
func (wr *WaitingRoom) SetRateFunc(fn RateFunc) {
	if fn == nil {
		wr.rateFunc.Store(nil)
		return
	}
	wr.rateFunc.Store(&fn)
}

func (wr *WaitingRoom) rateFuncLoad() RateFunc {
	if p := wr.rateFunc.Load(); p != nil {
		return *p
	}
	return nil
}

// PromoteResult is returned by [WaitingRoom.PromoteToken] and
// [WaitingRoom.PromoteTokenToFront].
type PromoteResult struct {
	Cost      float64
	PassToken string // empty when PassDuration is 0
}

// QuoteCost returns the read-only cost for token to jump to targetPosition
// (1 = front of line). Returns [ErrNotInitialised], [ErrInvalidTargetPosition],
// [ErrPromotionDisabled], [ErrTokenNotFound], or [ErrAlreadyAdmitted].
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
		return 0, nil
	}

	distance := currentPosition - targetPosition
	return float64(distance) * fn(wr.QueueDepth()), nil
}

// PromoteToken moves a queued token to targetPosition (1 = front) after
// payment verification and grants a VIP pass when [WaitingRoom.SetPassDuration]
// is enabled.
//
// Promotions are serialized and land at the exact ticket number sitting at
// targetPosition (accounting for ghosts), so a later payer never lands ahead
// of an earlier one to the same target. Fires [EventPromote] when moved.
//
// Returns [ErrNotInitialised], [ErrInvalidTargetPosition],
// [ErrPromotionDisabled], [ErrTokenNotFound], or [ErrAlreadyAdmitted].
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

	rate := fn(wr.QueueDepth())

	p, err := wr.moveToPosition(token, targetPosition)
	if err != nil {
		return PromoteResult{}, err
	}
	if !p.moved {
		return PromoteResult{}, nil
	}

	wr.emit(EventPromote, wr.snapshotFor(EventPromote, token, p.entry.clientKey, false))
	return PromoteResult{
		Cost:      float64(p.distance) * rate,
		PassToken: wr.GrantPass(),
	}, nil
}

// PromoteTokenToFront promotes token to position 1.
func (wr *WaitingRoom) PromoteTokenToFront(token string) (PromoteResult, error) {
	return wr.PromoteToken(token, 1)
}

// AdminPromote moves a queued token to targetPosition without requiring a
// RateFunc or issuing a VIP pass. Fires [EventPromote] when moved.
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

type promotion struct {
	moved    bool
	distance int64
	entry    ticketEntry
}

// moveToPosition moves token to target under wr.promoteMu. Fails with
// ErrTokenNotFound if token was removed or admitted concurrently.
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
	updated, ok := wr.tokens.promoteTicket(token, newTicket)
	if !ok {
		return promotion{}, ErrTokenNotFound{}
	}
	return promotion{moved: true, distance: current - target, entry: updated}, nil
}

// ticketForPosition solves n = edge + position + countBelow(n) by fixed-point
// iteration so a promoted ticket lands at the exact requested position.
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

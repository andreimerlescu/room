package room

// RemoveToken removes a queued ticket immediately and retires its ticket
// number so clients behind it move up at once without admitting anyone early.
//
// Fires [EventRemove] with Snapshot.Token and Snapshot.ClientKey on success.
// Returns [ErrNotInitialised] or [ErrTokenNotFound] (including when already
// admitted, removed, or reaped).
func (wr *WaitingRoom) RemoveToken(token string) error {
	if !wr.initialised.Load() {
		return ErrNotInitialised{}
	}
	entry, ok := wr.tokens.take(token)
	if !ok {
		return ErrTokenNotFound{}
	}
	wr.retire(entry.ticket)
	wr.emit(EventRemove, wr.snapshotFor(EventRemove, token, entry.clientKey, false))
	return nil
}

// RemoveTokensFunc removes every queued ticket for which match returns true
// and returns the count removed. match runs outside all room locks and may
// call back into the room; all matched tokens are then claimed under one lock
// and retired together.
//
// Fires [EventRemove] once (with empty Token) if at least one ticket was
// removed. Returns 0 if match is nil or the room is uninitialised.
func (wr *WaitingRoom) RemoveTokensFunc(match func(TicketInfo) bool) int {
	if match == nil || !wr.initialised.Load() {
		return 0
	}

	candidates := wr.tokens.snapshotEntries()
	matched := make([]string, 0)
	for _, c := range candidates {
		if match(wr.ticketInfo(c.token, c.entry)) {
			matched = append(matched, c.token)
		}
	}
	if len(matched) == 0 {
		return 0
	}

	taken := wr.tokens.takeMany(matched)
	if len(taken) == 0 {
		return 0
	}

	tickets := make([]int64, len(taken))
	for i, e := range taken {
		tickets[i] = e.ticket
	}
	wr.retireMany(tickets)
	wr.emit(EventRemove, wr.snapshot(EventRemove))
	return len(taken)
}

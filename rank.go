package room

import "sort"

// Ranked tickets.
//
// By default the line is first come, first served. SetTicketRank lets the
// application rank a waiting client — a signed-in member, a customer, a
// visitor in the middle of checkout — so that it waits ahead of every
// lower-ranked client and behind every client of equal or higher rank, in
// arrival order within a rank. Rank 0, the default, is an ordinary
// arrival; a new arrival always joins behind every ranked client.
//
// # How a ticket moves
//
// PromoteToken and AdminPromote give a ticket the number of the client
// already at the target position, so the two tie and become eligible
// together. A rank move inserts strictly instead: the ticket takes the
// number just behind the last waiting ticket of equal or higher rank
// (the front of the line if there is none), and every ticket between
// there and its old number moves back one place. Ghost-ledger entries in
// that range move with them.
//
// The set of occupied numbers is unchanged — only who holds which number
// changes — so ticket accounting, QueueDepth and the serving window are
// unaffected. Clients jumped over see their position grow by one, which
// is exactly what happened: someone was placed ahead of them.
//
// Rank only ever moves a ticket forward. Lowering a ticket's rank records
// the new rank, which later rank moves take into account, but does not
// move the ticket back.
//
// # Concurrency
//
// A rank move holds the token store's write lock for one pass over the
// store, and is serialized with promotions. If the serving window reaches
// the moved range while the move is in progress, a client may become
// eligible a moment early or late; the semaphore still bounds real
// concurrency, as with promotions.

// SetTicketRank records rank for a queued client and moves its ticket
// ahead of every waiting ticket of lower rank. It reports whether the
// ticket moved.
//
// Typical use is from middleware placed ahead of the room, after the
// request was queued:
//
//	if tok := issuedTicket(c); tok != "" {
//	    _, _ = wr.SetTicketRank(tok, rankFor(c))
//	}
//
// Returns ErrNotInitialised on an uninitialised WaitingRoom,
// ErrInvalidRank if rank < 0, ErrTokenNotFound if the token does not
// exist, and ErrAlreadyAdmitted if the ticket is already inside the
// serving window (its rank is still recorded). Callers ranking visitors
// as they arrive can treat ErrTokenNotFound and ErrAlreadyAdmitted as
// benign.
//
// Fires EventRank, with Snapshot.Token and Snapshot.ClientKey set, when
// the ticket moved.
//
// Cost: one pass over the token store under its write lock, which briefly
// blocks status polls. Call it once per ranked arrival, not per request.
//
// Related: TicketInfo.Rank, EventRank, WaitingRoom.AdminPromote
func (wr *WaitingRoom) SetTicketRank(token string, rank int) (bool, error) {
	if !wr.initialised.Load() {
		return false, ErrNotInitialised{}
	}
	if rank < 0 {
		return false, ErrInvalidRank{Given: rank}
	}

	// Promotions compute target numbers from the same sequence a rank
	// move rewrites, so the two are serialized.
	wr.promoteMu.Lock()
	defer wr.promoteMu.Unlock()

	moved, key, err := wr.placeByRank(token, rank)
	if err != nil || !moved {
		return false, err
	}
	wr.emit(EventRank, wr.snapshotFor(EventRank, token, key, false))
	return true, nil
}

// placeByRank records rank on the token's ticket and, when that ranks it
// above waiting tickets ahead of it, moves it in front of them. Caller
// holds wr.promoteMu.
func (wr *WaitingRoom) placeByRank(token string, rank int) (moved bool, clientKey string, err error) {
	ts := wr.tokens
	ts.mu.Lock()
	defer ts.mu.Unlock()

	entry, ok := ts.entries[token]
	if !ok {
		return false, "", ErrTokenNotFound{}
	}
	entry.rank = rank
	ts.entries[token] = entry

	edge := wr.nowServing.Load() + int64(wr.cap.Load())
	from := entry.ticket
	if from <= edge {
		return false, entry.clientKey, ErrAlreadyAdmitted{}
	}
	if rank == 0 {
		return false, entry.clientKey, nil
	}

	// Just behind the last waiting ticket ahead of this one whose rank is
	// at least as high, or the front of the line.
	to := edge + 1
	for t, e := range ts.entries {
		if t == token || e.ticket <= edge || e.ticket >= from || e.rank < rank {
			continue
		}
		if n := e.ticket + 1; n > to {
			to = n
		}
	}
	if to >= from {
		return false, entry.clientKey, nil
	}

	// Rotate [to, from] by one: every ticket in [to, from) moves back a
	// place and this ticket takes to. Everything in the range has a lower
	// rank, by the choice of to.
	jumped := 0
	for t, e := range ts.entries {
		if t != token && e.ticket >= to && e.ticket < from {
			e.ticket++
			ts.entries[t] = e
			jumped++
		}
	}
	if jumped == 0 {
		// Only vacancies or retired numbers lay ahead; nobody was
		// passed, so nothing moves.
		return false, entry.clientKey, nil
	}
	wr.ledger.shiftRange(to, from)
	entry.ticket = to
	ts.entries[token] = entry
	return true, entry.clientKey, nil
}

// shiftRange adds one to every retired ticket in [lo, hi), the ghosts
// inside a rank move's range, so they keep their place relative to the
// tickets around them. Sorted order is preserved: shifted entries land in
// [lo+1, hi], below or equal to every entry already at or above hi.
func (l *ghostLedger) shiftRange(lo, hi int64) {
	if l.size.Load() == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	i := sort.Search(len(l.tickets), func(i int) bool { return l.tickets[i] >= lo })
	for ; i < len(l.tickets) && l.tickets[i] < hi; i++ {
		l.tickets[i]++
	}
	l.publishLocked()
}

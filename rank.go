package room

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
// A rank move uses O(log N) order-statistic treap splits and lazy range-add
// (+1) under the token store's write lock (plus O(log G) ghost-ledger shift),
// serialized with promotions. If the serving window reaches the moved range
// while the move is in progress, a client may become eligible a moment early
// or late; the semaphore still bounds real concurrency, as with promotions.

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
// Cost: O(log N) under the token store's write lock.
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
// above waiting tickets ahead of it, moves it in front of them in O(log N).
// Caller holds wr.promoteMu.
func (wr *WaitingRoom) placeByRank(token string, rank int) (moved bool, clientKey string, err error) {
	ts := wr.tokens
	ts.mu.Lock()
	defer ts.mu.Unlock()

	node, ok := ts.entries[token]
	if !ok {
		return false, "", ErrTokenNotFound{}
	}
	ts.tree.setRank(node, rank)

	edge := wr.nowServing.Load() + int64(wr.cap.Load())
	from := effectiveTicket(node, ts.hasLazyShifts)
	if from <= edge {
		return false, node.clientKey, ErrAlreadyAdmitted{}
	}
	if rank == 0 {
		return false, node.clientKey, nil
	}

	// Partition treap into:
	//   inWindow:   ticket <= edge
	//   candidates: edge < ticket < from
	//   rest:       ticket >= from (contains node)
	inWindow, gtEdge := splitByTicket(ts.tree.root, edge, true)
	candidates, rest := splitByTicket(gtEdge, from, false)

	// Target is just behind the highest ticket in (edge, from) with
	// rank >= rank, or edge+1 if none.
	to := edge + 1
	if maxT, found := maxTicketWithRankAtLeast(candidates, rank); found {
		if maxT+1 > to {
			to = maxT + 1
		}
	}
	if to >= from {
		ts.tree.root = ticketMerge(ticketMerge(inWindow, candidates), rest)
		if ts.tree.root != nil {
			ts.tree.root.parent = nil
		}
		return false, node.clientKey, nil
	}

	// Split candidates into beforeTo (< to) and toShift ([to, from)).
	beforeTo, toShift := splitByTicket(candidates, to, false)
	if toShift == nil {
		// Only vacancies or retired numbers lay ahead; nobody was
		// passed, so nothing moves.
		ts.tree.root = ticketMerge(ticketMerge(inWindow, beforeTo), rest)
		if ts.tree.root != nil {
			ts.tree.root.parent = nil
		}
		return false, node.clientKey, nil
	}

	// Detach node from rest (its effective ticket in rest is from).
	restLeft, restMidRight := splitByKey(rest, from, node.createdAt, node.token, false)
	_, restRight := splitByKey(restMidRight, from, node.createdAt, node.token, true)
	restWithoutNode := ticketMerge(restLeft, restRight)
	node.detach()

	// Shift every ticket in [to, from) by +1 in O(1) via lazy delta, and
	// shift ghosts in [to, from) by +1 in O(log G).
	toShift.applyDelta(1)
	ts.hasLazyShifts = true
	wr.ledger.shiftRange(to, from)

	// Reconcile rare boundary tie if restWithoutNode contains a promoted
	// ticket already sitting at ticket == from.
	afterShift := mergeWithBoundaryTie(toShift, restWithoutNode, from)

	// Re-insert node at to (strictly > beforeTo and strictly < afterShift).
	node.ticket = to
	ts.tree.root = ticketMerge(ticketMerge(ticketMerge(inWindow, beforeTo), node), afterShift)
	if ts.tree.root != nil {
		ts.tree.root.parent = nil
	}
	return true, node.clientKey, nil
}

// shiftRange adds one to every retired ticket in [lo, hi), the ghosts
// inside a rank move's range, so they keep their place relative to the
// tickets around them, in O(log G).
func (l *ghostLedger) shiftRange(lo, hi int64) {
	if l.size.Load() == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.tree.shiftRange(lo, hi)
	l.publishLocked()
}

package room

// SetTicketRank records rank for a queued client and, when higher than waiting
// tickets ahead of it, moves its ticket behind the last equal-or-higher ranked
// waiting ticket (in arrival order within each rank), rotating intervening
// tickets and pending ghosts back by one in O(log N) / O(log G). Lowering a
// ticket's rank records the new value without moving it back.
//
// Fires [EventRank] when the ticket moved. Returns [ErrNotInitialised],
// [ErrInvalidRank] (rank < 0), [ErrTokenNotFound], or [ErrAlreadyAdmitted]
// (in-window tickets still record their rank).
func (wr *WaitingRoom) SetTicketRank(token string, rank int) (bool, error) {
	if !wr.initialised.Load() {
		return false, ErrNotInitialised{}
	}
	if rank < 0 {
		return false, ErrInvalidRank{Given: rank}
	}

	wr.promoteMu.Lock()
	defer wr.promoteMu.Unlock()

	moved, key, err := wr.placeByRank(token, rank)
	if err != nil || !moved {
		return false, err
	}
	wr.emit(EventRank, wr.snapshotFor(EventRank, token, key, false))
	return true, nil
}

// placeByRank records rank and rotates [to, from) by +1 in O(log N).
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

	// Partition treap into inWindow (<= edge), candidates ((edge, from)),
	// and rest (>= from, containing node).
	inWindow, gtEdge := splitByTicket(ts.tree.root, edge, true)
	candidates, rest := splitByTicket(gtEdge, from, false)

	to := edge + 1
	if maxT, found := maxTicketWithRankAtLeast(candidates, rank); found && maxT+1 > to {
		to = maxT + 1
	}
	if to >= from {
		ts.tree.root = ticketMerge(ticketMerge(inWindow, candidates), rest)
		if ts.tree.root != nil {
			ts.tree.root.parent = nil
		}
		return false, node.clientKey, nil
	}

	beforeTo, toShift := splitByTicket(candidates, to, false)
	if toShift == nil {
		ts.tree.root = ticketMerge(ticketMerge(inWindow, beforeTo), rest)
		if ts.tree.root != nil {
			ts.tree.root.parent = nil
		}
		return false, node.clientKey, nil
	}

	restLeft, restMidRight := splitByKey(rest, from, node.createdAt, node.token, false)
	_, restRight := splitByKey(restMidRight, from, node.createdAt, node.token, true)
	restWithoutNode := ticketMerge(restLeft, restRight)
	node.detach()

	toShift.applyDelta(1)
	ts.hasLazyShifts = true
	wr.ledger.shiftRange(to, from)

	afterShift := mergeWithBoundaryTie(toShift, restWithoutNode, from)
	node.ticket = to
	ts.tree.root = ticketMerge(ticketMerge(ticketMerge(inWindow, beforeTo), node), afterShift)
	if ts.tree.root != nil {
		ts.tree.root.parent = nil
	}
	return true, node.clientKey, nil
}

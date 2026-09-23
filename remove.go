package room

// RemoveToken releases a queued ticket immediately, as if its client had
// abandoned it and the reaper had already run.
//
// Use it when the application bans or kicks a waiting visitor. Without
// it, the ticket lingers until TokenTTL plus a reaper pass, inflating
// QueueDepth, the positions shown to everyone behind it, RateFunc surge
// pricing and the SetMaxQueueDepth breaker.
//
// # Effect on the queue
//
// The ticket is retired exactly once (see WaitingRoom.retire):
//
//   - If it was waiting, everyone behind it moves up one position at once,
//     nobody ahead of it moves, and QueueDepth drops by one. The serving
//     window skips the vacated number when it gets there, so nobody is
//     admitted early.
//   - If it was already inside the serving window (told ready, not yet
//     reloaded), its slot passes to the next ticket immediately.
//
// # What the client sees
//
// The token no longer exists, so the client's next /queue/status poll
// returns ready=true and its page reloads into the main handler, where it
// is treated as a new arrival. Enforce bans upstream of the WaitingRoom
// if the visitor must not re-queue.
//
// # Errors
//
// Returns ErrTokenNotFound if the token is unknown, already expired and
// reaped, already removed, or already claimed by an admission in
// progress (a removal cannot revoke an admission). Callers applying bans
// can treat ErrTokenNotFound as benign. Returns ErrNotInitialised on a
// WaitingRoom that has not been initialised.
//
// Fires EventRemove on success, with Snapshot.Token and Snapshot.ClientKey
// set. Safe for concurrent use, including against the request hot path.
//
// Related: WaitingRoom.RemoveTokensFunc, EventRemove
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

// RemoveTokensFunc removes every queued ticket for which match returns
// true and reports how many were removed.
//
// It is the batch form of RemoveToken — for example, dropping every
// client behind a banned address range in one call, matching on
// TicketInfo.ClientKey — and has identical per-ticket semantics. All
// matches are retired in one ledger merge and EventRemove fires once per
// call (not once per ticket, and with Snapshot.Token empty), and only if
// at least one ticket was removed.
//
// # How it works
//
//  1. Snapshot the token store under a read lock.
//  2. Call match for each entry with NO locks held, in unspecified order.
//  3. Claim every matched token that still exists under one write lock.
//
// Because no locks are held while match runs, match may be slow and may
// call back into the WaitingRoom (including RemoveToken). Tokens removed
// or admitted between steps 1 and 3 are skipped and not counted. Position
// values are computed per entry during step 2 and may drift slightly if
// the queue moves meanwhile.
//
// Cost is O(n) in live queued tokens plus the snapshot allocation. It is
// intended for administrative actions, not for per-request use.
//
// A nil match, or an uninitialised WaitingRoom, removes nothing.
//
// Related: WaitingRoom.RemoveToken, TicketInfo, EventRemove
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

// tokenSnapshot pairs a token with a copy of its entry.
type tokenSnapshot struct {
	token string
	entry ticketEntry
}

// snapshotEntries copies every entry under a read lock so callers can
// inspect them without holding the lock.
func (ts *tokenStore) snapshotEntries() []tokenSnapshot {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	out := make([]tokenSnapshot, 0, len(ts.entries))
	for token, entry := range ts.entries {
		out = append(out, tokenSnapshot{token: token, entry: entry})
	}
	return out
}

// takeMany is the batch form of take: under one write lock it removes
// every listed token that still exists and returns the removed entries.
// The caller owns the returned tickets and must retire them.
func (ts *tokenStore) takeMany(tokens []string) []ticketEntry {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	out := make([]ticketEntry, 0, len(tokens))
	for _, token := range tokens {
		if entry, ok := ts.entries[token]; ok {
			delete(ts.entries, token)
			out = append(out, entry)
		}
	}
	return out
}

package room

import (
	"sync"
	"sync/atomic"
	"time"
)

// tokenSnapshot pairs a token with a point-in-time copy of its entry.
type tokenSnapshot struct {
	token string
	entry ticketEntry
}

// pollOutcome classifies a /queue/status poll for a token carried in a cookie.
type pollOutcome uint8

const (
	pollUnknown     pollOutcome = iota // admitted, removed, reaped, or unknown
	pollExpired                        // TTL elapsed; removed and owned by caller
	pollRateLimited                    // polled within minInterval; still kept alive
	pollAccepted                       // normal poll
)

// pollResult is the outcome of tokenStore.poll.
type pollResult struct {
	outcome       pollOutcome
	entry         ticketEntry
	refreshCookie bool
}

// viewOutcome classifies a view of GET /queue/status/{id}.
type viewOutcome uint8

const (
	viewUnknown viewOutcome = iota
	viewRateLimited
	viewAccepted
)

// keyedEntry is the outcome of tokenStore.enterKey.
type keyedEntry struct {
	token   string
	entry   ticketEntry
	place   int64
	created bool
}

// tokenStore maps random token strings to live queued tickets, indexed by
// key (byKey), public ID (byID), line order (tree), and expiry (ttlHeap,
// unseenHeap). Every structural container is guarded by mu and kept consistent
// through putLocked and removeLocked; per-token poll timestamps are guarded
// by each node's leaf mutex so status polls and views run concurrently under
// mu.RLock.
type tokenStore struct {
	mu            sync.RWMutex
	entries       map[string]*tokenNode
	byKey         map[string]string
	byID          map[string]string
	tree          ticketTree
	ttlHeap       nodeHeap
	unseenHeap    nodeHeap
	highWater     int
	hasLazyShifts bool
	ttlNanos      atomic.Int64
}

func newTokenStore() *tokenStore {
	ts := &tokenStore{
		entries:    make(map[string]*tokenNode),
		byKey:      make(map[string]string),
		byID:       make(map[string]string),
		unseenHeap: nodeHeap{unseen: true},
	}
	ts.ttlNanos.Store(int64(DefaultTokenTTL))
	return ts
}

func (ts *tokenStore) ttl() time.Duration {
	return time.Duration(ts.ttlNanos.Load())
}

func (ts *tokenStore) setTTL(d time.Duration) {
	ts.ttlNanos.Store(int64(d))
}

func (ts *tokenStore) len() int {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	return len(ts.entries)
}

// putLocked stores entry under token across maps, tree, and expiry heaps.
// An existing entry under token is removed first. Caller holds ts.mu.Lock.
func (ts *tokenStore) putLocked(token string, entry ticketEntry) *tokenNode {
	if old, ok := ts.entries[token]; ok {
		ts.removeLocked(token, old)
	}
	node := newTokenNode(token, entry)
	ts.entries[token] = node
	if node.key != "" {
		ts.byKey[node.key] = token
	}
	if node.id != "" {
		ts.byID[node.id] = token
	}
	ts.tree.insert(node)
	ts.ttlHeap.push(node)
	if !node.seen {
		ts.unseenHeap.push(node)
	}
	if len(ts.entries) > ts.highWater {
		ts.highWater = len(ts.entries)
	}
	return node
}

// removeLocked deletes token from maps, tree, and intrusive expiry heaps.
// Caller holds ts.mu.Lock.
func (ts *tokenStore) removeLocked(token string, node *tokenNode) {
	delete(ts.entries, token)
	if node.key != "" && ts.byKey[node.key] == token {
		delete(ts.byKey, node.key)
	}
	if node.id != "" && ts.byID[node.id] == token {
		delete(ts.byID, node.id)
	}
	ts.tree.delete(node)
	ts.ttlHeap.remove(node)
	ts.unseenHeap.remove(node)
}

func (ts *tokenStore) set(token string, entry ticketEntry) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.putLocked(token, entry)
}

func (ts *tokenStore) setMany(entries map[string]ticketEntry) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	for token, e := range entries {
		ts.putLocked(token, e)
	}
}

func (ts *tokenStore) get(token string) (ticketEntry, bool) {
	ts.mu.RLock()
	node, ok := ts.entries[token]
	if !ok {
		ts.mu.RUnlock()
		return ticketEntry{}, false
	}
	eff := effectiveTicket(node, ts.hasLazyShifts)
	e := node.snapshotEntry(eff)
	ts.mu.RUnlock()
	return e, true
}

// take atomically removes token and returns its entry so that exactly one
// caller (admission, removal, or eviction) accounts for the ticket.
func (ts *tokenStore) take(token string) (ticketEntry, bool) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	node, ok := ts.entries[token]
	if !ok {
		return ticketEntry{}, false
	}
	eff := effectiveTicket(node, ts.hasLazyShifts)
	e := node.snapshotEntry(eff)
	ts.removeLocked(token, node)
	return e, true
}

func (ts *tokenStore) takeMany(tokens []string) []ticketEntry {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	out := make([]ticketEntry, 0, len(tokens))
	for _, token := range tokens {
		if node, ok := ts.entries[token]; ok {
			eff := effectiveTicket(node, ts.hasLazyShifts)
			out = append(out, node.snapshotEntry(eff))
			ts.removeLocked(token, node)
		}
	}
	return out
}

// markRendered records a waiting-page reload: restarts the sliding TTL, marks
// seen, resets the cookie-refresh clock, and records current pass status.
func (ts *tokenStore) markRendered(token string, now time.Time, hasPass bool) {
	ts.mu.RLock()
	node, ok := ts.entries[token]
	if !ok {
		ts.mu.RUnlock()
		return
	}
	node.mu.Lock()
	node.issuedAt = now
	node.seen = true
	node.cookieSetAt = now
	node.hasPass = hasPass
	node.mu.Unlock()
	ts.mu.RUnlock()
}

// promoteTicket moves token to newTicket, marks it promoted, and re-positions
// it in the order-statistic tree in O(log N). Never inserts a missing token.
func (ts *tokenStore) promoteTicket(token string, newTicket int64) (ticketEntry, bool) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	node, ok := ts.entries[token]
	if !ok {
		return ticketEntry{}, false
	}
	ts.tree.delete(node)
	node.ticket = newTicket
	node.promoted = true
	ts.tree.insert(node)
	return node.snapshotEntry(newTicket), true
}

// poll applies one /queue/status poll under ts.mu.RLock + node.mu, upgrading
// to ts.mu.Lock only on the rare expired-token branch.
func (ts *tokenStore) poll(token string, now time.Time, edge int64, minInterval, refreshEvery time.Duration) pollResult {
	ttl := ts.ttl()

	ts.mu.RLock()
	node, ok := ts.entries[token]
	if !ok || node.key != "" {
		ts.mu.RUnlock()
		return pollResult{outcome: pollUnknown}
	}
	node.mu.Lock()
	if now.Sub(node.issuedAt) > ttl {
		node.mu.Unlock()
		ts.mu.RUnlock()
		return ts.pollExpiredSlow(token, now, edge, minInterval, refreshEvery, ttl)
	}
	eff := effectiveTicket(node, ts.hasLazyShifts)
	res := applyPollMuLocked(node, eff, now, edge, minInterval, refreshEvery)
	node.mu.Unlock()
	ts.mu.RUnlock()
	return res
}

func (ts *tokenStore) pollExpiredSlow(token string, now time.Time, edge int64, minInterval, refreshEvery, ttl time.Duration) pollResult {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	node, ok := ts.entries[token]
	if !ok || node.key != "" {
		return pollResult{outcome: pollUnknown}
	}
	node.mu.Lock()
	eff := effectiveTicket(node, ts.hasLazyShifts)
	if now.Sub(node.issuedAt) > ttl {
		entry := node.toEntryMuLocked(eff)
		node.mu.Unlock()
		ts.removeLocked(token, node)
		return pollResult{outcome: pollExpired, entry: entry}
	}
	res := applyPollMuLocked(node, eff, now, edge, minInterval, refreshEvery)
	node.mu.Unlock()
	return res
}

func applyPollMuLocked(node *tokenNode, effTicket int64, now time.Time, edge int64, minInterval, refreshEvery time.Duration) pollResult {
	prev := node.lastPoll
	node.lastPoll = now
	node.issuedAt = now
	node.seen = true

	res := pollResult{outcome: pollAccepted}
	if !prev.IsZero() && now.Sub(prev) < minInterval {
		res.outcome = pollRateLimited
	} else if effTicket > edge && now.Sub(node.cookieSetAt) >= refreshEvery {
		node.cookieSetAt = now
		res.refreshCookie = true
	}
	res.entry = node.toEntryMuLocked(effTicket)
	return res
}

// view applies one GET /queue/status/{id} view under ts.mu.RLock + node.mu
// without touching issuedAt or seen.
func (ts *tokenStore) view(id string, now time.Time, minInterval time.Duration) (viewOutcome, int64) {
	ts.mu.RLock()
	t, ok := ts.byID[id]
	if !ok {
		ts.mu.RUnlock()
		return viewUnknown, 0
	}
	node := ts.entries[t]
	node.mu.Lock()
	if !node.viewedAt.IsZero() && now.Sub(node.viewedAt) < minInterval {
		node.mu.Unlock()
		ts.mu.RUnlock()
		return viewRateLimited, 0
	}
	node.viewedAt = now
	node.mu.Unlock()
	place := ts.tree.place(node)
	ts.mu.RUnlock()
	return viewAccepted, place
}

// enterKey refreshes an existing key's TTL under ts.mu.RLock + node.mu and
// returns its O(log N) place, or inserts a new key under ts.mu.Lock when
// reserve is non-nil.
func (ts *tokenStore) enterKey(key, token string, fresh ticketEntry, reserve func() int64, now time.Time) keyedEntry {
	ts.mu.RLock()
	if t, ok := ts.byKey[key]; ok {
		node := ts.entries[t]
		eff := effectiveTicket(node, ts.hasLazyShifts)
		node.mu.Lock()
		node.issuedAt = now
		node.seen = true
		e := node.toEntryMuLocked(eff)
		node.mu.Unlock()
		place := ts.tree.place(node)
		ts.mu.RUnlock()
		return keyedEntry{token: t, entry: e, place: place}
	}
	ts.mu.RUnlock()

	if reserve == nil {
		return keyedEntry{}
	}

	ts.mu.Lock()
	defer ts.mu.Unlock()
	if t, ok := ts.byKey[key]; ok {
		node := ts.entries[t]
		eff := effectiveTicket(node, ts.hasLazyShifts)
		node.mu.Lock()
		node.issuedAt = now
		node.seen = true
		e := node.toEntryMuLocked(eff)
		node.mu.Unlock()
		return keyedEntry{token: t, entry: e, place: ts.tree.place(node)}
	}
	fresh.ticket = reserve()
	node := ts.putLocked(token, fresh)
	return keyedEntry{token: token, entry: fresh, place: ts.tree.place(node), created: true}
}

func (ts *tokenStore) lookupKey(key string) (string, ticketEntry, bool) {
	ts.mu.RLock()
	t, ok := ts.byKey[key]
	if !ok {
		ts.mu.RUnlock()
		return "", ticketEntry{}, false
	}
	node := ts.entries[t]
	eff := effectiveTicket(node, ts.hasLazyShifts)
	e := node.snapshotEntry(eff)
	ts.mu.RUnlock()
	return t, e, true
}

func (ts *tokenStore) takeKey(key string) (string, ticketEntry, bool) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	t, ok := ts.byKey[key]
	if !ok {
		return "", ticketEntry{}, false
	}
	node := ts.entries[t]
	eff := effectiveTicket(node, ts.hasLazyShifts)
	e := node.snapshotEntry(eff)
	ts.removeLocked(t, node)
	return t, e, true
}

func (ts *tokenStore) placeOfID(id string) (int64, bool) {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	t, ok := ts.byID[id]
	if !ok {
		return 0, false
	}
	return ts.tree.place(ts.entries[t]), true
}

func (ts *tokenStore) placeOfToken(token string) (int64, bool) {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	node, ok := ts.entries[token]
	if !ok {
		return 0, false
	}
	return ts.tree.place(node), true
}

func (ts *tokenStore) snapshotEntries() []tokenSnapshot {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	out := make([]tokenSnapshot, 0, len(ts.entries))
	for token, node := range ts.entries {
		eff := effectiveTicket(node, ts.hasLazyShifts)
		out = append(out, tokenSnapshot{token: token, entry: node.snapshotEntry(eff)})
	}
	return out
}

func (ts *tokenStore) snapshotSorted() []tokenSnapshot {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	out := make([]tokenSnapshot, 0, ts.tree.len())
	ts.tree.walk(-1, func(node *tokenNode, effTicket int64) {
		out = append(out, tokenSnapshot{
			token: node.token,
			entry: node.snapshotEntry(effTicket),
		})
	})
	return out
}

// compactIfNeeded reclaims Go map bucket arrays and heap slice backing arrays
// after a traffic spike drains below 25% of highWater (above
// tokenStoreCompactFloor), and resets hasLazyShifts once empty.
func (ts *tokenStore) compactIfNeeded() {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	cur := len(ts.entries)
	if cur == 0 {
		ts.hasLazyShifts = false
	}
	if ts.highWater < tokenStoreCompactFloor || cur*4 >= ts.highWater {
		return
	}

	entries := make(map[string]*tokenNode, cur)
	for k, v := range ts.entries {
		entries[k] = v
	}
	ts.entries = entries

	byKey := make(map[string]string, len(ts.byKey))
	for k, v := range ts.byKey {
		byKey[k] = v
	}
	ts.byKey = byKey

	byID := make(map[string]string, len(ts.byID))
	for k, v := range ts.byID {
		byID[k] = v
	}
	ts.byID = byID

	ts.ttlHeap.compact(tokenStoreCompactFloor)
	ts.unseenHeap.compact(tokenStoreCompactFloor)
	ts.highWater = cur
}

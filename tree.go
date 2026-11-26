package room

import (
	"math/rand/v2"
	"sync"
	"time"
)

// tokenNode is one live queued ticket inside tokenStore.
//
// Field ownership and lock hierarchy:
//   - Immutable fields (token, createdAt, clientKey, key, id, skipReadyGrace)
//     never change after insertion and are safe to read under ts.mu.RLock or
//     ts.mu.Lock.
//   - Tree fields (ticket, lazy, rank, maxRank, promoted, prio, size, parent,
//     left, right) and intrusive heap fields (ttlKey, unseenKey, ttlHeapIdx,
//     unseenHeapIdx) are mutated only under ts.mu.Lock. Read-only queries under
//     ts.mu.RLock never mutate pointers or push down lazy deltas.
//   - Poll-mutable fields (issuedAt, lastPoll, readyAt, cookieSetAt, viewedAt,
//     seen, hasPass) are guarded by mu, a leaf lock always acquired after ts.mu.
type tokenNode struct {
	// Immutable identity fields.
	token          string
	createdAt      time.Time
	clientKey      string
	key            string
	id             string
	skipReadyGrace bool

	// Tree ordering and metadata fields (guarded by ts.mu).
	// Invariant: ticket already includes lazy; lazy is the pending delta
	// owed to left and right subtrees only.
	ticket   int64
	lazy     int64
	rank     int
	maxRank  int
	promoted bool
	prio     uint64
	size     int
	parent   *tokenNode
	left     *tokenNode
	right    *tokenNode

	// Intrusive expiry-heap fields (guarded by ts.mu.Lock).
	ttlKey        time.Time
	unseenKey     time.Time
	ttlHeapIdx    int // -1 when absent
	unseenHeapIdx int // -1 when absent

	// Poll-mutable fields (guarded by mu; leaf lock acquired after ts.mu).
	mu          sync.Mutex
	issuedAt    time.Time
	lastPoll    time.Time
	readyAt     time.Time
	cookieSetAt time.Time
	viewedAt    time.Time
	seen        bool
	hasPass     bool
}

func newTokenNode(token string, e ticketEntry) *tokenNode {
	return &tokenNode{
		token:          token,
		createdAt:      e.createdAt,
		clientKey:      e.clientKey,
		key:            e.key,
		id:             e.id,
		skipReadyGrace: e.skipReadyGrace,
		ticket:         e.ticket,
		rank:           e.rank,
		maxRank:        e.rank,
		promoted:       e.promoted,
		prio:           rand.Uint64(),
		size:           1,
		ttlKey:         e.issuedAt,
		unseenKey:      e.issuedAt,
		ttlHeapIdx:     -1,
		unseenHeapIdx:  -1,
		issuedAt:       e.issuedAt,
		lastPoll:       e.lastPoll,
		readyAt:        e.readyAt,
		cookieSetAt:    e.cookieSetAt,
		viewedAt:       e.viewedAt,
		seen:           e.seen,
		hasPass:        e.hasPass,
	}
}

// toEntryMuLocked snapshots this node into a ticketEntry using the resolved
// effectiveTicket. Caller holds ts.mu (RLock or Lock) and n.mu.
func (n *tokenNode) toEntryMuLocked(effectiveTicket int64) ticketEntry {
	return ticketEntry{
		ticket:         effectiveTicket,
		createdAt:      n.createdAt,
		issuedAt:       n.issuedAt,
		lastPoll:       n.lastPoll,
		readyAt:        n.readyAt,
		cookieSetAt:    n.cookieSetAt,
		clientKey:      n.clientKey,
		seen:           n.seen,
		promoted:       n.promoted,
		hasPass:        n.hasPass,
		skipReadyGrace: n.skipReadyGrace,
		rank:           n.rank,
		key:            n.key,
		id:             n.id,
		viewedAt:       n.viewedAt,
	}
}

// snapshotEntry acquires n.mu and returns a value copy of this node.
// Caller holds ts.mu (RLock or Lock).
func (n *tokenNode) snapshotEntry(effectiveTicket int64) ticketEntry {
	n.mu.Lock()
	e := n.toEntryMuLocked(effectiveTicket)
	n.mu.Unlock()
	return e
}

func (n *tokenNode) applyDelta(d int64) {
	if n == nil || d == 0 {
		return
	}
	n.ticket += d
	n.lazy += d
}

func (n *tokenNode) pushDown() {
	if n == nil || n.lazy == 0 {
		return
	}
	d := n.lazy
	n.lazy = 0
	if n.left != nil {
		n.left.applyDelta(d)
	}
	if n.right != nil {
		n.right.applyDelta(d)
	}
}

func (n *tokenNode) pullUp() {
	s := 1
	mr := n.rank
	if n.left != nil {
		n.left.parent = n
		s += n.left.size
		if n.left.maxRank > mr {
			mr = n.left.maxRank
		}
	}
	if n.right != nil {
		n.right.parent = n
		s += n.right.size
		if n.right.maxRank > mr {
			mr = n.right.maxRank
		}
	}
	n.size = s
	n.maxRank = mr
}

func (n *tokenNode) detach() {
	n.parent = nil
	n.left = nil
	n.right = nil
	n.lazy = 0
	n.size = 1
	n.maxRank = n.rank
}

// lessTicketKey defines the total line order: ticket number ascending, then
// issue time ascending, then token ascending.
func lessTicketKey(tA int64, cA time.Time, tokA string, tB int64, cB time.Time, tokB string) bool {
	if tA != tB {
		return tA < tB
	}
	if !cA.Equal(cB) {
		return cA.Before(cB)
	}
	return tokA < tokB
}

// ticketTree is a randomized order-statistic treap of *tokenNode ordered by
// (ticket, createdAt, token), augmented with subtree size, subtree maxRank,
// parent pointers, and lazy ticket range-add.
type ticketTree struct {
	root *tokenNode
}

func (t *ticketTree) len() int {
	if t.root == nil {
		return 0
	}
	return t.root.size
}

// effectiveTicket returns n's true ticket number without mutating the tree.
// Safe under ts.mu.RLock; O(1) when hasLazy is false, O(log N) otherwise.
func effectiveTicket(n *tokenNode, hasLazy bool) int64 {
	v := n.ticket
	if !hasLazy {
		return v
	}
	for p := n.parent; p != nil; p = p.parent {
		v += p.lazy
	}
	return v
}

// place returns n's 1-based place in line order without mutating the tree.
// Safe under ts.mu.RLock; O(log N).
func (t *ticketTree) place(n *tokenNode) int64 {
	if n == nil {
		return 0
	}
	var rank int64 = 1
	if n.left != nil {
		rank += int64(n.left.size)
	}
	for cur := n; cur.parent != nil; cur = cur.parent {
		p := cur.parent
		if cur == p.right {
			rank++
			if p.left != nil {
				rank += int64(p.left.size)
			}
		}
	}
	return rank
}

// setRank updates n.rank and propagates maxRank up to the root in O(log N).
func (t *ticketTree) setRank(n *tokenNode, rank int) {
	n.rank = rank
	for cur := n; cur != nil; cur = cur.parent {
		cur.pullUp()
	}
}

// insert places a detached node into the treap in O(log N).
func (t *ticketTree) insert(n *tokenNode) {
	n.detach()
	t.root = ticketInsertNode(t.root, n)
	if t.root != nil {
		t.root.parent = nil
	}
}

func ticketInsertNode(root, n *tokenNode) *tokenNode {
	left, right := splitByKey(root, n.ticket, n.createdAt, n.token, false)
	return ticketMerge(ticketMerge(left, n), right)
}

// delete removes n from the treap in O(log N) and detaches it with its
// effective ticket materialized in n.ticket.
func (t *ticketTree) delete(n *tokenNode) {
	eff := effectiveTicket(n, true)
	left, midRight := splitByKey(t.root, eff, n.createdAt, n.token, false)
	_, right := splitByKey(midRight, eff, n.createdAt, n.token, true)
	t.root = ticketMerge(left, right)
	if t.root != nil {
		t.root.parent = nil
	}
	n.ticket = eff
	n.detach()
}

func splitByTicket(n *tokenNode, limit int64, inclusive bool) (l, r *tokenNode) {
	return splitTree(n, limit, time.Time{}, "", false, inclusive)
}

func splitByKey(n *tokenNode, ticket int64, createdAt time.Time, token string, inclusive bool) (l, r *tokenNode) {
	return splitTree(n, ticket, createdAt, token, true, inclusive)
}

func splitTree(n *tokenNode, ticket int64, createdAt time.Time, token string, fullKey, inclusive bool) (l, r *tokenNode) {
	if n == nil {
		return nil, nil
	}
	n.pushDown()
	var goLeft bool
	if fullKey {
		if inclusive {
			goLeft = !lessTicketKey(ticket, createdAt, token, n.ticket, n.createdAt, n.token)
		} else {
			goLeft = lessTicketKey(n.ticket, n.createdAt, n.token, ticket, createdAt, token)
		}
	} else if inclusive {
		goLeft = n.ticket <= ticket
	} else {
		goLeft = n.ticket < ticket
	}
	if goLeft {
		l, r = splitTree(n.right, ticket, createdAt, token, fullKey, inclusive)
		n.right = l
		n.pullUp()
		if r != nil {
			r.parent = nil
		}
		n.parent = nil
		return n, r
	}
	l, r = splitTree(n.left, ticket, createdAt, token, fullKey, inclusive)
	n.left = r
	n.pullUp()
	if l != nil {
		l.parent = nil
	}
	n.parent = nil
	return l, n
}

func ticketMerge(a, b *tokenNode) *tokenNode {
	if a == nil {
		if b != nil {
			b.parent = nil
		}
		return b
	}
	if b == nil {
		a.parent = nil
		return a
	}
	if a.prio > b.prio {
		a.pushDown()
		a.right = ticketMerge(a.right, b)
		a.pullUp()
		a.parent = nil
		return a
	}
	b.pushDown()
	b.left = ticketMerge(a, b.left)
	b.pullUp()
	b.parent = nil
	return b
}

// maxTicketWithRankAtLeast returns the maximum ticket in subtree n whose node
// has rank >= minRank, in O(log N).
func maxTicketWithRankAtLeast(n *tokenNode, minRank int) (int64, bool) {
	if n == nil || n.maxRank < minRank {
		return 0, false
	}
	for cur := n; cur != nil; {
		cur.pushDown()
		if cur.right != nil && cur.right.maxRank >= minRank {
			cur = cur.right
		} else if cur.rank >= minRank {
			return cur.ticket, true
		} else {
			cur = cur.left
		}
	}
	return 0, false
}

// mergeWithBoundaryTie merges toShift (tickets <= boundary) and rest
// (tickets >= boundary). When rest contains promoted tie(s) at ticket ==
// boundary, the two tie groups are merged by full (ticket, createdAt, token)
// order so BST tie-breaker invariants remain exact.
func mergeWithBoundaryTie(toShift, rest *tokenNode, boundary int64) *tokenNode {
	if toShift == nil {
		return rest
	}
	if rest == nil {
		return toShift
	}
	cur := rest
	for cur.left != nil {
		cur.pushDown()
		cur = cur.left
	}
	cur.pushDown()
	if cur.ticket > boundary {
		return ticketMerge(toShift, rest)
	}
	shiftBelow, shiftAt := splitByTicket(toShift, boundary, false)
	if shiftAt == nil {
		return ticketMerge(shiftBelow, rest)
	}
	restAt, restAbove := splitByTicket(rest, boundary, true)
	if shiftAt.size > restAt.size {
		shiftAt, restAt = restAt, shiftAt
	}
	nodes := make([]*tokenNode, 0, shiftAt.size)
	collectDetached(shiftAt, &nodes)
	mergedAt := restAt
	for _, n := range nodes {
		mergedAt = ticketInsertNode(mergedAt, n)
	}
	return ticketMerge(ticketMerge(shiftBelow, mergedAt), restAbove)
}

func collectDetached(n *tokenNode, out *[]*tokenNode) {
	if n == nil {
		return
	}
	n.pushDown()
	l, r := n.left, n.right
	n.detach()
	collectDetached(l, out)
	*out = append(*out, n)
	collectDetached(r, out)
}

// walk visits up to limit nodes in line order (limit < 0 visits all), passing
// each node's effective ticket. Read-only; safe under ts.mu.RLock.
func (t *ticketTree) walk(limit int, visit func(n *tokenNode, effTicket int64)) {
	if limit == 0 || t.root == nil {
		return
	}
	count := 0
	walkNodes(t.root, 0, limit, &count, visit)
}

func walkNodes(n *tokenNode, accLazy int64, limit int, count *int, visit func(*tokenNode, int64)) {
	if n == nil || (limit > 0 && *count >= limit) {
		return
	}
	eff := n.ticket + accLazy
	childLazy := accLazy + n.lazy
	walkNodes(n.left, childLazy, limit, count, visit)
	if limit > 0 && *count >= limit {
		return
	}
	visit(n, eff)
	*count++
	if limit > 0 && *count >= limit {
		return
	}
	walkNodes(n.right, childLazy, limit, count, visit)
}

// walkWhile visits nodes in line order, passing each node's effective ticket,
// and stops immediately as soon as visit returns false. Read-only; safe under
// ts.mu.RLock or ts.mu.Lock.
func (t *ticketTree) walkWhile(visit func(n *tokenNode, effTicket int64) bool) {
	if t.root == nil {
		return
	}
	walkNodesWhile(t.root, 0, visit)
}

func walkNodesWhile(n *tokenNode, accLazy int64, visit func(*tokenNode, int64) bool) bool {
	if n == nil {
		return true
	}
	eff := n.ticket + accLazy
	childLazy := accLazy + n.lazy
	if !walkNodesWhile(n.left, childLazy, visit) {
		return false
	}
	if !visit(n, eff) {
		return false
	}
	return walkNodesWhile(n.right, childLazy, visit)
}

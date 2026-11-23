package room

import (
	"crypto/rand"
	"encoding/binary"
	"math"
	"sync"
	"time"
)

// splitMix64 is a lightweight, lock-free per-tree PRNG used solely for
// randomized treap heap priorities so that monotonic ticket arrivals
// (1, 2, 3, …) always produce O(log N) expected tree height.
type splitMix64 uint64

func newSplitMix64() splitMix64 {
	var b [8]byte
	if _, err := rand.Read(b[:]); err == nil {
		return splitMix64(binary.LittleEndian.Uint64(b[:]))
	}
	return splitMix64(time.Now().UnixNano())
}

func (s *splitMix64) next() uint64 {
	*s += 0x9e3779b97f4a7c15
	z := uint64(*s)
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

// ── tokenNode & ticketTree ───────────────────────────────────────────────────

// tokenNode is one live queued ticket inside tokenStore.
//
// Field ownership and lock hierarchy:
//
//   - Immutable fields (token, createdAt, clientKey, key, id): never change
//     after insertion; safe to read under ts.mu.RLock or ts.mu.Lock.
//   - Tree fields (ticket, lazy, rank, maxRank, promoted, prio, size, parent,
//     left, right) and intrusive heap fields (ttlKey, unseenKey, ttlHeapIdx,
//     unseenHeapIdx): mutated ONLY under ts.mu.Lock. Read-only queries under
//     ts.mu.RLock (effectiveTicket, place, firstN, walkInOrder) never mutate
//     tree pointers or push down lazy deltas.
//   - Poll-mutable fields (issuedAt, lastPoll, cookieSetAt, viewedAt, seen,
//     hasPass): guarded by mu, which is ALWAYS acquired AFTER ts.mu and is a
//     leaf lock (never acquires any other lock).
type tokenNode struct {
	// Immutable identity fields.
	token     string
	createdAt time.Time
	clientKey string
	key       string
	id        string

	// Tree-managed ordering & metadata fields (guarded by ts.mu).
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
	ttlHeapIdx    int // -1 when not in ttlHeap
	unseenHeapIdx int // -1 when not in unseenHeap

	// Poll-mutable fields (guarded by mu; always acquired after ts.mu).
	mu          sync.Mutex
	issuedAt    time.Time
	lastPoll    time.Time
	cookieSetAt time.Time
	viewedAt    time.Time
	seen        bool
	hasPass     bool
}

func newTokenNode(token string, e ticketEntry, prio uint64) *tokenNode {
	return &tokenNode{
		token:         token,
		createdAt:     e.createdAt,
		clientKey:     e.clientKey,
		key:           e.key,
		id:            e.id,
		ticket:        e.ticket,
		rank:          e.rank,
		maxRank:       e.rank,
		promoted:      e.promoted,
		prio:          prio,
		size:          1,
		ttlKey:        e.issuedAt,
		unseenKey:     e.issuedAt,
		ttlHeapIdx:    -1,
		unseenHeapIdx: -1,
		issuedAt:      e.issuedAt,
		lastPoll:      e.lastPoll,
		cookieSetAt:   e.cookieSetAt,
		viewedAt:      e.viewedAt,
		seen:          e.seen,
		hasPass:       e.hasPass,
	}
}

// toEntryMuLocked snapshots this node into a ticketEntry using the
// already-resolved effectiveTicket. Caller holds ts.mu (RLock or Lock)
// AND n.mu.
func (n *tokenNode) toEntryMuLocked(effectiveTicket int64) ticketEntry {
	return ticketEntry{
		ticket:      effectiveTicket,
		createdAt:   n.createdAt,
		issuedAt:    n.issuedAt,
		lastPoll:    n.lastPoll,
		cookieSetAt: n.cookieSetAt,
		clientKey:   n.clientKey,
		seen:        n.seen,
		promoted:    n.promoted,
		hasPass:     n.hasPass,
		rank:        n.rank,
		key:         n.key,
		id:          n.id,
		viewedAt:    n.viewedAt,
	}
}

// snapshotEntry acquires n.mu and returns a ticketEntry value copy.
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

// lessTicketKey defines the exact line order matching lessQueued:
// ticket number ascending, then createdAt ascending, then token ascending.
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
	rng  splitMix64
}

func newTicketTree() ticketTree {
	return ticketTree{rng: newSplitMix64()}
}

func (t *ticketTree) nextPrio() uint64 {
	return t.rng.next()
}

func (t *ticketTree) len() int {
	if t.root == nil {
		return 0
	}
	return t.root.size
}

// effectiveTicket returns n's true ticket number without mutating the tree.
// Safe under ts.mu.RLock. When hasLazy is false, runs in O(1).
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
// Safe under ts.mu.RLock; runs in O(log N).
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
// Caller holds ts.mu.Lock.
func (t *ticketTree) setRank(n *tokenNode, rank int) {
	n.rank = rank
	for cur := n; cur != nil; cur = cur.parent {
		cur.pullUp()
	}
}

// insert places a detached node into the treap in O(log N).
// Caller holds ts.mu.Lock.
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
// exact effective ticket materialized in n.ticket (n.lazy == 0).
// Caller holds ts.mu.Lock.
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

// splitByKey splits root by full key (ticket, createdAt, token).
// If inclusive is true, left contains keys <= target; otherwise keys < target.
func splitByKey(n *tokenNode, ticket int64, createdAt time.Time, token string, inclusive bool) (l, r *tokenNode) {
	if n == nil {
		return nil, nil
	}
	n.pushDown()
	var goLeft bool
	if inclusive {
		goLeft = !lessTicketKey(ticket, createdAt, token, n.ticket, n.createdAt, n.token)
	} else {
		goLeft = lessTicketKey(n.ticket, n.createdAt, n.token, ticket, createdAt, token)
	}
	if goLeft {
		l, r = splitByKey(n.right, ticket, createdAt, token, inclusive)
		n.right = l
		n.pullUp()
		if r != nil {
			r.parent = nil
		}
		n.parent = nil
		return n, r
	}
	l, r = splitByKey(n.left, ticket, createdAt, token, inclusive)
	n.left = r
	n.pullUp()
	if l != nil {
		l.parent = nil
	}
	n.parent = nil
	return l, n
}

// splitByTicket splits root by ticket number alone.
// If inclusive is true, left contains ticket <= limit; otherwise ticket < limit.
func splitByTicket(n *tokenNode, limit int64, inclusive bool) (l, r *tokenNode) {
	if n == nil {
		return nil, nil
	}
	n.pushDown()
	var goLeft bool
	if inclusive {
		goLeft = n.ticket <= limit
	} else {
		goLeft = n.ticket < limit
	}
	if goLeft {
		l, r = splitByTicket(n.right, limit, inclusive)
		n.right = l
		n.pullUp()
		if r != nil {
			r.parent = nil
		}
		n.parent = nil
		return n, r
	}
	l, r = splitByTicket(n.left, limit, inclusive)
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

// leftmostTicket returns the minimum effective ticket in subtree n (pushing
// lazy deltas along the left spine). Caller holds ts.mu.Lock.
func leftmostTicket(n *tokenNode) (int64, bool) {
	if n == nil {
		return 0, false
	}
	cur := n
	for cur.left != nil {
		cur.pushDown()
		cur = cur.left
	}
	cur.pushDown()
	return cur.ticket, true
}

// maxTicketWithRankAtLeast returns the maximum ticket in subtree n whose
// node has rank >= minRank, in O(log N). Caller holds ts.mu.Lock.
func maxTicketWithRankAtLeast(n *tokenNode, minRank int) (int64, bool) {
	if n == nil || n.maxRank < minRank {
		return 0, false
	}
	cur := n
	for cur != nil {
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
// (tickets >= boundary). When rest has promoted tie(s) sitting at ticket ==
// boundary, the two tie groups at boundary are merged by full
// (ticket, createdAt, token) key so secondary tie-breaker order stays exact.
func mergeWithBoundaryTie(toShift, rest *tokenNode, boundary int64) *tokenNode {
	if toShift == nil {
		return rest
	}
	if rest == nil {
		return toShift
	}
	minRest, ok := leftmostTicket(rest)
	if !ok || minRest > boundary {
		return ticketMerge(toShift, rest)
	}
	shiftBelow, shiftAt := splitByTicket(toShift, boundary, false)
	if shiftAt == nil {
		return ticketMerge(shiftBelow, rest)
	}
	restAt, restAbove := splitByTicket(rest, boundary, true)
	mergedAt := mergeTieSubtrees(shiftAt, restAt)
	return ticketMerge(ticketMerge(shiftBelow, mergedAt), restAbove)
}

// mergeTieSubtrees inserts every node from the smaller subtree into the
// larger subtree using full key ordering. Both subtrees share one ticket
// number (almost always 1–2 nodes).
func mergeTieSubtrees(a, b *tokenNode) *tokenNode {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	if a.size > b.size {
		a, b = b, a
	}
	// Collect detached nodes from the smaller subtree a, then insert into b.
	nodes := make([]*tokenNode, 0, a.size)
	collectDetached(a, &nodes)
	res := b
	for _, n := range nodes {
		res = ticketInsertNode(res, n)
	}
	return res
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

// firstN invokes visit on the first limit nodes in line order, passing each
// node's effective ticket. Read-only; safe under ts.mu.RLock; O(log N + limit).
func (t *ticketTree) firstN(limit int, visit func(n *tokenNode, effTicket int64)) {
	if limit <= 0 || t.root == nil {
		return
	}
	count := 0
	walkPrefix(t.root, 0, limit, &count, visit)
}

func walkPrefix(n *tokenNode, accLazy int64, limit int, count *int, visit func(*tokenNode, int64)) {
	if n == nil || *count >= limit {
		return
	}
	eff := n.ticket + accLazy
	childLazy := accLazy + n.lazy
	walkPrefix(n.left, childLazy, limit, count, visit)
	if *count >= limit {
		return
	}
	visit(n, eff)
	*count++
	if *count >= limit {
		return
	}
	walkPrefix(n.right, childLazy, limit, count, visit)
}

// walkInOrder invokes visit on every node in line order. Read-only; safe under
// ts.mu.RLock; O(N).
func (t *ticketTree) walkInOrder(visit func(n *tokenNode, effTicket int64)) {
	if t.root == nil {
		return
	}
	walkAll(t.root, 0, visit)
}

func walkAll(n *tokenNode, accLazy int64, visit func(*tokenNode, int64)) {
	if n == nil {
		return
	}
	eff := n.ticket + accLazy
	childLazy := accLazy + n.lazy
	walkAll(n.left, childLazy, visit)
	visit(n, eff)
	walkAll(n.right, childLazy, visit)
}

// ── ghostNode & ghostTree ────────────────────────────────────────────────────

// ghostNode is one retired ticket number inside ghostLedger. Duplicates are
// permitted (matching ghostLedger's contract).
type ghostNode struct {
	ticket int64
	lazy   int64
	prio   uint64
	size   int64
	left   *ghostNode
	right  *ghostNode
}

func (n *ghostNode) applyDelta(d int64) {
	if n == nil || d == 0 {
		return
	}
	n.ticket += d
	n.lazy += d
}

func (n *ghostNode) pushDown() {
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

func (n *ghostNode) pullUp() {
	s := int64(1)
	if n.left != nil {
		s += n.left.size
	}
	if n.right != nil {
		s += n.right.size
	}
	n.size = s
}

func ghostSize(n *ghostNode) int64 {
	if n == nil {
		return 0
	}
	return n.size
}

// ghostTree is a randomized order-statistic treap for ghostLedger with O(log G)
// insert, consumeUpTo, countBelow, and shiftRange.
type ghostTree struct {
	root *ghostNode
	rng  splitMix64
}

func newGhostTree() ghostTree {
	return ghostTree{rng: newSplitMix64()}
}

func (g *ghostTree) add(ticket int64) {
	node := &ghostNode{ticket: ticket, prio: g.rng.next(), size: 1}
	left, right := ghostSplit(g.root, ticket, true)
	g.root = ghostMerge(ghostMerge(left, node), right)
}

func (g *ghostTree) consumeUpTo(limit int64) int64 {
	consumed, rest := ghostSplit(g.root, limit, true)
	if consumed == nil {
		return 0
	}
	g.root = rest
	return consumed.size
}

func (g *ghostTree) countBelow(ticket int64) int64 {
	var count int64
	var accLazy int64
	for cur := g.root; cur != nil; {
		eff := cur.ticket + accLazy
		childLazy := accLazy + cur.lazy
		if eff < ticket {
			count += ghostSize(cur.left) + 1
			cur = cur.right
		} else {
			cur = cur.left
		}
		accLazy = childLazy
	}
	return count
}

func (g *ghostTree) shiftRange(lo, hi int64) {
	if g.root == nil || lo >= hi {
		return
	}
	a, bc := ghostSplit(g.root, lo, false)
	b, c := ghostSplit(bc, hi, false)
	if b != nil {
		b.applyDelta(1)
	}
	g.root = ghostMerge(ghostMerge(a, b), c)
}

func (g *ghostTree) minTicket() int64 {
	if g.root == nil {
		return math.MaxInt64
	}
	var accLazy int64
	cur := g.root
	for cur.left != nil {
		accLazy += cur.lazy
		cur = cur.left
	}
	return cur.ticket + accLazy
}

func (g *ghostTree) snapshot() []int64 {
	n := ghostSize(g.root)
	if n == 0 {
		return nil
	}
	out := make([]int64, 0, n)
	var walk func(node *ghostNode, accLazy int64)
	walk = func(node *ghostNode, accLazy int64) {
		if node == nil {
			return
		}
		eff := node.ticket + accLazy
		childLazy := accLazy + node.lazy
		walk(node.left, childLazy)
		out = append(out, eff)
		walk(node.right, childLazy)
	}
	walk(g.root, 0)
	return out
}

func ghostSplit(n *ghostNode, limit int64, inclusive bool) (l, r *ghostNode) {
	if n == nil {
		return nil, nil
	}
	n.pushDown()
	var goLeft bool
	if inclusive {
		goLeft = n.ticket <= limit
	} else {
		goLeft = n.ticket < limit
	}
	if goLeft {
		l, r = ghostSplit(n.right, limit, inclusive)
		n.right = l
		n.pullUp()
		return n, r
	}
	l, r = ghostSplit(n.left, limit, inclusive)
	n.left = r
	n.pullUp()
	return l, n
}

func ghostMerge(a, b *ghostNode) *ghostNode {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	if a.prio > b.prio {
		a.pushDown()
		a.right = ghostMerge(a.right, b)
		a.pullUp()
		return a
	}
	b.pushDown()
	b.left = ghostMerge(a, b.left)
	b.pullUp()
	return b
}

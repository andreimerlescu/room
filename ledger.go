package room

import (
	"math"
	"math/rand/v2"
	"sync"
	"sync/atomic"
)

// ghostLedger records retired ticket numbers (abandoned, expired, removed,
// timed out) that the serving window has not yet reached. Pending entries are
// subtracted from QueueDepth and positionOf immediately and consumed one
// nowServing advance each once the serving window reaches them.
//
// Backed by a randomized order-statistic treap guarded by mu so add,
// consumeUpTo, countBelow, and shiftRange run in O(log G), and consumed
// subtrees become eligible for GC immediately. lowest and size are mirrored
// atomically so the release hot path costs one atomic load when empty.
type ghostLedger struct {
	mu     sync.Mutex
	root   *ghostNode
	lowest atomic.Int64 // minimum pending ticket, or math.MaxInt64 when empty
	size   atomic.Int64 // pending ghost count
}

type ghostNode struct {
	ticket int64
	lazy   int64
	prio   uint64
	size   int64
	left   *ghostNode
	right  *ghostNode
}

func newGhostLedger() *ghostLedger {
	l := &ghostLedger{}
	l.lowest.Store(math.MaxInt64)
	return l
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

func ghostSplit(n *ghostNode, limit int64, inclusive bool) (l, r *ghostNode) {
	if n == nil {
		return nil, nil
	}
	n.pushDown()
	goLeft := n.ticket < limit
	if inclusive {
		goLeft = n.ticket <= limit
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

func (l *ghostLedger) publishLocked() {
	l.size.Store(ghostSize(l.root))
	if l.root == nil {
		l.lowest.Store(math.MaxInt64)
		return
	}
	var acc int64
	cur := l.root
	for cur.left != nil {
		acc += cur.lazy
		cur = cur.left
	}
	l.lowest.Store(cur.ticket + acc)
}

func (l *ghostLedger) addLocked(ticket int64) {
	node := &ghostNode{ticket: ticket, prio: rand.Uint64(), size: 1}
	left, right := ghostSplit(l.root, ticket, true)
	l.root = ghostMerge(ghostMerge(left, node), right)
}

func (l *ghostLedger) add(ticket int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.addLocked(ticket)
	l.publishLocked()
}

func (l *ghostLedger) addMany(tickets []int64) {
	if len(tickets) == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, t := range tickets {
		l.addLocked(t)
	}
	l.publishLocked()
}

// consumeUpTo removes every entry <= limit in O(log G) and returns the count.
func (l *ghostLedger) consumeUpTo(limit int64) int64 {
	if l.lowest.Load() > limit {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	consumed, rest := ghostSplit(l.root, limit, true)
	if consumed == nil {
		return 0
	}
	l.root = rest
	l.publishLocked()
	return consumed.size
}

// countBelow returns the number of entries strictly less than ticket in O(log G).
func (l *ghostLedger) countBelow(ticket int64) int64 {
	if l.size.Load() == 0 || l.lowest.Load() >= ticket {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	var count, acc int64
	for cur := l.root; cur != nil; {
		eff := cur.ticket + acc
		childAcc := acc + cur.lazy
		if eff < ticket {
			count += ghostSize(cur.left) + 1
			cur = cur.right
		} else {
			cur = cur.left
		}
		acc = childAcc
	}
	return count
}

// shiftRange increments every pending ghost in [lo, hi) by 1 in O(log G).
func (l *ghostLedger) shiftRange(lo, hi int64) {
	if l.size.Load() == 0 || lo >= hi {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	a, bc := ghostSplit(l.root, lo, false)
	b, c := ghostSplit(bc, hi, false)
	if b != nil {
		b.applyDelta(1)
	}
	l.root = ghostMerge(ghostMerge(a, b), c)
	l.publishLocked()
}

func (l *ghostLedger) len() int64 {
	return l.size.Load()
}

func (l *ghostLedger) minTicket() int64 {
	return l.lowest.Load()
}

// advance moves nowServing forward by n and consumes any ghosts the widened
// serving window now reaches.
func (wr *WaitingRoom) advance(n int64) {
	wr.nowServing.Add(n)
	wr.drainLedger()
}

// drainLedger consumes every ghost at or below nowServing+cap, advancing
// nowServing once per ghost until none remain below the edge.
func (wr *WaitingRoom) drainLedger() {
	for {
		edge := wr.nowServing.Load() + int64(wr.cap.Load())
		k := wr.ledger.consumeUpTo(edge)
		if k == 0 {
			return
		}
		wr.nowServing.Add(k)
	}
}

// retire accounts for a single ticket number that will never be admitted.
// The caller must own the ticket exclusively.
func (wr *WaitingRoom) retire(ticket int64) {
	wr.ledger.add(ticket)
	wr.drainLedger()
}

// retireMany is the batch form of retire used by the reaper and bulk removals.
func (wr *WaitingRoom) retireMany(tickets []int64) {
	if len(tickets) == 0 {
		return
	}
	wr.ledger.addMany(tickets)
	wr.drainLedger()
}

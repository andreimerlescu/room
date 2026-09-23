package room

import (
	"math"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
)

// ledgerShrinkFloor is the slice capacity above which the ghost ledger
// reallocates after a bulk consume that leaves it mostly empty. It keeps
// a one-off burst of retirements (a mass ban, a reap after a flood) from
// pinning a large backing array for the rest of the process lifetime.
const ledgerShrinkFloor = 1024

// ghostLedger records ticket numbers that will never be admitted but that
// the serving window has not yet reached.
//
// # Why this exists
//
// Admission is ticket <= nowServing + cap, and nowServing advances once
// per release. The admission budget is therefore (releases + cap). For
// that budget to stay equal to real capacity, every ticket number that
// will never be admitted — abandoned, expired, removed, timed out — must
// contribute exactly one extra advance. A number that contributes none
// sits inside the window forever as a "ghost", permanently removing one
// usable slot; with cap=1 a single such ghost freezes the queue.
//
// Contributing the advance immediately for a ghost that is still far back
// in the queue would make a head-of-queue client eligible early. That is
// harmless for one ghost, but a batch ban of a thousand tickets would make
// a thousand clients eligible at once and send them all into a blocking
// semaphore acquire. The ledger defers the advance instead: the ghost is
// recorded here and consumed (one advance each) at the moment the window
// actually reaches it. In-window ghosts are consumed immediately.
//
// Pending ledger entries are also subtracted from QueueDepth and from
// positionOf, so positions behind a retired ticket improve at once while
// positions ahead of it are unchanged.
//
// # Concurrency
//
// tickets is guarded by mu. lowest and size are published under mu and
// read lock-free, so the release hot path — which must check whether any
// ghost is now reachable — costs one atomic load when nothing is pending.
//
// Related: WaitingRoom.retire, WaitingRoom.drainLedger, WaitingRoom.advance
type ghostLedger struct {
	mu      sync.Mutex
	tickets []int64 // sorted ascending; duplicates permitted

	lowest atomic.Int64 // tickets[0], or math.MaxInt64 when empty
	size   atomic.Int64 // len(tickets)
}

func newGhostLedger() *ghostLedger {
	l := &ghostLedger{}
	l.lowest.Store(math.MaxInt64)
	return l
}

// publishLocked refreshes the lock-free mirrors. Caller must hold l.mu.
func (l *ghostLedger) publishLocked() {
	l.size.Store(int64(len(l.tickets)))
	if len(l.tickets) == 0 {
		l.lowest.Store(math.MaxInt64)
		return
	}
	l.lowest.Store(l.tickets[0])
}

// add records a single retired ticket number.
func (l *ghostLedger) add(ticket int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	i := sort.Search(len(l.tickets), func(i int) bool { return l.tickets[i] > ticket })
	l.tickets = slices.Insert(l.tickets, i, ticket)
	l.publishLocked()
}

// addMany records a batch of retired ticket numbers with a single sorted
// merge — O(n+m) rather than m separate O(n) inserts.
func (l *ghostLedger) addMany(tickets []int64) {
	if len(tickets) == 0 {
		return
	}
	incoming := slices.Clone(tickets)
	slices.Sort(incoming)

	l.mu.Lock()
	defer l.mu.Unlock()

	merged := make([]int64, 0, len(l.tickets)+len(incoming))
	i, j := 0, 0
	for i < len(l.tickets) && j < len(incoming) {
		if l.tickets[i] <= incoming[j] {
			merged = append(merged, l.tickets[i])
			i++
		} else {
			merged = append(merged, incoming[j])
			j++
		}
	}
	merged = append(merged, l.tickets[i:]...)
	merged = append(merged, incoming[j:]...)
	l.tickets = merged
	l.publishLocked()
}

// consumeUpTo removes every entry <= limit and returns how many were
// removed. Each removed entry is owed exactly one nowServing advance by
// the caller.
func (l *ghostLedger) consumeUpTo(limit int64) int64 {
	if l.lowest.Load() > limit {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	n := sort.Search(len(l.tickets), func(i int) bool { return l.tickets[i] > limit })
	if n == 0 {
		return 0
	}
	l.tickets = slices.Delete(l.tickets, 0, n)
	if c := cap(l.tickets); c > ledgerShrinkFloor && len(l.tickets)*4 < c {
		l.tickets = slices.Clone(l.tickets)
	}
	l.publishLocked()
	return int64(n)
}

// countBelow returns the number of entries strictly less than ticket —
// the ghosts ahead of that ticket in the queue.
func (l *ghostLedger) countBelow(ticket int64) int64 {
	if l.size.Load() == 0 || l.lowest.Load() >= ticket {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return int64(sort.Search(len(l.tickets), func(i int) bool { return l.tickets[i] >= ticket }))
}

// len returns the number of pending ghosts. Lock-free.
func (l *ghostLedger) len() int64 {
	return l.size.Load()
}

// minTicket returns the lowest pending ghost, or math.MaxInt64. Lock-free.
func (l *ghostLedger) minTicket() int64 {
	return l.lowest.Load()
}

// advance moves nowServing forward by n (one per completed request) and
// then consumes any ghosts the wider window now reaches.
//
// Related: WaitingRoom.release, WaitingRoom.drainLedger
func (wr *WaitingRoom) advance(n int64) {
	wr.nowServing.Add(n)
	wr.drainLedger()
}

// drainLedger consumes every ghost at or below the current window edge,
// advancing nowServing once per ghost, and repeats because each advance
// widens the window and may reach further ghosts.
//
// It must be called after anything that raises the window edge (advance,
// SetCap growth) and after anything that adds a ghost (retire). Together
// with Go's sequentially consistent atomics this guarantees no ghost is
// left below the edge: whichever of "add ghost" and "raise edge" happens
// second observes the other and drains it.
//
// If SetCap shrinks capacity concurrently, a ghost may be consumed against
// the pre-shrink edge. That admits at most one client early per such
// ghost; the semaphore still bounds real concurrency.
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

// retire accounts for a ticket number that will never be admitted.
//
// The caller MUST own the ticket exclusively — in practice, it must be
// the goroutine whose tokenStore.take / deleteIfExpired / write-locked
// delete actually removed the entry, or the goroutine that issued the
// ticket and failed to admit it. Retiring a ticket that is also admitted
// (or retiring it twice) over-advances nowServing.
//
// Related: WaitingRoom.retireMany, ghostLedger
func (wr *WaitingRoom) retire(ticket int64) {
	wr.ledger.add(ticket)
	wr.drainLedger()
}

// retireMany is the batch form of retire, used by the reaper so that a
// large eviction costs one sorted merge.
func (wr *WaitingRoom) retireMany(tickets []int64) {
	if len(tickets) == 0 {
		return
	}
	wr.ledger.addMany(tickets)
	wr.drainLedger()
}

package room

import (
	"container/heap"
	"slices"
)

// tokenStoreCompactFloor is the minimum high-water mark before reap()
// reallocates tokenStore maps and heap slices to reclaim post-spike memory.
const tokenStoreCompactFloor = 4096

// ttlMinHeap is an intrusive min-heap of live *tokenNode ordered by ttlKey
// (the last-seen issuedAt recorded in the heap). Storing issuedAt rather than
// issuedAt+TTL lets runtime SetTokenTTL changes take effect immediately with
// zero heap rebuild, while poll() never touches the heap.
//
// All methods require the caller to hold ts.mu.Lock.
type ttlMinHeap []*tokenNode

func (h ttlMinHeap) Len() int           { return len(h) }
func (h ttlMinHeap) Less(i, j int) bool { return h[i].ttlKey.Before(h[j].ttlKey) }
func (h ttlMinHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].ttlHeapIdx = i
	h[j].ttlHeapIdx = j
}
func (h *ttlMinHeap) Push(x any) {
	n := x.(*tokenNode)
	n.ttlHeapIdx = len(*h)
	*h = append(*h, n)
}
func (h *ttlMinHeap) Pop() any {
	old := *h
	last := len(old) - 1
	n := old[last]
	old[last] = nil
	n.ttlHeapIdx = -1
	*h = old[:last]
	return n
}

// unseenMinHeap is an intrusive min-heap of never-seen *tokenNode ordered by
// unseenKey (issuance issuedAt). Storing issuedAt lets runtime
// SetFirstPollGrace changes apply immediately with zero rebuild. Nodes are
// lazily dropped from this heap once seen becomes true, or removed immediately
// on admission/eviction via unseenHeapIdx.
//
// All methods require the caller to hold ts.mu.Lock.
type unseenMinHeap []*tokenNode

func (h unseenMinHeap) Len() int           { return len(h) }
func (h unseenMinHeap) Less(i, j int) bool { return h[i].unseenKey.Before(h[j].unseenKey) }
func (h unseenMinHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].unseenHeapIdx = i
	h[j].unseenHeapIdx = j
}
func (h *unseenMinHeap) Push(x any) {
	n := x.(*tokenNode)
	n.unseenHeapIdx = len(*h)
	*h = append(*h, n)
}
func (h *unseenMinHeap) Pop() any {
	old := *h
	last := len(old) - 1
	n := old[last]
	old[last] = nil
	n.unseenHeapIdx = -1
	*h = old[:last]
	return n
}

func (ts *tokenStore) removeFromHeapsLocked(n *tokenNode) {
	if n.ttlHeapIdx >= 0 {
		heap.Remove(&ts.ttlHeap, n.ttlHeapIdx)
	}
	if n.unseenHeapIdx >= 0 {
		heap.Remove(&ts.unseenHeap, n.unseenHeapIdx)
	}
}

// compactIfNeeded reclaims Go map bucket arrays and heap slice backing arrays
// after a traffic spike subsides (live count drops below 25% of highWater,
// above tokenStoreCompactFloor). Also clears hasLazyShifts once empty.
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

	if cap(ts.ttlHeap) > tokenStoreCompactFloor && len(ts.ttlHeap)*4 < cap(ts.ttlHeap) {
		ts.ttlHeap = slices.Clone(ts.ttlHeap)
	}
	if cap(ts.unseenHeap) > tokenStoreCompactFloor && len(ts.unseenHeap)*4 < cap(ts.unseenHeap) {
		ts.unseenHeap = slices.Clone(ts.unseenHeap)
	}
	ts.highWater = cur
}

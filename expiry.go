package room

import (
	"slices"
	"time"
)

// tokenStoreCompactFloor is the minimum high-water mark before reap()
// reallocates tokenStore maps and heap slices to reclaim post-spike memory.
const tokenStoreCompactFloor = 4096

// nodeHeap is a typed intrusive min-heap over *tokenNode keyed by recorded
// issuedAt (either ttlKey/ttlHeapIdx or unseenKey/unseenHeapIdx). Keying by
// issuedAt rather than issuedAt+TTL lets runtime SetTokenTTL and
// SetFirstPollGrace changes apply immediately with zero rebuild, while poll()
// never touches the heap.
//
// All methods require the caller to hold ts.mu.Lock.
type nodeHeap struct {
	nodes  []*tokenNode
	unseen bool
}

func (h *nodeHeap) len() int { return len(h.nodes) }

func (h *nodeHeap) peek() *tokenNode { return h.nodes[0] }

func (h *nodeHeap) key(n *tokenNode) time.Time {
	if h.unseen {
		return n.unseenKey
	}
	return n.ttlKey
}

func (h *nodeHeap) idx(n *tokenNode) int {
	if h.unseen {
		return n.unseenHeapIdx
	}
	return n.ttlHeapIdx
}

func (h *nodeHeap) setIdx(n *tokenNode, i int) {
	if h.unseen {
		n.unseenHeapIdx = i
	} else {
		n.ttlHeapIdx = i
	}
}

func (h *nodeHeap) swap(i, j int) {
	h.nodes[i], h.nodes[j] = h.nodes[j], h.nodes[i]
	h.setIdx(h.nodes[i], i)
	h.setIdx(h.nodes[j], j)
}

func (h *nodeHeap) less(i, j int) bool {
	return h.key(h.nodes[i]).Before(h.key(h.nodes[j]))
}

func (h *nodeHeap) push(n *tokenNode) {
	i := len(h.nodes)
	h.setIdx(n, i)
	h.nodes = append(h.nodes, n)
	h.siftUp(i)
}

func (h *nodeHeap) pop() *tokenNode {
	n := h.nodes[0]
	h.removeAt(0)
	return n
}

func (h *nodeHeap) remove(n *tokenNode) {
	if i := h.idx(n); i >= 0 {
		h.removeAt(i)
	}
}

func (h *nodeHeap) removeAt(i int) {
	last := len(h.nodes) - 1
	if i != last {
		h.swap(i, last)
	}
	removed := h.nodes[last]
	h.nodes[last] = nil
	h.setIdx(removed, -1)
	h.nodes = h.nodes[:last]
	if i < last {
		if !h.siftDown(i) {
			h.siftUp(i)
		}
	}
}

func (h *nodeHeap) fix(n *tokenNode) {
	if i := h.idx(n); i >= 0 {
		if !h.siftDown(i) {
			h.siftUp(i)
		}
	}
}

func (h *nodeHeap) init() {
	for i := len(h.nodes)/2 - 1; i >= 0; i-- {
		h.siftDown(i)
	}
}

func (h *nodeHeap) compact(floor int) {
	if cap(h.nodes) > floor && len(h.nodes)*4 < cap(h.nodes) {
		h.nodes = slices.Clone(h.nodes)
	}
}

func (h *nodeHeap) siftUp(j int) {
	for {
		i := (j - 1) / 2
		if i == j || !h.less(j, i) {
			break
		}
		h.swap(i, j)
		j = i
	}
}

func (h *nodeHeap) siftDown(i0 int) bool {
	i := i0
	n := len(h.nodes)
	for {
		j1 := 2*i + 1
		if j1 >= n || j1 < 0 {
			break
		}
		j := j1
		if j2 := j1 + 1; j2 < n && h.less(j2, j1) {
			j = j2
		}
		if !h.less(j, i) {
			break
		}
		h.swap(i, j)
		i = j
	}
	return i > i0
}

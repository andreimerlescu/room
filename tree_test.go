package room

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
	"time"
)

// assertStoreInvariants exhaustively validates every structural invariant of
// tokenStore: BST order (with accumulated lazy deltas), treap heap priorities,
// parent pointers, subtree size, subtree maxRank, map/tree bijection, key/ID
// indexes, and intrusive ttlHeap/unseenHeap order & indices.
func assertStoreInvariants(t *testing.T, ts *tokenStore) {
	t.Helper()
	ts.mu.RLock()
	defer ts.mu.RUnlock()

	if got, want := ts.tree.len(), len(ts.entries); got != want {
		t.Fatalf("tree.len()=%d != len(entries)=%d", got, want)
	}
	if ts.tree.root != nil && ts.tree.root.parent != nil {
		t.Fatalf("root.parent != nil")
	}

	seen := make(map[string]struct{}, len(ts.entries))
	var prevSet bool
	var prevTicket int64
	var prevCreated time.Time
	var prevToken string

	var check func(n, parent *tokenNode, accLazy int64) (size int, maxRank int)
	check = func(n, parent *tokenNode, accLazy int64) (int, int) {
		if n == nil {
			return 0, -1
		}
		if n.parent != parent {
			t.Fatalf("node %q: parent mismatch", n.token)
		}
		if parent != nil && n.prio > parent.prio {
			t.Fatalf("node %q: treap heap violation prio=%d > parent.prio=%d", n.token, n.prio, parent.prio)
		}
		if ts.entries[n.token] != n {
			t.Fatalf("node %q: missing or mismatched in entries map", n.token)
		}
		if _, dup := seen[n.token]; dup {
			t.Fatalf("node %q: visited twice in tree", n.token)
		}
		seen[n.token] = struct{}{}

		eff := n.ticket + accLazy
		if got := effectiveTicket(n, true); got != eff {
			t.Fatalf("node %q: effectiveTicket=%d != accLazy result %d", n.token, got, eff)
		}
		if !ts.hasLazyShifts && eff != n.ticket {
			t.Fatalf("node %q: hasLazyShifts=false but eff=%d != n.ticket=%d", n.token, eff, n.ticket)
		}

		childLazy := accLazy + n.lazy
		ls, lmr := check(n.left, n, childLazy)

		if prevSet && !lessTicketKey(prevTicket, prevCreated, prevToken, eff, n.createdAt, n.token) {
			t.Fatalf("BST order violation: (%d, %v, %q) >= (%d, %v, %q)",
				prevTicket, prevCreated, prevToken, eff, n.createdAt, n.token)
		}
		prevSet = true
		prevTicket = eff
		prevCreated = n.createdAt
		prevToken = n.token

		rs, rmr := check(n.right, n, childLazy)

		wantSize := ls + rs + 1
		if n.size != wantSize {
			t.Fatalf("node %q: size=%d, want %d", n.token, n.size, wantSize)
		}
		wantMR := n.rank
		if lmr > wantMR {
			wantMR = lmr
		}
		if rmr > wantMR {
			wantMR = rmr
		}
		if n.maxRank != wantMR {
			t.Fatalf("node %q: maxRank=%d, want %d", n.token, n.maxRank, wantMR)
		}
		return wantSize, wantMR
	}
	check(ts.tree.root, nil, 0)

	if len(seen) != len(ts.entries) {
		t.Fatalf("tree visited %d nodes, entries map has %d", len(seen), len(ts.entries))
	}

	// Verify place() matches 1..N in-order rank for every node.
	var idx int64
	ts.tree.walk(-1, func(n *tokenNode, _ int64) {
		idx++
		if p := ts.tree.place(n); p != idx {
			t.Fatalf("node %q: place()=%d, want %d", n.token, p, idx)
		}
	})

	// Verify ttlHeap.
	if ts.ttlHeap.len() != len(ts.entries) {
		t.Fatalf("ttlHeap.len()=%d != len(entries)=%d", ts.ttlHeap.len(), len(ts.entries))
	}
	for i, n := range ts.ttlHeap.nodes {
		if n.ttlHeapIdx != i {
			t.Fatalf("ttlHeap[%d] (%q): ttlHeapIdx=%d", i, n.token, n.ttlHeapIdx)
		}
		if ts.entries[n.token] != n {
			t.Fatalf("ttlHeap[%d] (%q): not in entries map", i, n.token)
		}
		if i > 0 {
			p := (i - 1) / 2
			if n.ttlKey.Before(ts.ttlHeap.nodes[p].ttlKey) {
				t.Fatalf("ttlHeap order violated at %d vs parent %d", i, p)
			}
		}
	}

	// Verify unseenHeap.
	for i, n := range ts.unseenHeap.nodes {
		if n.unseenHeapIdx != i {
			t.Fatalf("unseenHeap[%d] (%q): unseenHeapIdx=%d", i, n.token, n.unseenHeapIdx)
		}
		if ts.entries[n.token] != n {
			t.Fatalf("unseenHeap[%d] (%q): not in entries map", i, n.token)
		}
		if i > 0 {
			p := (i - 1) / 2
			if n.unseenKey.Before(ts.unseenHeap.nodes[p].unseenKey) {
				t.Fatalf("unseenHeap order violated at %d vs parent %d", i, p)
			}
		}
	}

	// Verify byKey / byID indexes.
	keyed := 0
	for tok, n := range ts.entries {
		if n.key != "" {
			keyed++
			if ts.byKey[n.key] != tok || ts.byID[n.id] != tok {
				t.Fatalf("keyed node %q (key=%q id=%q) not indexed", tok, n.key, n.id)
			}
		}
	}
	if len(ts.byKey) != keyed || len(ts.byID) != keyed {
		t.Fatalf("index counts mismatch: byKey=%d byID=%d keyed=%d", len(ts.byKey), len(ts.byID), keyed)
	}
}

// TestTree_DifferentialOracle compares WaitingRoom's treap-backed tokenStore
// and ghostLedger against a brute-force reference oracle across thousands of
// mixed insert, remove, promote (creating duplicate tickets), SetTicketRank
// (raising and lowering), and window-advance operations.
func TestTree_DifferentialOracle(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 3)
	// Keep window at edge=3 initially while queuing tickets 4..N.
	wr.nextTicket.Store(3)

	type refEntry struct {
		token     string
		ticket    int64
		createdAt time.Time
		rank      int
	}
	refEntries := make(map[string]*refEntry)
	var refGhosts []int64
	var refNowServing int64
	const capVal int64 = 3

	refDrain := func() {
		for {
			edge := refNowServing + capVal
			sort.Slice(refGhosts, func(i, j int) bool { return refGhosts[i] < refGhosts[j] })
			k := 0
			for k < len(refGhosts) && refGhosts[k] <= edge {
				k++
			}
			if k == 0 {
				return
			}
			refGhosts = refGhosts[k:]
			refNowServing += int64(k)
		}
	}
	refCountBelow := func(ticket int64) int64 {
		var c int64
		for _, g := range refGhosts {
			if g < ticket {
				c++
			}
		}
		return c
	}
	refPositionOf := func(ticket int64) int64 {
		raw := ticket - refNowServing - capVal
		if raw <= 0 {
			return raw
		}
		pos := raw - refCountBelow(ticket)
		if pos < 1 {
			pos = 1
		}
		return pos
	}
	refTicketForPos := func(position int64) int64 {
		edge := refNowServing + capVal
		n := edge + position
		for {
			next := edge + position + refCountBelow(n)
			if next == n {
				return n
			}
			n = next
		}
	}

	rng := rand.New(rand.NewPCG(42, 1337))
	baseTime := time.Now()
	var liveTokens []string
	tokenSeq := 0

	pickToken := func() string {
		if len(liveTokens) == 0 {
			return ""
		}
		return liveTokens[rng.IntN(len(liveTokens))]
	}
	dropLive := func(tok string) {
		for i, s := range liveTokens {
			if s == tok {
				liveTokens[i] = liveTokens[len(liveTokens)-1]
				liveTokens = liveTokens[:len(liveTokens)-1]
				return
			}
		}
	}

	for step := range 2500 {
		op := rng.IntN(100)
		switch {
		case op < 35 || len(liveTokens) < 5:
			// Insert a new queued token.
			tokenSeq++
			tok := fmt.Sprintf("tok-%05d", tokenSeq)
			ticket := wr.nextTicket.Add(1)
			ct := baseTime.Add(time.Duration(tokenSeq) * time.Microsecond)
			wr.tokens.set(tok, ticketEntry{
				ticket:    ticket,
				createdAt: ct,
				issuedAt:  ct,
			})
			refEntries[tok] = &refEntry{token: tok, ticket: ticket, createdAt: ct}
			liveTokens = append(liveTokens, tok)

		case op < 55:
			// Remove a random token (adds ghost + drains ledger).
			tok := pickToken()
			if err := wr.RemoveToken(tok); err != nil {
				t.Fatalf("step %d: RemoveToken(%q): %v", step, tok, err)
			}
			re := refEntries[tok]
			delete(refEntries, tok)
			dropLive(tok)
			refGhosts = append(refGhosts, re.ticket)
			refDrain()

		case op < 80:
			// SetTicketRank (both raising and lowering, ranks 0..5).
			tok := pickToken()
			newRank := rng.IntN(6)
			gotMoved, gotErr := wr.SetTicketRank(tok, newRank)

			re := refEntries[tok]
			re.rank = newRank
			edge := refNowServing + capVal
			from := re.ticket
			var wantErr error
			var wantMoved bool
			if from <= edge {
				wantErr = ErrAlreadyAdmitted{}
			} else if newRank > 0 {
				to := edge + 1
				for t2, e2 := range refEntries {
					if t2 == tok || e2.ticket <= edge || e2.ticket >= from || e2.rank < newRank {
						continue
					}
					if e2.ticket+1 > to {
						to = e2.ticket + 1
					}
				}
				if to < from {
					jumped := 0
					for t2, e2 := range refEntries {
						if t2 != tok && e2.ticket >= to && e2.ticket < from {
							e2.ticket++
							jumped++
						}
					}
					if jumped > 0 {
						for i := range refGhosts {
							if refGhosts[i] >= to && refGhosts[i] < from {
								refGhosts[i]++
							}
						}
						re.ticket = to
						wantMoved = true
					}
				}
			}
			if (gotErr != nil) != (wantErr != nil) || gotMoved != wantMoved {
				t.Fatalf("step %d: SetTicketRank(%q, %d) = (%v, %v), want (%v, %v)",
					step, tok, newRank, gotMoved, gotErr, wantMoved, wantErr)
			}

		case op < 92:
			// AdminPromote to a random target position (creates duplicate tickets!).
			tok := pickToken()
			target := int64(1 + rng.IntN(10))
			gotErr := wr.AdminPromote(tok, target)

			re := refEntries[tok]
			curPos := refPositionOf(re.ticket)
			var wantErr error
			if curPos <= 0 {
				wantErr = ErrAlreadyAdmitted{}
			} else if target < curPos {
				re.ticket = refTicketForPos(target)
			}
			if (gotErr != nil) != (wantErr != nil) {
				t.Fatalf("step %d: AdminPromote(%q, %d) err=%v, want %v", step, tok, target, gotErr, wantErr)
			}

		default:
			// Advance window by 1 (simulating a slot release).
			wr.advance(1)
			refNowServing++
			refDrain()
		}

		// Periodically (and on final step) assert exact equivalence with oracle.
		if step%50 == 0 || step == 2499 {
			assertStoreInvariants(t, wr.tokens)
			if wr.nowServing.Load() != refNowServing {
				t.Fatalf("step %d: nowServing=%d, want %d", step, wr.nowServing.Load(), refNowServing)
			}

			// Verify ghostLedger exact contents.
			sort.Slice(refGhosts, func(i, j int) bool { return refGhosts[i] < refGhosts[j] })
			gotGhosts := ghostSnapshot(wr.ledger)
			if len(gotGhosts) != len(refGhosts) {
				t.Fatalf("step %d: ledger len=%d, want %d", step, len(gotGhosts), len(refGhosts))
			}
			for i := range refGhosts {
				if gotGhosts[i] != refGhosts[i] {
					t.Fatalf("step %d: ledger[%d]=%d, want %d", step, i, gotGhosts[i], refGhosts[i])
				}
			}

			// Verify Queue ordering, tickets, positions, places, and ranks.
			refSorted := make([]*refEntry, 0, len(refEntries))
			for _, re := range refEntries {
				refSorted = append(refSorted, re)
			}
			sort.Slice(refSorted, func(i, j int) bool {
				return lessTicketKey(
					refSorted[i].ticket, refSorted[i].createdAt, refSorted[i].token,
					refSorted[j].ticket, refSorted[j].createdAt, refSorted[j].token,
				)
			})
			gotQ := wr.Queue(len(refSorted) + 10)
			if len(gotQ) != len(refSorted) {
				t.Fatalf("step %d: Queue len=%d, want %d", step, len(gotQ), len(refSorted))
			}
			for i, want := range refSorted {
				g := gotQ[i]
				if g.Token != want.token || g.Ticket != want.ticket || g.Rank != want.rank ||
					g.Place != int64(i+1) || g.Position != refPositionOf(want.ticket) {
					t.Fatalf("step %d slot %d: got %+v, want token=%q ticket=%d rank=%d place=%d pos=%d",
						step, i, g, want.token, want.ticket, want.rank, i+1, refPositionOf(want.ticket))
				}
			}
		}
	}
}

// TestTree_SetTicketRankWithPromotedBoundaryTie specifically tests the subtle
// case where a promoted ticket shares ticket number `from` with the ranked
// ticket while another ticket at `from-1` shifts into `from` with both earlier
// and later createdAt tie-breakers.
func TestTree_SetTicketRankWithPromotedBoundaryTie(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 1)
	now := time.Now()
	wr.nextTicket.Store(10)

	// Setup:
	//   ticket 2: "c-early" (createdAt t=1, rank 0) -> will shift to 3
	//   ticket 2: "c-late"  (createdAt t=9, rank 0) -> will shift to 3
	//   ticket 3: "b-tie"   (createdAt t=5, rank 0) -> stays at 3 (promoted tie!)
	//   ticket 3: "a-rank"  (createdAt t=6, rank 0) -> ranked to 2, moves to 2!
	wr.tokens.set("c-early", ticketEntry{ticket: 2, createdAt: now.Add(1 * time.Millisecond), issuedAt: now})
	wr.tokens.set("c-late", ticketEntry{ticket: 2, createdAt: now.Add(9 * time.Millisecond), issuedAt: now})
	wr.tokens.set("b-tie", ticketEntry{ticket: 3, createdAt: now.Add(5 * time.Millisecond), issuedAt: now})
	wr.tokens.set("a-rank", ticketEntry{ticket: 3, createdAt: now.Add(6 * time.Millisecond), issuedAt: now})
	wr.tokens.set("d-after", ticketEntry{ticket: 4, createdAt: now.Add(2 * time.Millisecond), issuedAt: now})

	moved, err := wr.SetTicketRank("a-rank", 2)
	if err != nil || !moved {
		t.Fatalf("SetTicketRank(a-rank, 2) = (%v, %v), want (true, nil)", moved, err)
	}
	assertStoreInvariants(t, wr.tokens)

	// Expected order:
	//   1. a-rank  (ticket 2)
	//   2. c-early (ticket 3, t=1)
	//   3. b-tie   (ticket 3, t=5)
	//   4. c-late  (ticket 3, t=9)
	//   5. d-after (ticket 4, t=2)
	want := []string{"a-rank", "c-early", "b-tie", "c-late", "d-after"}
	q := wr.Queue(10)
	if len(q) != len(want) {
		t.Fatalf("Queue len=%d, want %d", len(q), len(want))
	}
	for i, tok := range want {
		if q[i].Token != tok || q[i].Place != int64(i+1) {
			t.Errorf("slot %d: got (%q, ticket=%d, place=%d), want %q at place %d",
				i, q[i].Token, q[i].Ticket, q[i].Place, tok, i+1)
		}
	}
}

// TestReap_DynamicTTLAndGraceShrinkImmediate verifies that shortening
// SetTokenTTL or enabling SetFirstPollGrace at runtime takes effect on the
// very next reap() with zero heap rebuild.
func TestReap_DynamicTTLAndGraceShrinkImmediate(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 1)
	now := time.Now()
	wr.nextTicket.Store(10)

	// Insert with default 5m TTL and grace disabled:
	wr.tokens.set("old-seen", ticketEntry{ticket: 2, createdAt: now.Add(-45 * time.Second), issuedAt: now.Add(-45 * time.Second), seen: true})
	wr.tokens.set("old-unseen", ticketEntry{ticket: 3, createdAt: now.Add(-15 * time.Second), issuedAt: now.Add(-15 * time.Second), seen: false})
	wr.tokens.set("fresh-seen", ticketEntry{ticket: 4, createdAt: now.Add(-5 * time.Second), issuedAt: now.Add(-5 * time.Second), seen: true})
	wr.tokens.set("fresh-unseen", ticketEntry{ticket: 5, createdAt: now.Add(-5 * time.Second), issuedAt: now.Add(-5 * time.Second), seen: false})

	wr.reap()
	if wr.tokens.len() != 4 {
		t.Fatalf("before tightening settings: len=%d, want 4", wr.tokens.len())
	}

	// Tighten TTL to 30s and enable 10s grace at runtime.
	if err := wr.SetTokenTTL(30 * time.Second); err != nil {
		t.Fatal(err)
	}
	if err := wr.SetFirstPollGrace(10 * time.Second); err != nil {
		t.Fatal(err)
	}

	wr.reap()
	assertStoreInvariants(t, wr.tokens)

	if _, ok := wr.tokens.get("old-seen"); ok {
		t.Error("old-seen (45s > 30s TTL) should have been evicted")
	}
	if _, ok := wr.tokens.get("old-unseen"); ok {
		t.Error("old-unseen (15s > 10s grace) should have been evicted")
	}
	if _, ok := wr.tokens.get("fresh-seen"); !ok {
		t.Error("fresh-seen should have survived")
	}
	if _, ok := wr.tokens.get("fresh-unseen"); !ok {
		t.Error("fresh-unseen should have survived")
	}
}

// TestReap_LazyRekeyOverBatchSize verifies synchronized rollover when >
// reaperBatchSize tokens are refreshed via poll/touchIssuedAt before crossing
// TTL, leaving their heap ttlKey stale until reap() lazily re-keys them.
func TestReap_LazyRekeyOverBatchSize(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 1)
	if err := wr.SetTokenTTL(time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := wr.SetFirstPollGrace(10 * time.Second); err != nil {
		t.Fatal(err)
	}

	const total = reaperBatchSize + 500
	almostExpired := time.Now().Add(-50 * time.Second)
	wr.nextTicket.Store(int64(total + 10))
	for i := range total {
		tok := fmt.Sprintf("active-%04d", i)
		wr.tokens.set(tok, ticketEntry{
			ticket:    int64(10 + i),
			createdAt: almostExpired,
			issuedAt:  almostExpired,
		})
	}

	// Poll every token while still live (sets node.issuedAt=now, node.seen=true
	// under RLock; ttlKey and unseenKey remain at almostExpired).
	now := time.Now()
	for i := range total {
		tok := fmt.Sprintf("active-%04d", i)
		res := wr.tokens.poll(tok, now, 1, statusPollMinInterval, time.Minute, 0, 0)
		if res.outcome != pollAccepted {
			t.Fatalf("poll(%q): got outcome %v, want pollAccepted", tok, res.outcome)
		}
	}

	// Simulate ttlKey/unseenKey aging past TTL/grace while node.issuedAt stays fresh.
	wr.tokens.mu.Lock()
	past := now.Add(-2 * time.Minute)
	for _, n := range wr.tokens.entries {
		n.ttlKey = past
		n.unseenKey = past
	}
	wr.tokens.mu.Unlock()

	wr.reap()
	assertStoreInvariants(t, wr.tokens)

	if got := wr.tokens.len(); got != total {
		t.Errorf("reap evicted active tokens during lazy rollover: len=%d, want %d", got, total)
	}
	wr.tokens.mu.RLock()
	unseenLen := wr.tokens.unseenHeap.len()
	wr.tokens.mu.RUnlock()
	if unseenLen != 0 {
		t.Errorf("seen tokens were not drained from unseenHeap: len=%d, want 0", unseenLen)
	}
}

// TestCompact_ReclaimsAfterSpike verifies that low-water compaction in reap()
// shrinks heap slices and resets highWater after a spike drains, without
// losing any live tokens or corrupting tree/heap invariants.
func TestCompact_ReclaimsAfterSpike(t *testing.T) {
	t.Parallel()
	wr := newTestWR(t, 1)
	now := time.Now()

	const peak = tokenStoreCompactFloor + 2000
	const keep = 100
	wr.nextTicket.Store(int64(peak + 10))
	for i := range peak {
		wr.tokens.set(fmt.Sprintf("tok-%05d", i), ticketEntry{
			ticket:    int64(10 + i),
			createdAt: now.Add(time.Duration(i) * time.Microsecond),
			issuedAt:  now,
		})
	}

	// Remove all but `keep` tokens.
	for i := keep; i < peak; i++ {
		if err := wr.RemoveToken(fmt.Sprintf("tok-%05d", i)); err != nil {
			t.Fatalf("RemoveToken(%d): %v", i, err)
		}
	}

	wr.reap()
	assertStoreInvariants(t, wr.tokens)

	if wr.tokens.len() != keep {
		t.Fatalf("len=%d, want %d", wr.tokens.len(), keep)
	}
	wr.tokens.mu.RLock()
	ttlCap := cap(wr.tokens.ttlHeap.nodes)
	unseenCap := cap(wr.tokens.unseenHeap.nodes)
	hw := wr.tokens.highWater
	wr.tokens.mu.RUnlock()

	if ttlCap > tokenStoreCompactFloor {
		t.Errorf("ttlHeap backing slice not compacted: cap=%d > %d", ttlCap, tokenStoreCompactFloor)
	}
	if unseenCap > tokenStoreCompactFloor {
		t.Errorf("unseenHeap backing slice not compacted: cap=%d > %d", unseenCap, tokenStoreCompactFloor)
	}
	if hw != keep {
		t.Errorf("highWater=%d, want %d after compaction", hw, keep)
	}

	// Drain remaining tokens and verify hasLazyShifts resets cleanly on empty.
	wr.tokens.mu.Lock()
	wr.tokens.hasLazyShifts = true
	wr.tokens.mu.Unlock()
	for i := range keep {
		_ = wr.RemoveToken(fmt.Sprintf("tok-%05d", i))
	}
	wr.reap()
	wr.tokens.mu.RLock()
	lazy := wr.tokens.hasLazyShifts
	wr.tokens.mu.RUnlock()
	if lazy {
		t.Error("hasLazyShifts was not reset when store became empty")
	}
}

// TestConcurrent_TokenStoreAllOperations stresses all read-locked and
// write-locked tokenStore paths concurrently under -race.
func TestConcurrent_TokenStoreAllOperations(t *testing.T) {
	wr := newTestWR(t, 1)
	if err := wr.SetFirstPollGrace(10 * time.Second); err != nil {
		t.Fatal(err)
	}

	serving := make(chan struct{}, 1)
	release := make(chan struct{})
	r := newTestRouter(wr, serving, release)
	defer close(release)
	fillOneSlot(t, r, serving)

	// Pre-queue cookie tokens and keyed tickets.
	cookieToks := queueTokens(t, r, 40)
	keyIDs := make([]string, 40)
	for i := range keyIDs {
		id, _, err := wr.Enter(fmt.Sprintf("k-%d", i))
		if err != nil {
			t.Fatal(err)
		}
		keyIDs[i] = id
	}

	var wg sync.WaitGroup

	// Parallel /queue/status polls (RLock + node.mu).
	for g := range 6 {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := range 60 {
				tok := cookieToks[(g*13+i)%len(cookieToks)]
				pollStatus(r, tok)
			}
		}(g)
	}

	// Parallel /queue/status/{id} views (RLock + node.mu + O(log N) place).
	for g := range 4 {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := range 60 {
				id := keyIDs[(g*7+i)%len(keyIDs)]
				req := httptest.NewRequest(http.MethodGet, "/queue/status/"+id, nil)
				r.ServeHTTP(httptest.NewRecorder(), req)
			}
		}(g)
	}

	// Parallel Enter / TicketByKey / Position.
	for g := range 4 {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := range 40 {
				key := fmt.Sprintf("k-%d", (g*11+i)%len(keyIDs))
				_, _, _ = wr.Enter(key)
				_, _ = wr.TicketByKey(key)
				_, _ = wr.Position(keyIDs[(g*11+i)%len(keyIDs)])
			}
		}(g)
	}

	// Parallel SetTicketRank & AdminPromote.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i, tok := range cookieToks {
			_, _ = wr.SetTicketRank(tok, (i%4)+1)
			if i%5 == 0 {
				_ = wr.AdminPromote(tok, 1)
			}
		}
	}()

	// Parallel Queue, Export, Leave, RemoveToken, and reap.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range 20 {
			_ = wr.Queue(15)
			var buf bytes.Buffer
			_ = wr.Export(&buf)
			if i < 10 {
				_ = wr.Leave(fmt.Sprintf("k-%d", i))
			}
			wr.reap()
		}
	}()

	wg.Wait()
	assertStoreInvariants(t, wr.tokens)
}

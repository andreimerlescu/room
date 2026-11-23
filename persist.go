package room

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// persistFormatVersion is the version Export writes when no lease is held.
// Version-1 files carry keyed tickets' key and id fields too: a room that
// predates them ignores the fields and restores the tickets as cookie
// tickets, which is harmless.
const persistFormatVersion = 1

// persistFormatLeases is the version Export writes when leases are held. A
// room that predates leases refuses it rather than silently dropping the
// leases, which would let it admit more than its capacity. Import accepts
// both versions.
const persistFormatLeases = 2

// maxPersistedTokenLen bounds token values accepted by Import. Tokens
// issued by this package are 32 hex characters.
const maxPersistedTokenLen = 256

// persistFile is the on-disk representation written by Export.
//
// Tickets are stored in line order; their internal numbers are not
// persisted, because Import renumbers them.
type persistFile struct {
	Version    int               `json:"version"`
	ExportedAt time.Time         `json:"exported_at"`
	Tickets    []persistedTicket `json:"tickets"`
	Passes     []persistedPass   `json:"passes"`
	Leases     []persistedLease  `json:"leases,omitempty"`
}

type persistedTicket struct {
	Token     string    `json:"token"`
	CreatedAt time.Time `json:"created_at"`
	LastSeen  time.Time `json:"last_seen"`
	LastPoll  time.Time `json:"last_poll"`
	ClientKey string    `json:"client_key,omitempty"`
	Seen      bool      `json:"seen,omitempty"`
	Promoted  bool      `json:"promoted,omitempty"`
	HasPass   bool      `json:"has_pass,omitempty"`
	Rank      int       `json:"rank,omitempty"`
	Key       string    `json:"key,omitempty"`
	ID        string    `json:"id,omitempty"`
}

// persistedLease is one held lease. Leases are written in the order they
// were granted.
type persistedLease struct {
	Key        string    `json:"key"`
	AcquiredAt time.Time `json:"acquired_at"`
	RenewedAt  time.Time `json:"renewed_at"`
}

type persistedPass struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// ImportStats reports what Import restored and what it discarded.
type ImportStats struct {
	// Restored is the number of tickets placed back in the line.
	Restored int

	// DroppedStale is the number of tickets discarded because they had
	// already expired (TokenTTL), or had never been seen past the
	// first-poll grace, at export time.
	DroppedStale int

	// PassesRestored is the number of VIP passes restored.
	PassesRestored int

	// PassesExpired is the number of VIP passes discarded because they
	// had expired by the time of import.
	PassesExpired int

	// LeasesRestored is the number of leases restored. A restored lease
	// holds its slot as before the restart.
	LeasesRestored int
}

// Export writes the current waiting line and live VIP passes to w as
// versioned JSON, so that a restarted process can restore them with
// Import.
//
// # What is written
//
// Every queued ticket, in line order (the order Queue returns), with its
// token, issue time, last-seen and last-poll times, client key, rank,
// seen/promoted/has-pass flags, and — for a keyed ticket — its key and
// public ID; every unexpired VIP pass with its absolute expiry; and every
// held lease, by key. Requests being actively served are not queued and
// are not written. Internal ticket numbers are not written — the order is
// the line.
//
// The document is version 1 unless leases are held, when it is version 2,
// which a room older than v1.5.0 refuses rather than drop the leases.
//
// # Consistency
//
// Export takes one read-locked copy of the token store and is safe to
// call while traffic flows, but arrivals after that copy are not
// included. For a restart, stop accepting requests first (for example
// http.Server.Shutdown), then Export.
//
// # Security
//
// The output contains every queued client's room_ticket and every pass
// token — bearer credentials. Write it with restrictive permissions
// (0600), keep it off shared storage, and delete it after Import.
//
// Memory use is proportional to queue depth: the whole document is built
// before it is written.
//
// Returns ErrNotInitialised on an uninitialised WaitingRoom, or the
// writer's error wrapped.
//
// Related: WaitingRoom.Import
func (wr *WaitingRoom) Export(w io.Writer) error {
	if !wr.initialised.Load() {
		return ErrNotInitialised{}
	}

	now := time.Now()

	entries := wr.tokens.snapshotSorted()

	f := persistFile{
		Version:    persistFormatVersion,
		ExportedAt: now,
		Tickets:    make([]persistedTicket, 0, len(entries)),
		Passes:     wr.passes.snapshotLive(now),
		Leases:     wr.snapshotLeases(),
	}
	if len(f.Leases) > 0 {
		f.Version = persistFormatLeases
	}
	for _, s := range entries {
		f.Tickets = append(f.Tickets, persistedTicket{
			Token:     s.token,
			CreatedAt: s.entry.createdAt,
			LastSeen:  s.entry.issuedAt,
			LastPoll:  s.entry.lastPoll,
			ClientKey: s.entry.clientKey,
			Seen:      s.entry.seen,
			Promoted:  s.entry.promoted,
			HasPass:   s.entry.hasPass,
			Rank:      s.entry.rank,
			Key:       s.entry.key,
			ID:        s.entry.id,
		})
	}

	if err := json.NewEncoder(w).Encode(&f); err != nil {
		return fmt.Errorf("room: export: %w", err)
	}
	return nil
}

// Import restores a line written by Export into this WaitingRoom.
//
// # Preconditions
//
// Call Import after Init and after applying settings — in particular
// SetTokenTTL and SetFirstPollGrace, which decide what counts as stale —
// and before the WaitingRoom serves traffic. Import refuses a room that
// has issued any ticket or holds any pass (ErrImportNotEmpty).
//
// # What is restored
//
//   - Leases, held again: they take ticket numbers 1..L as admitted
//     tickets, and their slots stay in use until Release.
//   - Tickets, renumbered after the leases in their exported line order.
//     The serving window starts with only the leases inside it, so the
//     first Cap()-L restored clients are ready on their next poll; a
//     capacity different from the exporting process's is handled
//     naturally. Keyed tickets keep their key and public ID.
//   - All ticket timestamps are shifted forward by the downtime (time
//     between export and import), so a client's remaining TTL and
//     first-poll grace are as they were at export. Downtime is not
//     charged against waiting clients.
//   - Each client's cookies are re-sent on its first poll after import.
//   - VIP passes, with their absolute expiry. A pass is wall-clock time
//     the client paid for, so downtime counts against it; passes that
//     expired during the downtime are discarded.
//
// # What is discarded
//
// Tickets that were already dead at export time: last seen longer ago
// than this room's TokenTTL, or — when SetFirstPollGrace is enabled —
// never seen and older than the grace. They are counted in
// ImportStats.DroppedStale.
//
// # Atomicity
//
// The whole document is decoded and validated before any state changes;
// on ErrImportFormat the room is untouched. Ticket numbers 1..n are then
// claimed with a single compare-and-swap, so a request arriving during
// the import is issued a later number and queues behind the restored
// clients rather than colliding with them. A restored client that polls
// during the import itself sees its token as unknown and re-queues;
// import before serving to avoid this.
//
// No events fire during Import.
//
// Returns ErrNotInitialised, ErrImportFormat or ErrImportNotEmpty.
//
// Related: WaitingRoom.Export, ImportStats
func (wr *WaitingRoom) Import(r io.Reader) (ImportStats, error) {
	var stats ImportStats
	if !wr.initialised.Load() {
		return stats, ErrNotInitialised{}
	}

	var f persistFile
	if err := json.NewDecoder(r).Decode(&f); err != nil {
		return stats, ErrImportFormat{Reason: "decode: " + err.Error()}
	}
	if err := validatePersistFile(&f); err != nil {
		return stats, err
	}

	now := time.Now()
	shift := now.Sub(f.ExportedAt)
	if shift < 0 {
		shift = 0 // clock moved backwards; charge nothing
	}
	ttl := wr.tokens.ttl()
	grace := time.Duration(wr.firstPollGrace.Load())

	// Decide what survives, judged at export time.
	kept := make([]persistedTicket, 0, len(f.Tickets))
	for _, t := range f.Tickets {
		if f.ExportedAt.Sub(t.LastSeen) > ttl {
			stats.DroppedStale++
			continue
		}
		if grace > 0 && !t.Seen && f.ExportedAt.Sub(t.CreatedAt) > grace {
			stats.DroppedStale++
			continue
		}
		kept = append(kept, t)
	}

	// A keyed ticket written without an ID (by hand) gets one now, before
	// any state changes, so a crypto/rand failure leaves the room as it was.
	ids := make([]string, len(kept))
	for i, t := range kept {
		ids[i] = t.ID
		if t.Key != "" && ids[i] == "" {
			id, err := generateToken()
			if err != nil {
				return ImportStats{}, fmt.Errorf("room: import: %w", err)
			}
			ids[i] = id
		}
		if t.Key == "" {
			ids[i] = "" // an ID names a keyed ticket's place only
		}
	}

	// The room must be pristine.
	if wr.tokens.len() != 0 || wr.passes.len() != 0 ||
		wr.nowServing.Load() != 0 || wr.ledger.len() != 0 || wr.Held() != 0 {
		return ImportStats{}, ErrImportNotEmpty{}
	}
	// Claim ticket numbers 1..n atomically; fails if any ticket was
	// issued since Init.
	// Leases take numbers 1..L, as tickets already admitted and not yet
	// released; the line takes L+1..L+n. The window (nowServing 0, edge
	// cap) then has exactly cap-L places free, as before the restart.
	leaseCount := int64(len(f.Leases))
	if !wr.nextTicket.CompareAndSwap(0, leaseCount+int64(len(kept))) {
		return ImportStats{}, ErrImportNotEmpty{}
	}
	wr.restoreLeases(f.Leases, shift)
	stats.LeasesRestored = len(f.Leases)

	entries := make(map[string]ticketEntry, len(kept))
	for i, t := range kept {
		key := t.ClientKey
		if len(key) > maxClientKeyLen {
			key = strings.Clone(key[:maxClientKeyLen])
		}
		var lastPoll time.Time
		if !t.LastPoll.IsZero() {
			lastPoll = t.LastPoll.Add(shift)
		}
		entries[t.Token] = ticketEntry{
			ticket:    leaseCount + int64(i+1),
			createdAt: t.CreatedAt.Add(shift),
			issuedAt:  t.LastSeen.Add(shift),
			lastPoll:  lastPoll,
			// Zero cookieSetAt makes the first accepted poll re-send the
			// cookies with a fresh MaxAge.
			cookieSetAt: time.Time{},
			clientKey:   key,
			seen:        t.Seen,
			promoted:    t.Promoted,
			hasPass:     t.HasPass,
			rank:        t.Rank,
			key:         t.Key,
			id:          ids[i],
		}
	}
	wr.tokens.setMany(entries)
	stats.Restored = len(kept)

	for _, p := range f.Passes {
		if !now.Before(p.ExpiresAt) {
			stats.PassesExpired++
			continue
		}
		wr.passes.set(p.Token, passEntry{expiresAt: p.ExpiresAt})
		stats.PassesRestored++
	}

	return stats, nil
}

// validatePersistFile checks a decoded document before any state change.
func validatePersistFile(f *persistFile) error {
	if f.Version != persistFormatVersion && f.Version != persistFormatLeases {
		return ErrImportFormat{Reason: fmt.Sprintf("unsupported version %d (want %d or %d)", f.Version, persistFormatVersion, persistFormatLeases)}
	}
	if f.Version == persistFormatVersion && len(f.Leases) > 0 {
		return ErrImportFormat{Reason: fmt.Sprintf("leases need version %d", persistFormatLeases)}
	}
	if f.ExportedAt.IsZero() {
		return ErrImportFormat{Reason: "missing exported_at"}
	}
	seen := make(map[string]struct{}, len(f.Tickets))
	keys := make(map[string]struct{})
	ids := make(map[string]struct{})
	for i, t := range f.Tickets {
		if t.Token == "" || len(t.Token) > maxPersistedTokenLen {
			return ErrImportFormat{Reason: fmt.Sprintf("ticket %d: invalid token", i)}
		}
		if _, dup := seen[t.Token]; dup {
			return ErrImportFormat{Reason: fmt.Sprintf("ticket %d: duplicate token", i)}
		}
		seen[t.Token] = struct{}{}
		if t.Rank < 0 {
			return ErrImportFormat{Reason: fmt.Sprintf("ticket %d: negative rank", i)}
		}
		if t.Key != "" {
			if validKey(t.Key) != nil {
				return ErrImportFormat{Reason: fmt.Sprintf("ticket %d: invalid key", i)}
			}
			if _, dup := keys[t.Key]; dup {
				return ErrImportFormat{Reason: fmt.Sprintf("ticket %d: duplicate key", i)}
			}
			keys[t.Key] = struct{}{}
		}
		if t.ID != "" {
			if len(t.ID) > maxPersistedTokenLen {
				return ErrImportFormat{Reason: fmt.Sprintf("ticket %d: invalid id", i)}
			}
			if _, dup := ids[t.ID]; dup {
				return ErrImportFormat{Reason: fmt.Sprintf("ticket %d: duplicate id", i)}
			}
			ids[t.ID] = struct{}{}
		}
	}
	for i, l := range f.Leases {
		if validKey(l.Key) != nil {
			return ErrImportFormat{Reason: fmt.Sprintf("lease %d: invalid key", i)}
		}
		if _, dup := keys[l.Key]; dup {
			return ErrImportFormat{Reason: fmt.Sprintf("lease %d: key is in line or leased twice", i)}
		}
		keys[l.Key] = struct{}{}
	}
	for i, p := range f.Passes {
		if p.Token == "" || len(p.Token) > maxPersistedTokenLen {
			return ErrImportFormat{Reason: fmt.Sprintf("pass %d: invalid token", i)}
		}
	}
	return nil
}

// setMany inserts many entries under one write lock.
func (ts *tokenStore) setMany(entries map[string]ticketEntry) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	for token, e := range entries {
		ts.putLocked(token, e)
	}
}

// snapshotLive returns every pass that has not expired at now.
func (ps *passStore) snapshotLive(now time.Time) []persistedPass {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	out := make([]persistedPass, 0, len(ps.entries))
	for token, e := range ps.entries {
		if now.Before(e.expiresAt) {
			out = append(out, persistedPass{Token: token, ExpiresAt: e.expiresAt})
		}
	}
	return out
}

// snapshotLeases copies the held leases, in the order they were granted.
func (wr *WaitingRoom) snapshotLeases() []persistedLease {
	wr.leaseMu.RLock()
	defer wr.leaseMu.RUnlock()
	if len(wr.leases) == 0 {
		return nil
	}
	type held struct {
		key string
		l   leaseEntry
	}
	all := make([]held, 0, len(wr.leases))
	for k, l := range wr.leases {
		all = append(all, held{key: k, l: l})
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].l.ticket != all[j].l.ticket {
			return all[i].l.ticket < all[j].l.ticket
		}
		return all[i].key < all[j].key
	})
	out := make([]persistedLease, len(all))
	for i, h := range all {
		out[i] = persistedLease{Key: h.key, AcquiredAt: h.l.acquiredAt, RenewedAt: h.l.renewedAt}
	}
	return out
}

// restoreLeases installs leases as tickets 1..L, already admitted. Their
// times move forward by the downtime, so it is not charged against a
// lease TTL. When L exceeds the room's configured capacity, the semaphore
// temporarily expands to absorb all L slots and immediately shrinks back
// to cap (sema v1.2.0 preserves held slots on shrink), keeping wr.Len(),
// wr.Held(), and wr.occupancy identical.
func (wr *WaitingRoom) restoreLeases(leases []persistedLease, shift time.Duration) {
	if len(leases) == 0 {
		return
	}
	wr.leaseMu.Lock()
	for i, l := range leases {
		wr.leases[l.Key] = leaseEntry{
			ticket:     int64(i + 1),
			acquiredAt: l.AcquiredAt.Add(shift),
			renewedAt:  l.RenewedAt.Add(shift),
		}
	}
	wr.leaseMu.Unlock()

	curCap := int(wr.cap.Load())
	if len(leases) > curCap {
		_ = wr.sem.SetCap(len(leases))
		for range leases {
			wr.sem.TryAcquire()
			wr.occupancy.Add(1)
		}
		_ = wr.sem.SetCap(curCap)
		return
	}
	for range leases {
		wr.sem.TryAcquire()
		wr.occupancy.Add(1)
	}
}

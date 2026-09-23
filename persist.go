package room

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// persistFormatVersion is the version written by Export and the only one
// accepted by Import.
const persistFormatVersion = 1

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
}

// Export writes the current waiting line and live VIP passes to w as
// versioned JSON, so that a restarted process can restore them with
// Import.
//
// # What is written
//
// Every queued ticket, in line order (the order Queue returns), with its
// token, issue time, last-seen and last-poll times, client key, and
// seen/promoted/has-pass flags; and every unexpired VIP pass with its
// absolute expiry. Requests being actively served are not queued and are
// not written. Internal ticket numbers are not written — the order is the
// line.
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

	entries := wr.tokens.snapshotEntries()
	sort.Slice(entries, func(i, j int) bool { return lessQueued(entries[i], entries[j]) })

	f := persistFile{
		Version:    persistFormatVersion,
		ExportedAt: now,
		Tickets:    make([]persistedTicket, 0, len(entries)),
		Passes:     wr.passes.snapshotLive(now),
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
//   - Tickets, renumbered 1..n in their exported line order. The serving
//     window starts empty, so the first Cap() restored clients are ready
//     on their next poll; a capacity different from the exporting
//     process's is handled naturally.
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

	// The room must be pristine.
	if wr.tokens.len() != 0 || wr.passes.len() != 0 ||
		wr.nowServing.Load() != 0 || wr.ledger.len() != 0 {
		return ImportStats{}, ErrImportNotEmpty{}
	}
	// Claim ticket numbers 1..n atomically; fails if any ticket was
	// issued since Init.
	if !wr.nextTicket.CompareAndSwap(0, int64(len(kept))) {
		return ImportStats{}, ErrImportNotEmpty{}
	}

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
			ticket:    int64(i + 1),
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
	if f.Version != persistFormatVersion {
		return ErrImportFormat{Reason: fmt.Sprintf("unsupported version %d (want %d)", f.Version, persistFormatVersion)}
	}
	if f.ExportedAt.IsZero() {
		return ErrImportFormat{Reason: "missing exported_at"}
	}
	seen := make(map[string]struct{}, len(f.Tickets))
	for i, t := range f.Tickets {
		if t.Token == "" || len(t.Token) > maxPersistedTokenLen {
			return ErrImportFormat{Reason: fmt.Sprintf("ticket %d: invalid token", i)}
		}
		if _, dup := seen[t.Token]; dup {
			return ErrImportFormat{Reason: fmt.Sprintf("ticket %d: duplicate token", i)}
		}
		seen[t.Token] = struct{}{}
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
		ts.entries[token] = e
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

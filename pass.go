package room

import (
	"sync"
	"time"
)

// passEntry holds a time-limited VIP pass issued after a skip-the-line
// purchase. Passes outlive individual queue tickets and auto-promote their
// holder on re-entry.
type passEntry struct {
	expiresAt time.Time
}

// passStore maps VIP pass tokens (from the room_pass cookie) to expiry times.
type passStore struct {
	mu      sync.RWMutex
	entries map[string]passEntry
}

func newPassStore() *passStore {
	return &passStore{
		entries: make(map[string]passEntry),
	}
}

func (ps *passStore) set(token string, entry passEntry) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.entries[token] = entry
}

// get reports whether token exists and has not expired, lazily deleting
// expired entries on read.
func (ps *passStore) get(token string) (passEntry, bool) {
	entry, ok, _ := ps.lookup(token)
	return entry, ok
}

// lookup returns the pass entry, whether it is valid, and a rejection reason
// ("expired" or "unknown") when invalid.
func (ps *passStore) lookup(token string) (passEntry, bool, string) {
	now := time.Now()

	ps.mu.RLock()
	entry, ok := ps.entries[token]
	ps.mu.RUnlock()

	if !ok {
		return passEntry{}, false, RejectReasonUnknownKey
	}
	if !now.After(entry.expiresAt) {
		return entry, true, ""
	}

	ps.mu.Lock()
	if cur, still := ps.entries[token]; still && now.After(cur.expiresAt) {
		delete(ps.entries, token)
	}
	ps.mu.Unlock()
	return passEntry{}, false, RejectReasonExpired
}

func (ps *passStore) delete(token string) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	delete(ps.entries, token)
}

func (ps *passStore) len() int {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	return len(ps.entries)
}

func (ps *passStore) reap() {
	now := time.Now()
	ps.mu.Lock()
	defer ps.mu.Unlock()
	for token, entry := range ps.entries {
		if now.After(entry.expiresAt) {
			delete(ps.entries, token)
		}
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

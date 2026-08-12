// Package toolstore keeps the full bodies of tool results that aggressive context
// truncation elides, retrievable by the truncated marker's hash. Truncation becomes
// practically lossless: the model still sees a compact note, but the whole output is
// one fetch away instead of gone, and re-running the tool (which may have side effects
// or return a different result) is no longer the only way back to it.
package toolstore

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
)

// Entry is one stored tool result.
type Entry struct {
	// ID is the full (hex) SHA-256 of the result body — the same hash the truncation
	// marker carries.
	ID string
	// Tool is the name of the tool that produced the result, kept for diagnostics.
	Tool string
	// ToolUseID is the caller's tool_use id that this result answers, for tracing.
	ToolUseID string
	// Body is the complete, original tool output.
	Body string
}

// Store is a bounded, thread-safe cache of elided tool-result bodies keyed by their
// SHA-256. It is intentionally a small LRU in memory: the bodies are already out of the
// request path, they exist to be fetched back once or twice, and they must never become
// a second copy of the whole conversation sitting on disk.
type Store struct {
	mu       sync.Mutex
	maxBytes int
	entries  map[string]Entry
	order    []string
	bytes    int
}

// New returns a Store that keeps at most maxBytes of body, evicting least-recently
// fetched entries first. A non-positive maxBytes keeps everything (used by tests).
func New(maxBytes int) *Store {
	return &Store{maxBytes: maxBytes, entries: make(map[string]Entry)}
}

// Put stores one elided result under its content hash. A body that is already present
// is refreshed (moved to the front of the LRU) rather than duplicated.
func (s *Store) Put(id string, tool, toolUseID, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.entries[id]; ok {
		s.touch(id)
		return
	}
	s.entries[id] = Entry{ID: id, Tool: tool, ToolUseID: toolUseID, Body: body}
	s.order = append(s.order, id)
	s.bytes += len(body)
	s.evictLocked()
}

// Get returns the stored body for id, or ("", false). A hit refreshes the LRU order so
// an entry that keeps being fetched is not evicted between turns.
func (s *Store) Get(id string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[id]
	if !ok {
		return "", false
	}
	s.touch(id)
	return entry.Body, true
}

// touch moves id to the end of the LRU order. The caller holds s.mu. The byte count is
// untouched: the body was already counted when it was put, so a refresh must not
// inflate it.
func (s *Store) touch(id string) {
	for index, key := range s.order {
		if key == id {
			s.order = append(s.order[:index], s.order[index+1:]...)
			break
		}
	}
	s.order = append(s.order, id)
}

// evictLocked drops the least-recently-fetched entries until the store is back under
// maxBytes. The caller holds s.mu.
func (s *Store) evictLocked() {
	if s.maxBytes <= 0 {
		return
	}
	for s.bytes > s.maxBytes && len(s.order) > 0 {
		id := s.order[0]
		s.order = s.order[1:]
		if entry, ok := s.entries[id]; ok {
			s.bytes -= len(entry.Body)
			delete(s.entries, id)
		}
	}
}

// Size reports the number of stored entries, for metrics and tests.
func (s *Store) Size() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

// ID computes the content-hash key for a body — the same value the truncation marker
// records and a /ref/<id> fetch addresses.
func ID(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

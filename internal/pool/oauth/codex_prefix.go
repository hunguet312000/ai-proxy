package oauth

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"sync"
	"time"
)

// The upstream prompt cache can only be reused while the front of the payload stays
// byte-identical. LiteRouter builds that front deterministically, but the client
// composes it: if it rewrites its system prompt or tool definitions mid-session,
// every later turn re-pays for the entire context and nothing in the token numbers
// says why. A conversation running at 13% cache hit and one running at 60% look
// identical in the usage table.
//
// So the change is detected where it happens. Nothing is logged while a session
// behaves; a single warning fires the moment a prefix that should be frozen moves.
const (
	codexPrefixTTL        = 6 * time.Hour
	codexPrefixMaxEntries = 2048
)

type codexPrefixEntry struct {
	hash      string
	expiresAt time.Time
}

type codexPrefixTracker struct {
	mu      sync.Mutex
	entries map[string]codexPrefixEntry
}

var codexPrefixes = codexPrefixTracker{entries: make(map[string]codexPrefixEntry)}

// codexPrefixHash fingerprints the parts of a Codex payload that must not change
// across the turns of one conversation: the instructions block and the tool
// definitions. Both sit ahead of the conversation itself, so a change to either
// invalidates the cached prefix in full.
func codexPrefixHash(payload map[string]any) string {
	hash := sha256.New()
	if instructions, ok := payload["instructions"].(string); ok {
		_, _ = hash.Write([]byte(instructions))
	}
	hash.Write([]byte{0})
	if tools, ok := payload["tools"]; ok {
		// Marshalling is deterministic for these values, so equal tool sets hash equal.
		if encoded, err := json.Marshal(tools); err == nil {
			_, _ = hash.Write(encoded)
		}
	}
	return hex.EncodeToString(hash.Sum(nil)[:12])
}

// observe records the prefix for a session and reports whether it changed from the
// value seen before. A first sighting never counts as a change.
func (t *codexPrefixTracker) observe(sessionKey, hash string, now time.Time) (previous string, changed bool) {
	if sessionKey == "" || hash == "" {
		return "", false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for key, entry := range t.entries {
		if !entry.expiresAt.After(now) {
			delete(t.entries, key)
		}
	}
	if len(t.entries) >= codexPrefixMaxEntries {
		var oldestKey string
		var oldest time.Time
		for key, entry := range t.entries {
			if oldestKey == "" || entry.expiresAt.Before(oldest) {
				oldestKey, oldest = key, entry.expiresAt
			}
		}
		delete(t.entries, oldestKey)
	}
	entry, existed := t.entries[sessionKey]
	t.entries[sessionKey] = codexPrefixEntry{hash: hash, expiresAt: now.Add(codexPrefixTTL)}
	if !existed || entry.hash == hash {
		return entry.hash, false
	}
	return entry.hash, true
}

// warnOnCodexPrefixChange emits one warning per prefix change. It is deliberately
// quiet otherwise: the absence of these lines is the evidence that the prefix is
// stable and that a low cache-hit rate has to be explained upstream instead.
func warnOnCodexPrefixChange(payload map[string]any, sessionKey, model string) {
	hash := codexPrefixHash(payload)
	previous, changed := codexPrefixes.observe(sessionKey, hash, time.Now())
	if !changed {
		return
	}
	slog.Warn("codex cacheable prefix changed mid-conversation; the upstream prompt cache cannot be reused",
		"model", model, "previous_prefix", previous, "current_prefix", hash)
}

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
	hash string
	// The components behind hash, kept so a change can name what moved. The combined
	// hash alone proves the cache was lost but not which half of the prefix caused it,
	// and those have different causes: instructions move when the client rewrites its
	// system prompt or injects a per-turn reminder, tools move when the tool set
	// changes. Sizes rather than content — the prefix is the whole system prompt.
	parts     codexPrefixParts
	expiresAt time.Time
}

// codexPrefixParts is the prefix broken into the pieces that can move independently.
type codexPrefixParts struct {
	instructionHash  string
	instructionBytes int
	toolHash         string
	toolCount        int
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
	parts := codexPrefixComponents(payload)
	hash := sha256.New()
	_, _ = hash.Write([]byte(parts.instructionHash))
	hash.Write([]byte{0})
	_, _ = hash.Write([]byte(parts.toolHash))
	return hex.EncodeToString(hash.Sum(nil)[:12])
}

// codexPrefixComponents fingerprints the two halves of the prefix separately.
func codexPrefixComponents(payload map[string]any) codexPrefixParts {
	var parts codexPrefixParts
	instructions, _ := payload["instructions"].(string)
	parts.instructionBytes = len(instructions)
	instructionSum := sha256.Sum256([]byte(instructions))
	parts.instructionHash = hex.EncodeToString(instructionSum[:12])
	// Marshalling is deterministic for these values, so equal tool sets hash equal.
	encoded, err := json.Marshal(payload["tools"])
	if err != nil {
		encoded = nil
	}
	toolSum := sha256.Sum256(encoded)
	parts.toolHash = hex.EncodeToString(toolSum[:12])
	if tools, ok := payload["tools"].([]map[string]any); ok {
		parts.toolCount = len(tools)
	}
	return parts
}

// observe records the prefix for a session and reports whether it changed from the
// value seen before, along with the components it was made of. A first sighting never
// counts as a change.
func (t *codexPrefixTracker) observe(sessionKey, hash string, parts codexPrefixParts, now time.Time) (previous codexPrefixEntry, changed bool) {
	if sessionKey == "" || hash == "" {
		return codexPrefixEntry{}, false
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
	t.entries[sessionKey] = codexPrefixEntry{hash: hash, parts: parts, expiresAt: now.Add(codexPrefixTTL)}
	if !existed || entry.hash == hash {
		return entry, false
	}
	return entry, true
}

// warnOnCodexPrefixChange emits one warning per prefix change. It is deliberately
// quiet otherwise: the absence of these lines is the evidence that the prefix is
// stable and that a low cache-hit rate has to be explained upstream instead.
//
// It names which half moved. Without that the line proves only that the cache was
// lost, and the two halves are diagnosed differently — instructions move because the
// client rewrote its system prompt or the translation hoisted a per-turn block into
// it, tools move because the tool set itself changed.
func warnOnCodexPrefixChange(payload map[string]any, sessionKey, model string) {
	parts := codexPrefixComponents(payload)
	hash := codexPrefixHash(payload)
	previous, changed := codexPrefixes.observe(sessionKey, hash, parts, time.Now())
	if !changed {
		return
	}
	slog.Warn("codex cacheable prefix changed mid-conversation; the upstream prompt cache cannot be reused",
		"model", model, "previous_prefix", previous.hash, "current_prefix", hash,
		"instructions_changed", previous.parts.instructionHash != parts.instructionHash,
		"tools_changed", previous.parts.toolHash != parts.toolHash,
		"instruction_bytes", parts.instructionBytes,
		"previous_instruction_bytes", previous.parts.instructionBytes,
		"tool_count", parts.toolCount, "previous_tool_count", previous.parts.toolCount)
}

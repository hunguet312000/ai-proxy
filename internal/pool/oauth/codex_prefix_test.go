package oauth

import (
	"testing"
	"time"

	"literouter/internal/translator"
)

// observeHash is observe for the tracker tests, which are about the bookkeeping rather
// than the components: it supplies parts derived from the hash so a differing hash also
// differs in its parts, as it always does in production.
func (t *codexPrefixTracker) observeHash(sessionKey, hash string, now time.Time) (codexPrefixEntry, bool) {
	return t.observe(sessionKey, hash, codexPrefixParts{instructionHash: hash, toolHash: hash}, now)
}

func TestCodexPrefixHashIgnoresConversationGrowth(t *testing.T) {
	// Only instructions and tools are fingerprinted: those must stay frozen, while
	// the conversation itself is expected to grow every turn.
	base := map[string]any{
		"instructions": "you are a coding assistant",
		"tools":        []map[string]any{{"type": "function", "name": "Read"}},
		"input":        []map[string]any{{"type": "message", "role": "user"}},
	}
	grown := map[string]any{
		"instructions": "you are a coding assistant",
		"tools":        []map[string]any{{"type": "function", "name": "Read"}},
		"input": []map[string]any{
			{"type": "message", "role": "user"},
			{"type": "message", "role": "assistant"},
		},
	}
	if codexPrefixHash(base) != codexPrefixHash(grown) {
		t.Fatal("appending a turn changed the prefix hash")
	}
	changedTools := map[string]any{
		"instructions": "you are a coding assistant",
		"tools":        []map[string]any{{"type": "function", "name": "Write"}},
	}
	if codexPrefixHash(base) == codexPrefixHash(changedTools) {
		t.Fatal("a different tool set produced the same prefix hash")
	}
	changedInstructions := map[string]any{
		"instructions": "you are a different assistant",
		"tools":        []map[string]any{{"type": "function", "name": "Read"}},
	}
	if codexPrefixHash(base) == codexPrefixHash(changedInstructions) {
		t.Fatal("different instructions produced the same prefix hash")
	}
}

// The end-to-end property the tracker exists to watch: two turns of one conversation
// that differ only by a per-turn reminder must produce the same prefix. Measured against
// Claude Code 2.1.227, which injects those reminders as instruction-shaped messages —
// folding them into `instructions` moved the prefix and re-billed the system prompt
// every turn.
func TestCodexPrefixSurvivesAPerTurnReminder(t *testing.T) {
	turn := func(withReminder bool) map[string]any {
		messages := []translator.OpenAIMessage{
			{Role: "system", Content: "static system prompt"},
			{Role: "user", Content: "start the task"},
			{Role: "assistant", Content: "working"},
		}
		if withReminder {
			messages = append(messages, translator.OpenAIMessage{
				Role: "user", Content: "<system-reminder>Todo list: 1 item in progress.</system-reminder>"})
		}
		messages = append(messages, translator.OpenAIMessage{Role: "user", Content: "continue"})
		return openAIToCodexRequest(translator.OpenAIRequest{Messages: messages}, "gpt-5.6-luna")
	}

	plain, reminded := turn(false), turn(true)
	if codexPrefixHash(plain) != codexPrefixHash(reminded) {
		t.Fatalf("a per-turn reminder moved the cacheable prefix:\n%q\n%q",
			plain["instructions"], reminded["instructions"])
	}
	// It still has to reach the model, just later in the payload rather than in front.
	input, _ := reminded["input"].([]map[string]any)
	if len(input) != len(plain["input"].([]map[string]any))+1 {
		t.Fatalf("the reminder was dropped instead of moved: %d items", len(input))
	}
}

func TestCodexPrefixTrackerReportsOnlyRealChanges(t *testing.T) {
	tracker := codexPrefixTracker{entries: make(map[string]codexPrefixEntry)}
	now := time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC)

	if _, changed := tracker.observeHash("s1", "aaa", now); changed {
		t.Fatal("first sighting reported as a change")
	}
	if _, changed := tracker.observeHash("s1", "aaa", now.Add(time.Minute)); changed {
		t.Fatal("an unchanged prefix reported as a change")
	}
	previous, changed := tracker.observeHash("s1", "bbb", now.Add(2*time.Minute))
	if !changed || previous.hash != "aaa" {
		t.Fatalf("change not reported: previous=%q changed=%v", previous.hash, changed)
	}
	// The new value becomes the baseline, so the same break is not reported twice.
	if _, changed := tracker.observeHash("s1", "bbb", now.Add(3*time.Minute)); changed {
		t.Fatal("the same prefix reported as changed twice")
	}
	// Separate conversations must not be compared against each other.
	if _, changed := tracker.observeHash("s2", "ccc", now.Add(4*time.Minute)); changed {
		t.Fatal("a different session was compared against s1")
	}
	if _, changed := tracker.observeHash("", "ddd", now); changed {
		t.Fatal("an empty session key was tracked")
	}
}

func TestCodexPrefixTrackerIsBounded(t *testing.T) {
	tracker := codexPrefixTracker{entries: make(map[string]codexPrefixEntry)}
	now := time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC)
	for i := 0; i < codexPrefixMaxEntries+50; i++ {
		tracker.observeHash(string(rune('a'+i%26))+string(rune(i)), "hash", now.Add(time.Duration(i)*time.Second))
	}
	if len(tracker.entries) > codexPrefixMaxEntries {
		t.Fatalf("tracker grew to %d entries", len(tracker.entries))
	}
}

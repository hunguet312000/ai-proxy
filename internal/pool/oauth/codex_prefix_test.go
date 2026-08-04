package oauth

import (
	"testing"
	"time"
)

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

func TestCodexPrefixTrackerReportsOnlyRealChanges(t *testing.T) {
	tracker := codexPrefixTracker{entries: make(map[string]codexPrefixEntry)}
	now := time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC)

	if _, changed := tracker.observe("s1", "aaa", now); changed {
		t.Fatal("first sighting reported as a change")
	}
	if _, changed := tracker.observe("s1", "aaa", now.Add(time.Minute)); changed {
		t.Fatal("an unchanged prefix reported as a change")
	}
	previous, changed := tracker.observe("s1", "bbb", now.Add(2*time.Minute))
	if !changed || previous != "aaa" {
		t.Fatalf("change not reported: previous=%q changed=%v", previous, changed)
	}
	// The new value becomes the baseline, so the same break is not reported twice.
	if _, changed := tracker.observe("s1", "bbb", now.Add(3*time.Minute)); changed {
		t.Fatal("the same prefix reported as changed twice")
	}
	// Separate conversations must not be compared against each other.
	if _, changed := tracker.observe("s2", "ccc", now.Add(4*time.Minute)); changed {
		t.Fatal("a different session was compared against s1")
	}
	if _, changed := tracker.observe("", "ddd", now); changed {
		t.Fatal("an empty session key was tracked")
	}
}

func TestCodexPrefixTrackerIsBounded(t *testing.T) {
	tracker := codexPrefixTracker{entries: make(map[string]codexPrefixEntry)}
	now := time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC)
	for i := 0; i < codexPrefixMaxEntries+50; i++ {
		tracker.observe(string(rune('a'+i%26))+string(rune(i)), "hash", now.Add(time.Duration(i)*time.Second))
	}
	if len(tracker.entries) > codexPrefixMaxEntries {
		t.Fatalf("tracker grew to %d entries", len(tracker.entries))
	}
}

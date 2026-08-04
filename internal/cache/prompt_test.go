package cache

import (
	"strings"
	"testing"
)

func TestApplyPromptCacheAnthropic(t *testing.T) {
	messages := []PromptMessage{
		{Role: "system", Content: "short"},
		{Role: "user", Content: "short"},
		{Role: "user", Content: strings.Repeat("x", DefaultLongMessageBytes)},
	}
	result := ApplyPromptCache(ProviderAnthropic, "model", messages, 0)
	if !result.Messages[0].CacheControl || result.Messages[1].CacheControl || !result.Messages[2].CacheControl {
		t.Fatalf("messages = %#v", result.Messages)
	}
	if messages[0].CacheControl {
		t.Fatal("input mutated")
	}
}

func TestApplyPromptCacheOpenAISticksToFirstUserMessage(t *testing.T) {
	messages := []PromptMessage{{Role: "user", Content: "hello"}}
	first := ApplyPromptCache(ProviderOpenAI, "gpt-4o-mini", messages, 0)
	grown := ApplyPromptCache(ProviderOpenAI, "gpt-4o-2026", append(messages, PromptMessage{Role: "assistant", Content: "done"}), 0)
	changed := ApplyPromptCache(ProviderOpenAI, "o3", messages, 0)
	if len(first.PromptCacheKey) != 64 || first.PromptCacheKey != grown.PromptCacheKey || first.PromptCacheKey == changed.PromptCacheKey {
		t.Fatalf("keys = %q, %q, %q", first.PromptCacheKey, grown.PromptCacheKey, changed.PromptCacheKey)
	}
}

func TestStickyPromptCacheKeyPriorityAndPrivacy(t *testing.T) {
	conversation := StickyPromptCacheKey("gpt-4.1-mini", "private-conversation", "person@example.com", "secret prompt")
	fromUser := StickyPromptCacheKey("gpt-4.1", "", "person@example.com", "secret prompt")
	fromMessage := StickyPromptCacheKey("gpt-4.1", "", "", "secret prompt")
	if conversation == fromUser || fromUser == fromMessage || len(conversation) != 64 {
		t.Fatalf("keys = %q, %q, %q", conversation, fromUser, fromMessage)
	}
	for _, raw := range []string{"private-conversation", "person@example.com", "secret prompt"} {
		if strings.Contains(conversation, raw) || strings.Contains(fromUser, raw) || strings.Contains(fromMessage, raw) {
			t.Fatalf("raw value %q leaked in key", raw)
		}
	}
}

func TestApplyPromptCacheGrokPreservesRequest(t *testing.T) {
	messages := []PromptMessage{{Role: "system", Content: "system"}}
	result := ApplyPromptCache(ProviderGrok, "model", messages, 1)
	if result.PromptCacheKey != "" || result.Messages[0].CacheControl {
		t.Fatalf("result = %#v", result)
	}
}

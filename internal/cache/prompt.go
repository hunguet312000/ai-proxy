package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

const DefaultLongMessageBytes = 4 * 1024

type PromptProvider string

const (
	ProviderOpenAI    PromptProvider = "openai"
	ProviderAnthropic PromptProvider = "anthropic"
	ProviderGrok      PromptProvider = "grok"
)

type PromptMessage struct {
	Role         string
	Content      string
	CacheControl bool
}

type PromptCacheResult struct {
	Messages       []PromptMessage
	PromptCacheKey string
}

func ApplyPromptCache(provider PromptProvider, model string, messages []PromptMessage, longMessageBytes int) PromptCacheResult {
	if longMessageBytes <= 0 {
		longMessageBytes = DefaultLongMessageBytes
	}
	result := PromptCacheResult{Messages: append([]PromptMessage(nil), messages...)}
	switch provider {
	case ProviderAnthropic:
		for index := range result.Messages {
			message := &result.Messages[index]
			if message.Role == "system" || len(message.Content) >= longMessageBytes {
				message.CacheControl = true
			}
		}
	case ProviderOpenAI:
		result.PromptCacheKey = promptKey(model, messages)
	case ProviderGrok:
		// Grok has no stable explicit cache-control contract; preserve request unchanged.
	}
	return result
}

func StickyPromptCacheKey(model, conversationID, user, firstUserMessage string) string {
	seed := conversationID
	if seed == "" {
		seed = user
	}
	if seed == "" {
		seed = firstUserMessage
	}
	if seed == "" {
		return ""
	}
	hash := sha256.Sum256([]byte(modelFamily(model) + "\x00" + seed))
	return hex.EncodeToString(hash[:])
}

func promptKey(model string, messages []PromptMessage) string {
	for _, message := range messages {
		if message.Role == "user" && message.Content != "" {
			return StickyPromptCacheKey(model, "", "", message.Content)
		}
	}
	return ""
}

func modelFamily(model string) string {
	model = strings.ToLower(model)
	for _, family := range []string{"gpt-4.1", "gpt-4o", "gpt-4", "gpt-3.5", "chatgpt", "grok", "o1", "o3", "o4"} {
		if model == family || strings.HasPrefix(model, family+"-") {
			return family
		}
	}
	if index := strings.IndexAny(model, "@:"); index >= 0 {
		model = model[:index]
	}
	return model
}

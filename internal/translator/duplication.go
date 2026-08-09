package translator

import (
	"crypto/sha256"
)

// PromptDuplication measures how much of a request is content the model has already
// been shown verbatim earlier in the same request.
//
// This exists to answer a question before acting on it: collapsing repeated tool
// results is the one remaining large token saving available on providers with no
// server-side conversation state, but it rewrites what the model reads. Measuring the
// duplication first turns "this should help" into a number, and a number is what
// decides whether rewriting the prompt is worth any risk at all.
type PromptDuplication struct {
	// ToolBytes is the total size of tool-result content in the request.
	ToolBytes int
	// DuplicateToolBytes is the part of that which repeats content appearing again
	// later in the same request, byte for byte. Only exact repeats are counted: a
	// file read twice with different content is not duplication, it is history.
	DuplicateToolBytes int
	// TotalBytes is the size of all message content, for context.
	TotalBytes int
	// DuplicateResults counts the individual tool results that repeat.
	DuplicateResults int
}

// Ratio is the share of message content that is exactly-repeated tool output.
func (d PromptDuplication) Ratio() float64 {
	if d.TotalBytes <= 0 {
		return 0
	}
	return float64(d.DuplicateToolBytes) / float64(d.TotalBytes)
}

// MeasurePromptDuplication reports repeated tool output in a request.
//
// A result counts as duplicate only when an identical one appears *later*: the last
// occurrence is what the model would keep, so the earlier copies are the redundant
// ones. Nothing is modified here.
func MeasurePromptDuplication(request OpenAIRequest) PromptDuplication {
	var out PromptDuplication
	type occurrence struct {
		last int
		size int
	}
	seen := make(map[[32]byte]occurrence, len(request.Messages))

	for index, message := range request.Messages {
		text := openAIMessageText(message.Content)
		out.TotalBytes += len(text)
		if message.Role != "tool" || text == "" {
			continue
		}
		out.ToolBytes += len(text)
		digest := sha256.Sum256([]byte(text))
		seen[digest] = occurrence{last: index, size: len(text)}
	}
	for index, message := range request.Messages {
		if message.Role != "tool" {
			continue
		}
		text := openAIMessageText(message.Content)
		if text == "" {
			continue
		}
		digest := sha256.Sum256([]byte(text))
		if last, ok := seen[digest]; ok && last.last > index {
			out.DuplicateToolBytes += len(text)
			out.DuplicateResults++
		}
	}
	return out
}

// openAIMessageText flattens message content to text for comparison. It mirrors the
// gateway's own reading of content so the measurement matches what is actually sent.
func openAIMessageText(content any) string {
	switch typed := content.(type) {
	case string:
		return typed
	case []any:
		var out []byte
		for _, part := range typed {
			block, ok := part.(map[string]any)
			if !ok {
				continue
			}
			if text, ok := block["text"].(string); ok {
				out = append(out, text...)
			}
		}
		return string(out)
	case nil:
		return ""
	default:
		return ""
	}
}

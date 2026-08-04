package contextguard

import (
	"encoding/base64"
	"encoding/json"
	"unicode/utf8"

	"literouter/internal/provider"
)

func EstimateText(text string) int {
	if text == "" {
		return 0
	}
	bytes := len(text)
	runes := utf8.RuneCountInString(text)
	byBytes := (bytes + 3) / 4
	byRunes := (runes + 2) / 3
	if byRunes > byBytes {
		return byRunes
	}
	return byBytes
}

func EstimateRequest(request provider.Request) int {
	tokens := 8 + EstimateText(request.Model)
	for _, block := range request.System {
		tokens += estimateContent(block) + 4
	}
	for _, message := range request.Messages {
		tokens += 6 + EstimateText(message.Role)
		for _, block := range message.Content {
			tokens += estimateContent(block) + 4
		}
	}
	for _, tool := range request.Tools {
		tokens += 12 + EstimateText(tool.Name) + EstimateText(tool.Description) + EstimateText(string(tool.InputSchema))
	}
	if request.ToolChoice.Type != "" {
		tokens += 4 + EstimateText(request.ToolChoice.Type) + EstimateText(request.ToolChoice.Name)
	}
	return tokens
}

func estimateContent(block provider.Content) int {
	tokens := EstimateText(block.Type) + EstimateText(block.Text) + EstimateText(block.Thinking)
	if block.Type == "image" {
		tokens += estimateImage(block)
	} else {
		tokens += EstimateText(block.Data)
	}
	tokens += EstimateText(block.MediaType) + EstimateText(block.URL) + EstimateText(block.ToolUseID) + EstimateText(block.Name)
	if len(block.Input) > 0 {
		var compact json.RawMessage
		if json.Unmarshal(block.Input, &compact) == nil {
			tokens += EstimateText(string(block.Input))
		}
	}
	return tokens
}

// Image bytes are transport data, not language tokens. Providers tokenize
// vision independently; this conservative allowance covers common screenshots.
func estimateImage(block provider.Content) int {
	if block.URL != "" {
		return 2_000
	}
	if _, err := base64.StdEncoding.DecodeString(block.Data); err != nil {
		return max(EstimateText(block.Data), 2_000)
	}
	return 2_000
}

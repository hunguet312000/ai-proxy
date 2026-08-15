package provider

import (
	"encoding/json"
	"fmt"

	"literouter/internal/storage"
)

type Request struct {
	Model               string
	Messages            []Message
	System              []Content
	Tools               []Tool
	ToolChoice          ToolChoice
	ParallelToolCalls   *bool
	Temperature         *float64
	TopP                *float64
	Seed                *int64
	Stop                any
	ResponseFormat      json.RawMessage
	N                   int
	PresencePenalty     *float64
	FrequencyPenalty    *float64
	User                string
	MaxTokens           int
	MaxCompletionTokens int
	Stream              bool
	Effort              string
	Metadata            map[string]string
}

type Message struct {
	Role    string
	Content []Content
}

type Content struct {
	Type      string
	Text      string
	Thinking  string
	Data      string
	MediaType string
	URL       string
	ToolUseID string
	Name      string
	Input     json.RawMessage
	IsError   bool
	// CacheControl carries the Anthropic prompt-cache breakpoint. Dropping it made
	// every turn re-process the whole prefix at full price, so it must survive any
	// round-trip that ends up back on an Anthropic-native upstream.
	CacheControl map[string]string
}

type Tool struct {
	Name         string
	Description  string
	InputSchema  json.RawMessage
	Strict       *bool
	CacheControl map[string]string
}

type ToolChoice struct {
	Type string
	Name string
}

type Response struct {
	ID         string
	Model      string
	Role       string
	Content    []Content
	StopReason string
	Usage      Usage
}

type Usage struct {
	InputTokens         int
	OutputTokens        int
	CacheReadTokens     int
	CacheCreationTokens int
}

type StreamEvent struct {
	Type          string
	Response      *Response
	Index         int
	Content       *Content
	TextDelta     string
	ThinkingDelta string
	JSONDelta     string
	StopReason    string
	Usage         *Usage
	Err           error
}

func (request Request) Validate() error {
	if request.Model == "" {
		return fmt.Errorf("model is required")
	}
	if len(request.Messages) == 0 {
		return fmt.Errorf("messages are required")
	}
	if request.MaxTokens < 0 {
		return fmt.Errorf("max tokens cannot be negative")
	}
	switch request.Effort {
	case "", "low", "medium", "high", "xhigh", "max", storage.EffortOff:
	default:
		return fmt.Errorf("unsupported effort %q", request.Effort)
	}
	for _, message := range request.Messages {
		switch message.Role {
		case "user", "assistant", "tool":
		default:
			return fmt.Errorf("unsupported message role %q", message.Role)
		}
		if len(message.Content) == 0 {
			return fmt.Errorf("message content is required")
		}
	}
	for _, tool := range request.Tools {
		if tool.Name == "" || len(tool.InputSchema) == 0 || !json.Valid(tool.InputSchema) {
			return fmt.Errorf("tool name and valid input schema are required")
		}
	}
	return nil
}

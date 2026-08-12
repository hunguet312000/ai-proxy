package translator

import (
	"encoding/json"
	"fmt"

	"literouter/internal/provider"
)

type AnthropicRequest struct {
	Model        string                `json:"model"`
	System       []AnthropicContent    `json:"system,omitempty"`
	Messages     []AnthropicMessage    `json:"messages"`
	Tools        []AnthropicTool       `json:"tools,omitempty"`
	ToolChoice   *AnthropicToolChoice  `json:"tool_choice,omitempty"`
	Temperature  *float64              `json:"temperature,omitempty"`
	MaxTokens    int                   `json:"max_tokens"`
	Stream       bool                  `json:"stream,omitempty"`
	OutputConfig AnthropicOutputConfig `json:"output_config,omitempty"`
}

type AnthropicOutputConfig struct {
	Effort string `json:"effort,omitempty"`
}

func (request *AnthropicRequest) UnmarshalJSON(data []byte) error {
	type alias AnthropicRequest
	var raw struct {
		*alias
		System json.RawMessage `json:"system"`
	}
	raw.alias = (*alias)(request)
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if len(raw.System) > 0 {
		return unmarshalAnthropicContent(raw.System, &request.System)
	}
	return nil
}

type AnthropicMessage struct {
	Role    string             `json:"role"`
	Content []AnthropicContent `json:"content"`
}

func (message *AnthropicMessage) UnmarshalJSON(data []byte) error {
	var raw struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	message.Role = raw.Role
	return unmarshalAnthropicContent(raw.Content, &message.Content)
}

type AnthropicContent struct {
	Type         string            `json:"type"`
	Text         string            `json:"text,omitempty"`
	Thinking     string            `json:"thinking,omitempty"`
	Data         string            `json:"data,omitempty"`
	ID           string            `json:"id,omitempty"`
	Name         string            `json:"name,omitempty"`
	Input        json.RawMessage   `json:"input,omitempty"`
	ToolUseID    string            `json:"tool_use_id,omitempty"`
	Content      any               `json:"content,omitempty"`
	IsError      bool              `json:"is_error,omitempty"`
	Source       *AnthropicSource  `json:"source,omitempty"`
	CacheControl map[string]string `json:"cache_control,omitempty"`
}

type AnthropicSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

type AnthropicTool struct {
	Name         string            `json:"name"`
	Description  string            `json:"description,omitempty"`
	InputSchema  json.RawMessage   `json:"input_schema"`
	CacheControl map[string]string `json:"cache_control,omitempty"`
}

type AnthropicToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
}

type AnthropicResponse struct {
	ID         string             `json:"id"`
	Type       string             `json:"type"`
	Model      string             `json:"model"`
	Role       string             `json:"role"`
	Content    []AnthropicContent `json:"content"`
	StopReason string             `json:"stop_reason"`
	Usage      AnthropicUsage     `json:"usage"`
}

type AnthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

func FromAnthropicRequest(request AnthropicRequest) (provider.Request, error) {
	result := provider.Request{
		Model: request.Model, Temperature: request.Temperature, MaxTokens: request.MaxTokens, Stream: request.Stream,
		Effort: request.OutputConfig.Effort,
	}
	var err error
	result.System, err = fromAnthropicContent(request.System)
	if err != nil {
		return provider.Request{}, err
	}
	for _, message := range request.Messages {
		content, err := fromAnthropicContent(message.Content)
		if err != nil {
			return provider.Request{}, err
		}
		if message.Role == "system" || message.Role == "developer" {
			// Only instruction messages that open the request join the System block.
			// System is translated into the front of the upstream payload — for Codex
			// the `instructions` string, which is the byte range the prompt cache is
			// keyed on — so hoisting a block that arrives mid-conversation moves the
			// cacheable prefix and the whole system prompt is charged again. Claude Code
			// injects its per-turn reminders exactly there.
			//
			// A later one is carried as a user message instead: it keeps the position the
			// client chose, and it is the shape Anthropic uses for the same thing
			// anyway — reminders normally arrive as text blocks on a user message.
			if len(result.Messages) == 0 {
				result.System = append(result.System, content...)
				continue
			}
			result.Messages = append(result.Messages, provider.Message{Role: "user", Content: content})
			continue
		}
		result.Messages = append(result.Messages, provider.Message{Role: message.Role, Content: content})
	}
	for _, tool := range request.Tools {
		result.Tools = append(result.Tools, provider.Tool{Name: tool.Name, Description: tool.Description, InputSchema: tool.InputSchema, CacheControl: tool.CacheControl})
	}
	if request.ToolChoice != nil {
		result.ToolChoice = provider.ToolChoice{Type: request.ToolChoice.Type, Name: request.ToolChoice.Name}
	}
	return result, result.Validate()
}

func ToAnthropicRequest(request provider.Request) (AnthropicRequest, error) {
	if err := request.Validate(); err != nil {
		return AnthropicRequest{}, err
	}
	result := AnthropicRequest{
		Model: request.Model, Temperature: request.Temperature, MaxTokens: request.MaxTokens, Stream: request.Stream,
		System: toAnthropicContent(request.System), OutputConfig: AnthropicOutputConfig{Effort: request.Effort},
	}
	for _, message := range request.Messages {
		role := message.Role
		if role == "tool" {
			role = "user"
		}
		content := toAnthropicContent(message.Content)
		if len(result.Messages) > 0 && role == "user" && result.Messages[len(result.Messages)-1].Role == "user" && allToolResults(content) {
			result.Messages[len(result.Messages)-1].Content = append(result.Messages[len(result.Messages)-1].Content, content...)
			continue
		}
		result.Messages = append(result.Messages, AnthropicMessage{Role: role, Content: content})
	}
	for _, tool := range request.Tools {
		result.Tools = append(result.Tools, AnthropicTool{Name: tool.Name, Description: tool.Description, InputSchema: tool.InputSchema, CacheControl: tool.CacheControl})
	}
	if request.ToolChoice.Type != "" {
		choiceType := request.ToolChoice.Type
		if choiceType == "required" {
			choiceType = "any"
		}
		result.ToolChoice = &AnthropicToolChoice{Type: choiceType, Name: request.ToolChoice.Name}
	}
	return result, nil
}

func FromAnthropicResponse(response AnthropicResponse) (provider.Response, error) {
	content, err := fromAnthropicContent(response.Content)
	if err != nil {
		return provider.Response{}, err
	}
	return provider.Response{
		ID: response.ID, Model: response.Model, Role: response.Role, Content: content, StopReason: response.StopReason,
		Usage: provider.Usage{
			// provider.Usage.InputTokens is the whole prompt, cached part included — the
			// OpenAI convention, which is what the usage recorder and every OpenAI-shaped
			// translation already assume. Anthropic splits it the other way, reporting the
			// uncached remainder, so the cache counts are added back in here rather than
			// leaving InputTokens meaning one thing on one upstream and another elsewhere.
			InputTokens:     response.Usage.InputTokens + response.Usage.CacheReadInputTokens + response.Usage.CacheCreationInputTokens,
			OutputTokens:    response.Usage.OutputTokens,
			CacheReadTokens: response.Usage.CacheReadInputTokens, CacheCreationTokens: response.Usage.CacheCreationInputTokens,
		},
	}, nil
}

func ToAnthropicResponse(response provider.Response) AnthropicResponse {
	content := toAnthropicContent(response.Content)
	if len(content) == 0 {
		// Claude Code expects a terminal text block for non-tool responses.
		content = []AnthropicContent{{Type: "text", Text: " "}}
	}
	return AnthropicResponse{
		ID: response.ID, Type: "message", Model: response.Model, Role: response.Role, Content: content, StopReason: response.StopReason,
		Usage: AnthropicUsage{
			// The inverse of FromAnthropicResponse: input_tokens on the wire is the uncached
			// remainder, because the client sums all three to size the conversation. Sending
			// the total here double-counted the cached part and made every session look 1.2x
			// to 2.0x larger than it was, which is what dragged auto-compact forward.
			InputTokens:          max(0, response.Usage.InputTokens-response.Usage.CacheReadTokens-response.Usage.CacheCreationTokens),
			OutputTokens:         response.Usage.OutputTokens,
			CacheReadInputTokens: response.Usage.CacheReadTokens, CacheCreationInputTokens: response.Usage.CacheCreationTokens,
		},
	}
}

func fromAnthropicContent(content []AnthropicContent) ([]provider.Content, error) {
	result := make([]provider.Content, 0, len(content))
	for _, block := range content {
		switch block.Type {
		case "text":
			if block.Text != "" {
				result = append(result, provider.Content{Type: "text", Text: block.Text, CacheControl: block.CacheControl})
			}
		case "thinking":
			result = append(result, provider.Content{Type: "thinking", Thinking: block.Thinking, CacheControl: block.CacheControl})
		case "redacted_thinking":
			result = append(result, provider.Content{Type: "redacted_thinking", Data: block.Data, CacheControl: block.CacheControl})
		case "image":
			if block.Source == nil {
				return nil, fmt.Errorf("Anthropic image source is required")
			}
			result = append(result, provider.Content{Type: "image", MediaType: block.Source.MediaType, Data: block.Source.Data, URL: block.Source.URL, CacheControl: block.CacheControl})
		case "tool_use":
			if len(block.Input) == 0 || !json.Valid(block.Input) {
				return nil, fmt.Errorf("tool use %q has invalid input", block.ID)
			}
			result = append(result, provider.Content{Type: "tool_use", ToolUseID: block.ID, Name: block.Name, Input: block.Input, CacheControl: block.CacheControl})
		case "tool_result":
			text, images, err := anthropicToolResultContent(block.Content)
			if err != nil {
				return nil, err
			}
			result = append(result, provider.Content{Type: "tool_result", ToolUseID: block.ToolUseID, Text: text, IsError: block.IsError, CacheControl: block.CacheControl})
			// Images inside a tool result are hoisted alongside it rather than left nested.
			// The Read tool returns them that way for any image file, and MCP screenshot
			// tools do the same, so this is the ordinary path for "look at this picture" —
			// not an edge case. Left nested they were dropped on the way to the OpenAI shape,
			// whose tool messages carry a plain string, and the model answered confidently
			// about an image it had never been shown.
			result = append(result, images...)
		default:
			return nil, fmt.Errorf("unsupported Anthropic content type %q", block.Type)
		}
	}
	return result, nil
}

func toAnthropicContent(content []provider.Content) []AnthropicContent {
	result := make([]AnthropicContent, 0, len(content))
	for _, block := range content {
		switch block.Type {
		case "text":
			result = append(result, AnthropicContent{Type: "text", Text: block.Text, CacheControl: block.CacheControl})
		case "thinking":
			result = append(result, AnthropicContent{Type: "thinking", Thinking: block.Thinking, CacheControl: block.CacheControl})
		case "redacted_thinking":
			result = append(result, AnthropicContent{Type: "redacted_thinking", Data: block.Data, CacheControl: block.CacheControl})
		case "image":
			source := &AnthropicSource{Type: "base64", MediaType: block.MediaType, Data: block.Data}
			if block.URL != "" {
				source = &AnthropicSource{Type: "url", URL: block.URL}
			}
			result = append(result, AnthropicContent{Type: "image", Source: source, CacheControl: block.CacheControl})
		case "tool_use":
			result = append(result, AnthropicContent{Type: "tool_use", ID: block.ToolUseID, Name: block.Name, Input: block.Input, CacheControl: block.CacheControl})
		case "tool_result":
			result = append(result, AnthropicContent{Type: "tool_result", ToolUseID: block.ToolUseID, Content: block.Text, IsError: block.IsError, CacheControl: block.CacheControl})
		}
	}
	return result
}

func unmarshalAnthropicContent(data []byte, target *[]AnthropicContent) error {
	var text string
	if err := json.Unmarshal(data, &text); err == nil {
		*target = []AnthropicContent{{Type: "text", Text: text}}
		return nil
	}
	if err := json.Unmarshal(data, target); err != nil {
		return fmt.Errorf("decode Anthropic content: %w", err)
	}
	return nil
}

func allToolResults(content []AnthropicContent) bool {
	if len(content) == 0 {
		return false
	}
	for _, block := range content {
		if block.Type != "tool_result" {
			return false
		}
	}
	return true
}

func anthropicContentText(content any) (string, error) {
	text, _, err := anthropicToolResultContent(content)
	return text, err
}

// anthropicToolResultContent splits a tool result into the text it carries and any images
// nested inside it.
//
// The split exists because the two halves have to travel differently: the text belongs in
// the tool result, and an image cannot stay there at all once the request is translated.
func anthropicToolResultContent(content any) (string, []provider.Content, error) {
	switch value := content.(type) {
	case nil:
		return "", nil, nil
	case string:
		return value, nil, nil
	case []any:
		encoded, err := json.Marshal(value)
		if err != nil {
			return "", nil, err
		}
		var blocks []AnthropicContent
		if err := json.Unmarshal(encoded, &blocks); err != nil {
			return "", nil, err
		}
		var text string
		var images []provider.Content
		for _, block := range blocks {
			switch block.Type {
			case "text":
				text += block.Text
			case "image":
				if block.Source == nil {
					return "", nil, fmt.Errorf("Anthropic image source is required")
				}
				images = append(images, provider.Content{
					Type: "image", MediaType: block.Source.MediaType,
					Data: block.Source.Data, URL: block.Source.URL,
				})
			}
		}
		return text, images, nil
	default:
		return "", nil, fmt.Errorf("unsupported Anthropic tool result content %T", content)
	}
}

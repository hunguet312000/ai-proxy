package translator

import (
	"encoding/json"
	"fmt"
	"strings"

	"literouter/internal/provider"
	"literouter/internal/storage"
)

type OpenAIRequest struct {
	Model               string               `json:"model"`
	Messages            []OpenAIMessage      `json:"messages"`
	Tools               []OpenAITool         `json:"tools,omitempty"`
	ToolChoice          any                  `json:"tool_choice,omitempty"`
	Temperature         *float64             `json:"temperature,omitempty"`
	MaxTokens           int                  `json:"max_tokens,omitempty"`
	MaxCompletionTokens int                  `json:"max_completion_tokens,omitempty"`
	Stream              bool                 `json:"stream,omitempty"`
	StreamOptions       *OpenAIStreamOptions `json:"stream_options,omitempty"`
	PromptCacheKey      string               `json:"prompt_cache_key,omitempty"`
	TopP                *float64             `json:"top_p,omitempty"`
	Seed                *int64               `json:"seed,omitempty"`
	Stop                any                  `json:"stop,omitempty"`
	ResponseFormat      json.RawMessage      `json:"response_format,omitempty"`
	N                   int                  `json:"n,omitempty"`
	PresencePenalty     *float64             `json:"presence_penalty,omitempty"`
	FrequencyPenalty    *float64             `json:"frequency_penalty,omitempty"`
	User                string               `json:"user,omitempty"`
	ParallelToolCalls   *bool                `json:"parallel_tool_calls,omitempty"`
	Effort              string               `json:"-"`
	// ReasoningEffort is the reasoning_effort sent to OpenAI-compatible upstreams
	// (opencode, FPT AI, any custom provider that supports thinking mode). It is separate
	// from Effort, which is the internal routing effort; serializing Effort directly would
	// break the codex/OAuth paths that read it off the struct. Set in ToOpenAIRequest.
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	// ForceNoEffort is the "off" override: the request must not carry
	// reasoning_effort at all, whatever the client asked for. It has to travel as
	// its own flag because an empty ReasoningEffort alone is ambiguous — it also
	// means "nothing decided yet" — and the unified-rebuild path would otherwise
	// resurrect the client's ask from the provider form.
	ForceNoEffort bool `json:"-"`
}

type OpenAIStreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

type OpenAIMessage struct {
	Role       string           `json:"role"`
	Content    any              `json:"content,omitempty"`
	ToolCalls  []OpenAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	Name       string           `json:"name,omitempty"`
	// Reasoning captures reasoning_content on non-stream responses. A thinking model
	// (GLM, DeepSeek) can finish having emitted only reasoning and no content; that
	// reasoning is its only answer and must not be dropped as empty.
	Reasoning string `json:"reasoning_content,omitempty"`
}

type OpenAIContentPart struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ImageURL  *OpenAIImageURL `json:"image_url,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   any             `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

type OpenAIImageURL struct {
	URL string `json:"url"`
}

type OpenAITool struct {
	Type     string         `json:"type"`
	Function OpenAIFunction `json:"function"`
}

type OpenAIFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      *bool           `json:"strict,omitempty"`
}

type OpenAIToolCall struct {
	ID               string             `json:"id"`
	Type             string             `json:"type"`
	Function         OpenAIFunctionCall `json:"function"`
	ThoughtSignature string             `json:"thought_signature,omitempty"`
}

type OpenAIFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type OpenAIResponse struct {
	ID      string         `json:"id"`
	Model   string         `json:"model"`
	Choices []OpenAIChoice `json:"choices"`
	Usage   OpenAIUsage    `json:"usage"`
}

type OpenAIChoice struct {
	Index        int           `json:"index"`
	Message      OpenAIMessage `json:"message"`
	Delta        OpenAIMessage `json:"delta"`
	FinishReason string        `json:"finish_reason"`
}

type OpenAIUsage struct {
	PromptTokens             int  `json:"prompt_tokens"`
	CompletionTokens         int  `json:"completion_tokens"`
	PromptTokensReported     bool `json:"-"`
	CompletionTokensReported bool `json:"-"`
	PromptTokensDetails      struct {
		CachedTokens         int  `json:"cached_tokens"`
		CachedTokensReported bool `json:"-"`
	} `json:"prompt_tokens_details"`
}

func (usage *OpenAIUsage) UnmarshalJSON(data []byte) error {
	type plain OpenAIUsage
	var raw struct {
		PromptTokens     *int `json:"prompt_tokens"`
		CompletionTokens *int `json:"completion_tokens"`
		// prompt_tokens_estimated marks a count the proxy produced itself because the
		// upstream reports none. It stays out of the "reported" flags so the dashboard
		// keeps showing it as an estimate rather than as the provider's own figure.
		PromptTokensEstimated *bool `json:"prompt_tokens_estimated"`
		PromptTokensDetails   *struct {
			CachedTokens *int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*usage = OpenAIUsage{}
	if raw.PromptTokens != nil {
		usage.PromptTokens = *raw.PromptTokens
		usage.PromptTokensReported = raw.PromptTokensEstimated == nil || !*raw.PromptTokensEstimated
	}
	if raw.CompletionTokens != nil {
		usage.CompletionTokens = *raw.CompletionTokens
		usage.CompletionTokensReported = true
	}
	if raw.PromptTokensDetails != nil && raw.PromptTokensDetails.CachedTokens != nil {
		usage.PromptTokensDetails.CachedTokens = *raw.PromptTokensDetails.CachedTokens
		usage.PromptTokensDetails.CachedTokensReported = true
	}
	return nil
}

func ToOpenAIRequest(request provider.Request) (OpenAIRequest, error) {
	if err := request.Validate(); err != nil {
		return OpenAIRequest{}, err
	}
	result := OpenAIRequest{
		Model: request.Model, Temperature: request.Temperature, TopP: request.TopP, Seed: request.Seed,
		Stop: request.Stop, ResponseFormat: request.ResponseFormat, N: request.N,
		PresencePenalty: request.PresencePenalty, FrequencyPenalty: request.FrequencyPenalty, User: request.User,
		MaxTokens: request.MaxTokens, MaxCompletionTokens: request.MaxCompletionTokens, Stream: request.Stream,
		ParallelToolCalls: request.ParallelToolCalls, Effort: request.Effort,
		ReasoningEffort: request.Effort,
	}
	// The "off" override strips the effort the client asked for, so the wire
	// carries no reasoning_effort at all.
	if request.Effort == storage.EffortOff {
		result.ReasoningEffort = ""
		result.ForceNoEffort = true
	}
	if request.Stream {
		result.StreamOptions = &OpenAIStreamOptions{IncludeUsage: true}
	}
	if len(request.System) > 0 {
		content, err := toOpenAIContent(request.System)
		if err != nil {
			return OpenAIRequest{}, err
		}
		if content != nil {
			result.Messages = append(result.Messages, OpenAIMessage{Role: "system", Content: content})
		}
	}
	// Tool results only carry tool_use_id on the Anthropic wire. Providers that key
	// results by function name (Antigravity/Gemini) break once the originating
	// assistant turn is compacted away, so resolve names before the history shrinks.
	toolNames := make(map[string]string)
	for _, message := range request.Messages {
		for _, block := range message.Content {
			if block.Type == "tool_use" && block.ToolUseID != "" && block.Name != "" {
				toolNames[block.ToolUseID] = block.Name
			}
		}
	}
	for _, message := range request.Messages {
		translated, err := toOpenAIMessage(message, toolNames)
		if err != nil {
			return OpenAIRequest{}, err
		}
		result.Messages = append(result.Messages, translated...)
	}
	for _, tool := range request.Tools {
		result.Tools = append(result.Tools, OpenAITool{Type: "function", Function: OpenAIFunction{
			Name: tool.Name, Description: tool.Description, Parameters: tool.InputSchema, Strict: tool.Strict,
		}})
	}
	switch request.ToolChoice.Type {
	case "auto", "none", "required":
		result.ToolChoice = request.ToolChoice.Type
	case "any":
		result.ToolChoice = "required"
	case "tool":
		result.ToolChoice = map[string]any{"type": "function", "function": map[string]string{"name": request.ToolChoice.Name}}
	}
	return result, nil
}

func FromOpenAIRequest(request OpenAIRequest) (provider.Request, error) {
	result := provider.Request{
		Model: request.Model, Temperature: request.Temperature, TopP: request.TopP, Seed: request.Seed,
		Stop: request.Stop, ResponseFormat: request.ResponseFormat, N: request.N,
		PresencePenalty: request.PresencePenalty, FrequencyPenalty: request.FrequencyPenalty, User: request.User,
		MaxTokens: request.MaxTokens, MaxCompletionTokens: request.MaxCompletionTokens, Stream: request.Stream,
		Effort: request.Effort,
	}
	for _, message := range request.Messages {
		content, err := fromOpenAIContent(message.Content)
		if err != nil {
			return provider.Request{}, err
		}
		if message.Role == "system" || message.Role == "developer" {
			result.System = append(result.System, content...)
			continue
		}
		role := message.Role
		if role == "tool" {
			role = "user"
		}
		translated := provider.Message{Role: role, Content: content}
		if message.Role == "tool" {
			translated.Content = []provider.Content{{Type: "tool_result", ToolUseID: message.ToolCallID, Name: message.Name, Text: contentText(content)}}
		}
		for _, call := range message.ToolCalls {
			input := json.RawMessage(call.Function.Arguments)
			if !json.Valid(input) {
				return provider.Request{}, fmt.Errorf("tool call %q has invalid arguments", call.ID)
			}
			translated.Content = append(translated.Content, provider.Content{
				Type: "tool_use", ToolUseID: call.ID, Name: call.Function.Name, Input: input,
			})
		}
		result.Messages = append(result.Messages, translated)
	}
	for _, tool := range request.Tools {
		if tool.Type != "function" {
			return provider.Request{}, fmt.Errorf("unsupported OpenAI tool type %q", tool.Type)
		}
		result.Tools = append(result.Tools, provider.Tool{
			Name: tool.Function.Name, Description: tool.Function.Description, InputSchema: tool.Function.Parameters,
		})
	}
	result.ParallelToolCalls = request.ParallelToolCalls
	result.ToolChoice = fromOpenAIToolChoice(request.ToolChoice)
	return result, result.Validate()
}

func FromOpenAIResponse(response OpenAIResponse) (provider.Response, error) {
	if len(response.Choices) == 0 {
		return provider.Response{}, fmt.Errorf("OpenAI response has no choices")
	}
	choice := response.Choices[0]
	content, err := fromOpenAIContent(choice.Message.Content)
	if err != nil {
		return provider.Response{}, err
	}
	// A thinking model can finish having produced only reasoning_content and no content.
	// With finish_reason=stop the reasoning is the model's answer; without it the response
	// is empty and gets retried into the fallback. With finish_reason=length the model ran
	// out of tokens mid-reasoning — its deliberation is not an answer, so it stays dropped.
	// Emit reasoning as text only when there is nothing else and the model actually stopped.
	if len(content) == 0 && len(choice.Message.ToolCalls) == 0 &&
		choice.FinishReason == "stop" && strings.TrimSpace(choice.Message.Reasoning) != "" {
		content = append(content, provider.Content{Type: "text", Text: choice.Message.Reasoning})
	}
	for _, call := range choice.Message.ToolCalls {
		input := json.RawMessage(call.Function.Arguments)
		if !json.Valid(input) {
			return provider.Response{}, fmt.Errorf("tool call %q has invalid arguments", call.ID)
		}
		content = append(content, provider.Content{Type: "tool_use", ToolUseID: call.ID, Name: call.Function.Name, Input: input})
	}
	return provider.Response{
		ID: response.ID, Model: response.Model, Role: "assistant", Content: content,
		StopReason: fromOpenAIStopReason(choice.FinishReason),
		Usage: provider.Usage{
			InputTokens: response.Usage.PromptTokens, OutputTokens: response.Usage.CompletionTokens,
			CacheReadTokens: response.Usage.PromptTokensDetails.CachedTokens,
		},
	}, nil
}

func ToOpenAIResponse(response provider.Response) (OpenAIResponse, error) {
	messages, err := toOpenAIMessage(provider.Message{Role: "assistant", Content: response.Content}, nil)
	if err != nil {
		return OpenAIResponse{}, err
	}
	if len(messages) != 1 {
		return OpenAIResponse{}, fmt.Errorf("assistant response split unexpectedly")
	}
	result := OpenAIResponse{
		ID: response.ID, Model: response.Model,
		Choices: []OpenAIChoice{{Message: messages[0], FinishReason: toOpenAIStopReason(response.StopReason)}},
		Usage:   OpenAIUsage{PromptTokens: response.Usage.InputTokens, CompletionTokens: response.Usage.OutputTokens},
	}
	result.Usage.PromptTokensDetails.CachedTokens = response.Usage.CacheReadTokens
	return result, nil
}

func toOpenAIMessage(message provider.Message, toolNames map[string]string) ([]OpenAIMessage, error) {
	result := OpenAIMessage{Role: message.Role}
	parts := make([]OpenAIContentPart, 0, len(message.Content))
	toolResults := make([]OpenAIMessage, 0)
	for _, block := range message.Content {
		switch block.Type {
		case "thinking", "redacted_thinking":
			// Some OpenAI-compatible thinking-mode upstreams (opencode/Console Go) require
			// the assistant's reasoning_content to be passed back verbatim when its history
			// is re-sent; dropping it yields 400 "reasoning_content must be passed back".
			// Accumulate it on the assistant message rather than dropping it.
			if result.Role == "assistant" && block.Thinking != "" {
				result.Reasoning += block.Thinking
			}
			continue
		case "tool_use":
			arguments := string(block.Input)
			if arguments == "" {
				arguments = "{}"
			}
			result.ToolCalls = append(result.ToolCalls, OpenAIToolCall{
				ID: block.ToolUseID, Type: "function",
				Function: OpenAIFunctionCall{Name: block.Name, Arguments: arguments},
			})
		case "tool_result":
			name := block.Name
			if name == "" {
				name = toolNames[block.ToolUseID]
			}
			toolResults = append(toolResults, OpenAIMessage{Role: "tool", ToolCallID: block.ToolUseID, Name: name, Content: block.Text})
		default:
			part, err := toOpenAIContentPart(block)
			if err != nil {
				return nil, err
			}
			parts = append(parts, part)
		}
	}
	if len(parts) == 1 && parts[0].Type == "text" {
		result.Content = parts[0].Text
	} else if len(parts) > 0 {
		result.Content = parts
	}
	messages := make([]OpenAIMessage, 0, len(toolResults)+2)
	// A tool message has to sit immediately after the assistant turn that called the tool,
	// so anything else this Anthropic message carried — an image hoisted out of the tool
	// result, or text the user typed in the same turn — follows the tool results instead of
	// preceding them. Emitting it first put a user message between the tool_calls and their
	// answers, which is a sequence the OpenAI shape does not allow.
	if len(toolResults) > 0 && len(result.ToolCalls) == 0 {
		messages = append(messages, toolResults...)
		if result.Content != nil {
			messages = append(messages, OpenAIMessage{Role: result.Role, Content: result.Content})
		}
		return messages, nil
	}
	if result.Content != nil || len(result.ToolCalls) > 0 {
		messages = append(messages, result)
	}
	messages = append(messages, toolResults...)
	return messages, nil
}

func toOpenAIContent(content []provider.Content) (any, error) {
	parts := make([]OpenAIContentPart, 0, len(content))
	for _, block := range content {
		if block.Type == "thinking" || block.Type == "redacted_thinking" {
			// OpenAI-compatible chat APIs do not accept Anthropic reasoning blocks.
			continue
		}
		part, err := toOpenAIContentPart(block)
		if err != nil {
			return nil, err
		}
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		return nil, nil
	}
	if len(parts) == 1 && parts[0].Type == "text" {
		return parts[0].Text, nil
	}
	return parts, nil
}

func toOpenAIContentPart(block provider.Content) (OpenAIContentPart, error) {
	switch block.Type {
	case "text":
		return OpenAIContentPart{Type: "text", Text: block.Text}, nil
	case "image":
		url := block.URL
		if url == "" && block.Data != "" {
			url = "data:" + block.MediaType + ";base64," + block.Data
		}
		return OpenAIContentPart{Type: "image_url", ImageURL: &OpenAIImageURL{URL: url}}, nil
	default:
		return OpenAIContentPart{}, fmt.Errorf("unsupported content type %q", block.Type)
	}
}

func fromOpenAIContent(raw any) ([]provider.Content, error) {
	switch value := raw.(type) {
	case nil:
		return nil, nil
	case string:
		if value == "" {
			return nil, nil
		}
		return []provider.Content{{Type: "text", Text: value}}, nil
	case []OpenAIContentPart:
		return fromOpenAIContentParts(value)
	case []any:
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		var parts []OpenAIContentPart
		if err := json.Unmarshal(encoded, &parts); err != nil {
			return nil, err
		}
		return fromOpenAIContentParts(parts)
	default:
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		var text string
		if err := json.Unmarshal(encoded, &text); err == nil {
			return []provider.Content{{Type: "text", Text: text}}, nil
		}
		return nil, fmt.Errorf("unsupported OpenAI content %T", raw)
	}
}

func fromOpenAIContentParts(parts []OpenAIContentPart) ([]provider.Content, error) {
	result := make([]provider.Content, 0, len(parts))
	for _, part := range parts {
		switch part.Type {
		case "text", "input_text", "output_text":
			if part.Text != "" {
				result = append(result, provider.Content{Type: "text", Text: part.Text})
			}
		case "thinking", "reasoning":
			result = append(result, provider.Content{Type: "thinking", Thinking: part.Thinking})
		case "image_url", "input_image":
			if part.ImageURL == nil {
				return nil, fmt.Errorf("image URL is required")
			}
			result = append(result, imageContent(part.ImageURL.URL))
		default:
			return nil, fmt.Errorf("unsupported OpenAI content type %q", part.Type)
		}
	}
	return result, nil
}

func imageContent(rawURL string) provider.Content {
	const marker = ";base64,"
	if strings.HasPrefix(rawURL, "data:") {
		if index := strings.Index(rawURL, marker); index > len("data:") {
			return provider.Content{Type: "image", MediaType: rawURL[len("data:"):index], Data: rawURL[index+len(marker):]}
		}
	}
	return provider.Content{Type: "image", URL: rawURL}
}

func fromOpenAIToolChoice(choice any) provider.ToolChoice {
	switch value := choice.(type) {
	case string:
		if value == "required" {
			value = "any"
		}
		return provider.ToolChoice{Type: value}
	case map[string]any:
		if function, ok := value["function"].(map[string]any); ok {
			name, _ := function["name"].(string)
			return provider.ToolChoice{Type: "tool", Name: name}
		}
	}
	return provider.ToolChoice{}
}

func contentText(content []provider.Content) string {
	var result strings.Builder
	for _, block := range content {
		if block.Type == "text" {
			result.WriteString(block.Text)
		}
	}
	return result.String()
}

func fromOpenAIStopReason(reason string) string {
	switch reason {
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	default:
		return reason
	}
}

func toOpenAIStopReason(reason string) string {
	switch reason {
	case "end_turn", "stop_sequence":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	default:
		return reason
	}
}

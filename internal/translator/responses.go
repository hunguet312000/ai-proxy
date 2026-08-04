package translator

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type ResponsesReasoning struct {
	Effort  string          `json:"effort,omitempty"`
	Summary json.RawMessage `json:"summary,omitempty"`
}

type ResponsesRequest struct {
	Model              string              `json:"model"`
	Input              json.RawMessage     `json:"input"`
	Instructions       string              `json:"instructions,omitempty"`
	Reasoning          *ResponsesReasoning `json:"reasoning,omitempty"`
	Tools              []ResponsesTool     `json:"tools,omitempty"`
	ToolChoice         json.RawMessage     `json:"tool_choice,omitempty"`
	Temperature        *float64            `json:"temperature,omitempty"`
	TopP               *float64            `json:"top_p,omitempty"`
	MaxOutputTokens    int                 `json:"max_output_tokens,omitempty"`
	Stream             bool                `json:"stream,omitempty"`
	PromptCacheKey     string              `json:"prompt_cache_key,omitempty"`
	Text               json.RawMessage     `json:"text,omitempty"`
	ParallelToolCalls  *bool               `json:"parallel_tool_calls,omitempty"`
	PreviousResponseID string              `json:"previous_response_id,omitempty"`
	Background         bool                `json:"background,omitempty"`
	Conversation       json.RawMessage     `json:"conversation,omitempty"`
}

type ResponsesTool struct {
	Type              string          `json:"type"`
	Name              string          `json:"name"`
	Description       string          `json:"description,omitempty"`
	Parameters        json.RawMessage `json:"parameters"`
	Strict            *bool           `json:"strict,omitempty"`
	Tools             []ResponsesTool `json:"tools,omitempty"`
	ExternalWebAccess *bool           `json:"external_web_access,omitempty"`
}

type ResponsesInputItem struct {
	Type      string          `json:"type,omitempty"`
	Role      string          `json:"role,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	ID        string          `json:"id,omitempty"`
	CallID    string          `json:"call_id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Namespace string          `json:"namespace,omitempty"`
	Arguments string          `json:"arguments,omitempty"`
	Output    json.RawMessage `json:"output,omitempty"`
}

type ResponsesContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}

type ResponsesResponse struct {
	ID                 string                `json:"id"`
	Object             string                `json:"object"`
	CreatedAt          int64                 `json:"created_at"`
	Status             string                `json:"status"`
	Error              any                   `json:"error"`
	IncompleteDetails  any                   `json:"incomplete_details"`
	Model              string                `json:"model"`
	Instructions       any                   `json:"instructions"`
	Output             []ResponsesOutputItem `json:"output"`
	OutputText         string                `json:"output_text"`
	Usage              ResponsesUsage        `json:"usage"`
	ParallelToolCalls  bool                  `json:"parallel_tool_calls"`
	ToolChoice         any                   `json:"tool_choice"`
	Tools              []ResponsesTool       `json:"tools"`
	Temperature        *float64              `json:"temperature"`
	TopP               *float64              `json:"top_p"`
	MaxOutputTokens    int                   `json:"max_output_tokens"`
	PreviousResponseID any                   `json:"previous_response_id"`
	Store              bool                  `json:"store"`
}

type ResponsesOutputItem struct {
	Type      string                 `json:"type"`
	ID        string                 `json:"id"`
	Status    string                 `json:"status"`
	Role      string                 `json:"role,omitempty"`
	Content   []ResponsesOutputPart  `json:"content,omitempty"`
	CallID    string                 `json:"call_id,omitempty"`
	Name      string                 `json:"name,omitempty"`
	Namespace string                 `json:"namespace,omitempty"`
	Arguments string                 `json:"arguments,omitempty"`
	Summary   []ResponsesSummaryPart `json:"summary,omitempty"`
}

type ResponsesOutputPart struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	Annotations []any  `json:"annotations"`
}

type ResponsesSummaryPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type ResponsesUsage struct {
	InputTokens         int                         `json:"input_tokens"`
	InputTokensDetails  ResponsesInputTokenDetails  `json:"input_tokens_details"`
	OutputTokens        int                         `json:"output_tokens"`
	OutputTokensDetails ResponsesOutputTokenDetails `json:"output_tokens_details"`
	TotalTokens         int                         `json:"total_tokens"`
}

type ResponsesInputTokenDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

type ResponsesOutputTokenDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

func FromResponsesRequest(request ResponsesRequest) (OpenAIRequest, error) {
	if request.Model == "" {
		return OpenAIRequest{}, fmt.Errorf("model is required")
	}
	if len(bytes.TrimSpace(request.Input)) == 0 || bytes.Equal(bytes.TrimSpace(request.Input), []byte("null")) {
		return OpenAIRequest{}, fmt.Errorf("input is required")
	}
	if request.PreviousResponseID != "" {
		return OpenAIRequest{}, fmt.Errorf("previous_response_id is unsupported; resend the full input")
	}
	if request.Background {
		return OpenAIRequest{}, fmt.Errorf("background responses are unsupported")
	}
	if len(bytes.TrimSpace(request.Conversation)) > 0 && !bytes.Equal(bytes.TrimSpace(request.Conversation), []byte("null")) {
		return OpenAIRequest{}, fmt.Errorf("conversation is unsupported")
	}
	result := OpenAIRequest{
		Model: request.Model, Temperature: request.Temperature, TopP: request.TopP,
		MaxCompletionTokens: request.MaxOutputTokens, Stream: request.Stream,
		PromptCacheKey: request.PromptCacheKey, ParallelToolCalls: request.ParallelToolCalls,
	}
	if request.Reasoning != nil {
		result.Effort = request.Reasoning.Effort
	}
	if request.Instructions != "" {
		result.Messages = append(result.Messages, OpenAIMessage{Role: "system", Content: request.Instructions})
	}
	messages, err := responsesInputMessages(request.Input)
	if err != nil {
		return OpenAIRequest{}, err
	}
	result.Messages = append(result.Messages, messages...)
	for _, tool := range request.Tools {
		if err := appendResponsesTool(&result.Tools, tool, ""); err != nil {
			return OpenAIRequest{}, err
		}
	}
	if len(request.ToolChoice) > 0 {
		result.ToolChoice, err = responsesToolChoice(request.ToolChoice)
		if err != nil {
			return OpenAIRequest{}, err
		}
	}
	if len(request.Text) > 0 {
		result.ResponseFormat, err = responsesFormat(request.Text)
		if err != nil {
			return OpenAIRequest{}, err
		}
	}
	if len(result.Messages) == 0 {
		return OpenAIRequest{}, fmt.Errorf("input contains no supported messages")
	}
	return result, nil
}

func appendResponsesTool(result *[]OpenAITool, tool ResponsesTool, namespace string) error {
	switch tool.Type {
	case "function":
		if tool.Name == "" || len(tool.Parameters) == 0 || !json.Valid(tool.Parameters) {
			return fmt.Errorf("function tools require a name and valid parameters")
		}
		name := tool.Name
		if namespace != "" {
			name = namespace + "__" + name
		}
		*result = append(*result, OpenAITool{Type: "function", Function: OpenAIFunction{
			Name: name, Description: tool.Description, Parameters: tool.Parameters, Strict: tool.Strict,
		}})
		return nil
	case "namespace":
		if namespace != "" || tool.Name == "" || len(tool.Tools) == 0 {
			return fmt.Errorf("namespace tools require a name and nested tools")
		}
		for _, nested := range tool.Tools {
			if err := appendResponsesTool(result, nested, tool.Name); err != nil {
				return err
			}
		}
		return nil
	case "web_search":
		if tool.ExternalWebAccess == nil || *tool.ExternalWebAccess {
			return fmt.Errorf("web_search with external access is unsupported")
		}
		// Codex declares disabled web search as a capability marker; no upstream tool is needed.
		return nil
	default:
		return fmt.Errorf("tool type %q is unsupported", tool.Type)
	}
}

func responsesInputMessages(raw json.RawMessage) ([]OpenAIMessage, error) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []OpenAIMessage{{Role: "user", Content: text}}, nil
	}
	var items []ResponsesInputItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("input must be a string or item array")
	}
	messages := make([]OpenAIMessage, 0, len(items))
	assistantIndex := -1
	for _, item := range items {
		switch item.Type {
		case "", "message":
			role := item.Role
			if role == "developer" {
				role = "system"
			}
			if role != "system" && role != "user" && role != "assistant" {
				return nil, fmt.Errorf("message role %q is unsupported", item.Role)
			}
			content, err := responsesMessageContent(item.Content)
			if err != nil {
				return nil, err
			}
			messages = append(messages, OpenAIMessage{Role: role, Content: content})
			if role == "assistant" {
				assistantIndex = len(messages) - 1
			} else {
				assistantIndex = -1
			}
		case "function_call":
			callID := item.CallID
			if callID == "" {
				callID = item.ID
			}
			if callID == "" || item.Name == "" || !json.Valid([]byte(item.Arguments)) {
				return nil, fmt.Errorf("function_call requires call_id, name, and valid arguments")
			}
			name := item.Name
			if item.Namespace != "" {
				name = item.Namespace + "__" + name
			}
			if assistantIndex < 0 {
				messages = append(messages, OpenAIMessage{Role: "assistant"})
				assistantIndex = len(messages) - 1
			}
			messages[assistantIndex].ToolCalls = append(messages[assistantIndex].ToolCalls, OpenAIToolCall{
				ID: callID, Type: "function", Function: OpenAIFunctionCall{Name: name, Arguments: item.Arguments},
			})
		case "function_call_output":
			if item.CallID == "" {
				return nil, fmt.Errorf("function_call_output requires call_id")
			}
			output, err := responsesOutputText(item.Output)
			if err != nil {
				return nil, err
			}
			messages = append(messages, OpenAIMessage{Role: "tool", ToolCallID: item.CallID, Content: output})
			assistantIndex = -1
		case "reasoning":
			// Codex may replay opaque reasoning items. They have no Chat Completions equivalent.
		case "additional_tools":
			// Codex sends this capability metadata for models that support extra tools.
			// Chat Completions has no equivalent, so omit it from the translated input.
		default:
			return nil, fmt.Errorf("input item type %q is unsupported", item.Type)
		}
	}
	return messages, nil
}

func responsesMessageContent(raw json.RawMessage) (any, error) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text, nil
	}
	var parts []ResponsesContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, fmt.Errorf("message content must be text or content parts")
	}
	translated := make([]OpenAIContentPart, 0, len(parts))
	for _, part := range parts {
		switch part.Type {
		case "input_text", "output_text", "text":
			translated = append(translated, OpenAIContentPart{Type: "text", Text: part.Text})
		case "input_image":
			if part.ImageURL == "" {
				return nil, fmt.Errorf("input_image requires image_url")
			}
			translated = append(translated, OpenAIContentPart{Type: "image_url", ImageURL: &OpenAIImageURL{URL: part.ImageURL}})
		default:
			return nil, fmt.Errorf("content part type %q is unsupported", part.Type)
		}
	}
	return translated, nil
}

func responsesOutputText(raw json.RawMessage) (string, error) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text, nil
	}
	var parts []ResponsesContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", fmt.Errorf("function output must be text or content parts")
	}
	var result strings.Builder
	for _, part := range parts {
		if part.Type == "input_text" || part.Type == "output_text" || part.Type == "text" {
			result.WriteString(part.Text)
		}
	}
	return result.String(), nil
}

func responsesToolChoice(raw json.RawMessage) (any, error) {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("invalid tool_choice")
	}
	if choice, ok := value.(string); ok {
		if choice != "auto" && choice != "none" && choice != "required" {
			return nil, fmt.Errorf("tool_choice %q is unsupported", choice)
		}
		return choice, nil
	}
	object, ok := value.(map[string]any)
	if !ok || object["type"] != "function" {
		return nil, fmt.Errorf("only function tool_choice is supported")
	}
	name, _ := object["name"].(string)
	if name == "" {
		return nil, fmt.Errorf("function tool_choice requires name")
	}
	return map[string]any{"type": "function", "function": map[string]string{"name": name}}, nil
}

func responsesFormat(raw json.RawMessage) (json.RawMessage, error) {
	var text struct {
		Format json.RawMessage `json:"format"`
	}
	if err := json.Unmarshal(raw, &text); err != nil || len(text.Format) == 0 {
		return nil, fmt.Errorf("invalid text.format")
	}
	var format struct {
		Type   string          `json:"type"`
		Name   string          `json:"name,omitempty"`
		Schema json.RawMessage `json:"schema,omitempty"`
		Strict *bool           `json:"strict,omitempty"`
	}
	if err := json.Unmarshal(text.Format, &format); err != nil {
		return nil, fmt.Errorf("invalid text.format")
	}
	switch format.Type {
	case "text", "":
		return nil, nil
	case "json_object":
		return json.RawMessage(`{"type":"json_object"}`), nil
	case "json_schema":
		if format.Name == "" || len(format.Schema) == 0 || !json.Valid(format.Schema) {
			return nil, fmt.Errorf("json_schema format requires name and valid schema")
		}
		encoded, _ := json.Marshal(map[string]any{"type": "json_schema", "json_schema": map[string]any{
			"name": format.Name, "schema": format.Schema, "strict": format.Strict,
		}})
		return encoded, nil
	default:
		return nil, fmt.Errorf("text format %q is unsupported", format.Type)
	}
}

func ToResponsesResponse(response OpenAIResponse, request ResponsesRequest) ResponsesResponse {
	id := response.ID
	if strings.HasPrefix(id, "chatcmpl-") {
		id = "resp_" + strings.TrimPrefix(id, "chatcmpl-")
	} else if !strings.HasPrefix(id, "resp_") {
		id = "resp_" + strings.TrimPrefix(id, "response")
	}
	if id == "resp_" {
		id = fmt.Sprintf("resp_%d", time.Now().UnixNano())
	}
	result := NewResponsesResponse(id, response.Model, request)
	result.Status = "completed"
	if len(response.Choices) == 0 {
		result.Status = "failed"
		result.Error = map[string]any{"message": "upstream returned no choices", "type": "upstream_error"}
		return result
	}
	choice := response.Choices[0]
	if choice.FinishReason == "length" {
		result.Status = "incomplete"
		result.IncompleteDetails = map[string]string{"reason": "max_output_tokens"}
	}
	if text, ok := choice.Message.Content.(string); ok && text != "" {
		result.OutputText = text
		result.Output = append(result.Output, ResponsesOutputItem{
			Type: "message", ID: id + "_message", Status: "completed", Role: "assistant",
			Content: []ResponsesOutputPart{{Type: "output_text", Text: text, Annotations: []any{}}},
		})
	}
	for index, call := range choice.Message.ToolCalls {
		namespace, name := splitToolName(call.Function.Name)
		result.Output = append(result.Output, ResponsesOutputItem{
			Type: "function_call", ID: fmt.Sprintf("%s_fc_%d", id, index), Status: "completed",
			CallID: call.ID, Name: name, Namespace: namespace, Arguments: call.Function.Arguments,
		})
	}
	result.Usage = responsesUsage(response.Usage)
	return result
}

func SplitResponsesToolName(name string) (string, string) {
	return splitToolName(name)
}

func splitToolName(name string) (string, string) {
	parts := strings.SplitN(name, "__", 2)
	if len(parts) == 2 {
		return parts[0], parts[1]
	}
	return "", name
}

func NewResponsesResponse(id, model string, request ResponsesRequest) ResponsesResponse {
	if id == "" {
		id = fmt.Sprintf("resp_%d", time.Now().UnixNano())
	}
	parallel := true
	if request.ParallelToolCalls != nil {
		parallel = *request.ParallelToolCalls
	}
	var instructions any
	if request.Instructions != "" {
		instructions = request.Instructions
	}
	var toolChoice any = "auto"
	if len(request.ToolChoice) > 0 {
		_ = json.Unmarshal(request.ToolChoice, &toolChoice)
	}
	return ResponsesResponse{
		ID: id, Object: "response", CreatedAt: time.Now().Unix(), Status: "in_progress",
		Model: model, Instructions: instructions, Output: []ResponsesOutputItem{},
		ParallelToolCalls: parallel, ToolChoice: toolChoice, Tools: request.Tools,
		Temperature: request.Temperature, TopP: request.TopP, MaxOutputTokens: request.MaxOutputTokens,
		Store: false,
	}
}

func responsesUsage(usage OpenAIUsage) ResponsesUsage {
	return ResponsesUsage{
		InputTokens:         usage.PromptTokens,
		InputTokensDetails:  ResponsesInputTokenDetails{CachedTokens: usage.PromptTokensDetails.CachedTokens},
		OutputTokens:        usage.CompletionTokens,
		OutputTokensDetails: ResponsesOutputTokenDetails{},
		TotalTokens:         usage.PromptTokens + usage.CompletionTokens,
	}
}

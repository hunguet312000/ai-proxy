package oauth

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"literouter/internal/provider"
	"literouter/internal/translator"
)

type antigravityEnvelope struct {
	Project     string             `json:"project"`
	Model       string             `json:"model"`
	UserAgent   string             `json:"userAgent"`
	RequestType string             `json:"requestType"`
	RequestID   string             `json:"requestId,omitempty"`
	Request     antigravityRequest `json:"request"`
}

type antigravityRequest struct {
	Contents         []antigravityContent `json:"contents"`
	Tools            []antigravityTool    `json:"tools,omitempty"`
	ToolConfig       map[string]any       `json:"toolConfig,omitempty"`
	GenerationConfig map[string]any       `json:"generationConfig,omitempty"`
	SessionID        string               `json:"sessionId,omitempty"`
}

type antigravityContent struct {
	Role  string            `json:"role"`
	Parts []antigravityPart `json:"parts"`
}

type antigravityPart struct {
	Text             string                       `json:"text,omitempty"`
	FunctionCall     *antigravityFunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *antigravityFunctionResponse `json:"functionResponse,omitempty"`
	InlineData       *antigravityInlineData       `json:"inlineData,omitempty"`
	Thought          bool                         `json:"thought,omitempty"`
	ThoughtSignature string                       `json:"thoughtSignature,omitempty"`
}

type antigravityFunctionCall struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}

type antigravityFunctionResponse struct {
	Name     string         `json:"name"`
	Response map[string]any `json:"response"`
}

type antigravityInlineData struct {
	MIMEType string `json:"mimeType"`
	Data     string `json:"data"`
}

type antigravityTool struct {
	FunctionDeclarations []antigravityFunctionDeclaration `json:"functionDeclarations"`
}

type antigravityFunctionDeclaration struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

const (
	// Signatures must outlive a coding session's idle gaps, and one entry is needed
	// per tool call the conversation still replays — a real agent session produces
	// far more than the 512 this once allowed, and evicting one breaks the chain.
	antigravitySignatureTTL     = 2 * time.Hour
	antigravitySignatureMax     = 4096
	antigravitySignatureMaxSize = 8 * 1024
)

type antigravitySignatureEntry struct {
	signature string
	expiresAt time.Time
}

type antigravitySignatureStore struct {
	mu      sync.Mutex
	entries map[string]antigravitySignatureEntry
}

var antigravitySignatures = antigravitySignatureStore{entries: make(map[string]antigravitySignatureEntry)}

func (s *antigravitySignatureStore) put(sessionID, callID, signature string, now time.Time) {
	if sessionID == "" || callID == "" || signature == "" || signature == "skip_thought_signature_validator" || len(signature) > antigravitySignatureMaxSize {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune(now)
	if len(s.entries) >= antigravitySignatureMax {
		var oldestKey string
		var oldest time.Time
		for key, entry := range s.entries {
			if oldestKey == "" || entry.expiresAt.Before(oldest) {
				oldestKey, oldest = key, entry.expiresAt
			}
		}
		delete(s.entries, oldestKey)
	}
	s.entries[sessionID+"\x00"+callID] = antigravitySignatureEntry{signature: signature, expiresAt: now.Add(antigravitySignatureTTL)}
}

// lookup returns a cached signature without consuming it. Consuming was wrong by
// construction: the client replays the whole conversation on every turn, so the
// same tool call is looked up again on turn 3, turn 4, and so on. A one-shot read
// meant every call older than the previous turn silently degraded to
// "skip_thought_signature_validator", which breaks the model's reasoning chain and
// makes it stop mid-task instead of issuing the next tool call.
func (s *antigravitySignatureStore) lookup(sessionID, callID string, now time.Time) string {
	if sessionID == "" || callID == "" {
		return ""
	}
	key := sessionID + "\x00" + callID
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[key]
	if !ok {
		return ""
	}
	if !entry.expiresAt.After(now) {
		delete(s.entries, key)
		return ""
	}
	// Keep entries alive for as long as the conversation keeps referring to them,
	// so an active session cannot expire out from under itself.
	entry.expiresAt = now.Add(antigravitySignatureTTL)
	s.entries[key] = entry
	return entry.signature
}

func (s *antigravitySignatureStore) prune(now time.Time) {
	for key, entry := range s.entries {
		if !entry.expiresAt.After(now) {
			delete(s.entries, key)
		}
	}
}

// antigravitySessionKey identifies a conversation for thought-signature reuse.
func antigravitySessionKey(request translator.OpenAIRequest, conversationID string) string {
	return oauthSessionKey(request, conversationID, "seed_")
}

// oauthSessionKey derives a value that is stable for the life of one conversation.
// The X-Conversation-ID header is the authoritative source, but Claude Code does
// not send it, so keying on the header alone yields an empty string — which is what
// silently disabled the Antigravity signature cache. The first user turn is stable
// for the life of a session and identifies it well enough.
func oauthSessionKey(request translator.OpenAIRequest, conversationID, prefix string) string {
	if trimmed := strings.TrimSpace(conversationID); trimmed != "" {
		return trimmed
	}
	for _, message := range request.Messages {
		if message.Role != "user" {
			continue
		}
		if text := strings.TrimSpace(openAIContentText(message.Content)); text != "" {
			sum := sha256.Sum256([]byte(text))
			return prefix + hex.EncodeToString(sum[:12])
		}
	}
	return ""
}

// codexSessionID formats the conversation key as a UUID, which is the shape the
// Codex CLI sends. The backend appears to route on it, and routing is what decides
// whether the prompt cache can be reused: a request that lands on an instance with
// a cold cache re-pays for the entire prefix.
func codexSessionID(request translator.OpenAIRequest, conversationID string) string {
	seed := oauthSessionKey(request, conversationID, "")
	if seed == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("codex-session\x00" + seed))
	hexed := hex.EncodeToString(sum[:16])
	return hexed[0:8] + "-" + hexed[8:12] + "-" + hexed[12:16] + "-" + hexed[16:20] + "-" + hexed[20:32]
}

func antigravityCallID(sessionID, name string, args []byte, signature string) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte(sessionID))
	_, _ = hash.Write([]byte("\x00" + name + "\x00"))
	_, _ = hash.Write(args)
	_, _ = hash.Write([]byte("\x00" + signature))
	return "call_ag_" + hex.EncodeToString(hash.Sum(nil)[:8])
}

var antigravitySchemaFields = map[string]struct{}{
	"type": {}, "format": {}, "description": {}, "nullable": {}, "enum": {},
	"items": {}, "properties": {}, "required": {}, "minimum": {}, "maximum": {},
	"minItems": {}, "maxItems": {}, "minLength": {}, "maxLength": {}, "pattern": {},
	"anyOf": {}, "default": {}, "example": {}, "propertyOrdering": {},
}

func sanitizeAntigravitySchema(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return json.RawMessage(`{"type":"object"}`), nil
	}
	var schema any
	if err := json.Unmarshal(raw, &schema); err != nil {
		return nil, fmt.Errorf("decode Antigravity tool schema: %w", err)
	}
	cleanAntigravitySchema(schema, false)
	cleaned, err := json.Marshal(schema)
	if err != nil {
		return nil, fmt.Errorf("encode Antigravity tool schema: %w", err)
	}
	return cleaned, nil
}

func cleanAntigravitySchema(value any, propertyMap bool) {
	object, ok := value.(map[string]any)
	if !ok {
		if list, ok := value.([]any); ok {
			for _, item := range list {
				cleanAntigravitySchema(item, false)
			}
		}
		return
	}
	for key, child := range object {
		if !propertyMap {
			if _, supported := antigravitySchemaFields[key]; !supported {
				delete(object, key)
				continue
			}
		}
		cleanAntigravitySchema(child, key == "properties")
	}
}

func buildAntigravityEnvelope(request translator.OpenAIRequest, project, conversationID string) (antigravityEnvelope, error) {
	// Upstream keeps seeing the caller's own conversation id (possibly empty). Sending
	// a derived one was measured and did not help implicit caching — 47% mean without
	// it against 26% with it — so the caller's value is passed through unchanged.
	native := antigravityRequest{SessionID: conversationID}
	sessionID := antigravitySessionKey(request, conversationID)
	toolNames := make(map[string]string, len(request.Tools))
	callNames := make(map[string]string)
	for _, tool := range request.Tools {
		sanitized := sanitizeAntigravityToolName(tool.Function.Name)
		if previous, exists := toolNames[sanitized]; exists && previous != tool.Function.Name {
			return antigravityEnvelope{}, fmt.Errorf("Antigravity tool names %q and %q collide as %q", previous, tool.Function.Name, sanitized)
		}
		toolNames[sanitized] = tool.Function.Name
	}
	for _, message := range request.Messages {
		for _, call := range message.ToolCalls {
			callNames[call.ID] = sanitizeAntigravityToolName(call.Function.Name)
		}
	}
	for _, message := range request.Messages {
		content := antigravityContent{Role: message.Role}
		switch message.Role {
		case "assistant":
			content.Role = "model"
		case "system", "developer":
			content.Role = "user"
		case "tool":
			content.Role = "user"
			name := callNames[message.ToolCallID]
			if name == "" {
				name = sanitizeAntigravityToolName(message.Name)
			}
			if name == "_unknown" {
				return antigravityEnvelope{}, fmt.Errorf("Antigravity tool result %q has no matching function name", message.ToolCallID)
			}
			content.Parts = append(content.Parts, antigravityPart{FunctionResponse: &antigravityFunctionResponse{
				Name: name, Response: map[string]any{"result": openAIContentText(message.Content)},
			}})
		}
		if message.Role != "tool" {
			for _, part := range openAIContentParts(message.Content) {
				if part.Text != "" {
					content.Parts = append(content.Parts, antigravityPart{Text: part.Text})
				}
				if part.ImageURL != nil && strings.HasPrefix(part.ImageURL.URL, "data:") {
					if mime, data, ok := parseDataURL(part.ImageURL.URL); ok {
						content.Parts = append(content.Parts, antigravityPart{InlineData: &antigravityInlineData{MIMEType: mime, Data: data}})
					}
				}
			}
			for _, call := range message.ToolCalls {
				args := map[string]any{}
				if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
					args = map[string]any{"input": call.Function.Arguments}
				}
				signature := call.ThoughtSignature
				if signature == "" {
					signature = antigravitySignatures.lookup(sessionID, call.ID, time.Now())
				}
				if signature == "" {
					signature = "skip_thought_signature_validator"
				}
				content.Parts = append(content.Parts, antigravityPart{FunctionCall: &antigravityFunctionCall{Name: sanitizeAntigravityToolName(call.Function.Name), Args: args}, ThoughtSignature: signature})
			}
		}
		if len(content.Parts) > 0 {
			native.Contents = append(native.Contents, content)
		}
	}
	if len(request.Tools) > 0 {
		declarations := make([]antigravityFunctionDeclaration, 0, len(request.Tools))
		for _, tool := range request.Tools {
			parameters, err := sanitizeAntigravitySchema(tool.Function.Parameters)
			if err != nil {
				return antigravityEnvelope{}, fmt.Errorf("tool %q: %w", tool.Function.Name, err)
			}
			declarations = append(declarations, antigravityFunctionDeclaration{Name: sanitizeAntigravityToolName(tool.Function.Name), Description: tool.Function.Description, Parameters: parameters})
		}
		native.Tools = []antigravityTool{{FunctionDeclarations: declarations}}
		native.ToolConfig = map[string]any{"functionCallingConfig": map[string]any{"mode": "VALIDATED"}}
	}
	generation := map[string]any{}
	maxTokens := request.MaxTokens
	if request.MaxCompletionTokens > maxTokens {
		maxTokens = request.MaxCompletionTokens
	}
	if maxTokens > 64000 {
		maxTokens = 64000
	}
	if maxTokens > 0 {
		generation["maxOutputTokens"] = maxTokens
	}
	if request.Temperature != nil {
		generation["temperature"] = *request.Temperature
	}
	if request.TopP != nil {
		generation["topP"] = *request.TopP
	}
	if len(generation) > 0 {
		native.GenerationConfig = generation
	}
	return antigravityEnvelope{Project: project, Model: resolveAntigravityModel(request.Model), UserAgent: "antigravity", RequestType: "agent", Request: native}, nil
}

type antigravityCandidate struct {
	Content      antigravityContent `json:"content"`
	FinishReason string             `json:"finishReason"`
}

type antigravityUsage struct {
	Prompt     int `json:"promptTokenCount"`
	Candidates int `json:"candidatesTokenCount"`
	Total      int `json:"totalTokenCount"`
	// Gemini reports implicitly cached prefix tokens here. Not reading it made every
	// Antigravity request look like a 0% cache hit, so there was no way to tell
	// whether the cheap path was working at all.
	Cached int `json:"cachedContentTokenCount"`
}

type antigravityStreamEnvelope struct {
	Response struct {
		Candidates []antigravityCandidate `json:"candidates"`
		Usage      antigravityUsage       `json:"usageMetadata"`
	} `json:"response"`
	Candidates []antigravityCandidate `json:"candidates"`
	Usage      antigravityUsage       `json:"usageMetadata"`
	Error      *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

func (envelope antigravityStreamEnvelope) resolve() ([]antigravityCandidate, antigravityUsage) {
	if len(envelope.Response.Candidates) > 0 {
		return envelope.Response.Candidates, envelope.Response.Usage
	}
	return envelope.Candidates, envelope.Usage
}

func (envelope antigravityStreamEnvelope) err() error {
	if envelope.Error == nil {
		return nil
	}
	return &provider.ProviderError{Provider: "Antigravity OAuth", StatusCode: envelope.Error.Code, Code: envelope.Error.Status, Message: envelope.Error.Message}
}

func antigravitySSEToOpenAI(body io.Reader, fallbackModel, sessionID string) (translator.OpenAIResponse, error) {
	response := translator.OpenAIResponse{ID: "antigravity-response", Model: fallbackModel}
	message := translator.OpenAIMessage{Role: "assistant"}
	var text strings.Builder
	var finish string
	seenCalls := make(map[string]struct{})
	completed := false
	err := readAntigravitySSE(body, func(raw []byte) error {
		var envelope antigravityStreamEnvelope
		if err := json.Unmarshal(raw, &envelope); err != nil {
			return fmt.Errorf("decode Antigravity stream: %w", err)
		}
		if err := envelope.err(); err != nil {
			return err
		}
		candidates, usage := envelope.resolve()
		for _, candidate := range candidates {
			for _, part := range candidate.Content.Parts {
				if part.Text != "" && !part.Thought {
					appendAntigravityText(&text, part.Text)
				}
				if part.FunctionCall != nil {
					args, _ := json.Marshal(part.FunctionCall.Args)
					key := part.FunctionCall.Name + "\x00" + string(args) + "\x00" + part.ThoughtSignature
					if _, exists := seenCalls[key]; !exists {
						seenCalls[key] = struct{}{}
						callID := antigravityCallID(sessionID, part.FunctionCall.Name, args, part.ThoughtSignature)
						antigravitySignatures.put(sessionID, callID, part.ThoughtSignature, time.Now())
						message.ToolCalls = append(message.ToolCalls, translator.OpenAIToolCall{ID: callID, Type: "function", Function: translator.OpenAIFunctionCall{Name: part.FunctionCall.Name, Arguments: string(args)}, ThoughtSignature: part.ThoughtSignature})
					}
				}
			}
			if candidate.FinishReason != "" {
				completed = true
				finish = antigravityFinishReason(candidate.FinishReason, len(message.ToolCalls) > 0)
			}
		}
		if usage.Prompt > 0 || usage.Candidates > 0 {
			response.Usage.PromptTokens = usage.Prompt
			response.Usage.CompletionTokens = usage.Candidates
			response.Usage.PromptTokensReported = true
			response.Usage.CompletionTokensReported = true
			response.Usage.PromptTokensDetails.CachedTokens = usage.Cached
			response.Usage.PromptTokensDetails.CachedTokensReported = usage.Cached > 0
		}
		return nil
	})
	if err != nil {
		return translator.OpenAIResponse{}, err
	}
	if !completed {
		return translator.OpenAIResponse{}, fmt.Errorf("Antigravity stream ended without finish reason")
	}
	message.Content = text.String()
	if finish == "" {
		finish = antigravityFinishReason("STOP", len(message.ToolCalls) > 0)
	}
	response.Choices = []translator.OpenAIChoice{{Message: message, FinishReason: finish}}
	return response, nil
}

func readAntigravitySSE(reader io.Reader, emit func([]byte) error) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var data strings.Builder
	flush := func() error {
		payload := strings.TrimSpace(data.String())
		data.Reset()
		if payload == "" || payload == "[DONE]" {
			return nil
		}
		return emit([]byte(payload))
	}
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") || strings.HasPrefix(line, "event:") || strings.HasPrefix(line, "id:") || strings.HasPrefix(line, "retry:") {
			continue
		}
		if strings.HasPrefix(line, "data:") {
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			continue
		}
		if data.Len() == 0 && strings.HasPrefix(strings.TrimSpace(line), "{") {
			data.WriteString(strings.TrimSpace(line))
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return flush()
}

// appendAntigravityText folds a part into the running text and returns only the
// newly added span. Antigravity sends cumulative text on some turns and true
// deltas on others, so the caller needs the difference to stream incrementally.
func appendAntigravityText(output *strings.Builder, value string) string {
	current := output.String()
	if current == "" {
		output.WriteString(value)
		return value
	}
	if strings.HasPrefix(value, current) {
		delta := strings.TrimPrefix(value, current)
		output.WriteString(delta)
		return delta
	}
	if strings.HasPrefix(current, value) {
		return ""
	}
	output.WriteString(value)
	return value
}

// antigravitySSEToChatStream converts the Antigravity SSE feed into OpenAI chat
// chunks as they arrive. Buffering the whole response first meant the caller saw
// no bytes for the entire turn and treated long generations as a hung proxy.
func antigravitySSEToChatStream(body io.ReadCloser, fallbackModel, sessionID string) io.ReadCloser {
	reader, writer := io.Pipe()
	go func() {
		defer body.Close()
		var text strings.Builder
		seenCalls := make(map[string]struct{})
		toolIndex := 0
		finish := ""
		completed := false
		var usage antigravityUsage
		// Gemini sometimes ends a turn having produced nothing but whitespace and no
		// function call — typically the turn right after a parallel tool batch. Sent
		// on as a normal completed turn, that reaches the agent as end_turn, so the
		// client considers the task finished and waits for the user, which is the
		// "it stops and I have to type continue" symptom. Whitespace is therefore held
		// back: if nothing real follows, this writes no content at all and the gateway
		// recognises an empty turn and retries it instead of handing it to the client.
		var pending strings.Builder
		emittedContent := false
		emit := func(delta map[string]any) error {
			encoded, err := json.Marshal(map[string]any{
				"id": "antigravity-response", "object": "chat.completion.chunk", "model": fallbackModel,
				"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}},
			})
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(writer, "data: %s\n\n", encoded)
			return err
		}
		err := readAntigravitySSE(body, func(raw []byte) error {
			var envelope antigravityStreamEnvelope
			if err := json.Unmarshal(raw, &envelope); err != nil {
				return fmt.Errorf("decode Antigravity stream: %w", err)
			}
			if err := envelope.err(); err != nil {
				return err
			}
			candidates, chunkUsage := envelope.resolve()
			for _, candidate := range candidates {
				for _, part := range candidate.Content.Parts {
					if part.Text != "" && !part.Thought {
						if delta := appendAntigravityText(&text, part.Text); delta != "" {
							if !emittedContent && strings.TrimSpace(delta) == "" {
								pending.WriteString(delta)
							} else {
								payload := pending.String() + delta
								pending.Reset()
								if err := emit(map[string]any{"content": payload}); err != nil {
									return err
								}
								emittedContent = true
							}
						}
					}
					if part.FunctionCall == nil {
						continue
					}
					args, _ := json.Marshal(part.FunctionCall.Args)
					key := part.FunctionCall.Name + "\x00" + string(args) + "\x00" + part.ThoughtSignature
					if _, exists := seenCalls[key]; exists {
						continue
					}
					seenCalls[key] = struct{}{}
					// The turn is real, so any whitespace held back before it is genuine
					// output and is flushed rather than dropped.
					if pending.Len() > 0 {
						payload := pending.String()
						pending.Reset()
						if err := emit(map[string]any{"content": payload}); err != nil {
							return err
						}
						emittedContent = true
					}
					callID := antigravityCallID(sessionID, part.FunctionCall.Name, args, part.ThoughtSignature)
					antigravitySignatures.put(sessionID, callID, part.ThoughtSignature, time.Now())
					if err := emit(map[string]any{"tool_calls": []any{map[string]any{
						"index": toolIndex, "id": callID, "type": "function",
						"function": map[string]string{"name": part.FunctionCall.Name, "arguments": string(args)},
					}}}); err != nil {
						return err
					}
					toolIndex++
				}
				if candidate.FinishReason != "" {
					completed = true
					finish = antigravityFinishReason(candidate.FinishReason, toolIndex > 0)
				}
			}
			if chunkUsage.Prompt > 0 || chunkUsage.Candidates > 0 {
				usage = chunkUsage
			}
			return nil
		})
		if err == nil && !completed {
			err = fmt.Errorf("Antigravity stream ended without finish reason")
		}
		if err != nil {
			_ = writer.CloseWithError(err)
			return
		}
		if finish == "" {
			finish = antigravityFinishReason("STOP", toolIndex > 0)
		}
		if toolIndex == 0 && !emittedContent {
			// Surfacing this is the point: a turn with neither content nor a tool call
			// is what stalls the agent, and it is otherwise invisible in the logs.
			slog.Warn("Antigravity produced a turn with no content and no tool calls",
				"model", fallbackModel, "held_whitespace_bytes", pending.Len(), "finish_reason", finish)
		}
		terminal := map[string]any{
			"id": "antigravity-response", "object": "chat.completion.chunk", "model": fallbackModel,
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}},
		}
		if usage.Prompt > 0 || usage.Candidates > 0 {
			terminal["usage"] = map[string]any{
				"prompt_tokens": usage.Prompt, "completion_tokens": usage.Candidates,
				"total_tokens":          usage.Prompt + usage.Candidates,
				"prompt_tokens_details": map[string]int{"cached_tokens": usage.Cached},
			}
		}
		encoded, err := json.Marshal(terminal)
		if err != nil {
			_ = writer.CloseWithError(err)
			return
		}
		if _, err := fmt.Fprintf(writer, "data: %s\n\ndata: [DONE]\n\n", encoded); err != nil {
			_ = writer.CloseWithError(err)
			return
		}
		_ = writer.Close()
	}()
	return reader
}

func antigravityFinishReason(reason string, hasTools bool) string {
	if hasTools {
		return "tool_calls"
	}
	switch strings.ToUpper(strings.TrimSpace(reason)) {
	case "MAX_TOKENS", "MAX_OUTPUT_TOKENS":
		return "length"
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII":
		return "content_filter"
	default:
		return "stop"
	}
}

func parseDataURL(value string) (string, string, bool) {
	prefix, data, ok := strings.Cut(value, ",")
	if !ok || !strings.HasPrefix(prefix, "data:") || !strings.HasSuffix(prefix, ";base64") {
		return "", "", false
	}
	return strings.TrimSuffix(strings.TrimPrefix(prefix, "data:"), ";base64"), data, true
}
func sanitizeAntigravityToolName(name string) string {
	var b strings.Builder
	for i, r := range name {
		valid := r == '_' || r == '.' || r == ':' || r == '-' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || i > 0 && r >= '0' && r <= '9'
		if valid {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
		if b.Len() >= 64 {
			break
		}
	}
	if b.Len() == 0 {
		return "_unknown"
	}
	return b.String()
}

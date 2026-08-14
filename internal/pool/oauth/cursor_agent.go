package oauth

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"runtime"
	"strconv"
	"strings"

	"literouter/internal/contextguard"
	"literouter/internal/translator"
)

// Cursor 3.15 drives Composer through agent.v1.AgentService on api5, not through the
// aiserver.v1.ChatService endpoint on api2 that this package originally targeted.
// api2 now answers every inference request with a payment/rate-limit error dressed up
// as "Your version of Cursor is no longer supported", regardless of client version,
// machine id or model — while GetUsableModels on api5 accepts the same session and
// lists the models the plan may use. So the agent transport is the only working path.
//
// The schema is unpublished. Field numbers below were read out of the IDE's bundled
// protobuf-es descriptors (`out/vs/workbench/workbench.desktop.main.js`, the
// `makeMessageType("agent.v1.…")` definitions). They are load-bearing and produce no
// compile error when wrong, so each message is written out explicitly.
const (
	cursorAgentBaseURL = "https://agent.api5.cursor.sh"
	cursorAgentRunPath = "/agent.v1.AgentService/Run"
)

// agent.v1.AgentMode
const (
	cursorAgentModeAgent = 1
	cursorAgentModeAsk   = 2
)

// encodeAgentUserMessage builds agent.v1.UserMessage.
func encodeAgentUserMessage(text, messageID string, mode int) []byte {
	return protoConcat(
		protoField(1, protoBytes, text),
		protoField(2, protoBytes, messageID),
		protoField(4, protoVarint, mode),
	)
}

// encodeAgentRequestContext builds agent.v1.RequestContext. Only the environment
// block is populated: everything else describes an open workspace, which a proxy does
// not have, and the server treats the absent fields as "not collected".
func encodeAgentRequestContext(tools []translator.OpenAITool) []byte {
	env := protoConcat(
		protoField(1, protoBytes, cursorOSVersion()),
		protoField(3, protoBytes, "/bin/zsh"),
		protoField(10, protoBytes, cursorTimezone()),
	)
	parts := [][]byte{protoField(4, protoBytes, env)}
	for _, tool := range tools {
		parts = append(parts, protoField(7, protoBytes, encodeAgentToolDefinition(tool)))
	}
	return protoConcat(parts...)
}

// encodeAgentToolDefinition builds agent.v1.McpToolDefinition, which is how a caller's
// own tools are declared to the agent service.
//
// The schema goes in input_schema_json (field 6), not input_schema (field 3): the
// latter is a google.protobuf.Struct, and putting raw JSON there makes the server
// reject the whole message with "illegal tag".
func encodeAgentToolDefinition(tool translator.OpenAITool) []byte {
	schema := "{}"
	if len(tool.Function.Parameters) > 0 {
		schema = string(tool.Function.Parameters)
	}
	return protoConcat(
		protoField(1, protoBytes, tool.Function.Name),
		protoField(2, protoBytes, tool.Function.Description),
		protoField(4, protoBytes, cursorToolProvider),
		protoField(5, protoBytes, tool.Function.Name),
		protoField(6, protoBytes, schema),
	)
}

// cursorToolProvider names the MCP server the caller's tools appear to come from. The
// agent groups tools by it, so it has to be stable and distinct from a real server.
const cursorToolProvider = "literouter"

// encodeAgentModelDetails builds agent.v1.ModelDetails. The display fields are what
// the IDE shows in its picker; the server echoes them back and rejects an empty id.
func encodeAgentModelDetails(model string) []byte {
	return protoConcat(
		protoField(1, protoBytes, model),
		protoField(3, protoBytes, model),
		protoField(4, protoBytes, model),
	)
}

func cursorEffortEnabled() bool {
	enabled, _ := strconv.ParseBool(strings.TrimSpace(os.Getenv("LITEROUTER_CURSOR_EFFORT")))
	return enabled
}

func validCursorEffort(effort string) bool {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "low", "medium", "high", "xhigh", "max":
		return true
	default:
		return false
	}
}

// encodeAgentRunRequest builds agent.v1.AgentClientMessage carrying an AgentRunRequest.
// The stream's first frame must be this message; later frames carry blob replies.
func encodeAgentRunRequest(text, conversationID, messageID, model string, tools []translator.OpenAITool) []byte {
	return encodeAgentRunRequestWithState(text, conversationID, messageID, model, tools, nil, "")
}

// encodeAgentRunRequestWithState replays a stored ConversationStateStructure so the
// service rebuilds the history itself instead of receiving it again in the prompt.
func encodeAgentRunRequestWithState(text, conversationID, messageID, model string,
	tools []translator.OpenAITool, state []byte, effort string) []byte {
	// Cursor's agent schema exposes RequestedModel.parameters as generic id/value pairs.
	// Forwarding is opt-in because Composer support for the `effort` parameter is not
	// guaranteed by the private service; older/default behavior remains byte-identical.
	mode := cursorAgentModeAsk
	if len(tools) > 0 {
		mode = cursorAgentModeAgent
	}
	userMessageAction := protoConcat(
		protoField(1, protoBytes, encodeAgentUserMessage(text, messageID, mode)),
		protoField(2, protoBytes, encodeAgentRequestContext(tools)),
	)
	action := protoField(1, protoBytes, userMessageAction)
	if state == nil {
		state = []byte{} // no prior turns
	}
	requestedModel := protoConcat(protoField(1, protoBytes, model))
	if effort = strings.ToLower(strings.TrimSpace(effort)); validCursorEffort(effort) && cursorEffortEnabled() {
		requestedModel = protoConcat(
			requestedModel,
			protoField(3, protoBytes, protoConcat(
				protoField(1, protoBytes, "effort"),
				protoField(2, protoBytes, effort),
			)),
		)
	}
	runRequest := protoConcat(
		protoField(1, protoBytes, state),
		protoField(2, protoBytes, action),
		protoField(3, protoBytes, encodeAgentModelDetails(model)),
		protoField(5, protoBytes, conversationID),
		protoField(9, protoBytes, requestedModel), // requested_model
	)
	return connectFrame(protoField(1, protoBytes, runRequest), false)
}

// cursorAgentUpdate is one decoded agent.v1.InteractionUpdate.
type cursorAgentUpdate struct {
	Text     string
	Thinking string
	ToolCall *CursorToolCall
	Ended    bool
	// Tokens is the output-token increment the service reports as it generates. It is
	// a delta, not a running total: summing the stream reproduces the completion count
	// (verified against a 30-line completion — 31 tokens for 80 characters).
	Tokens int

	// Idle marks a frame that carries no content: a heartbeat, or the exec hand-off
	// the server sends when it expects the client to act. After a tool call these are
	// the only frames that arrive, because the agent is waiting for a result that this
	// proxy will not send on the same stream — so they are what ends the turn.
	Idle bool
}

// agent.v1.InteractionUpdate field numbers.
const (
	agentUpdateTextDelta       = 1
	agentUpdateToolCallStarted = 2
	agentUpdateThinkingDelta   = 4
	agentUpdateTokenDelta      = 8
	agentUpdateHeartbeat       = 13
	agentUpdateTurnEnded       = 14
)

// agent.v1.AgentServerMessage field numbers.
const (
	agentServerInteractionField = 1
	agentServerExecField        = 2
)

// decodeAgentServerMessage reads one agent.v1.AgentServerMessage frame. Unknown
// updates are reported as empty rather than as an error: the server adds new update
// kinds without notice and a proxy must not fail on one it has never seen.
func decodeAgentServerMessage(payload []byte) cursorAgentUpdate {
	fields := parseProtoFields(payload)
	if _, found := fields[agentServerExecField]; found {
		return cursorAgentUpdate{Idle: true}
	}
	values, ok := fields[agentServerInteractionField]
	if !ok || len(values) == 0 || values[0].WireType != protoBytes {
		return cursorAgentUpdate{}
	}
	update := parseProtoFields(values[0].Bytes)
	if _, found := update[agentUpdateHeartbeat]; found {
		return cursorAgentUpdate{Idle: true}
	}
	if text, found := agentNestedString(update, agentUpdateTextDelta, 1); found {
		return cursorAgentUpdate{Text: text}
	}
	if text, found := agentNestedString(update, agentUpdateThinkingDelta, 1); found {
		return cursorAgentUpdate{Thinking: text}
	}
	if _, found := update[agentUpdateTurnEnded]; found {
		return cursorAgentUpdate{Ended: true}
	}
	if values, found := update[agentUpdateTokenDelta]; found && len(values) > 0 {
		inner := parseProtoFields(values[0].Bytes)
		if counts, ok := inner[1]; ok && len(counts) > 0 {
			return cursorAgentUpdate{Tokens: int(counts[0].Varint)}
		}
	}
	if values, found := update[agentUpdateToolCallStarted]; found && len(values) > 0 {
		if call := decodeAgentToolCall(values[0].Bytes); call != nil {
			return cursorAgentUpdate{ToolCall: call}
		}
	}
	return cursorAgentUpdate{}
}

// decodeAgentToolCall reads agent.v1.ToolCallStartedUpdate.
//
// Only mcp_tool_call is handled: tools declared by the caller come back through it,
// while the rest of the ToolCall oneof is Cursor's own IDE tooling (shell, edit, grep)
// which a proxy has no way to execute and never declares.
func decodeAgentToolCall(payload []byte) *CursorToolCall {
	started := parseProtoFields(payload)
	inner, ok := started[agentToolCallField]
	if !ok || len(inner) == 0 {
		return nil
	}
	mcp, ok := parseProtoFields(inner[0].Bytes)[agentMcpToolCallField]
	if !ok || len(mcp) == 0 {
		return nil
	}
	args, ok := parseProtoFields(mcp[0].Bytes)[agentMcpArgsField]
	if !ok || len(args) == 0 {
		return nil
	}
	fields := parseProtoFields(args[0].Bytes)
	call := &CursorToolCall{Arguments: "{}"}
	if name, ok := agentString(fields, 1); ok {
		call.Name = name
	}
	if toolName, ok := agentString(fields, 5); ok && toolName != "" {
		call.Name = toolName
	}
	if id, ok := agentString(fields, 3); ok {
		call.ID = id
	}
	if id, ok := agentString(started, 1); ok && call.ID == "" {
		call.ID = id
	}
	if arguments, ok := fields[agentMcpArgsMapField]; ok && len(arguments) > 0 {
		if encoded, err := json.Marshal(decodeProtoValueMap(arguments)); err == nil {
			call.Arguments = string(encoded)
		}
	}
	return call
}

const (
	agentToolCallField    = 2 // ToolCallStartedUpdate.tool_call
	agentMcpToolCallField = 15
	agentMcpArgsField     = 1
	agentMcpArgsMapField  = 2 // McpArgs.args, a map<string, google.protobuf.Value>
)

// decodeProtoValueMap turns a protobuf map<string, google.protobuf.Value> into the
// JSON object an OpenAI client expects for tool arguments.
//
// A map field is not a wrapper message: each entry is its own occurrence of the field,
// carrying key in 1 and value in 2. Reading only the first occurrence — as decoding a
// Struct would — silently drops every argument but one.
func decodeProtoValueMap(entries []protoFieldValue) map[string]any {
	out := map[string]any{}
	for _, entry := range entries {
		if entry.WireType != protoBytes {
			continue
		}
		pair := parseProtoFields(entry.Bytes)
		key, ok := agentString(pair, 1)
		if !ok {
			continue
		}
		if values, ok := pair[2]; ok && len(values) > 0 {
			out[key] = decodeProtoValue(values[0].Bytes)
		} else {
			out[key] = nil
		}
	}
	return out
}

// decodeProtoValue converts a google.protobuf.Value. The oneof numbering is fixed by
// the well-known type, so it is safe to hard-code here.
func decodeProtoValue(payload []byte) any {
	fields := parseProtoFields(payload)
	if values, ok := fields[2]; ok && len(values) > 0 && values[0].WireType == 1 {
		return math.Float64frombits(binary.LittleEndian.Uint64(values[0].Bytes))
	}
	if text, ok := agentString(fields, 3); ok {
		return text
	}
	if values, ok := fields[4]; ok && len(values) > 0 {
		return values[0].Varint != 0
	}
	if values, ok := fields[5]; ok && len(values) > 0 {
		return decodeProtoValueMap(parseProtoFields(values[0].Bytes)[1])
	}
	if values, ok := fields[6]; ok && len(values) > 0 {
		var list []any
		for _, item := range parseProtoFields(values[0].Bytes)[1] {
			list = append(list, decodeProtoValue(item.Bytes))
		}
		return list
	}
	return nil
}

// cursorAgentRequestBody builds the single framed message that opens a run.
//
// When a conversation can be continued, only the messages the service has not seen are
// sent and the rest is replayed as state — that is where the token saving comes from.
// Otherwise the whole transcript is folded into the prompt, which is always correct.
func cursorAgentRequestBody(request translator.OpenAIRequest, model string,
	conversation *cursorConversation, pending []translator.OpenAIMessage) ([]byte, string, int, error) {
	prompt := agentPromptFromRequest(request)
	conversationID := cursorAgentConversationID(prompt)
	var state []byte
	if conversation != nil && len(pending) > 0 {
		delta := agentPromptFromRequest(translator.OpenAIRequest{Messages: pending})
		if delta != "" {
			prompt, state, conversationID = delta, conversation.state, conversation.id
		}
	}
	if prompt == "" {
		return nil, "", 0, fmt.Errorf("cursor request has no prompt text")
	}
	messageID, err := randomUUID()
	if err != nil {
		return nil, "", 0, err
	}
	// The prompt that actually goes upstream is what this request costs. With a
	// conversation replayed, that is the delta rather than the transcript, and
	// reporting the client's whole conversation instead would hide exactly the saving
	// the cache exists to produce.
	sent := contextguard.EstimateText(prompt)
	return encodeAgentRunRequestWithState(prompt, conversationID, messageID,
		resolveCursorModel(model), request.Tools, state, request.Effort), conversationID, sent, nil
}

// cursorAgentToChatStream converts the agent frame stream into OpenAI chat chunks.
//
// The turn_ended update is the terminator: the server keeps the stream open with
// heartbeats afterwards, so reading to EOF would hang until the connection drops.
func cursorAgentToChatStream(body io.ReadCloser, fallbackModel string) io.ReadCloser {
	session, _ := body.(*cursorRunSession)
	reader, writer := io.Pipe()
	go func() {
		defer body.Close()
		toolIndex := 0
		emittedTool := false
		outputTokens := 0

		emit := func(delta map[string]any) error {
			chunk := map[string]any{
				"id": "cursor-response", "object": "chat.completion.chunk", "model": fallbackModel,
				"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}},
			}
			encoded, err := json.Marshal(chunk)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(writer, "data: %s\n\n", encoded)
			return err
		}

		err := readCursorFrames(body, func(flags byte, payload []byte) error {
			if flags&connectFlagEndStream != 0 {
				return cursorEndStreamError(payload)
			}
			if session != nil && session.handleKV(payload) {
				return nil
			}
			update := decodeAgentServerMessage(payload)
			switch {
			case update.Ended:
				// The real terminal. Measured live: ENDED arrives only after the final
				// TEXT/TOKENS, and what follows are endless idle heartbeats — reading
				// past it would hang until the connection drops. The conversation
				// state blobs arrive just before it, so the cache still commits.
				return errCursorTurnEnded
			case update.Idle:
				// Before any tool call an idle frame is just a keep-alive. After one the
				// agent has handed control to the client and will never end the turn on
				// its own, so this is the only terminator that arrives.
				if emittedTool {
					return errCursorTurnEnded
				}
			case update.ToolCall != nil:
				call := update.ToolCall
				if err := emit(map[string]any{"tool_calls": []any{map[string]any{
					"index": toolIndex, "id": call.ID, "type": "function",
					"function": map[string]string{"name": call.Name, "arguments": call.Arguments},
				}}}); err != nil {
					return err
				}
				toolIndex++
				emittedTool = true
			case update.Text != "":
				return emit(map[string]any{"content": update.Text})
			case update.Thinking != "":
				return emit(map[string]any{"reasoning_content": update.Thinking})
			case update.Tokens > 0:
				// A delta of the token counter, emitted after every thinking/text
				// chunk — not a terminal. Measured live: TOKENS arrives after each
				// delta and the turn continues for a minute more. Ending here would
				// cut a long-thinking turn off right after its first thought.
				outputTokens += update.Tokens
			}
			return nil
		})
		if err != nil && !errors.Is(err, errCursorTurnEnded) {
			_ = writer.CloseWithError(err)
			return
		}
		session.finish()
		finish := "stop"
		if emittedTool {
			finish = "tool_calls"
		}
		terminal := map[string]any{
			"id": "cursor-response", "object": "chat.completion.chunk", "model": fallbackModel,
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}},
		}
		if outputTokens > 0 || session.promptTokens() > 0 {
			// The completion count is the service's own. The prompt count is this
			// proxy's measurement of what it sent, flagged so the dashboard shows it as
			// an estimate rather than as an upstream figure.
			usage := map[string]any{
				"completion_tokens": outputTokens, "total_tokens": outputTokens,
			}
			if sent := session.promptTokens(); sent > 0 {
				usage["prompt_tokens"] = sent
				usage["total_tokens"] = outputTokens + sent
			}
			terminal["usage"] = usage
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

// errCursorTurnEnded stops frame reading at the end of a turn. It is control flow, not
// a failure, and never reaches the caller.
var errCursorTurnEnded = errors.New("cursor agent turn ended")

// cursorAgentToOpenAI collects a whole agent run into one response for the
// non-streaming endpoints, sharing the frame reader with the streaming path.
func cursorAgentToOpenAI(body io.Reader, fallbackModel string) (translator.OpenAIResponse, error) {
	session, _ := body.(*cursorRunSession)
	response := translator.OpenAIResponse{ID: "cursor-response", Model: fallbackModel}
	message := translator.OpenAIMessage{Role: "assistant"}
	var text strings.Builder
	outputTokens := 0

	err := readCursorFrames(body, func(flags byte, payload []byte) error {
		if flags&connectFlagEndStream != 0 {
			return cursorEndStreamError(payload)
		}
		if session != nil && session.handleKV(payload) {
			return nil
		}
		update := decodeAgentServerMessage(payload)
		switch {
		case update.Ended:
			return errCursorTurnEnded
		case update.Idle:
			if len(message.ToolCalls) > 0 {
				return errCursorTurnEnded
			}
		case update.ToolCall != nil:
			message.ToolCalls = append(message.ToolCalls, translator.OpenAIToolCall{
				ID: update.ToolCall.ID, Type: "function",
				Function: translator.OpenAIFunctionCall{
					Name: update.ToolCall.Name, Arguments: update.ToolCall.Arguments,
				},
			})
		case update.Text != "":
			text.WriteString(update.Text)
		case update.Tokens > 0:
			outputTokens += update.Tokens
		}
		return nil
	})
	if err != nil && !errors.Is(err, errCursorTurnEnded) {
		return translator.OpenAIResponse{}, err
	}
	session.finish()
	message.Content = text.String()
	finish := "stop"
	if len(message.ToolCalls) > 0 {
		finish = "tool_calls"
	}
	response.Choices = []translator.OpenAIChoice{{Message: message, FinishReason: finish}}
	response.Usage.CompletionTokens = outputTokens
	response.Usage.CompletionTokensReported = true
	if sent := session.promptTokens(); sent > 0 {
		response.Usage.PromptTokens = sent
		response.Usage.PromptTokensReported = false
	}
	return response, nil
}

func agentString(fields map[int][]protoFieldValue, number int) (string, bool) {
	values, ok := fields[number]
	if !ok || len(values) == 0 || values[0].WireType != protoBytes {
		return "", false
	}
	return string(values[0].Bytes), true
}

func agentNestedString(fields map[int][]protoFieldValue, outer, inner int) (string, bool) {
	values, ok := fields[outer]
	if !ok || len(values) == 0 || values[0].WireType != protoBytes {
		return "", false
	}
	return agentString(parseProtoFields(values[0].Bytes), inner)
}

// cursorOSVersion reports the host in the shape the IDE uses, e.g. "darwin 24.6.0".
// Only the family is known here; the server uses it for telemetry, not for routing.
func cursorOSVersion() string {
	return fmt.Sprintf("%s %s", runtime.GOOS, runtime.GOARCH)
}

// agentPromptFromRequest flattens a conversation into the single prompt the run
// request carries. The agent schema keeps prior turns in conversation_state, which a
// stateless proxy cannot reproduce, so history is folded into the prompt instead —
// losing it would silently change the model's answer.
//
// Tool results are deduplicated, which the fold makes safe in a way the generic
// pipeline cannot: when the whole transcript becomes one prompt, an exact repeated
// result is a copy of what still appears later, so dropping the earlier copy keeps
// the model seeing every result once — it cannot walk away without a result it was
// shown before. Distinct results are never touched, and the repeated copy is left as
// a short marker so the model can tell the earlier read happened. Only the fold path
// gets this: a model that asks for the same file twice must still see the second
// read (edit quoting), but the first occurrence in the same fold is redundant by
// definition. The conversation cache keys off this prompt, so the dedup is a fixed
// transformation, not a moving target between turns.
func agentPromptFromRequest(request translator.OpenAIRequest) string {
	var out strings.Builder
	seen := make(map[string]struct{})
	duplicates := 0
	for _, message := range request.Messages {
		text := strings.TrimSpace(openAIContentText(message.Content))
		// An assistant turn that only called tools has no text at all; it must still
		// surface the tool calls, or the model sees the following tool result with
		// nothing to pair it to and calls the tool again.
		if text == "" && len(message.ToolCalls) == 0 {
			continue
		}
		switch message.Role {
		case "system", "developer":
			out.WriteString(text)
		case "user":
			out.WriteString("\n\n" + text)
		case "assistant":
			if text != "" {
				out.WriteString("\n\nAssistant: " + text)
			}
			// The tool calls this assistant turn made must survive the fold: without
			// them the model sees a bare tool result with no tool_use it can pair it
			// to, and answers by calling the tool again — the repeated-tool-call loop.
			for _, call := range message.ToolCalls {
				if call.Function.Name == "" {
					continue
				}
				out.WriteString("\n\nTool call: " + call.Function.Name + "(" + call.Function.Arguments + ")")
			}
		case "tool":
			if _, repeat := seen[text]; repeat {
				duplicates++
				continue
			}
			seen[text] = struct{}{}
			out.WriteString("\n\nTool result: " + text)
		}
	}
	if duplicates > 0 {
		slog.Info("cursor prompt deduplicated repeated tool results", "duplicate_results", duplicates,
			"messages", len(request.Messages))
	}
	return strings.TrimSpace(out.String())
}

// cursorAgentConversationID derives a stable id for a conversation so retries of the
// same turn are not treated as separate conversations.
func cursorAgentConversationID(prompt string) string {
	return uuidV5DNS("literouter-agent:" + prompt)
}

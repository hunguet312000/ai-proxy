package oauth

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"literouter/internal/translator"
)

// agentFrame wraps a payload the way the service does, so decoder tests read the same
// bytes the wire carries.
func agentFrame(payload []byte) []byte { return connectFrame(payload, false) }

// agentServerFrame builds AgentServerMessage{interaction_update: <update>}.
func agentServerFrame(update []byte) []byte {
	return agentFrame(protoField(agentServerInteractionField, protoBytes, update))
}

func TestEncodeAgentRunRequestCarriesModelPromptAndMode(t *testing.T) {
	frame := encodeAgentRunRequest("hello", "conversation-1", "message-1", "composer-2.5-fast", nil)
	if len(frame) < 5 {
		t.Fatalf("frame too short: %d bytes", len(frame))
	}
	client := parseProtoFields(frame[5:])
	runRequest, ok := client[1]
	if !ok || len(runRequest) == 0 {
		t.Fatal("AgentClientMessage carries no run_request")
	}
	fields := parseProtoFields(runRequest[0].Bytes)

	if got, _ := agentString(fields, 5); got != "conversation-1" {
		t.Errorf("conversation_id = %q, want conversation-1", got)
	}
	model, _ := agentNestedString(fields, 3, 1)
	if model != "composer-2.5-fast" {
		t.Errorf("model_details.model_id = %q, want composer-2.5-fast", model)
	}
	// requested_model must agree with model_details: the server picks the model from
	// one and bills the other, and a mismatch is not reported.
	requested, _ := agentNestedString(fields, 9, 1)
	if requested != model {
		t.Errorf("requested_model = %q, want %q", requested, model)
	}

	action := parseProtoFields(fields[2][0].Bytes)
	userAction := parseProtoFields(action[1][0].Bytes)
	message := parseProtoFields(userAction[1][0].Bytes)
	if text, _ := agentString(message, 1); text != "hello" {
		t.Errorf("user_message.text = %q, want hello", text)
	}
	if mode := message[4][0].Varint; mode != cursorAgentModeAsk {
		t.Errorf("mode = %d, want ask (%d) when no tools are declared", mode, cursorAgentModeAsk)
	}
}

func TestEncodeAgentRunRequestDeclaresToolsAndSwitchesToAgentMode(t *testing.T) {
	tools := []translator.OpenAITool{{Type: "function", Function: translator.OpenAIFunction{
		Name:        "get_weather",
		Description: "Get weather",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
	}}}
	frame := encodeAgentRunRequest("hi", "c", "m", "composer-2.5-fast", tools)
	fields := parseProtoFields(parseProtoFields(frame[5:])[1][0].Bytes)
	action := parseProtoFields(fields[2][0].Bytes)
	userAction := parseProtoFields(action[1][0].Bytes)

	message := parseProtoFields(userAction[1][0].Bytes)
	if mode := message[4][0].Varint; mode != cursorAgentModeAgent {
		t.Errorf("mode = %d, want agent (%d) when tools are declared", mode, cursorAgentModeAgent)
	}

	context := parseProtoFields(userAction[2][0].Bytes)
	declared, ok := context[7]
	if !ok || len(declared) != 1 {
		t.Fatalf("request context declares %d tools, want 1", len(declared))
	}
	tool := parseProtoFields(declared[0].Bytes)
	if name, _ := agentString(tool, 1); name != "get_weather" {
		t.Errorf("tool name = %q, want get_weather", name)
	}
	// The schema goes in input_schema_json (6). Field 3 is a Struct, and raw JSON there
	// makes the server reject the whole message.
	schema, ok := agentString(tool, 6)
	if !ok || !strings.Contains(schema, `"city"`) {
		t.Errorf("input_schema_json = %q, want the declared JSON schema", schema)
	}
	if _, present := tool[3]; present {
		t.Error("input_schema (field 3) must stay unset: it is a Struct, not JSON")
	}
}

func TestAgentPromptFoldsHistoryIncludingToolResults(t *testing.T) {
	prompt := agentPromptFromRequest(translator.OpenAIRequest{Messages: []translator.OpenAIMessage{
		{Role: "system", Content: "Be terse."},
		{Role: "user", Content: "weather?"},
		{Role: "assistant", Content: "checking"},
		{Role: "tool", Content: "22C"},
		{Role: "user", Content: "and tomorrow?"},
	}})
	for _, want := range []string{"Be terse.", "weather?", "checking", "22C", "and tomorrow?"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lost %q:\n%s", want, prompt)
		}
	}
}

func TestDecodeAgentToolCallReadsNameAndMapArguments(t *testing.T) {
	// McpArgs.args is a map<string, Value>: each entry is its own occurrence of field 2,
	// so a decoder that reads only the first drops every argument but one.
	stringValue := func(text string) []byte { return protoField(3, protoBytes, text) }
	entry := func(key string, value []byte) []byte {
		return protoField(agentMcpArgsMapField, protoBytes, protoConcat(
			protoField(1, protoBytes, key),
			protoField(2, protoBytes, value)))
	}
	args := protoConcat(
		protoField(1, protoBytes, "get_weather"),
		entry("city", stringValue("Hanoi")),
		entry("unit", stringValue("c")),
		protoField(3, protoBytes, "tool_abc"),
		protoField(5, protoBytes, "get_weather"),
	)
	started := protoConcat(
		protoField(1, protoBytes, "tool_abc"),
		protoField(agentToolCallField, protoBytes,
			protoField(agentMcpToolCallField, protoBytes,
				protoField(agentMcpArgsField, protoBytes, args))),
	)

	call := decodeAgentToolCall(started)
	if call == nil {
		t.Fatal("tool call did not decode")
	}
	if call.Name != "get_weather" || call.ID != "tool_abc" {
		t.Errorf("name/id = %q/%q, want get_weather/tool_abc", call.Name, call.ID)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(call.Arguments), &decoded); err != nil {
		t.Fatalf("arguments are not JSON: %v (%s)", err, call.Arguments)
	}
	if decoded["city"] != "Hanoi" || decoded["unit"] != "c" {
		t.Errorf("arguments = %s, want both map entries", call.Arguments)
	}
}

func TestDecodeProtoValueHandlesEveryKind(t *testing.T) {
	list := protoField(6, protoBytes, protoConcat(
		protoField(1, protoBytes, protoField(3, protoBytes, "a")),
		protoField(1, protoBytes, protoField(4, protoVarint, true)),
	))
	value := decodeProtoValue(list)
	items, ok := value.([]any)
	if !ok || len(items) != 2 || items[0] != "a" || items[1] != true {
		t.Fatalf("list value = %#v, want [a true]", value)
	}
	if got := decodeProtoValue(protoField(4, protoVarint, false)); got != false {
		t.Errorf("bool value = %#v, want false", got)
	}
}

func TestAgentStreamEmitsOpenAIChunksAndStopsAtTurnEnd(t *testing.T) {
	body := bytes.NewBuffer(nil)
	body.Write(agentServerFrame(protoField(agentUpdateThinkingDelta, protoBytes,
		protoField(1, protoBytes, "thinking"))))
	body.Write(agentServerFrame(protoField(agentUpdateTextDelta, protoBytes,
		protoField(1, protoBytes, "pong"))))
	body.Write(agentServerFrame(protoField(agentUpdateTurnEnded, protoBytes, []byte{})))
	// The service keeps heartbeating after the turn; reading past the end would hang.
	body.Write(agentServerFrame(protoField(agentUpdateHeartbeat, protoBytes, []byte{})))

	out, err := io.ReadAll(cursorAgentToChatStream(io.NopCloser(body), "composer-2.5-fast"))
	if err != nil {
		t.Fatalf("stream failed: %v", err)
	}
	text := string(out)
	for _, want := range []string{`"reasoning_content":"thinking"`, `"content":"pong"`,
		`"finish_reason":"stop"`, "data: [DONE]"} {
		if !strings.Contains(text, want) {
			t.Errorf("stream missing %s:\n%s", want, text)
		}
	}
}

func TestAgentStreamEndsTheTurnWhenAToolCallLeavesItIdle(t *testing.T) {
	// After a tool call the service never sends turn_ended: it waits for a result on
	// the same stream, which a stateless proxy will not send. The idle frame is the
	// only terminator, and without it the response never completes.
	args := protoConcat(
		protoField(1, protoBytes, "get_weather"),
		protoField(3, protoBytes, "tool_1"),
	)
	toolCall := protoField(agentUpdateToolCallStarted, protoBytes, protoConcat(
		protoField(1, protoBytes, "tool_1"),
		protoField(agentToolCallField, protoBytes,
			protoField(agentMcpToolCallField, protoBytes,
				protoField(agentMcpArgsField, protoBytes, args)))))

	body := bytes.NewBuffer(nil)
	body.Write(agentServerFrame(toolCall))
	body.Write(agentFrame(protoField(agentServerExecField, protoBytes, []byte{})))
	body.Write(agentServerFrame(protoField(agentUpdateHeartbeat, protoBytes, []byte{})))

	out, err := io.ReadAll(cursorAgentToChatStream(io.NopCloser(body), "composer-2.5-fast"))
	if err != nil {
		t.Fatalf("stream failed: %v", err)
	}
	text := string(out)
	if !strings.Contains(text, `"name":"get_weather"`) {
		t.Errorf("stream lost the tool call:\n%s", text)
	}
	if !strings.Contains(text, `"finish_reason":"tool_calls"`) {
		t.Errorf("stream did not finish as tool_calls:\n%s", text)
	}
}

func TestAgentStreamKeepsRunningThroughIdleFramesBeforeAnyToolCall(t *testing.T) {
	// A heartbeat before the model has produced anything is just a keep-alive. Ending
	// there would truncate every slow first token.
	body := bytes.NewBuffer(nil)
	body.Write(agentServerFrame(protoField(agentUpdateHeartbeat, protoBytes, []byte{})))
	body.Write(agentServerFrame(protoField(agentUpdateTextDelta, protoBytes,
		protoField(1, protoBytes, "late"))))
	body.Write(agentServerFrame(protoField(agentUpdateTurnEnded, protoBytes, []byte{})))

	out, err := io.ReadAll(cursorAgentToChatStream(io.NopCloser(body), "m"))
	if err != nil {
		t.Fatalf("stream failed: %v", err)
	}
	if !strings.Contains(string(out), `"content":"late"`) {
		t.Errorf("stream dropped content after an early heartbeat:\n%s", out)
	}
}

func TestAgentStreamSurfacesEndStreamErrors(t *testing.T) {
	failure := `{"error":{"code":"resource_exhausted","message":"Error","details":[{"debug":{"details":{"title":"Rate limited","detail":"slow down"}}}]}}`
	body := bytes.NewBuffer(nil)
	body.Write([]byte{connectFlagEndStream, 0, 0, 0, byte(len(failure))})
	body.WriteString(failure)

	_, err := io.ReadAll(cursorAgentToChatStream(io.NopCloser(body), "m"))
	if err == nil {
		t.Fatal("end-of-stream error was swallowed")
	}
	if !strings.Contains(err.Error(), "slow down") {
		t.Errorf("error = %v, want the actionable detail", err)
	}
}

func TestAgentNonStreamingCollectsTextAndToolCalls(t *testing.T) {
	body := bytes.NewBuffer(nil)
	body.Write(agentServerFrame(protoField(agentUpdateTextDelta, protoBytes,
		protoField(1, protoBytes, "po"))))
	body.Write(agentServerFrame(protoField(agentUpdateTextDelta, protoBytes,
		protoField(1, protoBytes, "ng"))))
	body.Write(agentServerFrame(protoField(agentUpdateTurnEnded, protoBytes, []byte{})))

	response, err := cursorAgentToOpenAI(body, "composer-2.5-fast")
	if err != nil {
		t.Fatalf("collect failed: %v", err)
	}
	if got := response.Choices[0].Message.Content; got != "pong" {
		t.Errorf("content = %q, want pong", got)
	}
	if got := response.Choices[0].FinishReason; got != "stop" {
		t.Errorf("finish_reason = %q, want stop", got)
	}
}

func TestCursorAgentRequestBodyRejectsAnEmptyPrompt(t *testing.T) {
	_, _, _, err := cursorAgentRequestBody(translator.OpenAIRequest{
		Messages: []translator.OpenAIMessage{{Role: "user", Content: "   "}},
	}, "composer-2.5-fast", nil, nil)
	if err == nil {
		t.Fatal("an empty prompt must be rejected rather than sent upstream")
	}
}

func TestCursorAgentConversationIDIsStablePerPrompt(t *testing.T) {
	first := cursorAgentConversationID("same prompt")
	if second := cursorAgentConversationID("same prompt"); first != second {
		t.Errorf("conversation id is not stable: %s != %s", first, second)
	}
	if other := cursorAgentConversationID("different"); other == first {
		t.Error("different prompts must not share a conversation id")
	}
}

func TestCursorRequestReportsWhatItActuallySends(t *testing.T) {
	// The cost of a request is the prompt that goes upstream. With a conversation
	// replayed that is the delta, so reporting the client's whole transcript would hide
	// the saving the cache exists to produce.
	history := make([]translator.OpenAIMessage, 0, 20)
	for index := 0; index < 20; index++ {
		history = append(history, translator.OpenAIMessage{
			Role: "user", Content: strings.Repeat("a long earlier turn about the codebase. ", 40)})
	}
	full := translator.OpenAIRequest{Model: "composer-2.5-fast", Messages: history}

	_, _, wholeConversation, err := cursorAgentRequestBody(full, "composer-2.5-fast", nil, nil)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if wholeConversation <= 0 {
		t.Fatal("a fresh conversation must report the prompt it sends")
	}

	pending := []translator.OpenAIMessage{{Role: "user", Content: "one short follow-up"}}
	_, _, delta, err := cursorAgentRequestBody(full, "composer-2.5-fast",
		&cursorConversation{id: "c", state: []byte{1}}, pending)
	if err != nil {
		t.Fatalf("encode delta: %v", err)
	}
	if delta <= 0 {
		t.Fatal("a continued conversation must still report what it sends")
	}
	if delta >= wholeConversation/2 {
		t.Errorf("continued request reported %d tokens against %d for the whole conversation; "+
			"the replayed history is being counted", delta, wholeConversation)
	}
}

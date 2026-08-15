package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
)

// cursorACPStream converts an ACP agent turn into the OpenAI stream chunk format
// the gateway already relays. It spawns one `agent acp` process, opens a session,
// sends the prompt, and forwards agent_message_chunk notifications as text deltas
// and tool_call notifications as tool_calls deltas — the same shapes
// cursorAgentToChatStream emits for the IDE protocol.
//
// The turn is finished by the session/prompt response (stopReason). Notifications
// arriving after it (heartbeats, usage) are ignored.

// cursorACPStreamReader is the io.ReadCloser the gateway consumes.
type cursorACPStreamReader struct {
	reader *io.PipeReader
	agent  *cursorACPAgent
	mu     sync.Mutex
	once   sync.Once
}

// cursorACPAutoApprove replies to every session/request_permission with
// allow-once, so the agent's tool loop does not block waiting for a human.
// LiteRouter is a proxy, not an interactive terminal: the tools the CLI agent
// runs (shell, file edits) are approved one at a time without asking the user.
// A future setting can switch this to reject-once for read-only proxies.
func cursorACPAutoApprove() func(method string, params json.RawMessage, reply func(any, error)) {
	return func(method string, params json.RawMessage, reply func(any, error)) {
		if method != "session/request_permission" {
			return
		}
		reply(map[string]any{
			"outcome": map[string]any{"outcome": "selected", "optionId": "allow-once"},
		}, nil)
	}
}

// runCursorACPTurn spawns an ACP agent, creates a session in cwd, prompts it, and
// returns a reader that yields OpenAI stream chunks until the turn ends.
func runCursorACPTurn(ctx context.Context, cwd, prompt, fallbackModel string) (io.ReadCloser, error) {
	reader, writer := io.Pipe()
	stream := &cursorACPStreamReader{reader: reader}

	// The ACP agent is spawned per turn. A long-lived process would survive many
	// turns and keep its conversation state, but the IDE protocol's cache does the
	// same job; a fresh process per turn is simpler and isolates failures.
	agent, err := newCursorACPAgent(cwd, func(method string, params json.RawMessage) {
		if method != "session/update" {
			return
		}
		// The wire shape nests the update under params.update:
		// {"sessionId":"…","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"…"}}}
		var envelope struct {
			Update *struct {
				SessionUpdate string `json:"sessionUpdate"`
				// Content is a single content block object, not an array: measured live.
				Content *struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
				ToolCall *struct {
					ToolCallID string `json:"toolCallId"`
					// Name is not on the wire; kind ("execute", "read", …) names the tool
					// family, title describes the action, rawInput carries the arguments.
					Kind     string          `json:"kind"`
					Title    string          `json:"title"`
					Status   string          `json:"status"`
					RawInput json.RawMessage `json:"rawInput"`
					Input    json.RawMessage `json:"input"`
				} `json:"toolCall"`
			} `json:"update"`
		}
		if err := json.Unmarshal(params, &envelope); err != nil {
			return
		}
		if envelope.Update == nil {
			return
		}
		update := envelope.Update
		switch update.SessionUpdate {
		case "agent_message_chunk":
			if update.Content != nil && update.Content.Type == "text" && update.Content.Text != "" {
				_ = emitACPChunk(writer, fallbackModel, map[string]any{"content": update.Content.Text})
			}
		case "agent_thought_chunk":
			// Thinking deltas are not forwarded as text (the model must not re-read its
			// own deliberation as an answer), but they can be surfaced as reasoning.
			if update.Content != nil && update.Content.Text != "" {
				_ = emitACPChunk(writer, fallbackModel, map[string]any{"reasoning_content": update.Content.Text})
			}
		case "tool_call":
			if update.ToolCall != nil {
				call := update.ToolCall
				name := call.Kind
				if name == "" {
					name = "cursor_tool"
				}
				args := string(call.RawInput)
				if args == "" {
					args = string(call.Input)
				}
				_ = emitACPChunk(writer, fallbackModel, map[string]any{
					"tool_calls": []any{map[string]any{
						"index": 0, "id": call.ToolCallID, "type": "function",
						"function": map[string]string{"name": name, "arguments": args},
					}},
				})
			}
		}
	}, cursorACPAutoApprove())
	if err != nil {
		_ = reader.CloseWithError(err)
		return nil, err
	}
	stream.agent = agent

	go func() {
		defer agent.close()
		defer writer.Close()
		if err := stream.runTurn(ctx, writer, prompt, fallbackModel); err != nil {
			_ = writer.CloseWithError(err)
		}
	}()

	return reader, nil
}

func (s *cursorACPStreamReader) runTurn(ctx context.Context, writer *io.PipeWriter, prompt, fallbackModel string) error {
	client := &cursorACPClient{agent: s.agent}
	if err := client.initialize(ctx); err != nil {
		return err
	}
	if err := client.authenticate(ctx); err != nil {
		return err
	}
	cwd := "."
	sessionID, err := client.newSession(ctx, cwd)
	if err != nil {
		return err
	}
	if _, err := client.prompt(ctx, sessionID, prompt); err != nil {
		return err
	}
	// The prompt returned; the turn is done.
	finish := "stop"
	terminal := map[string]any{
		"id": "cursor-response", "object": "chat.completion.chunk", "model": fallbackModel,
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}},
	}
	encoded, err := json.Marshal(terminal)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(writer, "data: %s\n\ndata: [DONE]\n\n", encoded)
	return err
}

func emitACPChunk(writer *io.PipeWriter, fallbackModel string, delta map[string]any) error {
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

func (s *cursorACPStreamReader) Read(p []byte) (int, error) {
	return s.reader.Read(p)
}

func (s *cursorACPStreamReader) Close() error {
	var err error
	s.once.Do(func() {
		err = s.reader.Close()
		if s.agent != nil {
			s.agent.close()
		}
	})
	return err
}

var _ = slog.Debug
var _ = errors.Is

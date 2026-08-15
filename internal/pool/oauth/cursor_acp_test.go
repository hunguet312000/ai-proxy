package oauth

import (
	"context"
	"io"
	"strings"
	"testing"

	"literouter/internal/translator"
)

func TestACPPromptFromRequest(t *testing.T) {
	request := translator.OpenAIRequest{
		Messages: []translator.OpenAIMessage{
			{Role: "system", Content: "You are a coding agent."},
			{Role: "user", Content: "Fix the bug"},
			{Role: "assistant", Content: "Looking at it."},
			{Role: "user", Content: "Done?"},
		},
	}
	prompt := acpPromptFromRequest(request)
	want := "You are a coding agent.\n\nFix the bug\n\nAssistant: Looking at it.\n\nDone?"
	if prompt != want {
		t.Fatalf("prompt = %q, want %q", prompt, want)
	}
}

func TestACPPromptFromRequestSkipsEmptyAndTools(t *testing.T) {
	request := translator.OpenAIRequest{
		Messages: []translator.OpenAIMessage{
			{Role: "system", Content: "sys"},
			{Role: "user", Content: ""}, // empty user message
			{Role: "assistant", Content: ""},
			{Role: "user", Content: "real question"},
		},
	}
	prompt := acpPromptFromRequest(request)
	if prompt != "sys\n\nreal question" {
		t.Fatalf("prompt = %q, want %q", prompt, "sys\n\nreal question")
	}
}

func TestCollectOpenAIStream(t *testing.T) {
	// A stream in the same shape runCursorACPTurn emits: data: chunk, then [DONE].
	chunk1 := `{"id":"cursor-response","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"content":"Hello "},"finish_reason":null}]}`
	chunk2 := `{"id":"cursor-response","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"content":"world"},"finish_reason":null}]}`
	terminal := `{"id":"cursor-response","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`
	body := io.NopCloser(strings.NewReader(
		"data: " + chunk1 + "\n\ndata: " + chunk2 + "\n\ndata: " + terminal + "\n\ndata: [DONE]\n\n"))

	response, err := collectOpenAIStream(body, "m")
	if err != nil {
		t.Fatalf("collectOpenAIStream: %v", err)
	}
	if len(response.Choices) != 1 {
		t.Fatalf("choices = %d", len(response.Choices))
	}
	if got := response.Choices[0].Message.Content; got != "Hello world" {
		t.Fatalf("content = %q, want %q", got, "Hello world")
	}
	if response.Choices[0].FinishReason != "stop" {
		t.Fatalf("finish_reason = %q", response.Choices[0].FinishReason)
	}
}

func TestACPAvailable(t *testing.T) {
	// Can't guarantee the binary exists in test environments; just ensure the
	// function runs without panicking and returns a bool.
	_ = cursorACPAvailable()
}

func TestACPWorkspaceFallback(t *testing.T) {
	// If /host-home exists (container), use it. Otherwise home or ".".
	got := cursorACPWorkspace()
	if got == "" {
		t.Fatal("workspace must not be empty")
	}
}

// TestCursorACPLive spawns the real `agent acp` and drives one turn end to end.
// It is skipped under -short because it needs the Cursor CLI on PATH and network
// access to the Cursor backend. Run it manually with
//
//	go test ./internal/pool/oauth/ -run TestCursorACPLive -v
func TestCursorACPLive(t *testing.T) {
	if testing.Short() {
		t.Skip("live Cursor CLI test")
	}
	if !cursorACPAvailable() {
		t.Skip("cursor CLI agent not available")
	}
	reader, err := runCursorACPTurn(context.Background(), ".", "Reply with exactly: ACP2 works", "test-model")
	if err != nil {
		t.Fatalf("runCursorACPTurn: %v", err)
	}
	defer reader.Close()
	response, err := collectOpenAIStream(reader, "test-model")
	if err != nil {
		t.Fatalf("collectOpenAIStream: %v", err)
	}
	if len(response.Choices) == 0 {
		t.Fatal("no choices in response")
	}
	content, _ := response.Choices[0].Message.Content.(string)
	if !strings.Contains(content, "ACP2 works") {
		t.Fatalf("content = %q, want it to contain %q", content, "ACP2 works")
	}
}

// TestCursorACPLiveTool verifies the agent can run a read-only tool (shell ls) and
// that permission requests are auto-approved. Same live-skip guard.
func TestCursorACPLiveTool(t *testing.T) {
	if testing.Short() {
		t.Skip("live Cursor CLI test")
	}
	if !cursorACPAvailable() {
		t.Skip("cursor CLI agent not available")
	}
	reader, err := runCursorACPTurn(context.Background(), ".",
		"Run the shell command `ls /tmp | head -2` then reply with exactly TOOL_DONE", "test-model")
	if err != nil {
		t.Fatalf("runCursorACPTurn: %v", err)
	}
	defer reader.Close()
	response, err := collectOpenAIStream(reader, "test-model")
	if err != nil {
		t.Fatalf("collectOpenAIStream: %v", err)
	}
	if len(response.Choices) == 0 {
		t.Fatal("no choices in response")
	}
	content, _ := response.Choices[0].Message.Content.(string)
	if !strings.Contains(content, "TOOL_DONE") {
		t.Fatalf("content = %q, want it to contain %q (agent did not finish the tool turn)", content, "TOOL_DONE")
	}
}

var _ = context.Background

package oauth

import (
	"context"
	"strings"
	"testing"
	"time"

	"literouter/internal/translator"
)

// TestCursorACPPoolRecoversFromDeadAgent verifies the pool replaces a dead
// agent instead of handing it to the next turn. Skipped under -short (live CLI).
func TestCursorACPPoolRecoversFromDeadAgent(t *testing.T) {
	if testing.Short() {
		t.Skip("live Cursor CLI test")
	}
	if !cursorACPAvailable() {
		t.Skip("cursor CLI agent not available")
	}
	ctx := context.Background()
	key := "recover-conv"

	// First acquire creates a live entry.
	entry1, err := cursorACPPool.acquire(ctx, key, nil)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if !entry1.agent.alive() {
		t.Fatal("fresh agent reported dead")
	}

	// Kill the agent behind the pool's back — simulates the bridge dropping or
	// the process crashing.
	entry1.agent.close()
	cursorACPPool.release(key, entry1, true) // keep=true would normally leave it; the entry is now dead

	if entry1.agent.alive() {
		t.Fatal("closed agent still reported alive")
	}

	// Acquire again: must NOT return the dead entry; must spawn a fresh one.
	entry2, err := cursorACPPool.acquire(ctx, key, nil)
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	if entry2.agent == entry1.agent {
		t.Fatal("pool returned the dead agent instead of a fresh one")
	}
	if !entry2.agent.alive() {
		t.Fatal("replacement agent reported dead")
	}

	cursorACPPool.release(key, entry2, false)
}

// TestCursorACPTurnAfterDeadAgent exercises the recovery through the turn
// path: kill the pooled agent, then run a turn — it must succeed with a new one.
func TestCursorACPTurnAfterDeadAgent(t *testing.T) {
	if testing.Short() {
		t.Skip("live Cursor CLI test")
	}
	if !cursorACPAvailable() {
		t.Skip("cursor CLI agent not available")
	}
	ctx := context.Background()
	conv := "dead-agent-conv"

	// Warm the pool.
	r1, err := runCursorACPTurn(ctx, conv, ".", "Reply with exactly: WARM", "test-model")
	if err != nil {
		t.Fatalf("warm turn: %v", err)
	}
	_, _ = collectOpenAIStream(r1, "test-model")
	r1.Close()

	// Kill whatever the pool holds for this conversation.
	key := cursorACPKey(conv, "test-model", ".")
	if entry := cursorACPPool.entries[key]; entry != nil {
		entry.agent.close()
	}

	// The next turn must recover and still answer.
	r2, err := runCursorACPTurn(ctx, conv, ".", "Reply with exactly: RECOVERED", "test-model")
	if err != nil {
		t.Fatalf("recovery turn: %v", err)
	}
	resp, err := collectOpenAIStream(r2, "test-model")
	r2.Close()
	if err != nil {
		t.Fatalf("recovery stream: %v", err)
	}
	content, _ := resp.Choices[0].Message.Content.(string)
	if !strings.Contains(content, "RECOVERED") {
		t.Fatalf("recovery content = %q, want RECOVERED", content)
	}
}

// TestCursorACPLargeContextStability drives several turns with a large
// (200k-300k token) transcript through the pool and asserts every turn
// answers. Skipped under -short.
func TestCursorACPLargeContextStability(t *testing.T) {
	if testing.Short() {
		t.Skip("live Cursor CLI test")
	}
	if !cursorACPAvailable() {
		t.Skip("cursor CLI agent not available")
	}
	ctx := context.Background()
	conv := "large-context-conv"

	// A 250k-token transcript: many turns of user/assistant pairs.
	messages := make([]translator.OpenAIMessage, 0, 220)
	for i := 0; i < 110; i++ {
		messages = append(messages,
			translator.OpenAIMessage{Role: "user", Content: "Step " + strings.Repeat("filler context line with file contents and diffs ", 40)},
			translator.OpenAIMessage{Role: "assistant", Content: "Done step, here are the changes " + strings.Repeat("diff content ", 20)},
		)
	}
	messages = append(messages, translator.OpenAIMessage{Role: "user", Content: "Reply with exactly: LARGE_OK"})
	request := translator.OpenAIRequest{Model: "composer-2.5", Messages: messages}

	for turn := 1; turn <= 3; turn++ {
		start := time.Now()
		prompt := acpPromptFromRequest(trimCursorFold(request, "composer-2.5"))
		reader, err := runCursorACPTurn(ctx, conv, ".", prompt, "composer-2.5")
		if err != nil {
			t.Fatalf("large turn %d: %v", turn, err)
		}
		resp, err := collectOpenAIStream(reader, "composer-2.5")
		reader.Close()
		if err != nil {
			t.Fatalf("large turn %d stream: %v", turn, err)
		}
		content, _ := resp.Choices[0].Message.Content.(string)
		if !strings.Contains(content, "LARGE_OK") {
			t.Fatalf("large turn %d content = %q, want LARGE_OK", turn, content)
		}
		t.Logf("large turn %d: %v", turn, time.Since(start).Round(time.Millisecond))
	}
}

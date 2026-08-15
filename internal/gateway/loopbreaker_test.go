package gateway

import (
	"strings"
	"testing"

	"literouter/internal/translator"
)

func toolResult(text string) translator.OpenAIMessage {
	return translator.OpenAIMessage{Role: "tool", Content: text}
}

func TestDetectEditLoopTriggersOnRepeatedFailures(t *testing.T) {
	messages := []translator.OpenAIMessage{
		toolResult("read file ok"),
		toolResult(`Edit failed: String to replace not found in file.`),
		toolResult(`Edit failed: String to replace not found in file.`),
	}
	if !detectEditLoop(messages) {
		t.Fatal("two repeated edit failures within window must trip the loop")
	}
}

func TestDetectEditLoopIgnoresSingleFailure(t *testing.T) {
	messages := []translator.OpenAIMessage{
		toolResult("read ok"),
		toolResult("Edit failed: text not found"),
		toolResult("all good now"),
	}
	if detectEditLoop(messages) {
		t.Fatal("a single failure must not trip the loop")
	}
}

func TestDetectEditLoopIgnoresNormalEdits(t *testing.T) {
	messages := []translator.OpenAIMessage{
		toolResult("applied the edit, 2 lines changed"),
		toolResult("read file contents: hello world"),
		toolResult("edit successful"),
	}
	if detectEditLoop(messages) {
		t.Fatal("successful edits must not trip the loop")
	}
}

func TestInjectEditLoopReminderAppendsOnlyWhenStuck(t *testing.T) {
	request := translator.OpenAIRequest{Messages: []translator.OpenAIMessage{
		toolResult("Edit failed: old_string not found"),
		toolResult("Edit failed: old_string not found"),
	}}
	got := injectEditLoopReminder(request)
	if len(got.Messages) != len(request.Messages)+1 {
		t.Fatalf("messages = %d, want %d", len(got.Messages), len(request.Messages)+1)
	}
	last := got.Messages[len(got.Messages)-1]
	// The reminder is a user message: it lands at the end of the transcript, and
	// Qwen3's template rejects a system message anywhere but the beginning.
	if last.Role != "user" || !strings.Contains(last.Content.(string), "STOP retrying") {
		t.Fatalf("last message = %#v, want the loop reminder as user", last)
	}

	// Normal traffic: unchanged.
	ok := translator.OpenAIRequest{Messages: []translator.OpenAIMessage{toolResult("all fine")}}
	if changed := injectEditLoopReminder(ok); len(changed.Messages) != 1 {
		t.Fatal("non-loop traffic must not gain a message")
	}
}

func TestEditLoopErrorMatchesCommonWording(t *testing.T) {
	cases := []string{
		"String to replace not found in file.",
		"Edit failed: old_string does not exist",
		"failed to find the target text",
		"no match found for the replacement",
		"the specified text is not present in the file",
	}
	for _, caseText := range cases {
		if !editLoopError(caseText) {
			t.Errorf("editLoopError(%q) = false, want true", caseText)
		}
	}
	notError := []string{"edit applied successfully", "replaced 2 occurrences", "file read: hello"}
	for _, caseText := range notError {
		if editLoopError(caseText) {
			t.Errorf("editLoopError(%q) = true, want false", caseText)
		}
	}
}

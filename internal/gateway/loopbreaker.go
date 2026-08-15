package gateway

import (
	"strings"

	"literouter/internal/translator"
)

// Loop-breaker: when a coding agent (Claude Code, Codex, opencode) gets stuck
// retrying the same failed edit, it burns turn after turn on an old_string that
// no longer matches the file, and a proxy that relays it faithfully keeps the
// loop alive. The proxy can see the pattern in the transcript — the repeated
// tool output carrying the same "not found" error — before the model does, so
// it injects one corrective instruction into the next request.
//
// The instruction is a message of its own, not an append to the last user turn,
// so it travels to the model without mutating the user's words. It is placed
// AFTER the transcript, never hoisted into instructions: hoisting would move
// the cacheable system prefix (see openAIToCodexRequest).

// editLoopErrors are the tool-result error signatures that indicate a file edit
// failed because the target text was not found. They are deliberately broad —
// the exact wording differs across tools and clients.
var editLoopErrors = []string{
	"string to replace not found",
	"not found in file",
	"old_string",
	"cannot find",
	"failed to find",
	"no match found",
	"does not exist in the file",
	"text not found",
	"is not present",
	// Claude Code's own failure text for a rejected/conflicting edit.
	"error editing file",
	"error applying edit",
	"edit failed",
	"failed to edit",
	"concurrent modification",
	"conflicting change",
	"file was modified",
	"target file changed",
	"out of date",
	"stale",
}

// editLoopError reports whether a tool output looks like a failed text replacement.
func editLoopError(output string) bool {
	lower := strings.ToLower(output)
	for _, signature := range editLoopErrors {
		if strings.Contains(lower, signature) {
			return true
		}
	}
	return false
}

// editLoopWindow is how many recent tool results are scanned. A loop is many
// turns of the same failure; a single transient failure must not trip it.
const editLoopWindow = 8

// editLoopThreshold is how many edit-failure tool results within the window
// count as a stuck loop.
const editLoopThreshold = 2

// messageText flattens a message's content to plain text. Content is either a
// plain string or a list of content parts; both occur across providers.
func messageText(content any) string {
	switch value := content.(type) {
	case string:
		return value
	case []translator.OpenAIContentPart:
		var text strings.Builder
		for _, part := range value {
			text.WriteString(part.Text)
		}
		return text.String()
	case []any:
		var text strings.Builder
		for _, item := range value {
			if part, ok := item.(translator.OpenAIContentPart); ok {
				text.WriteString(part.Text)
			}
		}
		return text.String()
	}
	return ""
}

// detectEditLoop scans the recent tool results for repeated edit failures.
// It returns true once the transcript shows the model is stuck.
func detectEditLoop(messages []translator.OpenAIMessage) bool {
	failures := 0
	scanned := 0
	for index := len(messages) - 1; index >= 0 && scanned < editLoopWindow; index-- {
		message := messages[index]
		if message.Role != "tool" {
			continue
		}
		scanned++
		if editLoopError(messageText(message.Content)) {
			failures++
			if failures >= editLoopThreshold {
				return true
			}
		}
	}
	return false
}

// editLoopReminder is the corrective instruction injected when a loop is
// detected. It is concrete and actionable, and names the failure mode.
const editLoopReminder = `You appear to be stuck retrying the same failed file edit — the last few tool results show "text to replace not found" errors. STOP retrying the same old_string. Instead:
1. Read the file again with a read tool to get its CURRENT content.
2. Make old_string match EXACTLY what the file contains now (including indentation and whitespace).
3. If you meant to ADD new content, anchor on a line that already exists in the file, and put the new text next to it — do not target text that is not there.
4. If the text really does not exist, do not invent it. Report that the target is absent and ask what to do next.
Then proceed with one attempt only; do not repeat the same edit.`

// injectEditLoopReminder appends the corrective message to the request when a
// stuck edit loop is detected. It returns the request unchanged when there is
// no loop, so normal traffic pays nothing.
//
// The reminder is a user message, not a system message: it arrives at the end
// of the transcript, and templates like Qwen3's reject a system message
// anywhere but the very beginning ("System message must be at the beginning").
// As user text it reads naturally as the operator's own instruction to stop
// retrying the same edit.
func injectEditLoopReminder(request translator.OpenAIRequest) translator.OpenAIRequest {
	if !detectEditLoop(request.Messages) {
		return request
	}
	request.Messages = append(request.Messages, translator.OpenAIMessage{
		Role:    "user",
		Content: editLoopReminder,
	})
	return request
}

package contextguard

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"literouter/internal/provider"
)

const compactMarker = "[literouter:compact-v1"

func compact(request *provider.Request, policy Policy, available int) {
	seen := make(map[[32]byte]string)
	oldBoundary := max(0, len(request.Messages)-policy.KeepRecentTurns)
	for messageIndex := range request.Messages {
		message := &request.Messages[messageIndex]
		for contentIndex := range message.Content {
			block := &message.Content[contentIndex]
			if strings.HasPrefix(block.Text, compactMarker) {
				continue
			}
			// Reasoning is not part of the user's durable intent. Omit only older
			// internal thinking, never user/assistant text or tool calls.
			if block.Type == "thinking" && messageIndex < oldBoundary {
				block.Thinking = "[older reasoning omitted; conclusions retained in assistant message]"
				continue
			}
			if block.Type != "tool_result" || block.Text == "" {
				continue
			}
			hash := sha256.Sum256([]byte(block.Text))
			if reference, ok := seen[hash]; ok && messageIndex < oldBoundary {
				// Exact duplicates carry no new information; keep a stable reference.
				block.Text = fmt.Sprintf("%s duplicate=%s hash=%s]", compactMarker, reference, shortHash(hash))
				continue
			}
			seen[hash] = block.ToolUseID
			// Keep every unique tool result byte-for-byte. Generic head/tail or log
			// compression can remove the one line needed by the next model response.
			// If duplicates/reasoning removal is insufficient, summarize complete
			// older turns instead of destructively truncating tool evidence.
		}
	}
	// Deliberately do not drop unique older tool results here. If safe compaction
	// is insufficient, gateway.prepareContext summarizes older complete turns.
	_ = available
}

func shortHash(hash [32]byte) string {
	return hex.EncodeToString(hash[:6])
}

package contextguard

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"literouter/internal/cache"
	"literouter/internal/provider"
)

const (
	compactMarker   = "[literouter:compact-v1"
	supersedeMarker = "[literouter:supersede-v1"
	truncateMarker  = "[literouter:truncate-v1"
	// proxyMarkerPrefix is shared by every rewrite this package makes. A block that
	// already starts with it is never reprocessed, which keeps every stage
	// idempotent and keeps a summary/trim note out of the dedup hash set.
	proxyMarkerPrefix = "[literouter:"

	defaultTruncateThreshold = 4096
	defaultTruncateHead      = 1536
	defaultTruncateTail      = 640
	defaultBoundaryQuantum   = 8
	// intactErrorBytes is how large an old error result may be and still survive
	// untouched: error text is the highest-value old content because it explains
	// what was fixed.
	intactErrorBytes = 16 << 10
)

// compact runs the mechanical stages on everything older than the sticky
// boundary. Stages 1–2 (thinking elision, exact dedup) are lossless and always
// on; stages 3–4 (superseded collapse, head/tail truncation) are lossy and only
// run in aggressive mode. All gates compare the pre-compaction estimate, which
// only grows as the client appends history, so a stage that has activated never
// deactivates — no flapping, no prefix churn from stage oscillation.
func compact(request *provider.Request, policy Policy, available, beforeTokens int) {
	boundary := stickyOldBoundary(len(request.Messages), policy)
	elideAndDedup(request, boundary)
	if !policy.Aggressive {
		return
	}
	collapseSuperseded(request, boundary)
	truncateRatio := policy.TruncateRatio
	if truncateRatio <= 0 {
		truncateRatio = 0.82
	}
	if float64(beforeTokens) >= float64(available)*truncateRatio {
		truncateOldToolResults(request, boundary, policy)
	}
}

// AggressiveCompact applies every mechanical stage with the maximal boundary and
// unconditional truncation. It exists for the post-rejection retry path: the
// upstream has already refused the request, so prefix stability is lost either
// way, and truncating old tool output is strictly better than dropping whole
// turns.
func AggressiveCompact(request provider.Request, policy Policy) provider.Request {
	result := cloneRequest(request)
	if policy.KeepRecentTurns < 1 {
		policy.KeepRecentTurns = 1
	}
	boundary := max(0, len(result.Messages)-policy.KeepRecentTurns)
	elideAndDedup(&result, boundary)
	collapseSuperseded(&result, boundary)
	truncateOldToolResults(&result, boundary, policy)
	return result
}

// stickyOldBoundary quantizes the old/recent boundary to a K-message grid. The
// raw boundary slides by one on every appended message, which rewrites a prefix
// the upstream had cached on every single turn; snapping to the grid keeps the
// compacted prefix byte-identical for K consecutive turns and batches the
// reprice into one turn per quantum. Between advances the verbatim-recent window
// is strictly larger than KeepRecentTurns — more recency, never less.
func stickyOldBoundary(messageCount int, policy Policy) int {
	quantum := policy.BoundaryQuantum
	if quantum < 1 {
		quantum = 1
	}
	raw := max(0, messageCount-policy.KeepRecentTurns)
	return (raw / quantum) * quantum
}

// elideAndDedup is the always-on lossless pass: old thinking blocks become a
// stub, and an old tool result byte-identical to an earlier one becomes a stable
// reference to it. First occurrence stays full, which is the prefix-friendly
// direction.
func elideAndDedup(request *provider.Request, boundary int) {
	seen := make(map[[32]byte]string)
	for messageIndex := range request.Messages {
		message := &request.Messages[messageIndex]
		for contentIndex := range message.Content {
			block := &message.Content[contentIndex]
			if strings.HasPrefix(block.Text, proxyMarkerPrefix) {
				continue
			}
			// Reasoning is not part of the user's durable intent. Omit only older
			// internal thinking, never user/assistant text or tool calls.
			if block.Type == "thinking" && messageIndex < boundary {
				block.Thinking = "[older reasoning omitted; conclusions retained in assistant message]"
				continue
			}
			if block.Type != "tool_result" || block.Text == "" {
				continue
			}
			hash := sha256.Sum256([]byte(block.Text))
			if reference, ok := seen[hash]; ok && messageIndex < boundary {
				// Exact duplicates carry no new information; keep a stable reference.
				block.Text = fmt.Sprintf("%s duplicate=%s hash=%s]", compactMarker, reference, shortHash(hash))
				continue
			}
			seen[hash] = block.ToolUseID
		}
	}
}

// toolCall identifies one historical tool invocation for the supersede pass.
type toolCall struct {
	name string
	key  string
}

// collapseSuperseded replaces an old tool result whose exact call — same tool,
// byte-identical canonical arguments — was repeated later in the conversation.
// Coding transcripts are dominated by re-reads of the same file or re-runs of
// the same command, and only the newest copy reflects current state; the older
// copies are stale and become a short note.
//
// The note deliberately does not name the newer tool_use id: naming it would
// rewrite every older marker each time the call is repeated again, repricing the
// upstream cache for nothing. The note text is a pure function of the replaced
// block, written once and stable forever.
func collapseSuperseded(request *provider.Request, boundary int) {
	if boundary == 0 {
		return
	}
	calls := make(map[string]toolCall)
	for _, message := range request.Messages {
		for _, block := range message.Content {
			if block.Type != "tool_use" || block.ToolUseID == "" {
				continue
			}
			if key, ok := canonicalToolKey(block.Name, block.Input); ok {
				calls[block.ToolUseID] = toolCall{name: block.Name, key: key}
			}
		}
	}
	type position struct{ message, content int }
	newest := make(map[string]position)
	skip := func(block provider.Content) bool {
		return block.Type != "tool_result" || block.Text == "" || block.IsError ||
			strings.HasPrefix(block.Text, proxyMarkerPrefix)
	}
	for messageIndex, message := range request.Messages {
		for contentIndex, block := range message.Content {
			if skip(block) {
				continue
			}
			if call, ok := calls[block.ToolUseID]; ok {
				newest[call.key] = position{messageIndex, contentIndex}
			}
		}
	}
	for messageIndex := range request.Messages[:boundary] {
		message := &request.Messages[messageIndex]
		for contentIndex := range message.Content {
			block := &message.Content[contentIndex]
			if skip(*block) {
				continue
			}
			call, ok := calls[block.ToolUseID]
			if !ok {
				continue
			}
			if latest := newest[call.key]; latest.message == messageIndex && latest.content == contentIndex {
				continue
			}
			hash := sha256.Sum256([]byte(block.Text))
			block.Text = fmt.Sprintf(
				"%s tool=%s hash=%s] Superseded by a newer call with identical arguments later in this conversation; that newer tool_result is authoritative.",
				supersedeMarker, call.name, shortHash(hash))
		}
	}
}

// canonicalToolKey identifies a tool call by name plus compacted argument JSON.
// Byte-identical arguments after compaction is the safe general rule: a Read of
// the same file with a different offset is correctly a different call.
func canonicalToolKey(name string, input json.RawMessage) (string, bool) {
	if name == "" {
		return "", false
	}
	if len(input) == 0 {
		return name + "\x00", true
	}
	var compacted bytes.Buffer
	if err := json.Compact(&compacted, input); err != nil {
		return "", false
	}
	return name + "\x00" + compacted.String(), true
}

// truncateOldToolResults applies the head/tail stage uniformly to every old
// unique tool result over the threshold. Uniform application — rather than
// picking the largest victims until under budget — is what keeps the output a
// pure function of the message list, so successive requests produce the same
// bytes.
func truncateOldToolResults(request *provider.Request, boundary int, policy Policy) {
	if boundary == 0 {
		return
	}
	names := make(map[string]string)
	for _, message := range request.Messages {
		for _, block := range message.Content {
			if block.Type == "tool_use" && block.ToolUseID != "" {
				names[block.ToolUseID] = block.Name
			}
		}
	}
	for messageIndex := range request.Messages[:boundary] {
		message := &request.Messages[messageIndex]
		for contentIndex := range message.Content {
			block := &message.Content[contentIndex]
			if block.Type != "tool_result" || block.Text == "" || strings.HasPrefix(block.Text, proxyMarkerPrefix) {
				continue
			}
			if replacement, ok := truncateToolResult(names[block.ToolUseID], block.ToolUseID, block.Text, block.IsError, policy); ok {
				block.Text = replacement
			}
		}
	}
}

// truncateToolResult shrinks one old bulky tool result: structure-aware
// compression first (diff hunks, grep matches, log highlights survive), then a
// line-aligned head/tail cap. The head keeps the file paths and line numbers
// that lead tool output; the tail keeps exit status and final errors. The marker
// tells the model to re-run the tool rather than guess at elided content.
func truncateToolResult(name, toolUseID, text string, isError bool, policy Policy) (string, bool) {
	head, tail, threshold := policy.truncateSizes()
	if isError {
		if len(text) <= intactErrorBytes {
			return "", false
		}
		head, tail = head*2, tail*2
	}
	if len(text) <= threshold {
		return "", false
	}
	body := text
	if !isError {
		if result := cache.CompressForHistory(name, text); result.Method != "none" {
			body = result.Compressed
		}
	}
	if len(body) > head+tail {
		body = headTailByBytes(body, head, tail)
	}
	replacement := fmt.Sprintf("%s tool_use_id=%s original_bytes=%d hash=%s]\n%s",
		truncateMarker, toolUseID, len(text), shortHash(sha256.Sum256([]byte(text))), body)
	if len(replacement) >= len(text) {
		return "", false
	}
	return replacement, true
}

// truncateSizes resolves the head/tail/threshold knobs, falling back to package
// defaults so a zero-valued Policy still truncates sanely.
func (policy Policy) truncateSizes() (head, tail, threshold int) {
	head, tail, threshold = policy.TruncateHeadBytes, policy.TruncateTailBytes, policy.TruncateThresholdBytes
	if head <= 0 {
		head = defaultTruncateHead
	}
	if tail <= 0 {
		tail = defaultTruncateTail
	}
	if threshold <= 0 {
		threshold = defaultTruncateThreshold
	}
	return head, tail, threshold
}

// headTailByBytes keeps the first head and last tail bytes, snapped outward to
// line boundaries, with an elision note in between.
func headTailByBytes(text string, head, tail int) string {
	if head+tail >= len(text) {
		return text
	}
	headEnd := head
	if index := strings.LastIndexByte(text[:head], '\n'); index > 0 {
		headEnd = index
	}
	tailStart := len(text) - tail
	if index := strings.IndexByte(text[tailStart:], '\n'); index >= 0 {
		tailStart += index + 1
	}
	if tailStart <= headEnd {
		return text
	}
	elided := text[headEnd:tailStart]
	return fmt.Sprintf(
		"%s\n[... %d lines (%d bytes) elided by the proxy to fit the context window; re-run the tool if you need the full output ...]\n%s",
		text[:headEnd], strings.Count(elided, "\n"), len(elided), text[tailStart:])
}

func shortHash(hash [32]byte) string {
	return hex.EncodeToString(hash[:6])
}

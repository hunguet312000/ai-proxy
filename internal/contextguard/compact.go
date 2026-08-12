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
//
// Snapping never reports "no old history" when there is some. With the defaults
// (keep 6, quantum 8) the grid rounds every count below 14 messages down to zero,
// which switched off all four stages at once — dedup, supersede and truncation all
// gate on `index < boundary`, and a transcript with one real user turn never opens
// a second summary unit either. A subagent holding six 350 KB tool results inside
// thirteen messages therefore skipped the whole ladder and landed on trim, which
// deletes entire tool chains, where truncation would have kept all six at ~2 KB
// with a recovery hint on each. Quantization is a cache optimization layered over
// the boundary, so it may delay a rewrite but must not cancel one: below the first
// grid line the raw boundary stands. The churn that costs is bounded — Prepare only
// calls compact once a request is over SoftRatio, and AggressiveCompact has always
// used the raw boundary for the same reason.
func stickyOldBoundary(messageCount int, policy Policy) int {
	quantum := policy.BoundaryQuantum
	if quantum < 1 {
		quantum = 1
	}
	raw := max(0, messageCount-policy.KeepRecentTurns)
	if snapped := (raw / quantum) * quantum; snapped > 0 {
		return snapped
	}
	return raw
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
	calls := make(map[string]resultSource)
	for _, message := range request.Messages {
		for _, block := range message.Content {
			if block.Type == "tool_use" && block.ToolUseID != "" {
				calls[block.ToolUseID] = resultSource{name: block.Name, input: block.Input}
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
			call := calls[block.ToolUseID]
			if replacement, ok := truncateToolResult(call, block.ToolUseID, block.Text, block.IsError, policy); ok {
				block.Text = replacement
			}
		}
	}
}

// resultSource is the call that produced a tool result, carried to the truncation note
// so it can say what to re-run. Both fields come from the tool_use block, which
// truncation never touches — the note only makes salient what is already one message up.
// Distinct from toolCall, which the supersede pass keys on a canonicalized form.
type resultSource struct {
	name  string
	input json.RawMessage
}

// maxRecoveryArgumentBytes caps the arguments quoted in a truncation note. A file path
// belongs there; the 40 KB body of a Write call does not — quoting that would grow the
// prefix this stage exists to shrink.
const maxRecoveryArgumentBytes = 200

// recoveryHint is what the note tells the model to do to get the elided bytes back.
//
// Naming the call is the whole point of it: "re-run the tool" leaves the model to work
// the call out from a tool_use_id, and models act on names and arguments, not ids. So
// the note that mattered least — the one read exactly when the model needs the missing
// content — was the one saying least.
//
// It is a pure function of the call being replaced, so the note stays byte-identical as
// the conversation grows. That is the same property collapseSuperseded protects by
// deliberately not naming the newer tool_use id: a note that changes later rewrites the
// prefix and reprices the upstream cache for nothing.
func recoveryHint(call resultSource) string {
	if call.name == "" {
		return "re-run the tool"
	}
	if len(call.input) == 0 {
		return "re-run " + call.name
	}
	var compacted bytes.Buffer
	if err := json.Compact(&compacted, call.input); err != nil || compacted.Len() > maxRecoveryArgumentBytes {
		return "re-run " + call.name
	}
	return "re-run " + call.name + " with " + compacted.String()
}

// fetchHint is the reference-store alternative to re-running the tool. It returns
// empty when no store is wired (re-running stays the only path), so the note's bytes
// are unchanged for every caller that does not use the feature.
func fetchHint(policy Policy, call resultSource, fullID string) string {
	if policy.StoreResult == nil || fullID == "" {
		return ""
	}
	command := call.name
	if command == "" {
		command = "the tool"
	}
	// The URL is a best-effort pointer, not a contract: the store is per-process and
	// evicts under pressure, so the hint always pairs it with the re-run path.
	return " (or fetch the full output: GET /ref/" + fullID + " — the result of " + command + ")"
}

// truncateToolResult shrinks one old bulky tool result: structure-aware
// compression first (diff hunks, grep matches, log highlights survive), then a
// line-aligned head/tail cap. The head keeps the file paths and line numbers
// that lead tool output; the tail keeps exit status and final errors. The marker
// tells the model to re-run the tool rather than guess at elided content, and —
// when the policy carries a result store — records the content-hash under which
// the full body can be fetched back, turning the lossy cut into practically
// lossless retrieval.
func truncateToolResult(call resultSource, toolUseID, text string, isError bool, policy Policy) (string, bool) {
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
	// spec carries what stays true about the tool's own output as the body is rewritten,
	// so the note describes the output rather than whatever this stage left of it.
	spec := elision{originalLines: lineCount(text), contiguous: true}
	body := text
	if !isError {
		// A head_tail compression is this stage's own operation counted in lines, with an
		// elision note of its own. Running it first nested two notes — and, the reason it
		// matters, renumbered the body: the range reported here described the compressed
		// copy while appearing to describe the tool's output. Measured at 600 lines, the
		// note claimed "lines 21-113 of 121" for content whose real gap was 21-592 of 600,
		// which sends the model to re-read what it already has and leaves the rest missing.
		//
		// Structural methods earn their place — they keep the matches, hunks and highlights
		// a positional cut destroys — and after one of those the gap is scattered rather
		// than a span, which is what spec.contiguous records.
		if result := cache.CompressForHistory(call.name, text); result.Method != "none" && result.Method != "head_tail" {
			body, spec.contiguous = result.Compressed, false
		}
	}
	// fullID is computed lazily and only when a store is wired, so callers that do not
	// use the feature pay nothing. The marker always carries the shortened hash; the
	// reference store needs the full one, which is what the fetch hint addresses.
	var fullID string
	if policy.StoreResult != nil {
		fullID = sha256Hex(text)
	}
	switch {
	case len(body) > head+tail:
		body = headTailByBytes(body, head, tail, recoveryHint(call)+fetchHint(policy, call, fullID), spec)
	case len(body) < len(text):
		// Compression alone met the cap, so headTailByBytes never ran — and it was carrying
		// the note. Seen on a real transcript: a 274-line file read collapsed to 143 bytes
		// with nothing left saying content was dropped, let alone what to re-run. The note
		// belongs to the truncation, not to whichever stage happened to perform it.
		//
		// It leads the body rather than trailing it. What survives a structural pass is
		// dotted with the compressor's own small notes — "2 unchanged lines omitted", forty
		// times over — and a single note underneath them reads as one more of the same. Put
		// first, next to the header, it is the frame for everything that follows.
		body = fmt.Sprintf(
			"[... %s elided by the proxy to fit the context window; %s if you need the full output ...]\n%s",
			spec.describeCompressed(body, len(text)), recoveryHint(call)+fetchHint(policy, call, fullID), body)
	}
	replacement := fmt.Sprintf("%s tool_use_id=%s original_bytes=%d hash=%s]\n%s",
		truncateMarker, toolUseID, len(text), shortHash(sha256.Sum256([]byte(text))), body)
	if len(replacement) >= len(text) {
		return "", false
	}
	if policy.StoreResult != nil {
		// Only capture when the cut is real (the shortened form is strictly smaller,
		// which is what the length check above decided). The full hash, not the marker's
		// shortened one, is the key: it is collision-safe and is what a fetch endpoint
		// would address. The store is fire-and-forget — the request path must not stall
		// on a body it is already eliding.
		policy.StoreResult(sha256Hex(text), call.name, toolUseID, text)
	}
	return replacement, true
}

func sha256Hex(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
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

// lineCount counts \n-separated lines, treating a trailing newline as ending the last
// line rather than starting an empty one. Tool output almost always ends with one, and
// counting it would report every result one line longer than it is.
func lineCount(text string) int {
	if text == "" {
		return 0
	}
	count := strings.Count(text, "\n") + 1
	if strings.HasSuffix(text, "\n") {
		count--
	}
	return count
}

// elision is what the note can truthfully say about the lines that went missing.
//
// Two shapes, because two things happened. A body cut from the tool's own output has a
// gap: one span, which a Read can fetch back with offset and limit. A body that was
// structurally compressed first had lines selected out of it — matches kept, context
// dropped — so what is missing is scattered and no span describes it. Saying "lines
// 21-592" there would send the model to fetch the wrong lines, so it gets the count
// instead, which is the part that stayed true.
type elision struct {
	// originalLines is the tool output's own line count, before any compression.
	originalLines int
	// contiguous is whether the surviving text is a single span of that output.
	contiguous bool
}

// describe renders the extent for the note. text is the body being cut, and headEnd and
// tailStart bound the part that will not survive.
func (spec elision) describe(text string, headEnd, tailStart int) string {
	bytes := fmt.Sprintf("%d bytes", tailStart-headEnd)
	if spec.originalLines <= 0 {
		return bytes
	}
	if spec.contiguous {
		headLines, tailLines := lineCount(text[:headEnd]), lineCount(text[tailStart:])
		if spec.originalLines-headLines-tailLines <= 0 {
			return bytes
		}
		return fmt.Sprintf("lines %d-%d of %d (%s)",
			headLines+1, spec.originalLines-tailLines, spec.originalLines, bytes)
	}
	kept := outputLines(text[:headEnd]) + outputLines(text[tailStart:])
	return spec.scattered(spec.originalLines-kept, bytes)
}

// scattered words a non-contiguous loss. It carries the share as well as the count
// because the count alone is read against whatever else is in the block, and a body that
// survived structural compression is full of the compressor's own small numbers — "2
// unchanged lines omitted" — for a large one to be mistaken for.
func (spec elision) scattered(missing int, bytes string) string {
	if missing <= 0 {
		return bytes
	}
	return fmt.Sprintf("%d of %d lines (%d%% of the output), scattered (%s)",
		missing, spec.originalLines, missing*100/spec.originalLines, bytes)
}

// describeCompressed is the extent when compression alone met the cap: there is no
// head/tail gap, so what is missing is whatever the compressor selected away.
func (spec elision) describeCompressed(body string, originalBytes int) string {
	bytes := fmt.Sprintf("%d bytes", originalBytes-len(body))
	if spec.originalLines <= 0 {
		return bytes
	}
	return spec.scattered(spec.originalLines-outputLines(body), bytes)
}

// outputLines counts lines that came from the tool, discounting the notes a compressor
// left behind. Those are not lines of the output, and counting them as kept would
// understate what is missing.
func outputLines(text string) int {
	count := lineCount(text)
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "[... ") {
			count--
		}
	}
	return max(count, 0)
}

// headTailByBytes keeps the first head and last tail bytes, snapped outward to
// line boundaries, with an elision note in between.
func headTailByBytes(text string, head, tail int, recovery string, spec elision) string {
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
	// Derived from the kept parts rather than by counting newlines in the elided slice,
	// which starts and ends mid-boundary and is off by one in both directions.
	return fmt.Sprintf(
		"%s\n[... %s elided by the proxy to fit the context window; %s if you need the full output ...]\n%s",
		text[:headEnd], spec.describe(text, headEnd, tailStart), recovery, text[tailStart:])
}

func shortHash(hash [32]byte) string {
	return hex.EncodeToString(hash[:6])
}

package contextguard

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"literouter/internal/cache"
	"literouter/internal/provider"
)

// fewHunkDiff is diff output whose hunk headers are few but whose bodies are fat, so
// compressDiff alone lands under the head/tail cap. That is the branch where the note has
// nowhere else to live, and where a real transcript lost its content silently.
func fewHunkDiff() string {
	var builder strings.Builder
	builder.WriteString("diff --git a/x.go b/x.go\n")
	for index := range 12 {
		fmt.Fprintf(&builder, "@@ -%d,60 +%d,60 @@ func f%d()\n", index*60, index*60, index)
		for line := range 60 {
			fmt.Fprintf(&builder, " untouched context line %d carrying a fair amount of text\n", line)
		}
	}
	return builder.String()
}

// requireCompressedOnlyBranch pins which path truncateToolResult takes, using the same
// condition production does. Size alone cannot tell the branches apart — both end up a
// couple of kilobytes — and a test that assumes the wrong one proves nothing.
func requireCompressedOnlyBranch(t *testing.T, text string) {
	t.Helper()
	head, tail, _ := Policy{}.truncateSizes()
	compressed := cache.CompressForHistory("Bash", text)
	if compressed.Method == "none" || compressed.Method == "head_tail" {
		t.Fatalf("input is not structurally compressed (method %q)", compressed.Method)
	}
	if len(compressed.Compressed) > head+tail {
		t.Fatalf("compression left %d bytes, over the %d cap: this takes the head/tail branch",
			len(compressed.Compressed), head+tail)
	}
}

// numberedLines builds a body whose every line names its own 1-based number, so a test
// can read the elided range straight out of the surviving head and tail.
func numberedLines(count int, trailingNewline bool) string {
	lines := make([]string, 0, count)
	for index := 1; index <= count; index++ {
		lines = append(lines, fmt.Sprintf("line %d: %s", index, strings.Repeat("payload ", 8)))
	}
	body := strings.Join(lines, "\n")
	if trailingNewline {
		body += "\n"
	}
	return body
}

func TestLineCountTreatsATrailingNewlineAsEndingTheLastLine(t *testing.T) {
	for _, test := range []struct {
		text string
		want int
	}{
		{"", 0},
		{"a", 1},
		{"a\n", 1},
		{"a\nb", 2},
		{"a\nb\n", 2},
		{"a\n\nb", 3},
	} {
		if got := lineCount(test.text); got != test.want {
			t.Errorf("lineCount(%q) = %d, want %d", test.text, got, test.want)
		}
	}
}

// The range has to be right or it is worse than the count it replaced: a model told to
// fetch lines 39-4250 will fetch exactly that.
func TestTruncationReportsTheElidedLineRange(t *testing.T) {
	for _, trailing := range []bool{false, true} {
		text := numberedLines(600, trailing)
		replacement, ok := truncateToolResult(
			resultSource{name: "Read", input: json.RawMessage(`{"file_path":"/repo/a.go"}`)},
			"use-1", text, false, Policy{})
		if !ok {
			t.Fatalf("trailing=%v: not truncated", trailing)
		}
		// Read the boundaries back out of what survived rather than recomputing them the
		// same way the implementation does, which would only prove it agrees with itself.
		lastKept, firstKept := lastHeadLine(t, replacement), firstTailLine(t, replacement)
		want := fmt.Sprintf("lines %d-%d of 600", lastKept+1, firstKept-1)
		if !strings.Contains(replacement, want) {
			t.Fatalf("trailing=%v: want %q in note, got:\n%s", trailing, want, noteOf(t, replacement))
		}
	}
}

func TestTruncationNamesTheToolAndItsArgumentsToRecover(t *testing.T) {
	replacement, ok := truncateToolResult(
		resultSource{name: "Read", input: json.RawMessage("{\n  \"file_path\": \"/repo/a.go\"\n}")},
		"use-1", numberedLines(400, false), false, Policy{})
	if !ok {
		t.Fatal("not truncated")
	}
	// Compacted, so whitespace in the client's JSON cannot change the note's bytes.
	if !strings.Contains(replacement, `re-run Read with {"file_path":"/repo/a.go"}`) {
		t.Fatalf("recovery hint wrong:\n%s", noteOf(t, replacement))
	}
}

func TestRecoveryHintDegradesWithoutInventingACall(t *testing.T) {
	for _, test := range []struct {
		name   string
		call   resultSource
		want   string
		absent string
	}{
		{
			name: "no name known",
			call: resultSource{input: json.RawMessage(`{"file_path":"/a.go"}`)},
			want: "re-run the tool",
		},
		{
			name: "name but no arguments",
			call: resultSource{name: "Bash"},
			want: "re-run Bash",
		},
		{
			name:   "arguments too large to quote",
			call:   resultSource{name: "Write", input: json.RawMessage(`{"content":"` + strings.Repeat("x", maxRecoveryArgumentBytes) + `"}`)},
			want:   "re-run Write",
			absent: "xxxx",
		},
		{
			name: "arguments that are not valid JSON",
			call: resultSource{name: "Bash", input: json.RawMessage(`{"command":`)},
			want: "re-run Bash",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			hint := recoveryHint(test.call)
			if hint != test.want {
				t.Fatalf("hint = %q, want %q", hint, test.want)
			}
			if test.absent != "" && strings.Contains(hint, test.absent) {
				t.Fatalf("hint quoted oversized arguments: %q", hint)
			}
		})
	}
}

// One enormous line cut in the middle elides no whole line at all; reporting "lines 2-1"
// would be worse than saying nothing.
func TestTruncationFallsBackToBytesWhenNoWholeLineIsElided(t *testing.T) {
	replacement, ok := truncateToolResult(
		resultSource{name: "Bash"}, "use-1", strings.Repeat("x", 30_000), false, Policy{})
	if !ok {
		t.Fatal("not truncated")
	}
	note := noteOf(t, replacement)
	if strings.Contains(note, "lines ") {
		t.Fatalf("a range was reported with no whole line elided: %s", note)
	}
	if !strings.Contains(note, "bytes elided") {
		t.Fatalf("byte fallback missing: %s", note)
	}
}

// Structural compression selects lines rather than cutting a span, so no contiguous
// range describes what is missing. Claiming one would send the model to fetch the wrong
// lines — the failure this pairing produced before: a note reading "lines 21-113 of 121"
// for content whose real gap was lines 21-592 of 600. The count still holds, so that is
// what the note reports.
func TestStructuralCompressionReportsAScatteredCountNotARange(t *testing.T) {
	for _, test := range []struct {
		name  string
		call  resultSource
		text  string
		lines int
	}{
		{
			name:  "grep selects matching lines",
			call:  resultSource{name: "Grep", input: json.RawMessage(`{"pattern":"match"}`)},
			text:  strings.Repeat("src/pkg/a.go:12: match here with some detail\n", 900),
			lines: 900,
		},
		{
			name:  "a diff keeps its hunks",
			call:  resultSource{name: "Bash", input: json.RawMessage(`{"command":"git diff"}`)},
			text:  "diff --git a/x b/x\n" + strings.Repeat("+added line of code here\n", 900),
			lines: 901,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			replacement, ok := truncateToolResult(test.call, "use-1", test.text, false, Policy{})
			if !ok {
				t.Fatal("not truncated")
			}
			note := noteOf(t, replacement)
			if strings.Contains(note, "lines 1-") || strings.Contains(note, " of "+fmt.Sprint(test.lines)+" (") {
				t.Fatalf("a contiguous range survived structural compression: %s", note)
			}
			// Arithmetic, not just wording: what the note says is missing plus what actually
			// survived has to be the tool's own line count.
			var missing, total, share int
			if _, err := fmt.Sscanf(note, "[... %d of %d lines (%d%% of the output), scattered", &missing, &total, &share); err != nil {
				t.Fatalf("no scattered count in note (%v): %s", err, note)
			}
			if want := missing * 100 / total; share != want {
				t.Fatalf("share = %d%%, want %d%%", share, want)
			}
			if total != test.lines {
				t.Fatalf("total = %d, want the tool's own %d lines", total, test.lines)
			}
			if kept := survivingOutputLines(replacement); missing+kept != total {
				t.Fatalf("missing %d + kept %d = %d, want %d", missing, kept, missing+kept, total)
			}
			// The hint does not depend on addressability.
			if !strings.Contains(note, "re-run ") {
				t.Fatalf("recovery hint lost: %s", note)
			}
		})
	}
}

// Structural compression can meet the cap on its own, and then the head/tail step never
// runs. The note was attached to that step, so those results lost their content and said
// nothing about it — found on a real transcript, where a 274-line file read became 143
// bytes with no hint at all. The note belongs to the truncation, not to whichever stage
// happened to perform it.
func TestCompressionMeetingTheCapAloneStillLeavesAHint(t *testing.T) {
	// A diff compresses to hunk headers, far under the head+tail cap.
	var builder strings.Builder
	builder.WriteString("diff --git a/x.go b/x.go\n")
	for index := range 400 {
		fmt.Fprintf(&builder, "@@ -%d,4 +%d,4 @@ func f%d()\n context line\n-old line\n+new line\n context\n",
			index, index, index)
	}
	text := builder.String()

	replacement, ok := truncateToolResult(
		resultSource{name: "Bash", input: json.RawMessage(`{"command":"git diff"}`)},
		"use-1", text, false, Policy{})
	if !ok {
		t.Fatal("not truncated")
	}
	head, tail, _ := Policy{}.truncateSizes()
	if len(replacement) > head+tail+len(text)/10 {
		t.Fatalf("this case is meant to exercise compression meeting the cap alone, got %d bytes", len(replacement))
	}
	note := noteOf(t, replacement)
	if !strings.Contains(note, "elided by the proxy") {
		t.Fatalf("no elision note on a compressed-only result:\n%s", replacement)
	}
	if !strings.Contains(note, `re-run Bash with {"command":"git diff"}`) {
		t.Fatalf("no recovery hint on a compressed-only result: %s", note)
	}
	var missing, total, share int
	if _, err := fmt.Sscanf(note, "[... %d of %d lines (%d%% of the output), scattered", &missing, &total, &share); err != nil {
		t.Fatalf("extent unreadable (%v): %s", err, note)
	}
	if want := missing * 100 / total; share != want {
		t.Fatalf("share = %d%%, want %d%%", share, want)
	}
	if total != lineCount(text) {
		t.Fatalf("total = %d, want the source's %d lines", total, lineCount(text))
	}
	if missing <= 0 || missing >= total {
		t.Fatalf("missing = %d of %d makes no sense", missing, total)
	}
}

// What survives a structural pass is dotted with the compressor's own small notes, so a
// single note underneath them reads as one more of the same — measured live, the model
// then sampled the remains instead of recovering them. The note leads the body.
func TestTheCompressedOnlyNoteLeadsTheBody(t *testing.T) {
	text := fewHunkDiff()
	replacement, ok := truncateToolResult(
		resultSource{name: "Bash", input: json.RawMessage(`{"command":"git diff"}`)},
		"use-1", text, false, Policy{})
	if !ok {
		t.Fatal("not truncated")
	}
	requireCompressedOnlyBranch(t, text)
	ours := strings.Index(replacement, "elided by the proxy")
	if ours < 0 {
		t.Fatal("no note")
	}
	// It must sit above every note the compressor left behind.
	if theirs := strings.Index(replacement, "unchanged lines omitted"); theirs >= 0 && theirs < ours {
		t.Fatalf("a compressor note precedes ours, which buries the real extent:\n%s", replacement[:400])
	}
	// And directly under the header, so it frames the body rather than trailing it.
	header := strings.Index(replacement, "]\n")
	if header < 0 {
		t.Fatalf("no header:\n%s", replacement[:min(200, len(replacement))])
	}
	if !strings.HasPrefix(replacement[header+2:], "[... ") {
		t.Fatalf("note does not open the body:\n%s", replacement[:min(300, len(replacement))])
	}
}

// The line-counting head/tail compression is this stage's own operation. Running both
// nested two elision notes inside one result.
func TestTruncationLeavesExactlyOneElisionNote(t *testing.T) {
	replacement, ok := truncateToolResult(
		resultSource{name: "Read", input: json.RawMessage(`{"file_path":"/repo/a.go"}`)},
		"use-1", numberedLines(900, true), false, Policy{})
	if !ok {
		t.Fatal("not truncated")
	}
	if count := strings.Count(replacement, "elided by the proxy"); count != 1 {
		t.Fatalf("elision notes = %d, want 1:\n%s", count, replacement)
	}
	if strings.Contains(replacement, "truncated from middle") {
		t.Fatalf("the compressor's own note survived inside ours:\n%s", replacement)
	}
}

// The note is part of the cached prefix. If it changed as the conversation grew, every
// later turn would reprice the whole prefix — the failure collapseSuperseded avoids by
// refusing to name the newer tool_use id.
func TestTruncationNoteIsUnchangedAsTheConversationGrows(t *testing.T) {
	build := func(chains int) provider.Request {
		messages := []provider.Message{
			{Role: "user", Content: []provider.Content{{Type: "text", Text: "audit"}}},
		}
		for index := range chains {
			messages = append(messages,
				provider.Message{Role: "assistant", Content: []provider.Content{{
					Type: "tool_use", ToolUseID: fmt.Sprintf("call_%d", index), Name: "Read",
					Input: json.RawMessage(fmt.Sprintf(`{"file_path":"/repo/f%d.go"}`, index))}}},
				provider.Message{Role: "user", Content: []provider.Content{{
					Type: "tool_result", ToolUseID: fmt.Sprintf("call_%d", index),
					Text: numberedLines(400, true)}}},
			)
		}
		return provider.Request{Model: "model", Messages: messages}
	}
	policy := AggressivePolicy(DefaultPolicy())

	shorter, longer := build(20), build(28)
	truncateOldToolResults(&shorter, stickyOldBoundary(len(shorter.Messages), policy), policy)
	truncateOldToolResults(&longer, stickyOldBoundary(len(longer.Messages), policy), policy)

	// call_2's result is old in both, so its rewritten bytes must be identical.
	first, second := toolResultFor(t, shorter, "call_2"), toolResultFor(t, longer, "call_2")
	if first != second {
		t.Fatalf("the note changed as history grew, repricing the prefix:\n%s\n---\n%s",
			noteOf(t, first), noteOf(t, second))
	}
	if !strings.Contains(first, `re-run Read with {"file_path":"/repo/f2.go"}`) {
		t.Fatalf("note lost its own call: %s", noteOf(t, first))
	}
}

// Each result must describe its own call, not the first or last one seen.
func TestEachTruncatedResultNamesItsOwnCall(t *testing.T) {
	messages := []provider.Message{
		{Role: "user", Content: []provider.Content{{Type: "text", Text: "audit"}}},
	}
	for index := range 20 {
		messages = append(messages,
			provider.Message{Role: "assistant", Content: []provider.Content{{
				Type: "tool_use", ToolUseID: fmt.Sprintf("call_%d", index), Name: "Read",
				Input: json.RawMessage(fmt.Sprintf(`{"file_path":"/repo/f%d.go"}`, index))}}},
			provider.Message{Role: "user", Content: []provider.Content{{
				Type: "tool_result", ToolUseID: fmt.Sprintf("call_%d", index),
				Text: numberedLines(400, true)}}},
		)
	}
	request := provider.Request{Model: "model", Messages: messages}
	policy := AggressivePolicy(DefaultPolicy())
	truncateOldToolResults(&request, stickyOldBoundary(len(request.Messages), policy), policy)

	for index := range 5 {
		id := fmt.Sprintf("call_%d", index)
		want := fmt.Sprintf(`re-run Read with {"file_path":"/repo/f%d.go"}`, index)
		if got := toolResultFor(t, request, id); !strings.Contains(got, want) {
			t.Fatalf("%s: want %q, got:\n%s", id, want, noteOf(t, got))
		}
	}
}

// Running the stage over its own output must not re-truncate or double-annotate.
func TestTruncationNoteIsNotReprocessed(t *testing.T) {
	build := func() provider.Request {
		return provider.Request{Model: "model", Messages: []provider.Message{
			{Role: "user", Content: []provider.Content{{Type: "text", Text: "audit"}}},
			{Role: "assistant", Content: []provider.Content{{
				Type: "tool_use", ToolUseID: "call_0", Name: "Read",
				Input: json.RawMessage(`{"file_path":"/repo/a.go"}`)}}},
			{Role: "user", Content: []provider.Content{{
				Type: "tool_result", ToolUseID: "call_0", Text: numberedLines(600, true)}}},
			{Role: "assistant", Content: []provider.Content{{Type: "text", Text: "done"}}},
		}}
	}
	policy := AggressivePolicy(DefaultPolicy())
	request := build()
	truncateOldToolResults(&request, 3, policy)
	once := toolResultFor(t, request, "call_0")
	truncateOldToolResults(&request, 3, policy)
	if twice := toolResultFor(t, request, "call_0"); twice != once {
		t.Fatalf("second pass rewrote the block:\n%s\n---\n%s", noteOf(t, once), noteOf(t, twice))
	}
	if strings.Count(once, "elided by the proxy") != 1 {
		t.Fatalf("note appears more than once:\n%s", once)
	}
}

// --- helpers that read the note back rather than recompute it -------------------------

// noteOf extracts this stage's own elision note. It anchors on "elided by the proxy"
// rather than the first "[... ": a structurally compressed body carries notes of the
// compressor's own, and picking the first bracket found one of those instead.
func noteOf(t *testing.T, replacement string) string {
	t.Helper()
	anchor := strings.Index(replacement, "elided by the proxy")
	if anchor < 0 {
		return replacement
	}
	start := strings.LastIndex(replacement[:anchor], "[... ")
	if start < 0 {
		start = anchor
	}
	end := strings.Index(replacement[anchor:], "...]")
	if end < 0 {
		return replacement[start:]
	}
	return replacement[start : anchor+end+4]
}

// lastHeadLine is the number of the final line still present before the note.
func lastHeadLine(t *testing.T, replacement string) int {
	t.Helper()
	head := replacement[:strings.Index(replacement, "\n[... ")]
	lines := strings.Split(head, "\n")
	return lineNumberOf(t, lines[len(lines)-1])
}

// firstTailLine is the number of the first line present after the note.
func firstTailLine(t *testing.T, replacement string) int {
	t.Helper()
	rest := replacement[strings.Index(replacement, "...]\n")+len("...]\n"):]
	return lineNumberOf(t, strings.SplitN(rest, "\n", 2)[0])
}

func lineNumberOf(t *testing.T, line string) int {
	t.Helper()
	var number int
	if _, err := fmt.Sscanf(line, "line %d:", &number); err != nil {
		t.Fatalf("cannot read a line number out of %q: %v", line, err)
	}
	return number
}

// survivingOutputLines counts the tool's own lines left in a truncated block: everything
// except the proxy's header, its elision note, and any note a compressor left behind.
func survivingOutputLines(replacement string) int {
	count := 0
	for _, line := range strings.Split(replacement, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
		case strings.HasPrefix(trimmed, proxyMarkerPrefix):
		case strings.HasPrefix(trimmed, "[... "):
		default:
			count++
		}
	}
	return count
}

func toolResultFor(t *testing.T, request provider.Request, toolUseID string) string {
	t.Helper()
	for _, message := range request.Messages {
		for _, block := range message.Content {
			if block.Type == "tool_result" && block.ToolUseID == toolUseID {
				return block.Text
			}
		}
	}
	t.Fatalf("no tool_result for %s", toolUseID)
	return ""
}

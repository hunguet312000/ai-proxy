package cache

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestCompressToolResultSmallUnchanged(t *testing.T) {
	result := CompressToolResult("cat", "small")
	if result.Compressed != "small" || result.Method != "none" || result.SavedTokens != 0 {
		t.Fatalf("result = %#v", result)
	}
}

func TestCompressToolResultSafePreservesUniqueEvidence(t *testing.T) {
	content := numberedLines(400)
	result := CompressToolResult("read_file", content)
	if result.Method != "none" || result.Compressed != content || result.SavedTokens != 0 {
		t.Fatalf("result = %#v", result)
	}
}

func TestCompressToolResultAggressivePreservesEvidence(t *testing.T) {
	// A later Edit has to reproduce bytes a Read returned, and a Grep line number
	// has to still be real, so aggressive mode must leave these results untouched.
	// Unknown tools default to no compression for the same reason.
	content := numberedLines(400)
	for _, tool := range []string{"read_file", "Read", "cat", "grep", "rg", "Edit", "Write", "bash", "mcp__custom__thing"} {
		result := CompressToolResultMode(tool, content, CompressionAggressive)
		if result.Method != "none" || result.Compressed != content || result.SavedTokens != 0 {
			t.Fatalf("tool %q was compressed: method=%q saved=%d", tool, result.Method, result.SavedTokens)
		}
	}
}

func TestCompressToolResultDiff(t *testing.T) {
	var content strings.Builder
	content.WriteString("diff --git a/file.go b/file.go\n--- a/file.go\n+++ b/file.go\n@@ -1,500 +1,500 @@\n")
	for i := range 500 {
		fmt.Fprintf(&content, " context %d\n", i)
	}
	content.WriteString("-old\n+new\n")
	result := CompressToolResultMode("git", content.String(), CompressionAggressive)
	if result.Method != "git_diff" || !strings.Contains(result.Compressed, "diff --git") || !strings.Contains(result.Compressed, "+new") || !strings.Contains(result.Compressed, "omitted") {
		t.Fatalf("compressed = %q", result.Compressed)
	}
}

func TestCompressToolResultListingAndLog(t *testing.T) {
	listing := numberedLines(500)
	listed := CompressToolResultMode("find", listing, CompressionAggressive)
	if listed.Method != "listing" || !strings.Contains(listed.Compressed, "500 paths") {
		t.Fatalf("listing = %#v", listed)
	}
	var log strings.Builder
	for i := range 500 {
		if i == 250 {
			log.WriteString("ERROR database unavailable\n")
		} else {
			fmt.Fprintf(&log, "INFO line %d\n", i)
		}
	}
	logged := CompressToolResultMode("app.log", log.String(), CompressionAggressive)
	if logged.Method != "log" || !strings.Contains(logged.Compressed, "ERROR database unavailable") {
		t.Fatalf("log = %#v", logged)
	}
}

func TestCompressGitShowAndKeepGrepContext(t *testing.T) {
	var show strings.Builder
	show.WriteString("commit abc\nAuthor: A\nDate: today\n\ndiff --git a/a b/a\n@@ -1 +1 @@\n")
	show.WriteString(strings.Repeat(" context\n", 400))
	show.WriteString("-old\n+new\n")
	if result := CompressToolResultMode("git show", show.String(), CompressionAggressive); result.Method != "git_diff" {
		t.Fatalf("git show method = %q", result.Method)
	}
	lines := make([]string, 500)
	for index := range lines {
		lines[index] = fmt.Sprintf("context %d", index)
	}
	lines[250] = "file.go:42:MATCH"
	result := CompressToolResultMode("rg", strings.Join(lines, "\n"), CompressionAggressive)
	for index := 247; index <= 253; index++ {
		if !strings.Contains(result.Compressed, lines[index]) {
			t.Fatalf("missing context line %d in %q", index, result.Compressed)
		}
	}
}

func TestJSONCompressionStaysValid(t *testing.T) {
	content := "{\n  \"items\": [\n" + strings.Repeat("    {\"value\": 1},\n", 300) + "    {\"value\": 2}\n  ]\n}"
	result := CompressToolResultMode("output", content, CompressionAggressive)
	if result.Method != "json_compact" || !json.Valid([]byte(result.Compressed)) {
		t.Fatalf("result = %#v", result)
	}
}

func TestCompressForHistorySmallUnchanged(t *testing.T) {
	result := CompressForHistory("read_file", "small")
	if result.Compressed != "small" || result.Method != "none" || result.SavedTokens != 0 {
		t.Fatalf("result = %#v", result)
	}
}

func TestCompressForHistoryCompressesEvidenceTools(t *testing.T) {
	// Past the compaction boundary a Read result is no longer edit-quoting material,
	// so unlike the live-turn path evidence tools are fair game and unknown tools
	// fall back to head/tail rather than "none".
	content := numberedLines(400)
	for _, tool := range []string{"read_file", "cat", "bash", "mcp__custom__thing"} {
		result := CompressForHistory(tool, content)
		if result.Method != "head_tail" || len(result.Compressed) >= len(content) {
			t.Fatalf("tool %q: method=%q len=%d (original %d)", tool, result.Method, len(result.Compressed), len(content))
		}
		if !strings.Contains(result.Compressed, "line-000") || !strings.Contains(result.Compressed, "line-399") {
			t.Fatalf("tool %q lost head or tail: %q", tool, result.Compressed[:200])
		}
	}
}

func TestCompressForHistoryGrepKeepsMatchContext(t *testing.T) {
	lines := make([]string, 500)
	for index := range lines {
		lines[index] = fmt.Sprintf("context %d", index)
	}
	lines[250] = "file.go:42:MATCH"
	result := CompressForHistory("rg", strings.Join(lines, "\n"))
	if result.Method != "grep" {
		t.Fatalf("method = %q", result.Method)
	}
	if !strings.Contains(result.Compressed, "file.go:42:MATCH") {
		t.Fatalf("match line lost: %q", result.Compressed)
	}
}

func TestCompressForHistoryStructuredFormats(t *testing.T) {
	var diff strings.Builder
	diff.WriteString("diff --git a/file.go b/file.go\n@@ -1,500 +1,500 @@\n")
	diff.WriteString(strings.Repeat(" context\n", 500))
	diff.WriteString("-old\n+new\n")
	if result := CompressForHistory("bash", diff.String()); result.Method != "git_diff" || !strings.Contains(result.Compressed, "+new") {
		t.Fatalf("diff = %#v", result)
	}
	jsonContent := "{\n  \"items\": [\n" + strings.Repeat("    {\"value\": 1},\n", 300) + "    {\"value\": 2}\n  ]\n}"
	if result := CompressForHistory("output", jsonContent); result.Method != "json_compact" || !json.Valid([]byte(result.Compressed)) {
		t.Fatalf("json = %#v", result)
	}
}

func TestCompressForHistoryNeverExpands(t *testing.T) {
	content := strings.Repeat("x", compressionThreshold)
	result := CompressForHistory("unknown", content)
	if len(result.Compressed) > len(content) {
		t.Fatalf("compressed grew from %d to %d", len(content), len(result.Compressed))
	}
}

func TestCompressionNeverExpands(t *testing.T) {
	content := strings.Repeat("x", compressionThreshold)
	result := CompressToolResult("unknown", content)
	if len(result.Compressed) > len(content) {
		t.Fatalf("compressed grew from %d to %d", len(content), len(result.Compressed))
	}
}

func BenchmarkCompressSmallToolResult(b *testing.B) {
	for b.Loop() {
		_ = CompressToolResult("read_file", "small result")
	}
}

func BenchmarkCompressToolResult(b *testing.B) {
	content := numberedLines(10_000)
	b.ReportAllocs()
	for b.Loop() {
		_ = CompressToolResult("read_file", content)
	}
}

func numberedLines(count int) string {
	var builder strings.Builder
	for index := range count {
		fmt.Fprintf(&builder, "line-%03d some content that is long enough for compression\n", index)
	}
	return builder.String()
}

// A result that merely mentions a diff is not one. Matching the substring anywhere sent
// an agent's read of this package's own source — the detection literal is in it — through
// compressDiff, which keeps hunk headers and drops the rest: 9,082 bytes of Go returned as
// 143. Seen on a real transcript, not constructed.
func TestDiffDetectionRequiresALineThatStartsADiff(t *testing.T) {
	goSource := "1\tpackage cache\n2\t\n3\tfunc detect(content string) bool {\n" +
		"4\t\treturn strings.Contains(content, \"diff --git\")\n5\t}\n" +
		strings.Repeat("6\t// padding line to clear the compression threshold\n", 200)
	if looksLikeDiff(goSource) {
		t.Fatal("source that mentions the literal must not be read as a diff")
	}
	result := CompressForHistory("Read", goSource)
	if result.Method == "git_diff" {
		t.Fatalf("a source file was diff-compressed: %d -> %d bytes", len(goSource), len(result.Compressed))
	}

	// Real diff output still is one, whether it leads or follows a commit header.
	for _, real := range []string{
		"diff --git a/x.go b/x.go\n@@ -1,2 +1,2 @@\n-old\n+new\n",
		"commit abc123\nAuthor: someone\n\n    message\n\ndiff --git a/x.go b/x.go\n@@ -1 +1 @@\n-a\n+b\n",
		"$ git diff\ndiff --git a/x.go b/x.go\n@@ -1 +1 @@\n-a\n+b\n",
	} {
		if !looksLikeDiff(real) {
			t.Fatalf("real diff output not detected: %q", real[:min(40, len(real))])
		}
	}
}

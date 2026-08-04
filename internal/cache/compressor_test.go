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

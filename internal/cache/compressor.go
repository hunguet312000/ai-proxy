package cache

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

type CompressionMode string

const (
	CompressionSafe       CompressionMode = "safe"
	CompressionAggressive CompressionMode = "aggressive"
	compressionThreshold                  = 2048
)

type CompressionResult struct {
	OriginalTokens int    `json:"original_tokens"`
	Compressed     string `json:"compressed"`
	SavedTokens    int    `json:"saved_tokens"`
	Method         string `json:"method"`
}

func CompressToolResult(toolName, content string) CompressionResult {
	return CompressToolResultMode(toolName, content, CompressionSafe)
}

func CompressToolResultMode(toolName, content string, mode CompressionMode) CompressionResult {
	originalTokens := estimateTokens(content)
	// Safe mode is deliberately byte-preserving. Generic truncation can remove
	// the only diagnostic line needed by the next model response.
	if mode != CompressionAggressive || len(content) < compressionThreshold {
		return CompressionResult{OriginalTokens: originalTokens, Compressed: content, Method: "none"}
	}
	name := strings.ToLower(filepath.Base(strings.TrimSpace(toolName)))
	// Coding agents quote tool output back verbatim: an Edit needs the exact bytes a
	// Read returned to match old_string, and a Grep line number must still be real.
	// Truncating those results is what makes a later edit fail, so they are never
	// compressed and unknown tools default to no compression rather than head/tail.
	if isEvidenceTool(name) {
		return CompressionResult{OriginalTokens: originalTokens, Compressed: content, Method: "none"}
	}
	var compressed, method string
	switch {
	case strings.Contains(content, "diff --git") || strings.HasPrefix(strings.TrimSpace(content), "commit "):
		compressed, method = compressDiff(content, mode), "git_diff"
	case slices.Contains([]string{"ls", "tree", "find"}, name):
		compressed, method = compressListing(content, mode), "listing"
	case json.Valid([]byte(content)):
		compressed, method = compactJSON(content), "json_compact"
	case strings.Contains(name, "log"):
		compressed, method = compressLog(content, mode), "log"
	default:
		return CompressionResult{OriginalTokens: originalTokens, Compressed: content, Method: "none"}
	}
	if len(compressed) >= len(content) {
		return CompressionResult{OriginalTokens: originalTokens, Compressed: content, Method: "none"}
	}
	compressedTokens := estimateTokens(compressed)
	return CompressionResult{
		OriginalTokens: originalTokens,
		Compressed:     compressed,
		SavedTokens:    max(originalTokens-compressedTokens, 0),
		Method:         method,
	}
}

// isEvidenceTool reports tools whose output a later turn must reproduce exactly.
func isEvidenceTool(name string) bool {
	if slices.Contains([]string{
		"cat", "read", "read_file", "view", "open",
		"edit", "write", "str_replace", "str_replace_based_edit_tool", "apply_patch",
		"grep", "rg", "ag", "ripgrep", "search",
		"bash", "sh", "shell", "run", "exec", "terminal",
	}, name) {
		return true
	}
	return strings.HasPrefix(name, "read") || strings.HasPrefix(name, "write") || strings.HasPrefix(name, "edit")
}

func compressDiff(content string, mode CompressionMode) string {
	lines := strings.Split(content, "\n")
	result := make([]string, 0, len(lines)/2)
	omitted := 0
	flush := func() {
		if omitted > 0 {
			result = append(result, fmt.Sprintf(" [... %d unchanged lines omitted ...]", omitted))
			omitted = 0
		}
	}
	for _, line := range lines {
		keep := strings.HasPrefix(line, "diff --git ") || strings.HasPrefix(line, "index ") ||
			strings.HasPrefix(line, "--- ") || strings.HasPrefix(line, "+++ ") ||
			strings.HasPrefix(line, "@@ ") || strings.HasPrefix(line, "+") || strings.HasPrefix(line, "-") ||
			strings.HasPrefix(line, "commit ") || strings.HasPrefix(line, "Author:") || strings.HasPrefix(line, "Date:")
		if keep {
			flush()
			result = append(result, line)
		} else {
			omitted++
		}
	}
	flush()
	return limitLines(result, lineLimit(mode, 800, 300))
}

func compressListing(content string, mode CompressionMode) string {
	lines := nonEmptyLines(content)
	limit := lineLimit(mode, 300, 120)
	if len(lines) <= limit {
		return content
	}
	important := make([]string, 0, limit)
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		depth := strings.Count(line, "│") + strings.Count(line, "    ")
		if depth <= 1 || isImportantPath(trimmed) {
			important = append(important, line)
		}
		if len(important) == limit {
			break
		}
	}
	if len(important) < limit {
		seen := make(map[string]struct{}, len(important))
		for _, line := range important {
			seen[line] = struct{}{}
		}
		for _, line := range lines {
			if _, ok := seen[line]; ok {
				continue
			}
			important = append(important, line)
			if len(important) == limit {
				break
			}
		}
	}
	return fmt.Sprintf("[%d paths; showing %d top-level/important]\n%s", len(lines), len(important), strings.Join(important, "\n"))
}

func compressGrep(content string, mode CompressionMode) string {
	lines := strings.Split(content, "\n")
	limit := lineLimit(mode, 400, 160)
	if len(lines) <= limit {
		return content
	}
	keep := make(map[int]struct{}, limit)
	for index, line := range lines {
		if !grepMatchLine(line) {
			continue
		}
		for contextIndex := max(0, index-3); contextIndex <= min(len(lines)-1, index+3); contextIndex++ {
			keep[contextIndex] = struct{}{}
		}
	}
	indexes := make([]int, 0, len(keep))
	for index := range keep {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	if len(indexes) == 0 {
		return compressHeadTail(content, mode)
	}
	if len(indexes) > limit {
		indexes = indexes[:limit]
	}
	result := make([]string, 0, len(indexes)+1)
	previous := -2
	for _, index := range indexes {
		if index > previous+1 {
			result = append(result, "[... context omitted ...]")
		}
		result = append(result, lines[index])
		previous = index
	}
	return strings.Join(result, "\n")
}

func grepMatchLine(line string) bool {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, "--") {
		return false
	}
	if strings.Contains(line, ":") {
		return true
	}
	return strings.Contains(strings.ToLower(line), "match")
}

func compressHeadTail(content string, mode CompressionMode) string {
	lines := strings.Split(content, "\n")
	head, tail := 150, 100
	if mode == CompressionAggressive {
		head, tail = 80, 40
	}
	if len(lines) <= head+tail {
		return content
	}
	result := append([]string(nil), lines[:head]...)
	result = append(result, fmt.Sprintf("[... %d lines truncated from middle ...]", len(lines)-head-tail))
	result = append(result, lines[len(lines)-tail:]...)
	return strings.Join(result, "\n")
}

func compactJSON(content string) string {
	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(content)); err != nil {
		return content
	}
	return compact.String()
}

func compressLog(content string, mode CompressionMode) string {
	lines := strings.Split(content, "\n")
	limit := lineLimit(mode, 300, 120)
	if len(lines) <= limit {
		return content
	}
	highlights := make([]string, 0, limit/3)
	for _, line := range lines {
		lower := strings.ToLower(line)
		if strings.Contains(lower, "error") || strings.Contains(lower, "warn") || strings.Contains(lower, "fatal") || strings.Contains(lower, "panic") {
			highlights = append(highlights, line)
			if len(highlights) == limit/3 {
				break
			}
		}
	}
	head := (limit - len(highlights)) * 2 / 3
	tail := limit - len(highlights) - head
	result := append([]string(nil), lines[:head]...)
	if len(highlights) > 0 {
		result = append(result, "[highlighted warnings/errors]")
		result = append(result, highlights...)
	}
	result = append(result, fmt.Sprintf("[... %d log lines omitted ...]", len(lines)-head-tail))
	result = append(result, lines[len(lines)-tail:]...)
	return strings.Join(result, "\n")
}

func limitLines(lines []string, limit int) string {
	if len(lines) <= limit {
		return strings.Join(lines, "\n")
	}
	head := limit * 3 / 4
	tail := limit - head
	result := append([]string(nil), lines[:head]...)
	result = append(result, fmt.Sprintf("[... %d compacted lines omitted ...]", len(lines)-limit))
	result = append(result, lines[len(lines)-tail:]...)
	return strings.Join(result, "\n")
}

func nonEmptyLines(content string) []string {
	lines := strings.Split(content, "\n")
	result := lines[:0]
	for _, line := range lines {
		if strings.TrimSpace(line) != "" {
			result = append(result, line)
		}
	}
	return result
}

func lineLimit(mode CompressionMode, safe, aggressive int) int {
	if mode == CompressionAggressive {
		return aggressive
	}
	return safe
}

func estimateTokens(content string) int {
	if content == "" {
		return 0
	}
	return (len(content) + 3) / 4
}

func isImportantPath(path string) bool {
	lower := strings.ToLower(path)
	for _, name := range []string{"readme", "dockerfile", "go.mod", "package.json", "pyproject.toml", "cargo.toml", ".env.example", "config"} {
		if strings.Contains(lower, name) {
			return true
		}
	}
	return false
}

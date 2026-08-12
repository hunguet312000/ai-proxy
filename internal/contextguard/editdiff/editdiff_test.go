package editdiff

import (
	"fmt"
	"strings"
	"testing"
)

func split(text string) []string {
	lines := strings.Split(text, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// lcsLength is a plain DP LCS — the reference oracle the engine's Kept must match.
func lcsLength(a, b []string) int {
	dp := make([][]int, len(a)+1)
	for index := range dp {
		dp[index] = make([]int, len(b)+1)
	}
	for i := 1; i <= len(a); i++ {
		for j := 1; j <= len(b); j++ {
			if a[i-1] == b[j-1] {
				dp[i][j] = dp[i-1][j-1] + 1
			} else if dp[i-1][j] > dp[i][j-1] {
				dp[i][j] = dp[i-1][j]
			} else {
				dp[i][j] = dp[i][j-1]
			}
		}
	}
	return dp[len(a)][len(b)]
}

// keptLines is `before` with the script's spans removed — the common-subsequence
// content the marker claims survives.
func keptLines(before string, script Script) []string {
	lines := split(before)
	for _, span := range script.Spans {
		for index := span.From - 1; index < span.To; index++ {
			lines[index] = ""
		}
	}
	var kept []string
	for _, line := range lines {
		if line != "" {
			kept = append(kept, line)
		}
	}
	return kept
}

func isSubsequence(needle, haystack []string) bool {
	index := 0
	for _, line := range haystack {
		if index < len(needle) && needle[index] == line {
			index++
		}
	}
	return index == len(needle)
}

// TestDiffMatchesReferenceLCS is the engine's correctness oracle: whenever Diff
// reports a near-duplicate, Kept must equal the true LCS length, Changed must be
// the number of `before` lines lost, and the kept lines must be a common
// subsequence of both texts.
func TestDiffMatchesReferenceLCS(t *testing.T) {
	cases := []struct {
		name, before, after string
		wantOK              bool // false = engine must reject (not a near-duplicate)
	}{
		// changed==0 cases are no-ops: the engine reports nothing to delete, which is
		// the right contract — identical blocks belong to exact-dedup, and a pure
		// insertion deletes no `before` lines so a diff marker saves nothing.
		{"identical", strings.Repeat("same line\n", 200), strings.Repeat("same line\n", 200), false},
		{"insert into the middle", "x\ny\nz\n", "x\na\ny\nb\nz\n", false},
		{"prefix insert", "aaa\nbbb\nccc\nddd\neee\n", "xxx\naaa\nbbb\nccc\nddd\neee\n", false},
		{"suffix append", "aaa\nbbb\nccc\n", "aaa\nbbb\nccc\nddd\neee\n", false},
		{"change a few middle lines", "a\nb\nc\nd\ne\nf\ng\nh\ni\nj\n", "a\nb\nc\nX\nY\ne\nf\ng\nh\nj\n", true},
		{"no trailing newline", "alpha\nbeta\ngamma\ndelta", "alpha\nbeta\nomega\nomega2\ndelta", true},
		{"delete two tail lines", "1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n", "1\n2\n3\n4\n5\n6\n7\n8\n", true},
		{"reorder same lines", "a\nb\nc\nd\ne\nf\n", "f\ne\nd\nc\nb\na\n", false},
		{"unrelated", strings.Repeat("module a\n", 200), strings.Repeat("module b\n", 210), false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			script, ok := Diff(testCase.before, testCase.after)
			if ok != testCase.wantOK {
				t.Fatalf("Diff() ok = %v, want %v", ok, testCase.wantOK)
			}
			if !ok {
				return
			}
			a, b := split(testCase.before), split(testCase.after)
			if script.Total != len(a) {
				t.Fatalf("Total = %d, want %d", script.Total, len(a))
			}
			wantKept := lcsLength(a, b)
			if script.Kept != wantKept {
				t.Fatalf("Kept = %d, reference LCS = %d", script.Kept, wantKept)
			}
			if script.Changed != len(a)-wantKept {
				t.Fatalf("Changed = %d, want %d", script.Changed, len(a)-wantKept)
			}
			kept := keptLines(testCase.before, script)
			if len(kept) != wantKept {
				t.Fatalf("kept lines = %d, want %d", len(kept), wantKept)
			}
			if !isSubsequence(kept, a) || !isSubsequence(kept, b) {
				t.Fatal("kept lines are not a common subsequence of both texts")
			}
		})
	}
}

func TestDiffChangedMiddleSpan(t *testing.T) {
	var before, after []string
	for index := 0; index < 300; index++ {
		line := fmt.Sprintf("line %d", index)
		before = append(before, line)
		if index < 40 || index > 42 {
			after = append(after, line)
		}
	}
	script, ok := Diff(strings.Join(before, "\n"), strings.Join(after, "\n"))
	if !ok {
		t.Fatal("a 3-line change in a 300-line body is exactly the near-duplicate case")
	}
	if script.Changed != 3 || len(script.Spans) != 1 {
		t.Fatalf("script = %+v, want Changed=3 and one span", script)
	}
	if script.Spans[0] != (Span{From: 41, To: 43}) {
		t.Fatalf("span = %+v, want 41-43", script.Spans[0])
	}
}

func TestDiffScatteredChangesProduceMergedSpans(t *testing.T) {
	var before, after []string
	for index := 0; index < 100; index++ {
		line := fmt.Sprintf("line %d", index)
		before = append(before, line)
		if index != 10 && index != 40 && index != 41 && index != 42 {
			after = append(after, line)
		}
	}
	script, ok := Diff(strings.Join(before, "\n"), strings.Join(after, "\n"))
	if !ok {
		t.Fatal("scattered small changes are a near-duplicate")
	}
	if script.Changed != 4 || len(script.Spans) != 2 {
		t.Fatalf("script = %+v, want Changed=4 and two spans", script)
	}
	if script.Spans[0] != (Span{From: 11, To: 11}) || script.Spans[1] != (Span{From: 41, To: 43}) {
		t.Fatalf("spans = %+v", script.Spans)
	}
}

func TestDiffInsertedOnlyIsANoOp(t *testing.T) {
	var before, after []string
	for index := 0; index < 100; index++ {
		line := fmt.Sprintf("line %d", index)
		before = append(before, line)
		if index == 50 {
			after = append(after, "brand new line")
		}
		after = append(after, line)
	}
	// Every `before` line survives; nothing is deleted, so there is nothing for a
	// diff marker to say — exact dedup does not fire (texts differ), but no bytes
	// would be saved either. The engine reports no-op.
	if _, ok := Diff(strings.Join(before, "\n"), strings.Join(after, "\n")); ok {
		t.Fatal("a pure insertion was reported as a deletable diff")
	}
}

func TestDiffRoundTripLargeFile(t *testing.T) {
	var before, after []string
	for index := 0; index < 800; index++ {
		line := fmt.Sprintf("file.go:%d: some diagnostic content", index)
		before = append(before, line)
		switch index {
		case 200, 201, 202:
			// three changed lines
		default:
			after = append(after, line)
		}
	}
	script, ok := Diff(strings.Join(before, "\n")+"\n", strings.Join(after, "\n"))
	if !ok {
		t.Fatal("large near-duplicate rejected")
	}
	if script.Changed != 3 {
		t.Fatalf("Changed = %d, want 3", script.Changed)
	}
	a, b := split(strings.Join(before, "\n")+"\n"), split(strings.Join(after, "\n"))
	if script.Kept != lcsLength(a, b) {
		t.Fatal("Kept != reference LCS on a large body")
	}
	if !isSubsequence(keptLines(strings.Join(before, "\n")+"\n", script), b) {
		t.Fatal("kept lines not a subsequence of after on a large body")
	}
}

func TestDiffRejectsReorder(t *testing.T) {
	var linesA, linesB []string
	for index := 0; index < 100; index++ {
		linesA = append(linesA, fmt.Sprintf("line %d", index))
		linesB = append(linesB, fmt.Sprintf("line %d", index))
	}
	for left, right := 0, len(linesB)-1; left < right; left, right = left+1, right-1 {
		linesB[left], linesB[right] = linesB[right], linesB[left]
	}
	if _, ok := Diff(strings.Join(linesA, "\n"), strings.Join(linesB, "\n")); ok {
		t.Fatal("a reordered body (same lines, different order) reported as near-duplicate")
	}
}

func TestDiffRejectsOversizedBodies(t *testing.T) {
	big := strings.Repeat("filler\n", maxDiffLines+1)
	if _, ok := Diff(big, big); ok {
		t.Fatal("an oversized body bypassed the size bound")
	}
}

func TestDiffIsDeterministic(t *testing.T) {
	before := strings.Repeat("content line\n", 150)
	after := strings.Repeat("content line\n", 120) + "changed\n"
	first, ok1 := Diff(before, after)
	second, ok2 := Diff(before, after)
	if !ok1 || !ok2 {
		t.Fatal("diff not ok")
	}
	if first.Changed != second.Changed || len(first.Spans) != len(second.Spans) {
		t.Fatalf("diff not deterministic: %+v vs %+v", first, second)
	}
}

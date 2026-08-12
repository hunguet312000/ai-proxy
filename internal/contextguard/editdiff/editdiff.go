// Package editdiff computes bounded line-level edit scripts between two texts. It
// exists so the context pipeline can collapse an older tool result that is a
// near-duplicate of a later one — the re-read of a file that changed by a line or
// two — into a compact marker naming the difference, instead of keeping and
// re-billing the whole body.
//
// The search is deliberately bounded to near-duplicates: an order-insensitive
// overlap pre-filter rejects unrelated texts instantly, and the Myers search stops
// as soon as the edit distance leaves the band where "duplicate" still applies. A
// completely different 500-line file never pays for a full diff.
package editdiff

import (
	"slices"
	"strings"
)

// Span is a run of consecutive changed (deleted) lines in the older text, 1-based
// and inclusive.
type Span struct {
	From int
	To   int
}

// Script is the bounded edit script turning `before` into `after`.
type Script struct {
	// Total is the number of lines in `before`.
	Total int
	// Kept is the number of `before` lines that survive into `after` (the LCS length).
	Kept int
	// Changed is the number of `before` lines absent from `after` (Total - Kept).
	Changed int
	// Spans lists the runs of changed lines in `before`, ascending, non-overlapping.
	Spans []Span
}

const (
	// minOverlap is the smallest share of `before` that must survive into `after`
	// for the pair to count as a near-duplicate. Lower and the "duplicate" framing
	// is dishonest — two big outputs sharing a tenth of their lines are unrelated.
	minOverlap = 0.70
	// maxChangedLines caps how many differing lines a marker may describe. Beyond
	// that the edit script itself is no longer a compact summary.
	maxChangedLines = 64
	// maxDiffLines bounds the size of either body a diff is attempted on. Coding
	// tool results above ~1500 lines are handled by the truncation stage instead;
	// bounding keeps the Myers trace small.
	maxDiffLines = 1500
	// maxDExplored bounds the Myers edit-distance search. Together with the length
	// skew pre-filter it keeps the worst case a couple of megabytes of trace rather
	// than the full n*m.
	maxDExplored = 600
)

// Diff returns the bounded edit script turning before into after, or ok=false when
// the texts are not near-duplicates. On ok=false the returned Script must be
// ignored.
func Diff(before, after string) (Script, bool) {
	a := splitLines(before)
	b := splitLines(after)
	n, m := len(a), len(b)
	if n == 0 || m == 0 || n > maxDiffLines || m > maxDiffLines {
		return Script{}, false
	}
	// A near-duplicate cannot be orders of magnitude apart in size: the newer read
	// of a file is the same file. 4x is deliberately loose — the overlap pre-filter
	// and the changed-line cap do the real gating — so a file that grew by a couple
	// of inserted lines is not rejected by size alone.
	if n > 4*m || m > 4*n {
		return Script{}, false
	}
	// Order-insensitive overlap pre-filter. Even if ordering were ignored and every
	// shared line counted, below the bar the LCS certainly is.
	if !plausibleOverlap(a, b) {
		return Script{}, false
	}
	// 2*LCS = n + m - D, and overlap >= minOverlap needs LCS >= minOverlap*n, i.e.
	// D <= n + m - 2*minOverlap*n. That is the band worth searching.
	budget := min(maxDExplored, max(1, int(float64(n)+float64(m)-2*minOverlap*float64(n))))
	trace, d, found := myers(a, b, budget)
	if !found {
		return Script{}, false
	}
	kept := (n + m - d) / 2
	changed := n - kept
	if changed == 0 || changed > maxChangedLines {
		return Script{}, false
	}
	ops := backtrack(trace, a, b, d)
	return Script{Total: n, Kept: kept, Changed: changed, Spans: deletedSpans(ops)}, true
}

func splitLines(text string) []string {
	lines := strings.Split(text, "\n")
	// Tool output virtually always ends with a newline; the trailing empty string
	// is an artifact of the split, not a line.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// plausibleOverlap reports whether the share of `before` lines that also appear in
// `after` (ignoring order and duplicates) meets the bar. It is an upper bound on
// the LCS, so failing it proves the pair is not a near-duplicate without running
// the diff.
func plausibleOverlap(a, b []string) bool {
	distinct := make(map[string]struct{}, len(a))
	for _, line := range a {
		distinct[line] = struct{}{}
	}
	shared := 0
	for _, line := range b {
		if _, ok := distinct[line]; ok {
			shared++
		}
	}
	return float64(shared) >= minOverlap*float64(len(a))
}

// myers runs the classic O((N+M)D) greedy shortest-edit-script search, bounded to
// at most maxD edits. It returns the trace of V-array snapshots and the edit
// distance, or found=false when the bound is exceeded.
//
// trace[d] is the V array after processing distance d; backtrack() consumes it to
// reconstruct the script.
func myers(a, b []string, maxD int) (trace [][]int, d int, found bool) {
	n, m := len(a), len(b)
	offset := maxD + 1
	v := make([]int, 2*offset+2)
	trace = make([][]int, 0, maxD+1)
	for d = 0; d <= maxD; d++ {
		for k := -d; k <= d; k += 2 {
			idx := k + offset
			var x int
			if k == -d || (k != d && v[idx-1] < v[idx+1]) {
				// Move down: insert the corresponding line from b.
				x = v[idx+1]
			} else {
				// Move right: delete a line from a.
				x = v[idx-1] + 1
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
			}
			v[idx] = x
			if x >= n && y >= m {
				return append(trace, append([]int(nil), v...)), d, true
			}
		}
		trace = append(trace, append([]int(nil), v...))
	}
	return trace, 0, false
}

// editOp is one step of the reconstructed script.
type editOp struct {
	insert bool // true: a line is inserted from b; false: a line is deleted from a
	line   int  // 0-based line index in the respective source
}

// backtrack walks the trace from the end to the start, emitting the edit ops in
// reverse and then reversing them into forward order.
func backtrack(trace [][]int, a, b []string, d int) []editOp {
	var offset int
	if len(trace) > 0 {
		offset = (len(trace[0]) - 2) / 2
	}
	n, m := len(a), len(b)
	x, y := n, m
	ops := make([]editOp, 0, d+1)
	for di := d; di >= 0; di-- {
		v := trace[di]
		k := x - y
		idx := k + offset
		var prevK int
		if k == -di || (k != di && v[idx-1] < v[idx+1]) {
			prevK = k + 1
		} else {
			prevK = k - 1
		}
		prevX := v[prevK+offset]
		prevY := prevX - prevK
		for x > prevX && y > prevY {
			// Diagonal: a[x-1] and b[y-1] match.
			x--
			y--
		}
		if x == prevX {
			// Vertical move: b[y-1] was inserted.
			y--
			ops = append(ops, editOp{insert: true, line: y})
		} else {
			// Horizontal move: a[x-1] was deleted.
			x--
			ops = append(ops, editOp{insert: false, line: x})
		}
	}
	slices.Reverse(ops)
	return ops
}

// deletedSpans condenses the deleted a-lines of a script into ascending,
// non-overlapping 1-based spans.
func deletedSpans(ops []editOp) []Span {
	var lines []int
	for _, op := range ops {
		if !op.insert {
			lines = append(lines, op.line)
		}
	}
	if len(lines) == 0 {
		return nil
	}
	slices.Sort(lines)
	spans := make([]Span, 0, 4)
	start, prev := lines[0], lines[0]
	for _, line := range lines[1:] {
		if line == prev+1 {
			prev = line
			continue
		}
		spans = append(spans, Span{From: start + 1, To: prev + 1})
		start, prev = line, line
	}
	spans = append(spans, Span{From: start + 1, To: prev + 1})
	return spans
}

func abs(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

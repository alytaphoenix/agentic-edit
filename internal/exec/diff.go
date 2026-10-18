package exec

import (
	"sort"
	"strings"
)

// diffOp is one rendered diff line.
type diffOp struct {
	kind byte // ' ', '-', '+'
	line string
}

// LineDiff returns a simple line-level diff of old vs new. Unchanged lines are
// prefixed with ' ', removed lines with '-', and added lines with '+'.
//
// The implementation trims the common prefix/suffix first, then uses a
// patience-style anchor pass for larger regions and a bounded LCS table for
// small ones. That keeps a one-line change in a 100k-line file cheap instead of
// allocating an O(n*m) table (the old implementation used 221 MB for a
// 5,000-line file).
func LineDiff(old, new string) string {
	a := splitLines(old)
	b := splitLines(new)
	ops := diffLines(a, b, 0)
	var sb strings.Builder
	for _, op := range ops {
		if op.kind == ' ' {
			sb.WriteString("  ")
		} else {
			sb.WriteByte(op.kind)
		}
		sb.WriteString(op.line)
		sb.WriteByte('\n')
	}
	return sb.String()
}

const (
	// maxDiffCells bounds the LCS table for a middle region.
	maxDiffCells = 1_000_000
	// maxDiffDepth bounds recursive patience anchoring.
	maxDiffDepth = 32
)

func diffLines(a, b []string, depth int) []diffOp {
	// Common prefix.
	start := 0
	for start < len(a) && start < len(b) && a[start] == b[start] {
		start++
	}
	// Common suffix, not crossing the prefix.
	endA, endB := len(a), len(b)
	for endA > start && endB > start && a[endA-1] == b[endB-1] {
		endA--
		endB--
	}

	var ops []diffOp
	for i := 0; i < start; i++ {
		ops = append(ops, diffOp{' ', a[i]})
	}
	ops = append(ops, diffMiddle(a[start:endA], b[start:endB], depth)...)
	for i := endA; i < len(a); i++ {
		ops = append(ops, diffOp{' ', a[i]})
	}
	return ops
}

func diffMiddle(a, b []string, depth int) []diffOp {
	if len(a) == 0 {
		ops := make([]diffOp, 0, len(b))
		for _, line := range b {
			ops = append(ops, diffOp{'+', line})
		}
		return ops
	}
	if len(b) == 0 {
		ops := make([]diffOp, 0, len(a))
		for _, line := range a {
			ops = append(ops, diffOp{'-', line})
		}
		return ops
	}
	if len(a)*len(b) <= maxDiffCells {
		return lcsDiff(a, b)
	}
	if depth < maxDiffDepth {
		if anchors := patienceAnchors(a, b); len(anchors) > 0 {
			var ops []diffOp
			prevA, prevB := 0, 0
			for _, an := range anchors {
				ops = append(ops, diffMiddle(a[prevA:an.a], b[prevB:an.b], depth+1)...)
				ops = append(ops, diffOp{' ', a[an.a]})
				prevA, prevB = an.a+1, an.b+1
			}
			ops = append(ops, diffMiddle(a[prevA:], b[prevB:], depth+1)...)
			return ops
		}
	}
	// No cheap alignment: show the region as a replacement. This is safe and
	// memory-bounded; it is only used for very large, heavily rewritten files.
	ops := make([]diffOp, 0, len(a)+len(b))
	for _, line := range a {
		ops = append(ops, diffOp{'-', line})
	}
	for _, line := range b {
		ops = append(ops, diffOp{'+', line})
	}
	return ops
}

// lcsDiff is the classic O(n*m) LCS, used only for small middle regions.
func lcsDiff(a, b []string) []diffOp {
	n, m := len(a), len(b)
	table := make([][]int, n+1)
	for i := range table {
		table[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				table[i][j] = table[i+1][j+1] + 1
			} else if table[i+1][j] >= table[i][j+1] {
				table[i][j] = table[i+1][j]
			} else {
				table[i][j] = table[i][j+1]
			}
		}
	}
	ops := make([]diffOp, 0, n+m)
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, diffOp{' ', a[i]})
			i++
			j++
		case table[i+1][j] >= table[i][j+1]:
			ops = append(ops, diffOp{'-', a[i]})
			i++
		default:
			ops = append(ops, diffOp{'+', b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, diffOp{'-', a[i]})
	}
	for ; j < m; j++ {
		ops = append(ops, diffOp{'+', b[j]})
	}
	return ops
}

type anchor struct{ a, b int }

// patienceAnchors finds lines that occur exactly once in both sides and whose
// positions form a longest increasing subsequence. Matching those anchors splits
// a large rewrite into smaller regions without an n*m table.
func patienceAnchors(a, b []string) []anchor {
	countA := make(map[string]int, len(a))
	posA := make(map[string]int, len(a))
	for i, line := range a {
		countA[line]++
		posA[line] = i
	}
	countB := make(map[string]int, len(b))
	posB := make(map[string]int, len(b))
	for i, line := range b {
		countB[line]++
		posB[line] = i
	}
	pairs := make([]anchor, 0, len(a))
	for line, n := range countA {
		if n != 1 || countB[line] != 1 {
			continue
		}
		pairs = append(pairs, anchor{a: posA[line], b: posB[line]})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].a < pairs[j].a })
	if len(pairs) == 0 {
		return nil
	}

	// Longest increasing subsequence on the b positions.
	prev := make([]int, len(pairs))
	tails := make([]int, 0, len(pairs))
	for i, p := range pairs {
		prev[i] = -1
		j := sort.Search(len(tails), func(k int) bool { return pairs[tails[k]].b >= p.b })
		if j > 0 {
			prev[i] = tails[j-1]
		}
		if j == len(tails) {
			tails = append(tails, i)
		} else {
			tails[j] = i
		}
	}
	out := make([]anchor, 0, len(tails))
	for i := tails[len(tails)-1]; i >= 0; i = prev[i] {
		out = append(out, pairs[i])
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// splitLines splits on newlines, dropping a single trailing newline so that
// identical files produce an all-context diff (no spurious empty final line).
func splitLines(s string) []string {
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

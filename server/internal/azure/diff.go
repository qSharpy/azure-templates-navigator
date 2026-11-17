package azure

import (
	"fmt"
	"strings"
)

// Line diffs of expanded pipelines (Myers' O(ND) algorithm).

// Op is one line of a diff: ' ' kept, '-' removed, '+' added.
type Op struct {
	Kind byte
	Text string
}

// SplitLines splits text into lines without their newline characters.
func SplitLines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// Diff returns the edit script that turns a into b.
func Diff(a, b []string) []Op {
	// trim the common head and tail: expansions mostly differ in a few places
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	var ops []Op
	for _, l := range a[:pre] {
		ops = append(ops, Op{' ', l})
	}
	ops = append(ops, myers(a[pre:len(a)-suf], b[pre:len(b)-suf])...)
	for _, l := range a[len(a)-suf:] {
		ops = append(ops, Op{' ', l})
	}
	return ops
}

func myers(a, b []string) []Op {
	n, m := len(a), len(b)
	if n == 0 && m == 0 {
		return nil
	}
	max := n + m
	off := max
	v := make([]int, 2*max+2)
	var trace [][]int
	found := false
	for d := 0; d <= max && !found; d++ {
		trace = append(trace, append([]int(nil), v...))
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[off+k-1] < v[off+k+1]) {
				x = v[off+k+1]
			} else {
				x = v[off+k-1] + 1
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
			}
			v[off+k] = x
			if x >= n && y >= m {
				found = true
				break
			}
		}
	}
	// walk back through the trace; trace[d] holds v before step d
	trace = append(trace, v)
	x, y := n, m
	var rev []Op
	for d := len(trace) - 2; d >= 0; d-- {
		vd := trace[d]
		k := x - y
		var pk int
		if k == -d || (k != d && vd[off+k-1] < vd[off+k+1]) {
			pk = k + 1
		} else {
			pk = k - 1
		}
		px := vd[off+pk]
		py := px - pk
		for x > px && y > py {
			x--
			y--
			rev = append(rev, Op{' ', a[x]})
		}
		if d > 0 {
			if x == px {
				y--
				rev = append(rev, Op{'+', b[y]})
			} else {
				x--
				rev = append(rev, Op{'-', a[x]})
			}
		}
	}
	for x > 0 && y > 0 {
		x--
		y--
		rev = append(rev, Op{' ', a[x]})
	}
	out := make([]Op, len(rev))
	for i := range rev {
		out[i] = rev[len(rev)-1-i]
	}
	return out
}

// Count returns the number of added and removed lines.
func Count(ops []Op) (added, removed int) {
	for _, o := range ops {
		switch o.Kind {
		case '+':
			added++
		case '-':
			removed++
		}
	}
	return
}

// Unified renders a diff in unified format with context lines of context.
func Unified(ops []Op, nameA, nameB string, context int) string {
	var sb strings.Builder
	type hunk struct{ start, end int }
	var hunks []hunk
	for i, o := range ops {
		if o.Kind == ' ' {
			continue
		}
		s, e := i-context, i+context+1
		if s < 0 {
			s = 0
		}
		if e > len(ops) {
			e = len(ops)
		}
		if len(hunks) > 0 && s <= hunks[len(hunks)-1].end {
			hunks[len(hunks)-1].end = e
		} else {
			hunks = append(hunks, hunk{s, e})
		}
	}
	if len(hunks) == 0 {
		return ""
	}
	fmt.Fprintf(&sb, "--- %s\n+++ %s\n", nameA, nameB)
	// line numbers before each op
	la, lb := make([]int, len(ops)+1), make([]int, len(ops)+1)
	for i, o := range ops {
		la[i+1], lb[i+1] = la[i], lb[i]
		if o.Kind != '+' {
			la[i+1]++
		}
		if o.Kind != '-' {
			lb[i+1]++
		}
	}
	for _, h := range hunks {
		na, nb := la[h.end]-la[h.start], lb[h.end]-lb[h.start]
		sa, sb2 := la[h.start]+1, lb[h.start]+1
		if na == 0 {
			sa--
		}
		if nb == 0 {
			sb2--
		}
		fmt.Fprintf(&sb, "@@ -%d,%d +%d,%d @@\n", sa, na, sb2, nb)
		for _, o := range ops[h.start:h.end] {
			sb.WriteByte(o.Kind)
			sb.WriteString(o.Text)
			sb.WriteByte('\n')
		}
	}
	return sb.String()
}

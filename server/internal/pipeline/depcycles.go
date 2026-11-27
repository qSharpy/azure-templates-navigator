package pipeline

import (
	"strconv"
	"strings"
)

// DependencyEdge is one dependency of a stage or job that is part of a cycle:
// a name in its `dependsOn:`, or, for a stage without `dependsOn:`, the
// implicit dependency on the stage before it.
type DependencyEdge struct {
	Owner    NamedItem
	On       string // the stage or job depended on, as written
	Implicit bool   // the previous stage, not a dependsOn entry
	// Where to report it: the name in dependsOn, or the owner's name.
	Line, Col, End int
	// Cycle is the loop it closes, owner first and owner last
	// (A → B → A).
	Cycle []string
}

// DependencyCycles returns the dependencies that make stages or jobs depend
// on themselves, directly or through others. Azure rejects such a run when it
// is queued ("Stage B depends on stage C which creates a cycle"). Only the
// items of one list in this file are related: a stage or job under a
// ${{ }} directive, or next to a template call, is left out, because which
// items end up beside it is decided elsewhere.
func DependencyCycles(lines []string) []DependencyEdge {
	mask := BlockScalarMask(lines)
	type node struct {
		item    NamedItem
		scope   int // the line of the list's key (-1 for the top level)
		deps    []DependencyEdge
		hasDeps bool
	}
	var nodes []*node
	byKey := map[string]*node{}
	key := func(scope int, job bool, name string) string {
		k := "s"
		if job {
			k = "j"
		}
		return k + "\x00" + strconv.Itoa(scope) + "\x00" + strings.ToLower(name)
	}
	byLine := map[int]*node{}
	for _, it := range FindNamedItems(lines) {
		if IsExpression(it.Name) || strings.Contains(it.Name, "${{") {
			continue
		}
		scope, ok := listScope(lines, mask, it.Line)
		if !ok {
			continue
		}
		n := &node{item: it, scope: scope}
		k := key(scope, it.IsJob(), it.Name)
		if byKey[k] != nil {
			continue // a duplicate name: reported as duplicate-name
		}
		byKey[k] = n
		byLine[it.Line] = n
		nodes = append(nodes, n)
	}
	for _, d := range FindDependsOn(lines) {
		n := byLine[d.Owner.Line]
		if n == nil {
			continue
		}
		n.hasDeps = true
		for _, nm := range d.Names {
			n.deps = append(n.deps, DependencyEdge{Owner: n.item, On: nm.Name, Line: nm.Line, Col: nm.Col, End: nm.End})
		}
	}
	// A stage without dependsOn depends on the stage before it.
	for _, n := range nodes {
		if n.hasDeps || n.item.IsJob() {
			continue
		}
		if prev, ok := previousEntry(lines, mask, n.item.Line); ok {
			if p := byLine[prev]; p != nil && !p.item.IsJob() && p.scope == n.scope {
				n.deps = append(n.deps, DependencyEdge{Owner: n.item, On: p.item.Name, Implicit: true,
					Line: n.item.Line, Col: n.item.Col, End: n.item.End})
			}
		}
	}
	target := func(n *node, name string) *node { return byKey[key(n.scope, n.item.IsJob(), name)] }
	// path returns the names from a to b following dependencies, or nil.
	path := func(a, b *node) []string {
		prev := map[*node]*node{a: nil}
		queue := []*node{a}
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			if cur == b {
				var names []string
				for x := cur; x != nil; x = prev[x] {
					names = append([]string{x.item.Name}, names...)
				}
				return names
			}
			for _, e := range cur.deps {
				if t := target(cur, e.On); t != nil {
					if _, seen := prev[t]; !seen {
						prev[t] = cur
						queue = append(queue, t)
					}
				}
			}
		}
		return nil
	}
	var out []DependencyEdge
	for _, n := range nodes {
		for _, e := range n.deps {
			t := target(n, e.On)
			if t == nil {
				continue
			}
			p := path(t, n)
			if p == nil {
				continue
			}
			e.Cycle = append([]string{n.item.Name}, p...)
			out = append(out, e)
		}
	}
	return out
}

// listScope returns the line of the key that holds the list the item on line
// is an entry of (-1 at the top level). ok is false when the entry sits
// under a ${{ }} directive.
func listScope(lines []string, mask []bool, line int) (int, bool) {
	ind := Indent(lines[line])
	for j := line - 1; j >= 0; j-- {
		l := lines[j]
		if mask[j] || IsBlank(l) || IsComment(l) {
			continue
		}
		li := Indent(l)
		t := strings.TrimSpace(StripComment(l))
		if li > ind || li == ind && strings.HasPrefix(t, "-") {
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(strings.TrimPrefix(t, "-")), "${{") {
			return 0, false
		}
		if !strings.HasSuffix(t, ":") {
			return 0, false // not a plain key: give up
		}
		return j, true
	}
	return -1, true
}

// previousEntry returns the line of the list entry just before the entry on
// line, in the same list.
func previousEntry(lines []string, mask []bool, line int) (int, bool) {
	ind := Indent(lines[line])
	for j := line - 1; j >= 0; j-- {
		l := lines[j]
		if mask[j] || IsBlank(l) || IsComment(l) {
			continue
		}
		li := Indent(l)
		if li > ind {
			continue
		}
		if li == ind && strings.HasPrefix(strings.TrimSpace(l), "-") {
			return j, true
		}
		return 0, false
	}
	return 0, false
}

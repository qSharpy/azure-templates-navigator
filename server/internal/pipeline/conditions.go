package pipeline

import (
	"regexp"
	"strings"
)

// Branch is one `${{ if }}`, `${{ elseif }}`, `${{ else }}` or `${{ each }}`
// key and the lines under it.
type Branch struct {
	Line    int
	Kind    string // if | elseif | else | each
	Expr    string // the condition (if / elseif)
	BodyEnd int    // last line of the body
	Parent  int    // index of the enclosing branch, or -1
	Prev    int    // for elseif / else: the branch before it in the chain, or -1
}

// Conditions are the template-expression branches of a document.
type Conditions struct {
	Branches []Branch
	inner    []int // per line: the innermost branch whose body holds it, or -1
}

var controlPartsRe = regexp.MustCompile(`^\s*-?\s*\$\{\{\s*(if|elseif|else|each)\b(.*?)\}\}\s*:`)

// FindConditions reads the branches of a document. Lines inside block
// scalars are text, so `${{ if }}:` there is not a branch.
func FindConditions(lines []string) *Conditions {
	c := &Conditions{inner: make([]int, len(lines))}
	for i := range c.inner {
		c.inner[i] = -1
	}
	var mask []bool
	for i, l := range lines {
		if !strings.Contains(l, "${{") {
			continue
		}
		if mask == nil {
			mask = BlockScalarMask(lines)
		}
		if mask[i] {
			continue
		}
		m := controlPartsRe.FindStringSubmatch(StripComment(l))
		if m == nil {
			continue
		}
		b := Branch{Line: i, Kind: m[1], Expr: strings.TrimSpace(m[2]), BodyEnd: i, Parent: c.inner[i], Prev: -1}
		ind, listItem := Indent(l), Indent(l) < KeyColumn(l)
		for j := i + 1; j < len(lines); j++ {
			lj := lines[j]
			if IsBlank(lj) || IsComment(lj) {
				continue
			}
			in := Indent(lj)
			if in > ind || (in == ind && !listItem && strings.HasPrefix(strings.TrimSpace(lj), "-")) {
				b.BodyEnd = j
				continue
			}
			break
		}
		if b.Kind == "elseif" || b.Kind == "else" {
			b.Prev = c.chainBefore(lines, i, b.Parent)
		}
		idx := len(c.Branches)
		c.Branches = append(c.Branches, b)
		for j := i + 1; j <= b.BodyEnd; j++ {
			c.inner[j] = idx
		}
	}
	return c
}

// chainBefore finds the if / elseif that an elseif / else on line follows:
// the previous branch with the same parent and key column whose body ends
// right before line.
func (c *Conditions) chainBefore(lines []string, line, parent int) int {
	col := KeyColumn(lines[line])
	for k := len(c.Branches) - 1; k >= 0; k-- {
		b := c.Branches[k]
		if b.Parent != parent || b.BodyEnd >= line {
			continue
		}
		if KeyColumn(lines[b.Line]) != col || (b.Kind != "if" && b.Kind != "elseif") {
			return -1
		}
		for j := b.BodyEnd + 1; j < line; j++ {
			if !IsBlank(lines[j]) && !IsComment(lines[j]) {
				return -1
			}
		}
		return k
	}
	return -1
}

// Taken reports whether Azure expands branch k with these bindings.
func (c *Conditions) Taken(k int, b Bindings) Tri {
	br := c.Branches[k]
	switch br.Kind {
	case "each":
		return True
	case "if":
		return EvalCondition(br.Expr, b)
	}
	// elseif / else: every earlier condition in the chain is false
	t := True
	for p := br.Prev; p >= 0; p = c.Branches[p].Prev {
		t = t.And(EvalCondition(c.Branches[p].Expr, b).Not())
		if c.Branches[p].Kind == "if" {
			break
		}
	}
	if br.Prev < 0 {
		t = Unknown // an else we couldn't attach to its if
	}
	if br.Kind == "elseif" {
		t = t.And(EvalCondition(br.Expr, b))
	}
	return t
}

// Live reports whether a line is expanded with these bindings: every branch
// that encloses it is taken.
func (c *Conditions) Live(line int, b Bindings) Tri {
	if c == nil || line < 0 || line >= len(c.inner) {
		return True
	}
	t := True
	for k := c.inner[line]; k >= 0 && t != False; k = c.Branches[k].Parent {
		t = t.And(c.Taken(k, b))
	}
	return t
}

// Guard returns the line of the innermost branch around line whose
// condition is not known to be true with these bindings, or -1.
func (c *Conditions) Guard(line int, b Bindings) int {
	if c == nil || line < 0 || line >= len(c.inner) {
		return -1
	}
	for k := c.inner[line]; k >= 0; k = c.Branches[k].Parent {
		if c.Taken(k, b) != True {
			return c.Branches[k].Line
		}
	}
	return -1
}

package pipeline

import (
	"sort"
	"strconv"
	"strings"
)

// A small evaluator for the template expressions that guard ${{ if }}
// branches. It is three-valued: anything it can't decide for certain (a
// variable, a function it doesn't know, a comparison between values of
// different types) is Unknown, so a branch is only ever pruned when Azure
// would certainly skip it.

// Tri is a three-valued truth value.
type Tri int

// Truth values.
const (
	Unknown Tri = iota
	False
	True
)

func triOf(b bool) Tri {
	if b {
		return True
	}
	return False
}

// Not negates a truth value (Unknown stays Unknown).
func (t Tri) Not() Tri {
	switch t {
	case True:
		return False
	case False:
		return True
	}
	return Unknown
}

// And combines two truth values.
func (t Tri) And(u Tri) Tri {
	if t == False || u == False {
		return False
	}
	if t == True && u == True {
		return True
	}
	return Unknown
}

// ValueKind is the type of a known expression value.
type ValueKind int

// Value kinds.
const (
	KindNull ValueKind = iota
	KindBool
	KindNumber
	KindString
)

// Value is a known expression value.
type Value struct {
	Kind ValueKind
	Bool bool
	Num  float64
	Str  string
}

// BoolValue, NumberValue and StringValue build values.
func BoolValue(b bool) Value      { return Value{Kind: KindBool, Bool: b} }
func NumberValue(n float64) Value { return Value{Kind: KindNumber, Num: n} }
func StringValue(s string) Value  { return Value{Kind: KindString, Str: s} }
func (v Value) truthy() bool {
	return v.Kind == KindBool && v.Bool || v.Kind == KindNumber && v.Num != 0 || v.Kind == KindString && v.Str != ""
}
func (v Value) String() string        { return v.key() }
func (v Value) sameKind(w Value) bool { return v.Kind == w.Kind }

func (v Value) key() string {
	switch v.Kind {
	case KindBool:
		return strconv.FormatBool(v.Bool)
	case KindNumber:
		return strconv.FormatFloat(v.Num, 'g', -1, 64)
	case KindString:
		return "'" + v.Str + "'"
	}
	return "null"
}

// ParseLiteral types a YAML scalar as a parameter of the declared type would
// hold it. ok is false when the text is not a plain literal of that type.
func ParseLiteral(typ, raw string) (Value, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || IsExpression(raw) || strings.HasPrefix(raw, "{") || strings.HasPrefix(raw, "[") ||
		strings.HasPrefix(raw, "|") || strings.HasPrefix(raw, ">") || strings.HasPrefix(raw, "&") || strings.HasPrefix(raw, "*") {
		return Value{}, false
	}
	switch strings.ToLower(typ) {
	case "boolean":
		switch strings.ToLower(Unquote(raw)) {
		case "true":
			return BoolValue(true), true
		case "false":
			return BoolValue(false), true
		}
	case "number":
		if n, err := strconv.ParseFloat(Unquote(raw), 64); err == nil {
			return NumberValue(n), true
		}
	case "string":
		return StringValue(Unquote(raw)), true
	}
	return Value{}, false
}

// Bindings are the parameter values known in one context, by lower-case
// name. A parameter that is absent is unknown.
type Bindings map[string]Value

// Key identifies the bindings (for de-duplicating contexts).
func (b Bindings) Key() string {
	if len(b) == 0 {
		return ""
	}
	names := make([]string, 0, len(b))
	for n := range b {
		names = append(names, n)
	}
	sort.Strings(names)
	var sb strings.Builder
	for _, n := range names {
		sb.WriteString(n + "=" + b[n].key() + ";")
	}
	return sb.String()
}

// ForwardedParam returns the parameter a whole expression refers to, when it is
// exactly `${{ parameters.x }}` or `${{ parameters['x'] }}`.
func ForwardedParam(raw string) (string, bool) {
	raw = strings.TrimSpace(Unquote(raw))
	if !strings.HasPrefix(raw, "${{") || !strings.HasSuffix(raw, "}}") {
		return "", false
	}
	n, err := parseExpr(raw[3 : len(raw)-2])
	if err || n.kind != nodePath || len(n.path) != 2 || !strings.EqualFold(n.path[0], "parameters") {
		return "", false
	}
	return n.path[1], true
}

// EvalCondition evaluates the expression of an ${{ if }} (without the
// `${{ if` and `}}`) as Azure decides whether to expand the branch.
func EvalCondition(expr string, b Bindings) Tri {
	n, err := parseExpr(expr)
	if err {
		return Unknown
	}
	v, ok := n.eval(b)
	if !ok {
		return Unknown
	}
	return triOf(v.truthy())
}

// ---------------------------------------------------------------------------
// parsing
// ---------------------------------------------------------------------------

type nodeKind int

const (
	nodeLit nodeKind = iota
	nodePath
	nodeCall
)

type node struct {
	kind nodeKind
	lit  Value
	path []string // parameters.x.y → [parameters x y]; a non-literal index makes the path unknowable ("")
	fn   string
	args []node
}

type exprParser struct {
	s   string
	i   int
	bad bool
}

func parseExpr(s string) (node, bool) {
	p := &exprParser{s: s}
	n := p.expr()
	p.space()
	if p.i != len(p.s) {
		p.bad = true
	}
	return n, p.bad
}

func (p *exprParser) space() {
	for p.i < len(p.s) && (p.s[p.i] == ' ' || p.s[p.i] == '\t' || p.s[p.i] == '\n' || p.s[p.i] == '\r') {
		p.i++
	}
}

func (p *exprParser) peek() byte {
	p.space()
	if p.i < len(p.s) {
		return p.s[p.i]
	}
	return 0
}

func (p *exprParser) eat(c byte) bool {
	if p.peek() == c {
		p.i++
		return true
	}
	return false
}

func isIdentByte(c byte, first bool) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || !first && (c >= '0' && c <= '9' || c == '-')
}

func (p *exprParser) ident() string {
	p.space()
	start := p.i
	for p.i < len(p.s) && isIdentByte(p.s[p.i], p.i == start) {
		p.i++
	}
	return p.s[start:p.i]
}

func (p *exprParser) expr() node {
	if p.bad {
		return node{}
	}
	c := p.peek()
	switch {
	case c == '\'':
		return node{kind: nodeLit, lit: StringValue(p.str())}
	case c == '-' || c >= '0' && c <= '9':
		start := p.i
		p.i++
		for p.i < len(p.s) && (p.s[p.i] >= '0' && p.s[p.i] <= '9' || p.s[p.i] == '.') {
			p.i++
		}
		n, err := strconv.ParseFloat(p.s[start:p.i], 64)
		if err != nil {
			p.bad = true
		}
		return node{kind: nodeLit, lit: NumberValue(n)}
	case isIdentByte(c, true):
		id := p.ident()
		switch strings.ToLower(id) {
		case "true":
			return node{kind: nodeLit, lit: BoolValue(true)}
		case "false":
			return node{kind: nodeLit, lit: BoolValue(false)}
		case "null":
			return node{kind: nodeLit, lit: Value{}}
		}
		if p.eat('(') {
			n := node{kind: nodeCall, fn: strings.ToLower(id)}
			if !p.eat(')') {
				for {
					n.args = append(n.args, p.expr())
					if p.bad || p.eat(')') {
						break
					}
					if !p.eat(',') {
						p.bad = true
						break
					}
				}
			}
			return n
		}
		n := node{kind: nodePath, path: []string{id}}
		for !p.bad {
			switch {
			case p.eat('.'):
				seg := p.ident()
				if seg == "" {
					p.bad = true
				}
				n.path = append(n.path, seg)
			case p.eat('['):
				idx := p.expr()
				if !p.eat(']') {
					p.bad = true
				}
				if idx.kind == nodeLit && idx.lit.Kind == KindString {
					n.path = append(n.path, idx.lit.Str)
				} else {
					n.path = append(n.path, "")
				}
			default:
				return n
			}
		}
		return n
	}
	p.bad = true
	return node{}
}

func (p *exprParser) str() string {
	p.i++ // opening quote
	var sb strings.Builder
	for p.i < len(p.s) {
		c := p.s[p.i]
		p.i++
		if c == '\'' {
			if p.i < len(p.s) && p.s[p.i] == '\'' {
				sb.WriteByte('\'')
				p.i++
				continue
			}
			return sb.String()
		}
		sb.WriteByte(c)
	}
	p.bad = true
	return ""
}

// ---------------------------------------------------------------------------
// evaluation
// ---------------------------------------------------------------------------

func (n node) eval(b Bindings) (Value, bool) {
	switch n.kind {
	case nodeLit:
		return n.lit, true
	case nodePath:
		if len(n.path) < 2 || !strings.EqualFold(n.path[0], "parameters") {
			return Value{}, false
		}
		for _, seg := range n.path[1:] {
			if seg == "" {
				return Value{}, false
			}
		}
		v, ok := b[strings.ToLower(strings.Join(n.path[1:], "."))]
		return v, ok
	}
	switch n.fn {
	case "not":
		if len(n.args) != 1 {
			return Value{}, false
		}
		v, ok := n.args[0].eval(b)
		return BoolValue(!v.truthy()), ok
	case "and", "or":
		if len(n.args) < 2 {
			return Value{}, false
		}
		decided := n.fn == "or" // the value that decides: true for or, false for and
		unknown := false
		for _, a := range n.args {
			v, ok := a.eval(b)
			if !ok {
				unknown = true
				continue
			}
			if v.truthy() == decided {
				return BoolValue(decided), true
			}
		}
		return BoolValue(!decided), !unknown
	case "eq", "ne":
		if len(n.args) != 2 {
			return Value{}, false
		}
		t := equal(n.args[0], n.args[1], b)
		if n.fn == "ne" {
			t = t.Not()
		}
		return BoolValue(t == True), t != Unknown
	case "in", "notin":
		if len(n.args) < 2 {
			return Value{}, false
		}
		found := False
		for _, a := range n.args[1:] {
			switch equal(n.args[0], a, b) {
			case True:
				found = True
			case Unknown:
				if found != True {
					found = Unknown
				}
			}
		}
		if n.fn == "notin" {
			found = found.Not()
		}
		return BoolValue(found == True), found != Unknown
	case "startswith", "endswith", "contains":
		if len(n.args) != 2 {
			return Value{}, false
		}
		x, ok1 := n.args[0].eval(b)
		y, ok2 := n.args[1].eval(b)
		if !ok1 || !ok2 || x.Kind != KindString || y.Kind != KindString {
			return Value{}, false
		}
		s, sub := strings.ToLower(x.Str), strings.ToLower(y.Str)
		switch n.fn {
		case "startswith":
			return BoolValue(strings.HasPrefix(s, sub)), true
		case "endswith":
			return BoolValue(strings.HasSuffix(s, sub)), true
		}
		return BoolValue(strings.Contains(s, sub)), true
	}
	return Value{}, false
}

// equal compares like Azure's eq for values of the same type (strings
// ignore case). Values of different types convert in ways that are easy to
// get wrong, so their comparison is Unknown.
func equal(x, y node, b Bindings) Tri {
	v, ok1 := x.eval(b)
	w, ok2 := y.eval(b)
	if !ok1 || !ok2 || !v.sameKind(w) {
		return Unknown
	}
	switch v.Kind {
	case KindBool:
		return triOf(v.Bool == w.Bool)
	case KindNumber:
		return triOf(v.Num == w.Num)
	case KindString:
		return triOf(strings.EqualFold(v.Str, w.Str))
	}
	return True // null == null
}

// ScalarValue types a scalar inside an object value the way YAML does:
// true / false, null, numbers, and strings (quoted or plain). ok is false
// for anything that is not a plain scalar (expressions, collections).
func ScalarValue(raw string) (Value, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || IsExpression(raw) || strings.ContainsAny(raw[:1], "{[|>&*!") {
		return Value{}, false
	}
	if raw[0] == '\'' || raw[0] == '"' {
		return StringValue(Unquote(raw)), true
	}
	switch raw {
	case "true", "True", "TRUE":
		return BoolValue(true), true
	case "false", "False", "FALSE":
		return BoolValue(false), true
	case "null", "Null", "NULL", "~":
		return Value{}, true
	}
	if n, err := strconv.ParseFloat(raw, 64); err == nil {
		return NumberValue(n), true
	}
	return StringValue(raw), true
}

// ObjectMembers reads the scalar members of a mapping value into bindings
// under prefix ("name.key", nested "name.key.sub"), lower-cased. The value
// is either a flow mapping written at (line, col) or, when col is past the
// end of the line's content, a block mapping under line. Lists and
// expressions are left out (unknown).
func ObjectMembers(lines []string, line, col int, prefix string, into Bindings) {
	if line < 0 || line >= len(lines) {
		return
	}
	clean := StripComment(lines[line])
	if col < len(clean) {
		if clean[col] != '{' {
			return
		}
		fm := ParseFlowMapping(lines, line, col, KeyColumn(lines[line]))
		if !fm.Closed {
			return
		}
		for _, e := range fm.Entries {
			key := prefix + "." + strings.ToLower(e.Key)
			if strings.HasPrefix(e.Value, "{") {
				ObjectMembers(lines, e.ValuePos.Line, e.ValuePos.Col, key, into)
			} else if v, ok := ScalarValue(e.Value); ok {
				into[key] = v
			}
		}
		return
	}
	base := Indent(lines[line])
	child := -1
	for i := line + 1; i < len(lines); i++ {
		l := lines[i]
		if IsBlank(l) || IsComment(l) {
			continue
		}
		ind := Indent(l)
		if ind <= base {
			return
		}
		if child < 0 {
			child = ind
		}
		if ind != child || IsControlLine(l) {
			if ind < child {
				return
			}
			continue // a nested value, read through its key; or a conditional member
		}
		m := passedEntryRe.FindStringSubmatchIndex(StripComment(l))
		if m == nil || strings.HasPrefix(strings.TrimSpace(l), "-") {
			return // a list, not a mapping
		}
		c := StripComment(l)
		key := prefix + "." + strings.ToLower(c[m[4]:m[5]])
		if m[6] < 0 {
			ObjectMembers(lines, i, len(c), key, into)
			continue
		}
		raw := strings.TrimSpace(c[m[6]:m[7]])
		if strings.HasPrefix(raw, "{") {
			ObjectMembers(lines, i, m[6], key, into)
		} else if v, ok := ScalarValue(raw); ok {
			into[key] = v
		}
	}
}

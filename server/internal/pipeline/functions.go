package pipeline

import "strings"

// expressionKeywords open a template directive (`${{ if eq(a, b) }}`,
// `${{ elseif(...) }}`): they are not function calls.
var expressionKeywords = map[string]bool{"if": true, "elseif": true, "else": true, "each": true, "insert": true}

// FuncCall is a function called inside a `${{ }}` expression.
type FuncCall struct {
	Name     string // as written
	Line     int
	Col, End int // byte columns of the name
}

// UnknownFunctions returns the calls to functions Azure doesn't know inside
// `${{ }}` expressions, script bodies included (Azure expands those too).
// Text in quotes is skipped.
func UnknownFunctions(lines []string) []FuncCall {
	var out []FuncCall
	mask := BlockScalarMask(lines)
	for i, l := range lines {
		if !strings.Contains(l, "${{") || strings.HasPrefix(strings.TrimSpace(l), "#") {
			continue
		}
		clean := l
		if !mask[i] {
			clean = StripComment(l)
		}
		for start := 0; ; {
			k := strings.Index(clean[start:], "${{")
			if k < 0 {
				break
			}
			open := start + k + 3
			closeAt := strings.Index(clean[open:], "}}")
			if closeAt < 0 {
				break
			}
			out = append(out, unknownCalls(clean[open:open+closeAt], i, open)...)
			start = open + closeAt + 2
		}
	}
	return out
}

func unknownCalls(expr string, line, base int) []FuncCall {
	var out []FuncCall
	for j := 0; j < len(expr); {
		c := expr[j]
		if c == '\'' { // a string literal; '' is an escaped quote
			j++
			for j < len(expr) {
				if expr[j] == '\'' {
					if j+1 < len(expr) && expr[j+1] == '\'' {
						j += 2
						continue
					}
					break
				}
				j++
			}
			j++
			continue
		}
		if !isIdentByte(c, true) {
			j++
			continue
		}
		s := j
		for j < len(expr) && isIdentByte(expr[j], false) {
			j++
		}
		name := expr[s:j]
		if s > 0 && (expr[s-1] == '.' || isIdentByte(expr[s-1], false)) {
			continue // a member (parameters.x) or the tail of a number
		}
		k := j
		for k < len(expr) && (expr[k] == ' ' || expr[k] == '\t') {
			k++
		}
		if k >= len(expr) || expr[k] != '(' {
			continue
		}
		low := strings.ToLower(name)
		if ExpressionFunctions[low] || expressionKeywords[low] {
			continue
		}
		out = append(out, FuncCall{Name: name, Line: line, Col: base + s, End: base + j})
	}
	return out
}

// FunctionNames are the functions Azure DevOps accepts in compile-time
// expressions (`${{ }}`), as documented (mixed case). Anything else is
// rejected when the run is queued ("Unrecognized value: 'add'"). Checked one
// by one with the preview API.
var FunctionNames = []string{"and", "coalesce", "contains", "containsValue", "convertToJson", "endsWith",
	"eq", "format", "ge", "gt", "in", "iif", "join", "le", "length", "lower", "lt", "ne", "not", "notIn", "or",
	"replace", "split", "startsWith", "trim", "upper", "xor"}

// RuntimeFunctions only exist in run-time expressions (`condition:`,
// `$[ ]`): in `${{ }}` Azure rejects them too.
var RuntimeFunctions = map[string]bool{"counter": true, "always": true, "canceled": true, "failed": true,
	"succeeded": true, "succeededorfailed": true}

// ExpressionFunctions is FunctionNames in lower case.
var ExpressionFunctions = func() map[string]bool {
	m := map[string]bool{}
	for _, n := range FunctionNames {
		m[strings.ToLower(n)] = true
	}
	return m
}()

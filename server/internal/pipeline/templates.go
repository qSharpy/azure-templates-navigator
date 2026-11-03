package pipeline

import (
	"regexp"
	"strings"
)

// TemplateRef is a `template:` reference found in a document.
type TemplateRef struct {
	Ref      string // reference with quotes stripped, e.g. "templates/build.yml@tools"
	Line     int    // 0-based line
	KeyCol   int    // byte column of the "template" key
	ValueCol int    // byte column where the reference value starts
	ValueEnd int    // byte column where the reference value ends
}

// Path returns the path portion of the reference (before '@').
func (t TemplateRef) Path() string {
	p, _ := SplitRef(t.Ref)
	return p
}

// Alias returns the repository alias (after '@'), or "".
func (t TemplateRef) Alias() string {
	_, a := SplitRef(t.Ref)
	return a
}

// IsDynamic reports whether the reference contains an expression.
func (t TemplateRef) IsDynamic() bool { return IsExpression(t.Ref) }

// SplitRef splits "path@alias" into its parts.
func SplitRef(ref string) (path, alias string) {
	ref = strings.TrimSpace(ref)
	if at := strings.LastIndex(ref, "@"); at >= 0 {
		return strings.TrimSpace(ref[:at]), strings.TrimSpace(ref[at+1:])
	}
	return ref, ""
}

// A template key must be an actual YAML key: optionally a list item marker,
// then `template:`. This avoids matching `echo template: x` inside scripts.
var templateLineRe = regexp.MustCompile(`^(\s*(?:-\s+)?)(template)\s*:\s*(.*)$`)

// TemplateRefOnLine returns the template reference on a line, if any.
func TemplateRefOnLine(line string, lineNo int) (TemplateRef, bool) {
	clean := StripComment(line)
	m := templateLineRe.FindStringSubmatchIndex(clean)
	if m == nil {
		return TemplateRef{}, false
	}
	raw := strings.TrimSpace(clean[m[6]:m[7]])
	if raw == "" {
		return TemplateRef{}, false
	}
	valueStart := m[6] + strings.Index(clean[m[6]:], raw)
	return TemplateRef{
		Ref:      Unquote(raw),
		Line:     lineNo,
		KeyCol:   m[4],
		ValueCol: valueStart,
		ValueEnd: valueStart + len(raw),
	}, true
}

// FindTemplateRefs returns every template reference in a document. Lines
// inside block scalars (`script: |` bodies) are text and are skipped.
func FindTemplateRefs(lines []string) []TemplateRef {
	var out []TemplateRef
	var mask []bool
	for i, l := range lines {
		if !strings.Contains(l, "template") {
			continue
		}
		if mask == nil {
			mask = BlockScalarMask(lines)
		}
		if mask[i] {
			continue
		}
		if r, ok := TemplateRefOnLine(l, i); ok {
			out = append(out, r)
		}
	}
	return out
}

var (
	paramsKeyRe    = regexp.MustCompile(`^\s*(?:-\s+)?parameters\s*:\s*$`)
	paramsAnyKeyRe = regexp.MustCompile(`^\s*(?:-\s+)?parameters\s*:(?:\s|$)`)
	keyPrefixRe    = regexp.MustCompile(`^\s*(?:-\s+)?`)
)

// IsParametersKey reports whether the line is a bare `parameters:` key
// (optionally the first key of a list item, `- parameters:`).
func IsParametersKey(line string) bool {
	return paramsKeyRe.MatchString(StripComment(line))
}

// KeyColumn is the byte column where the mapping key on a line starts, i.e.
// after the indentation and an optional `- ` list marker.
func KeyColumn(line string) int {
	return len(keyPrefixRe.FindString(line))
}

// MappingSiblings returns the lines (in source order, including `line`) whose
// keys belong to the same YAML mapping as the key on `line`.
//
// `template:` and `parameters:` are siblings in both call forms:
//
//   - template: x.yml        extends:
//     parameters:              template: x.yml
//     a: 1                   parameters:
//     a: 1
//
// so call-site lookups must go through the mapping rather than through
// "lines indented deeper than the template line".
func MappingSiblings(lines []string, line int) []int {
	if line < 0 || line >= len(lines) {
		return nil
	}
	col := KeyColumn(lines[line])
	var before []int
	// A line indented less than its key column opens a list item (`- k:`);
	// nothing above it belongs to this mapping.
	if Indent(lines[line]) == col {
		for i := line - 1; i >= 0; i-- {
			l := lines[i]
			if IsBlank(l) || IsComment(l) {
				continue
			}
			ind := Indent(l)
			if ind > col {
				continue // body of an earlier sibling
			}
			if ind == col {
				if strings.HasPrefix(strings.TrimSpace(l), "-") {
					break // a compact list item at this column is a different item
				}
				before = append(before, i)
				continue
			}
			if KeyColumn(l) == col {
				before = append(before, i) // `- first: key` of this item
			}
			break
		}
	}
	out := make([]int, 0, len(before)+4)
	for i := len(before) - 1; i >= 0; i-- {
		out = append(out, before[i])
	}
	out = append(out, line)
	for i := line + 1; i < len(lines); i++ {
		l := lines[i]
		if IsBlank(l) || IsComment(l) {
			continue
		}
		ind := Indent(l)
		if ind > col {
			continue
		}
		if ind < col || strings.HasPrefix(strings.TrimSpace(l), "-") {
			break
		}
		out = append(out, i)
	}
	return out
}

// siblingTemplate returns the `template:` key that shares a mapping with the
// key on `line`, or -1.
func siblingTemplate(lines []string, line int) int {
	col := KeyColumn(lines[line])
	for _, i := range MappingSiblings(lines, line) {
		if r, ok := TemplateRefOnLine(lines[i], i); ok && r.KeyCol == col {
			return i
		}
	}
	return -1
}

// CallSiteContext describes where a cursor sits relative to a template call.
type CallSiteContext struct {
	TemplateLine int  // line of the owning `template:` key
	InParameters bool // the cursor is inside that call's `parameters:` block
	ParamsLine   int  // line of the `parameters:` key (when InParameters)
}

// FindCallSite walks upward from cursorLine to find the template call that
// owns it. Allowed intermediate ancestors are the `parameters:` key and
// template-expression control lines (${{ if }}, ${{ each }}...), so the
// cursor can be anywhere inside a parameter block, including conditional
// sections. Any other shallower line means the cursor is outside a call.
func FindCallSite(lines []string, cursorLine int) (CallSiteContext, bool) {
	if cursorLine < 0 || cursorLine >= len(lines) {
		return CallSiteContext{}, false
	}
	minIndent := effectiveIndent(lines, cursorLine)
	ctx := CallSiteContext{TemplateLine: -1, ParamsLine: -1}
	for i := cursorLine - 1; i >= 0; i-- {
		l := lines[i]
		if IsBlank(l) || IsComment(l) {
			continue
		}
		ind := Indent(l)
		if ind >= minIndent {
			continue
		}
		minIndent = ind
		if _, ok := TemplateRefOnLine(l, i); ok {
			ctx.TemplateLine = i
			return ctx, true
		}
		if IsParametersKey(l) {
			if ctx.InParameters {
				// A second parameters: ancestor means we're in a nested
				// structure (e.g. the template's own declaration block).
				return CallSiteContext{}, false
			}
			ctx.InParameters = true
			ctx.ParamsLine = i
			if t := siblingTemplate(lines, i); t >= 0 {
				ctx.TemplateLine = t
				return ctx, true
			}
			continue
		}
		if IsControlLine(l) {
			continue
		}
		return CallSiteContext{}, false
	}
	return CallSiteContext{}, false
}

// FindParametersLine returns the line of the `parameters:` key that is a
// sibling of a template call's `template:` key, or -1. The block may come
// before or after `template:`. The key may open a block or carry an inline
// value (`parameters: { a: 1 }`, `parameters: ${{ parameters }}`).
func FindParametersLine(lines []string, templateLine int) int {
	if templateLine < 0 || templateLine >= len(lines) {
		return -1
	}
	for _, i := range MappingSiblings(lines, templateLine) {
		if i != templateLine && paramsAnyKeyRe.MatchString(StripComment(lines[i])) {
			return i
		}
	}
	return -1
}

// BlockEnd returns the last line (inclusive) of the block that starts at
// `start`, i.e. the last non-blank line indented deeper than `start`.
func BlockEnd(lines []string, start int) int {
	base := Indent(lines[start])
	last := start
	for i := start + 1; i < len(lines); i++ {
		l := lines[i]
		if IsBlank(l) {
			continue
		}
		if Indent(l) <= base {
			break
		}
		last = i
	}
	return last
}

// effectiveIndent is the indent of a line; for a blank line (cursor on an
// empty line) it is inferred from the previous non-blank line: one level
// deeper when that line opens a block ("key:"), otherwise the same level.
func effectiveIndent(lines []string, line int) int {
	cur := lines[line]
	if !IsBlank(cur) || len(cur) > 0 {
		return Indent(cur)
	}
	for i := line - 1; i >= 0; i-- {
		if IsBlank(lines[i]) || IsComment(lines[i]) {
			continue
		}
		ind := Indent(lines[i])
		if strings.HasSuffix(StripComment(lines[i]), ":") {
			return ind + 2
		}
		return ind
	}
	return 0
}

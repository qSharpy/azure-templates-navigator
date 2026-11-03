package pipeline

import (
	"regexp"
	"strings"
)

// PassedParam is a parameter passed at a template call site.
type PassedParam struct {
	Name      string
	Value     string // raw inline value ("" for block values)
	IsBlock   bool   // value is a nested mapping / sequence / block scalar
	Line      int
	NameCol   int
	ValueLine int // line of the value (differs from Line only in multi-line flow mappings)
	ValueCol  int
	ValueEnd  int // end column of the inline value on ValueLine, before any comment

	// Flow is set for entries of a `parameters: { a: 1 }` flow mapping.
	// DelStart/DelEnd is the text to remove to drop the entry, including
	// one adjacent comma.
	Flow     bool
	DelStart Pos
	DelEnd   Pos
}

// CallSite is the parsed `parameters:` block of a template call.
type CallSite struct {
	TemplateLine    int
	ParamsLine      int // -1 when there is no parameters block
	Passed          map[string]PassedParam
	Order           []string // names in source order
	EachPassthrough bool     // `${{ each p in parameters }}:` forwards everything
	Forwarded       bool     // `parameters: ${{ parameters }}`: the whole value is an expression
	HasConditionals bool     // parameters inside ${{ if }} blocks
	LastLine        int      // last line of the parameters block (or template line)

	// Flow is the parsed mapping for `parameters: { a: 1, b: 2 }`.
	Flow *FlowMapping
}

// Unknowable reports whether the passed parameters cannot be determined
// statically (they are forwarded through an expression).
func (cs CallSite) Unknowable() bool { return cs.EachPassthrough || cs.Forwarded }

var (
	passedEntryRe = regexp.MustCompile(`^(\s*)([\p{L}\p{N}_.-]+)\s*:(?:\s+(.*))?$`)
	eachParamsRe  = regexp.MustCompile(`\$\{\{\s*each\s+\w+\s+in\s+parameters\s*\}\}\s*:`)
)

// ParseCallSite parses the parameters passed to the template on templateLine.
//
// Handles:
//   - plain `name: value` entries
//   - object / list / block-scalar values (their nested keys are NOT parameters)
//   - entries nested inside ${{ if }} / ${{ else }} blocks
//   - `${{ each parameter in parameters }}:` pass-through
func ParseCallSite(lines []string, templateLine int) CallSite {
	cs := CallSite{TemplateLine: templateLine, ParamsLine: -1, Passed: map[string]PassedParam{}, LastLine: templateLine}
	if templateLine < 0 || templateLine >= len(lines) {
		return cs
	}
	cs.ParamsLine = FindParametersLine(lines, templateLine)
	if cs.ParamsLine < 0 {
		return cs
	}
	paramsIndent := KeyColumn(lines[cs.ParamsLine])
	cs.LastLine = cs.ParamsLine

	if v, vcol := inlineValue(lines[cs.ParamsLine]); v != "" {
		switch {
		case strings.HasPrefix(v, "{"):
			parseFlowCallSite(&cs, lines, vcol, paramsIndent)
		case IsExpression(v):
			cs.Forwarded = true
		}
		return cs
	}

	skipDeeperThan := -1 // inside the body of a block-valued parameter
	for i := cs.ParamsLine + 1; i < len(lines); i++ {
		raw := lines[i]
		if IsBlank(raw) || IsComment(raw) {
			continue
		}
		ind := Indent(raw)
		if ind <= paramsIndent {
			break
		}
		cs.LastLine = i
		trimmed := strings.TrimSpace(raw)

		if skipDeeperThan >= 0 {
			if ind > skipDeeperThan || (ind == skipDeeperThan && strings.HasPrefix(trimmed, "-")) {
				continue
			}
			skipDeeperThan = -1
		}

		if eachParamsRe.MatchString(raw) {
			cs.EachPassthrough = true
			skipDeeperThan = ind
			continue
		}
		if IsControlLine(raw) {
			cs.HasConditionals = true
			continue
		}

		clean := StripComment(raw)
		m := passedEntryRe.FindStringSubmatchIndex(clean)
		if m == nil {
			continue
		}
		name := clean[m[4]:m[5]]
		value := ""
		valueCol := len(clean)
		if m[6] >= 0 {
			value = strings.TrimSpace(clean[m[6]:m[7]])
			valueCol = m[6]
		}
		isBlock := value == "" || strings.HasPrefix(value, "|") || strings.HasPrefix(value, ">")
		if _, dup := cs.Passed[name]; !dup {
			cs.Passed[name] = PassedParam{Name: name, Value: value, IsBlock: isBlock, Line: i, NameCol: m[4],
				ValueLine: i, ValueCol: valueCol, ValueEnd: len(strings.TrimRight(clean, " \t"))}
			cs.Order = append(cs.Order, name)
		}
		if isBlock {
			skipDeeperThan = ind
		}
	}
	return cs
}

// inlineValue returns the value written after `key:` on a line (comment
// stripped) and its byte column, or "" when the key opens a block.
func inlineValue(line string) (string, int) {
	clean := strings.TrimRight(StripComment(line), " \t")
	i := strings.Index(clean[KeyColumn(clean):], ":")
	if i < 0 {
		return "", len(clean)
	}
	i += KeyColumn(clean) + 1
	for i < len(clean) && (clean[i] == ' ' || clean[i] == '\t') {
		i++
	}
	return clean[i:], i
}

// parseFlowCallSite fills cs from `parameters: { a: 1, b: 2 }`.
func parseFlowCallSite(cs *CallSite, lines []string, openCol, keyCol int) {
	fm := ParseFlowMapping(lines, cs.ParamsLine, openCol, keyCol)
	cs.Flow = &fm
	if fm.Closed {
		cs.LastLine = fm.Close.Line
	} else if n := len(fm.Entries); n > 0 {
		cs.LastLine = fm.Entries[n-1].End.Line
	}
	for k, e := range fm.Entries {
		if _, dup := cs.Passed[e.Key]; dup {
			continue
		}
		pp := PassedParam{Name: e.Key, Value: e.Value, Line: e.KeyPos.Line, NameCol: e.KeyPos.Col,
			ValueLine: e.ValuePos.Line, ValueCol: e.ValuePos.Col, ValueEnd: e.End.Col, Flow: true}
		if e.End.Line != e.ValuePos.Line {
			// a value spanning lines: range the first line only
			pp.ValueEnd = len(strings.TrimRight(StripComment(lines[e.ValuePos.Line]), " \t"))
		}
		switch {
		case k+1 < len(fm.Entries):
			pp.DelStart, pp.DelEnd = e.KeyPos, fm.Entries[k+1].KeyPos
		case k > 0:
			pp.DelStart, pp.DelEnd = fm.Entries[k-1].End, e.End
		default:
			pp.DelStart, pp.DelEnd = e.KeyPos, e.End
		}
		cs.Passed[e.Key] = pp
		cs.Order = append(cs.Order, e.Key)
	}
}

// PassedParamAt finds the parameter passed at a call site under (line, col),
// in either the block form or a `parameters: { ... }` flow mapping. It
// returns the line of the owning `template:` key.
func PassedParamAt(lines []string, line, col int) (templateLine int, pp PassedParam, ok bool) {
	if line < 0 || line >= len(lines) {
		return -1, PassedParam{}, false
	}
	if ctx, found := FindCallSite(lines, line); found && ctx.InParameters {
		cs := ParseCallSite(lines, ctx.TemplateLine)
		for _, n := range cs.Order {
			if p := cs.Passed[n]; p.Line == line && !p.Flow {
				return ctx.TemplateLine, p, true
			}
		}
	}
	// Flow mapping: find the `parameters: {` line at or above the cursor.
	minIndent := Indent(lines[line]) + 1
	for i := line; i >= 0 && line-i < 200; i-- {
		l := lines[i]
		if IsBlank(l) || IsComment(l) {
			continue
		}
		ind := Indent(l)
		if ind >= minIndent && i != line {
			continue
		}
		if ind < minIndent {
			minIndent = ind
		}
		if !paramsAnyKeyRe.MatchString(StripComment(l)) {
			if ind == 0 {
				break
			}
			continue
		}
		if v, _ := inlineValue(l); !strings.HasPrefix(v, "{") {
			break
		}
		t := siblingTemplate(lines, i)
		if t < 0 {
			break
		}
		cs := ParseCallSite(lines, t)
		if cs.Flow == nil || line > cs.LastLine {
			break
		}
		cur := Pos{line, col}
		for _, n := range cs.Order {
			p := cs.Passed[n]
			start, end := Pos{p.Line, p.NameCol}, Pos{p.ValueLine, p.ValueEnd}
			if end.Line == start.Line && end.Col < p.NameCol+len(p.Name) {
				end.Col = p.NameCol + len(p.Name)
			}
			if !cur.Before(start) && !end.Before(cur) {
				return t, p, true
			}
		}
		break
	}
	return -1, PassedParam{}, false
}

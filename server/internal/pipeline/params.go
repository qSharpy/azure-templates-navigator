package pipeline

import (
	"regexp"
	"strings"
)

// Param is a parameter declared in a template's top-level `parameters:` block.
type Param struct {
	Name        string   `json:"name"`
	Type        string   `json:"type"`              // "string" when omitted; "" for legacy map-form (untyped)
	Default     string   `json:"default,omitempty"` // raw default text (may be "" for block defaults)
	HasDefault  bool     `json:"hasDefault"`
	Required    bool     `json:"required"` // no default => required (Azure runtime behaviour)
	DisplayName string   `json:"displayName,omitempty"`
	Values      []string `json:"values,omitempty"` // allowed values, when declared
	Line        int      `json:"line"`             // 0-based line of the name (the `- name:` entry)
	NameCol     int      `json:"nameCol"`          // byte column of the name on that line

	// TypeWritten: the entry has a `type:` (Type is "string" without one).
	TypeWritten bool `json:"-"`

	// EntryLine is where the entry starts: Line, except for an inline entry
	// whose name is written on a later line than its `- {`.
	EntryLine int `json:"-"`

	// Where an inline `default:` value is written (DefaultLine is 0 and the
	// columns are equal when the default is a block or absent).
	DefaultLine, DefaultCol, DefaultEnd int `json:"-"`

	// Inline is true for an entry written as a flow mapping
	// (`- { name: x, type: string, default: 'a' }`), possibly over several
	// lines.
	Inline bool `json:"-"`
}

var (
	topParamsRe  = regexp.MustCompile(`^parameters\s*:\s*(.*)$`)
	nameEntryRe  = regexp.MustCompile(`^(\s*)-\s+name\s*:\s*(.*)$`)
	flowEntryRe  = regexp.MustCompile(`^(\s*)-\s+\{`)
	propRe       = regexp.MustCompile(`^(\s*)(?:-\s+)?([A-Za-z_][\w]*)\s*:\s*(.*)$`)
	listItemRe   = regexp.MustCompile(`^\s*-\s+(.*)$`)
	mapFormKeyRe = regexp.MustCompile(`^(\s*)([A-Za-z_][\w-]*)\s*:\s*(.*)$`)
)

// FindTopLevelParameters returns the line of the top-level `parameters:` key or -1.
func FindTopLevelParameters(lines []string) int {
	for i, l := range lines {
		if topParamsRe.MatchString(StripComment(l)) {
			return i
		}
	}
	return -1
}

// ParseParameters parses the top-level `parameters:` block of a template.
// Both the modern list form (`- name: x`, or inline as
// `- { name: x, type: string }`) and the legacy map form (`x: default`) are
// supported.
func ParseParameters(lines []string) []Param {
	start := FindTopLevelParameters(lines)
	if start < 0 {
		return nil
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		l := lines[i]
		if IsBlank(l) || IsComment(l) {
			continue
		}
		// A column-0 line that is not a list item ends the block.
		if Indent(l) == 0 && !strings.HasPrefix(l, "-") {
			end = i
			break
		}
	}

	// Detect the form and base indent from the first content line.
	baseIndent := -1
	listForm := true
	for i := start + 1; i < end; i++ {
		l := lines[i]
		if IsBlank(l) || IsComment(l) {
			continue
		}
		baseIndent = Indent(l)
		listForm = strings.HasPrefix(strings.TrimSpace(l), "-")
		break
	}
	if baseIndent < 0 {
		return nil
	}
	if !listForm {
		return parseMapFormParams(lines, start+1, end, baseIndent)
	}

	var params []Param
	for i := start + 1; i < end; i++ {
		l := StripComment(lines[i])
		if fm := flowEntryRe.FindStringSubmatch(l); fm != nil && len(fm[1]) == baseIndent {
			if p, ok := parseInlineParam(lines, i, len(fm[0])-1, baseIndent, end); ok {
				params = append(params, p)
			}
			continue
		}
		m := nameEntryRe.FindStringSubmatch(l)
		if m == nil || len(m[1]) != baseIndent {
			continue
		}
		name := Unquote(m[2])
		p := Param{Name: name, Type: "string", Line: i, EntryLine: i, NameCol: strings.Index(lines[i], name)}
		if p.NameCol < 0 {
			p.NameCol = 0
		}
		// Properties live at an indent deeper than the "-".
		propIndent := -1
		for j := i + 1; j < end; j++ {
			sub := lines[j]
			if IsBlank(sub) || IsComment(sub) {
				continue
			}
			ind := Indent(sub)
			if ind <= baseIndent {
				break
			}
			if propIndent < 0 {
				propIndent = ind
			}
			if ind != propIndent {
				continue
			}
			pm := propRe.FindStringSubmatch(StripComment(sub))
			if pm == nil {
				continue
			}
			key, val := pm[2], strings.TrimSpace(pm[3])
			switch key {
			case "type":
				p.Type, p.TypeWritten = Unquote(val), true
			case "default":
				p.HasDefault = true
				p.Default = val
				if val == "" || val == "|" || val == ">" || strings.HasPrefix(val, "|") || strings.HasPrefix(val, ">") {
					p.Default = summariseBlock(lines, j, end)
				} else {
					// val is the trimmed tail of the (comment-stripped) line
					c := len(StripComment(sub)) - len(val)
					p.DefaultLine, p.DefaultCol, p.DefaultEnd = j, c, c+len(val)
				}
			case "displayName":
				p.DisplayName = Unquote(val)
			case "values":
				p.Values = parseValueList(lines, j, end, val)
			}
		}
		p.Required = !p.HasDefault
		params = append(params, p)
	}
	return params
}

// parseInlineParam reads a list entry written as a flow mapping whose `{` is
// at (line, col): `- { name: x, type: string, default: 'a', values: [a, b] }`.
// The mapping may span lines. An entry without a name is not a parameter.
func parseInlineParam(lines []string, line, col, baseIndent, end int) (Param, bool) {
	fm := ParseFlowMapping(lines, line, col, baseIndent)
	p := Param{Type: "string", Inline: true, Line: -1, EntryLine: line}
	for _, e := range fm.Entries {
		if e.ValuePos.Line >= end {
			break
		}
		val := e.Value
		if e.ValuePos.Line != e.End.Line {
			val = strings.Join(strings.Fields(val), " ")
		}
		switch e.Key {
		case "name":
			p.Name = Unquote(val)
			p.Line = e.ValuePos.Line
			p.NameCol = e.ValuePos.Col
			if c := strings.Index(lines[p.Line][e.ValuePos.Col:], p.Name); c >= 0 && p.Name != "" {
				p.NameCol += c
			}
		case "type":
			p.Type, p.TypeWritten = Unquote(val), true
		case "default":
			p.HasDefault = true
			p.Default = val
			if e.End.Col > e.ValuePos.Col || e.End.Line > e.ValuePos.Line {
				p.DefaultLine, p.DefaultCol = e.ValuePos.Line, e.ValuePos.Col
				p.DefaultEnd = e.End.Col
				if e.End.Line != e.ValuePos.Line {
					p.DefaultEnd = len(strings.TrimRight(StripComment(lines[e.ValuePos.Line]), " \t"))
				}
			}
		case "displayName":
			p.DisplayName = Unquote(val)
		case "values":
			p.Values = parseValueList(lines, e.ValuePos.Line, end, val)
		}
	}
	if p.Name == "" || p.Line < 0 {
		return Param{}, false
	}
	p.Required = !p.HasDefault
	return p, true
}

func parseMapFormParams(lines []string, from, end, baseIndent int) []Param {
	var params []Param
	for i := from; i < end; i++ {
		l := StripComment(lines[i])
		m := mapFormKeyRe.FindStringSubmatch(l)
		if m == nil || len(m[1]) != baseIndent {
			continue
		}
		val := strings.TrimSpace(m[3])
		if val == "" {
			val = summariseBlock(lines, i, end)
		}
		params = append(params, Param{
			Name: m[2], Type: "", Default: val, HasDefault: true,
			Line: i, EntryLine: i, NameCol: len(m[1]),
		})
	}
	return params
}

// summariseBlock produces a short one-line description of a block value.
func summariseBlock(lines []string, keyLine, end int) string {
	base := Indent(lines[keyLine])
	var parts []string
	for i := keyLine + 1; i < end && len(parts) < 4; i++ {
		if IsBlank(lines[i]) {
			continue
		}
		if Indent(lines[i]) <= base {
			break
		}
		parts = append(parts, strings.TrimSpace(StripComment(lines[i])))
	}
	if len(parts) == 0 {
		return ""
	}
	s := strings.Join(parts, " ")
	if len(s) > 60 {
		s = s[:57] + "..."
	}
	return s
}

func parseValueList(lines []string, keyLine, end int, inline string) []string {
	var out []string
	if strings.HasPrefix(inline, "[") {
		inner := strings.TrimSuffix(strings.TrimPrefix(inline, "["), "]")
		for _, v := range strings.Split(inner, ",") {
			if v = Unquote(strings.TrimSpace(v)); v != "" {
				out = append(out, v)
			}
		}
		return out
	}
	base := Indent(lines[keyLine])
	for i := keyLine + 1; i < end; i++ {
		l := lines[i]
		if IsBlank(l) || IsComment(l) {
			continue
		}
		ind := Indent(l)
		// list items may sit at the same indent as "values:" (compact style)
		if ind < base || (ind == base && !strings.HasPrefix(strings.TrimSpace(l), "-")) {
			break
		}
		if m := listItemRe.FindStringSubmatch(StripComment(l)); m != nil {
			out = append(out, Unquote(m[1]))
		} else {
			break
		}
	}
	return out
}

// ParamByName returns a declared parameter by name. Azure matches parameter
// names case-insensitively, so a name that differs only in case finds the
// declaration too (an exact match wins).
func ParamByName(params []Param, name string) (Param, bool) {
	for _, p := range params {
		if p.Name == name {
			return p, true
		}
	}
	for _, p := range params {
		if strings.EqualFold(p.Name, name) {
			return p, true
		}
	}
	return Param{}, false
}

var defaultKeyRe = regexp.MustCompile(`^\s*(?:-\s+)?default\s*:`)

// DefaultKeyLine returns the line of a typed parameter's `default:` key, or
// -1 (none, or a legacy map-form parameter). For an inline entry it is the
// line its default value starts on (the value is at DefaultCol there).
func DefaultKeyLine(lines []string, p Param) int {
	if p.Type == "" || p.Line < 0 || p.Line >= len(lines) {
		return -1
	}
	if p.Inline {
		if !p.HasDefault || p.DefaultEnd <= p.DefaultCol {
			return -1
		}
		return p.DefaultLine
	}
	for _, i := range MappingSiblings(lines, p.Line) {
		if defaultKeyRe.MatchString(lines[i]) {
			return i
		}
	}
	return -1
}

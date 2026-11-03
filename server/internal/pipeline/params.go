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
	Line        int      `json:"line"`             // 0-based line of the `- name:` entry
	NameCol     int      `json:"nameCol"`          // byte column of the name on that line
}

var (
	topParamsRe  = regexp.MustCompile(`^parameters\s*:\s*(.*)$`)
	nameEntryRe  = regexp.MustCompile(`^(\s*)-\s+name\s*:\s*(.*)$`)
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
// Both the modern list form (`- name: x`) and the legacy map form
// (`x: default`) are supported.
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
		m := nameEntryRe.FindStringSubmatch(l)
		if m == nil || len(m[1]) != baseIndent {
			continue
		}
		name := Unquote(m[2])
		p := Param{Name: name, Type: "string", Line: i, NameCol: strings.Index(lines[i], name)}
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
				p.Type = Unquote(val)
			case "default":
				p.HasDefault = true
				p.Default = val
				if val == "" || val == "|" || val == ">" || strings.HasPrefix(val, "|") || strings.HasPrefix(val, ">") {
					p.Default = summariseBlock(lines, j, end)
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
			Line: i, NameCol: len(m[1]),
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

// ParamByName returns a declared parameter by name.
func ParamByName(params []Param, name string) (Param, bool) {
	for _, p := range params {
		if p.Name == name {
			return p, true
		}
	}
	return Param{}, false
}

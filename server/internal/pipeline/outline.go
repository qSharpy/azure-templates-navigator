package pipeline

import (
	"regexp"
	"strings"
)

// Symbol is a node of a pipeline outline (LSP document symbols: the
// Structure view in IntelliJ, the Outline view and breadcrumbs in VS Code).
type Symbol struct {
	Name   string
	Detail string
	// Kind: section | extends | template | stage | job | deployment | step |
	// parameter | variable | repository | resource | condition
	Kind     string
	Line     int // first line
	EndLine  int // last line (inclusive)
	SelStart int // byte columns on Line of the identifying text
	SelEnd   int
	Children []*Symbol
}

var (
	outlineItemRe    = regexp.MustCompile(`^(\s*)-\s+([A-Za-z_]\w*)\s*:\s*(.*)$`)
	outlineKeyRe     = regexp.MustCompile(`^(\s*)([A-Za-z_]\w*)\s*:\s*(.*)$`)
	outlineControlRe = regexp.MustCompile(`^(\s*)(?:-\s+)?\$\{\{\s*(.*?)\s*\}\}\s*:\s*$`)

	outlineSections = map[string]bool{
		"parameters": true, "variables": true, "resources": true, "stages": true, "jobs": true,
		"steps": true, "extends": true, "trigger": true, "pr": true, "schedules": true, "pool": true,
	}
	outlineStepKeys = map[string]bool{
		"task": true, "script": true, "bash": true, "pwsh": true, "powershell": true, "checkout": true,
		"download": true, "downloadBuild": true, "publish": true, "getPackage": true, "reviewApp": true,
	}
	outlineResourceKeys = map[string]string{
		"repository": "repository", "pipeline": "resource", "container": "resource",
		"package": "resource", "build": "resource", "webhook": "resource",
	}
)

type outlineFrame struct {
	sym    *Symbol
	indent int
	keyCol int
	isItem bool
	// sameIndent: lines at the node's own indent still belong to it. Used for
	// `template:` under `extends:`, whose `parameters:` is a sibling key.
	sameIndent bool
}

func (fr outlineFrame) contains(line string) bool {
	ind := Indent(line)
	if ind > fr.indent {
		return true
	}
	if ind == fr.indent {
		if fr.sameIndent {
			return true
		}
		// compact sequences: `steps:` followed by `- script:` at column 0
		return !fr.isItem && strings.HasPrefix(strings.TrimSpace(line), "-")
	}
	return false
}

// Outline builds the document outline of a pipeline or template.
func Outline(lines []string) []*Symbol {
	var roots []*Symbol
	var stack []outlineFrame
	attach := func(s *Symbol) {
		if len(stack) == 0 {
			roots = append(roots, s)
		} else {
			top := stack[len(stack)-1].sym
			top.Children = append(top.Children, s)
		}
	}

	for i := 0; i < len(lines); i++ {
		raw := lines[i]
		if IsBlank(raw) || IsComment(raw) {
			continue
		}
		for len(stack) > 0 && !stack[len(stack)-1].contains(raw) {
			stack = stack[:len(stack)-1]
		}
		for _, fr := range stack {
			fr.sym.EndLine = i
		}
		clean := strings.TrimRight(StripComment(raw), " \t")
		ind := Indent(clean)

		// top-level blocks with dedicated child parsing
		if ind == 0 {
			if m := outlineKeyRe.FindStringSubmatch(clean); m != nil && (m[2] == "parameters" || m[2] == "variables") && m[3] == "" {
				end := BlockEnd(lines, i)
				for end+1 < len(lines) && !IsBlank(lines[end+1]) && Indent(lines[end+1]) == 0 && strings.HasPrefix(lines[end+1], "-") {
					end = BlockEnd(lines, end+1)
				}
				sec := &Symbol{Name: m[2], Kind: "section", Line: i, EndLine: end, SelStart: 0, SelEnd: len(m[2])}
				if m[2] == "parameters" {
					sec.Children = paramSymbols(lines, end)
				} else {
					sec.Children = variableSymbols(lines, i, end)
				}
				roots = append(roots, sec)
				stack = stack[:0]
				i = end
				continue
			}
		}

		var sym *Symbol
		fr := outlineFrame{indent: ind, keyCol: KeyColumn(clean)}
		var top *outlineFrame
		if len(stack) > 0 {
			top = &stack[len(stack)-1]
		}

		if m := outlineControlRe.FindStringSubmatchIndex(clean); m != nil {
			expr := clean[m[4]:m[5]]
			sym = &Symbol{Name: expr, Kind: "condition", SelStart: m[4], SelEnd: m[5]}
			fr.isItem = strings.HasPrefix(strings.TrimSpace(clean), "-")
		} else if m := outlineItemRe.FindStringSubmatchIndex(clean); m != nil {
			key, val := clean[m[4]:m[5]], strings.TrimSpace(clean[m[6]:m[7]])
			fr.isItem = true
			vs, ve := m[6], m[6]+len(val)
			if val == "" {
				vs, ve = m[4], m[5]
			}
			switch {
			case key == "stage" || key == "job" || key == "deployment":
				name := Unquote(val)
				if name == "" {
					name = key
				}
				sym = &Symbol{Name: name, Kind: key, SelStart: vs, SelEnd: ve}
			case key == "template":
				sym = &Symbol{Name: Unquote(val), Kind: "template", SelStart: vs, SelEnd: ve}
			case outlineStepKeys[key]:
				sym = &Symbol{Name: stepName(lines, i, key, val), Kind: "step", SelStart: m[4], SelEnd: ve}
			case outlineResourceKeys[key] != "" && inSection(stack, "resources"):
				sym = &Symbol{Name: Unquote(val), Detail: key, Kind: outlineResourceKeys[key], SelStart: vs, SelEnd: ve}
			}
		} else if m := outlineKeyRe.FindStringSubmatchIndex(clean); m != nil {
			key, val := clean[m[4]:m[5]], strings.TrimSpace(clean[m[6]:m[7]])
			switch {
			case ind == 0 && outlineSections[key]:
				kind := "section"
				if key == "extends" {
					kind = "extends"
				}
				sym = &Symbol{Name: key, Detail: val, Kind: kind, SelStart: m[4], SelEnd: m[5]}
			case key == "template" && top != nil && top.sym.Kind == "extends":
				sym = &Symbol{Name: Unquote(val), Kind: "template", SelStart: m[6], SelEnd: m[6] + len(val)}
				fr.sameIndent = true
			case key == "displayName" && top != nil && ind == top.keyCol && val != "":
				dn := Unquote(val)
				switch top.sym.Kind {
				case "step":
					top.sym.Detail, top.sym.Name = top.sym.Name, dn
				case "stage", "job", "deployment":
					top.sym.Detail = dn
				}
			case key == "name" && top != nil && ind == top.keyCol && top.sym.Kind == "repository":
				top.sym.Detail = Unquote(val)
			}
		}
		if sym == nil {
			continue
		}
		sym.Line, sym.EndLine = i, i
		attach(sym)
		fr.sym = sym
		stack = append(stack, fr)
	}
	return roots
}

func inSection(stack []outlineFrame, name string) bool {
	return len(stack) > 0 && stack[0].sym.Kind == "section" && stack[0].sym.Name == name
}

func stepName(lines []string, line int, key, val string) string {
	v := Unquote(val)
	if v == "" || v == "|" || v == ">" || strings.HasPrefix(v, "|") || strings.HasPrefix(v, ">") {
		v = ""
		for j := line + 1; j < len(lines); j++ {
			if IsBlank(lines[j]) {
				continue
			}
			if Indent(lines[j]) > Indent(lines[line]) {
				v = strings.TrimSpace(lines[j])
			}
			break
		}
	}
	if key == "task" {
		return v
	}
	if len(v) > 50 {
		v = v[:47] + "..."
	}
	return key + ": " + v
}

func paramSymbols(lines []string, end int) []*Symbol {
	var out []*Symbol
	for _, p := range ParseParameters(lines) {
		detail := typeOrAny(p.Type)
		if p.Required {
			detail += " · required"
		} else if p.Default != "" {
			d := p.Default
			if len(d) > 30 {
				d = d[:27] + "..."
			}
			detail += " = " + d
		}
		last := BlockEnd(lines, p.Line)
		if last > end {
			last = end
		}
		out = append(out, &Symbol{Name: p.Name, Detail: detail, Kind: "parameter", Line: p.Line, EndLine: last,
			SelStart: p.NameCol, SelEnd: p.NameCol + len(p.Name)})
	}
	return out
}

func typeOrAny(t string) string {
	if t == "" {
		return "any"
	}
	return t
}

var (
	outlineVarRe   = regexp.MustCompile(`^(\s*)-\s+(name|group|template)\s*:\s*(.*)$`)
	outlineValueRe = regexp.MustCompile(`^\s*value\s*:\s*(.*)$`)
)

func variableSymbols(lines []string, start, end int) []*Symbol {
	var out []*Symbol
	childIndent := -1
	for i := start + 1; i <= end; i++ {
		l := StripComment(lines[i])
		if IsBlank(l) || IsComment(lines[i]) {
			continue
		}
		ind := Indent(l)
		if childIndent < 0 {
			childIndent = ind
		}
		if ind != childIndent {
			if len(out) > 0 && out[len(out)-1].Kind == "variable" && out[len(out)-1].Detail == "" {
				if m := outlineValueRe.FindStringSubmatch(l); m != nil {
					out[len(out)-1].Detail = Unquote(strings.TrimSpace(m[1]))
				}
			}
			if len(out) > 0 {
				out[len(out)-1].EndLine = i
			}
			continue
		}
		if m := outlineVarRe.FindStringSubmatchIndex(l); m != nil {
			key, val := l[m[4]:m[5]], strings.TrimSpace(l[m[6]:m[7]])
			s := &Symbol{Name: Unquote(val), Kind: "variable", Line: i, EndLine: i, SelStart: m[6], SelEnd: m[6] + len(val)}
			switch key {
			case "group":
				s.Detail = "variable group"
			case "template":
				s.Kind = "template"
			}
			out = append(out, s)
			continue
		}
		if m := outlineKeyRe.FindStringSubmatchIndex(l); m != nil {
			out = append(out, &Symbol{Name: l[m[4]:m[5]], Detail: Unquote(strings.TrimSpace(l[m[6]:m[7]])), Kind: "variable",
				Line: i, EndLine: i, SelStart: m[4], SelEnd: m[5]})
		}
	}
	return out
}

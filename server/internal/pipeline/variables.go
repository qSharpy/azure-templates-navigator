package pipeline

import (
	"regexp"
	"strings"
)

// Variable is a variable defined in a `variables:` block.
type Variable struct {
	Name  string
	Value string
	Line  int
}

// VariableGroup is a `- group: name` reference.
type VariableGroup struct {
	Name string
	Line int
}

// Variables holds every variable defined anywhere in a document (pipeline,
// stage or job level). When a name is defined several times the first
// definition wins.
type Variables struct {
	Vars   map[string]Variable
	Groups []VariableGroup
	Order  []string
}

var (
	variablesKeyRe = regexp.MustCompile(`^(\s*)(?:-\s+)?variables\s*:\s*$`)
	varMapRe       = regexp.MustCompile(`^(\s*)([A-Za-z_][\w.-]*)\s*:\s*(.*)$`)
	varGroupRe     = regexp.MustCompile(`^\s*-\s+group\s*:\s*(.+)$`)
	varNameRe      = regexp.MustCompile(`^\s*-\s+name\s*:\s*(.+)$`)
	varValueRe     = regexp.MustCompile(`^\s+value\s*:\s*(.*)$`)
)

// ParseVariables parses all `variables:` blocks (map and list form).
func ParseVariables(lines []string) Variables {
	vs := Variables{Vars: map[string]Variable{}}
	add := func(v Variable) {
		if _, ok := vs.Vars[v.Name]; !ok {
			vs.Vars[v.Name] = v
			vs.Order = append(vs.Order, v.Name)
		}
	}
	for i := 0; i < len(lines); i++ {
		m := variablesKeyRe.FindStringSubmatch(StripComment(lines[i]))
		if m == nil {
			continue
		}
		keyIndent := Indent(lines[i])
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "-") {
			keyIndent += 2
		}
		base := -1
		isList := false
		var curName string
		curLine := -1
		j := i + 1
		for ; j < len(lines); j++ {
			l := StripComment(lines[j])
			if IsBlank(l) {
				continue
			}
			ind := Indent(l)
			trimmed := strings.TrimSpace(l)
			if ind < keyIndent || (ind == keyIndent && !strings.HasPrefix(trimmed, "-")) {
				break
			}
			if base < 0 {
				base = ind
				isList = strings.HasPrefix(trimmed, "-")
			}
			if ind < base {
				break
			}
			if !isList {
				if ind == base {
					if mm := varMapRe.FindStringSubmatch(l); mm != nil {
						add(Variable{Name: mm[2], Value: Unquote(mm[3]), Line: j})
					}
				}
				continue
			}
			if ind == base {
				if g := varGroupRe.FindStringSubmatch(l); g != nil {
					vs.Groups = append(vs.Groups, VariableGroup{Name: Unquote(g[1]), Line: j})
					curName = ""
					continue
				}
				if n := varNameRe.FindStringSubmatch(l); n != nil {
					curName, curLine = Unquote(n[1]), j
					continue
				}
				curName = ""
				continue
			}
			if curName != "" {
				if v := varValueRe.FindStringSubmatch(l); v != nil {
					add(Variable{Name: curName, Value: Unquote(v[1]), Line: curLine})
					curName = ""
				}
			}
		}
		i = j - 1
	}
	return vs
}

// VariableRef is a variable usage in a line.
type VariableRef struct {
	Name       string
	Start, End int // byte columns
}

var varUseRes = []*regexp.Regexp{
	regexp.MustCompile(`\$\(([\w.]+)\)`),
	regexp.MustCompile(`\$\{\{\s*variables\.([\w.]+)\s*\}\}`),
	regexp.MustCompile(`\$\{\{\s*variables\[['"]([\w.]+)['"]\]\s*\}\}`),
	regexp.MustCompile(`\$\[\s*variables\.([\w.]+)\s*\]`),
	regexp.MustCompile(`\$\[\s*variables\[['"]([\w.]+)['"]\]\s*\]`),
}

// VariableRefAt returns the variable reference under a byte column.
func VariableRefAt(line string, col int) (VariableRef, bool) {
	for _, re := range varUseRes {
		for _, m := range re.FindAllStringSubmatchIndex(line, -1) {
			if col >= m[0] && col <= m[1] {
				return VariableRef{Name: line[m[2]:m[3]], Start: m[0], End: m[1]}, true
			}
		}
	}
	return VariableRef{}, false
}

// SystemVariablePrefixes are Azure DevOps predefined variable namespaces.
var SystemVariablePrefixes = []string{"Build.", "System.", "Agent.", "Pipeline.", "Environment.", "Release.", "Deployment.", "Strategy.", "Resources.", "Checks.", "TF_BUILD"}

// IsSystemVariable reports whether a name looks like a predefined variable.
func IsSystemVariable(name string) bool {
	for _, p := range SystemVariablePrefixes {
		if strings.HasPrefix(strings.ToLower(name), strings.ToLower(p)) {
			return true
		}
	}
	return false
}

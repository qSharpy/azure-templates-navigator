package pipeline

import (
	"regexp"
	"strings"
)

// PassedParam is a parameter passed at a template call site.
type PassedParam struct {
	Name     string
	Value    string // raw inline value ("" for block values)
	IsBlock  bool   // value is a nested mapping / sequence / block scalar
	Line     int
	NameCol  int
	ValueCol int
}

// CallSite is the parsed `parameters:` block of a template call.
type CallSite struct {
	TemplateLine    int
	ParamsLine      int // -1 when there is no parameters block
	Passed          map[string]PassedParam
	Order           []string // names in source order
	EachPassthrough bool     // `${{ each p in parameters }}:` forwards everything
	HasConditionals bool     // parameters inside ${{ if }} blocks
	LastLine        int      // last line of the parameters block (or template line)
}

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
			cs.Passed[name] = PassedParam{Name: name, Value: value, IsBlock: isBlock, Line: i, NameCol: m[4], ValueCol: valueCol}
			cs.Order = append(cs.Order, name)
		}
		if isBlock {
			skipDeeperThan = ind
		}
	}
	return cs
}

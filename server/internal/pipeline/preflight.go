package pipeline

import (
	"regexp"
	"strings"
)

// Helpers for the "will this pipeline run?" checks: the structure Azure
// validates when a run is queued (names, dependsOn, template kinds,
// repository aliases) and the output-variable wiring that only fails once
// the run is under way. Like the rest of this package they work on lines,
// skip block-scalar bodies (`script: |`), and report byte columns.

// ---------------------------------------------------------------------------
// Tabs
// ---------------------------------------------------------------------------

// TabIndentedLines returns the lines whose indentation contains a tab. YAML
// forbids tabs there, so Azure rejects the file before anything runs. Script
// bodies are text and may contain tabs.
func TabIndentedLines(lines []string) []int {
	var out []int
	var mask []bool
	for i, l := range lines {
		ind := l[:Indent(l)]
		if !strings.Contains(ind, "\t") || IsBlank(l) {
			continue
		}
		if mask == nil {
			mask = BlockScalarMask(lines)
		}
		if !mask[i] {
			out = append(out, i)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Stage / job names
// ---------------------------------------------------------------------------

// NamedItem is a `- stage: X`, `- job: X` or `- deployment: X` list item.
type NamedItem struct {
	Kind     string // stage | job | deployment
	Name     string // unquoted
	Line     int
	Col, End int // byte columns of the name as written (quotes included)
}

// IsJob reports whether the item is a job (regular or deployment).
func (n NamedItem) IsJob() bool { return n.Kind == "job" || n.Kind == "deployment" }

var namedItemRe = regexp.MustCompile(`^(\s*)-\s+(stage|job|deployment)\s*:[ \t]*(.*)$`)

// FindNamedItems returns every stage, job and deployment item outside script
// bodies. Items with an empty name are skipped.
func FindNamedItems(lines []string) []NamedItem {
	mask := BlockScalarMask(lines)
	var out []NamedItem
	for i, l := range lines {
		if mask[i] || !strings.Contains(l, "-") {
			continue
		}
		clean := StripComment(l)
		m := namedItemRe.FindStringSubmatchIndex(clean)
		if m == nil {
			continue
		}
		raw := strings.TrimSpace(clean[m[6]:m[7]])
		name := Unquote(raw)
		if name == "" {
			continue
		}
		out = append(out, NamedItem{Kind: clean[m[4]:m[5]], Name: name, Line: i, Col: m[6], End: m[6] + len(raw)})
	}
	return out
}

var (
	identRe       = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	compileExprRe = regexp.MustCompile(`\$\{\{.*?\}\}`)
	anyExprRe     = regexp.MustCompile(`\$\{\{.*?\}\}|\$\(.*?\)|\$\[.*?\]`)
)

// ValidName reports whether a stage or job name is accepted by Azure: only
// letters, digits and '_', not starting with a digit. `${{ }}` parts are
// unknown at edit time and are assumed to produce valid text. Names that use
// runtime syntax ($(), $[]) are not checked.
func ValidName(name string) bool {
	if strings.Contains(name, "$(") || strings.Contains(name, "$[") {
		return true
	}
	return identRe.MatchString(compileExprRe.ReplaceAllString(name, "X"))
}

// NameMatcher matches names that may contain `${{ }}` expressions: each
// expression matches any text. Matching is case-insensitive, like Azure's
// stage, job and step names.
type NameMatcher struct {
	exact   map[string]bool
	pattern []*regexp.Regexp
	literal []string // literal names, for "did you mean"
}

// NewNameMatcher returns an empty matcher.
func NewNameMatcher() *NameMatcher { return &NameMatcher{exact: map[string]bool{}} }

// Add registers a name.
func (m *NameMatcher) Add(name string) {
	if !strings.Contains(name, "${{") && !strings.Contains(name, "$(") && !strings.Contains(name, "$[") {
		k := strings.ToLower(name)
		if !m.exact[k] {
			m.exact[k] = true
			m.literal = append(m.literal, name)
		}
		return
	}
	var b strings.Builder
	b.WriteString("(?i)^")
	rest := name
	for {
		loc := anyExprRe.FindStringIndex(rest)
		if loc == nil {
			b.WriteString(regexp.QuoteMeta(rest))
			break
		}
		b.WriteString(regexp.QuoteMeta(rest[:loc[0]]))
		b.WriteString(".*")
		rest = rest[loc[1]:]
	}
	b.WriteString("$")
	if re, err := regexp.Compile(b.String()); err == nil {
		m.pattern = append(m.pattern, re)
	}
}

// Has reports whether name matches a registered name.
func (m *NameMatcher) Has(name string) bool {
	if m.exact[strings.ToLower(name)] {
		return true
	}
	for _, re := range m.pattern {
		if re.MatchString(name) {
			return true
		}
	}
	return false
}

// Literals returns the registered names without expressions, in order.
func (m *NameMatcher) Literals() []string { return m.literal }

// ---------------------------------------------------------------------------
// dependsOn
// ---------------------------------------------------------------------------

// NameLoc is one name written in source.
type NameLoc struct {
	Name     string
	Line     int
	Col, End int
}

// DependsOn is the `dependsOn:` of a stage or job.
type DependsOn struct {
	Owner NamedItem // the stage or job that declares it
	Line  int       // line of the dependsOn key
	Names []NameLoc // literal names (expressions are skipped)
}

var (
	dependsOnRe = regexp.MustCompile(`^(\s*)dependsOn\s*:[ \t]*(.*)$`)
	listEntryRe = regexp.MustCompile(`^(\s*)-[ \t]+(.*)$`)
)

// FindDependsOn returns the `dependsOn:` keys that belong to a stage or a
// job. A `dependsOn:` passed as a template parameter is not one of them: what
// it names depends on how the template uses it.
func FindDependsOn(lines []string) []DependsOn {
	mask := BlockScalarMask(lines)
	var out []DependsOn
	for i, l := range lines {
		if mask[i] || !strings.Contains(l, "dependsOn") {
			continue
		}
		clean := StripComment(l)
		m := dependsOnRe.FindStringSubmatchIndex(clean)
		if m == nil {
			continue
		}
		owner, ok := ownerItem(lines, i)
		if !ok {
			continue
		}
		d := DependsOn{Owner: owner, Line: i}
		val := strings.TrimSpace(clean[m[4]:m[5]])
		valCol := m[4] + strings.Index(clean[m[4]:], val)
		switch {
		case val == "":
			d.Names = blockListNames(lines, mask, i)
		case strings.HasPrefix(val, "["):
			d.Names = flowListNames(clean, valCol, i)
		default:
			if n, ok := nameLoc(val, i, valCol); ok {
				d.Names = []NameLoc{n}
			}
		}
		out = append(out, d)
	}
	return out
}

// ownerItem returns the stage/job item whose mapping contains the key on line.
func ownerItem(lines []string, line int) (NamedItem, bool) {
	col := KeyColumn(lines[line])
	for _, j := range MappingSiblings(lines, line) {
		if KeyColumn(lines[j]) != col {
			continue
		}
		clean := StripComment(lines[j])
		if m := namedItemRe.FindStringSubmatchIndex(clean); m != nil {
			raw := strings.TrimSpace(clean[m[6]:m[7]])
			return NamedItem{Kind: clean[m[4]:m[5]], Name: Unquote(raw), Line: j, Col: m[6], End: m[6] + len(raw)}, true
		}
	}
	return NamedItem{}, false
}

func nameLoc(raw string, line, col int) (NameLoc, bool) {
	raw = strings.TrimSpace(raw)
	name := Unquote(raw)
	if name == "" || IsExpression(name) {
		return NameLoc{}, false
	}
	return NameLoc{Name: name, Line: line, Col: col, End: col + len(raw)}, true
}

func flowListNames(clean string, open, line int) []NameLoc {
	closeAt := strings.Index(clean[open:], "]")
	if closeAt < 0 {
		return nil // multi-line flow list: not worth guessing
	}
	inner := clean[open+1 : open+closeAt]
	var out []NameLoc
	pos := open + 1
	for _, part := range strings.Split(inner, ",") {
		lead := len(part) - len(strings.TrimLeft(part, " \t"))
		if n, ok := nameLoc(part, line, pos+lead); ok {
			out = append(out, n)
		}
		pos += len(part) + 1
	}
	return out
}

func blockListNames(lines []string, mask []bool, keyLine int) []NameLoc {
	base := Indent(lines[keyLine])
	var out []NameLoc
	for j := keyLine + 1; j < len(lines); j++ {
		l := lines[j]
		if IsBlank(l) || IsComment(l) {
			continue
		}
		ind := Indent(l)
		if ind < base || (ind == base && !strings.HasPrefix(strings.TrimSpace(l), "-")) || mask[j] {
			break
		}
		clean := StripComment(l)
		m := listEntryRe.FindStringSubmatchIndex(clean)
		if m == nil {
			break
		}
		raw := strings.TrimSpace(clean[m[4]:m[5]])
		if n, ok := nameLoc(raw, j, m[4]); ok {
			out = append(out, n)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Duplicate names
// ---------------------------------------------------------------------------

// DuplicateNames returns the items that repeat an earlier sibling's name:
// two stages in the same `stages:` list, or two jobs in the same `jobs:`
// list. Items under `${{ if }}` / `${{ each }}` are not compared, because
// only one branch may be expanded.
func DuplicateNames(lines []string) []NamedItem {
	var out []NamedItem
	var walk func(syms []*Symbol)
	walk = func(syms []*Symbol) {
		seen := map[string]bool{}
		for _, s := range syms {
			switch s.Kind {
			case "stage", "job", "deployment":
				if IsExpression(s.Name) {
					break
				}
				key := s.Kind
				if key == "deployment" {
					key = "job"
				}
				key += "\x00" + strings.ToLower(s.Name)
				if seen[key] {
					out = append(out, NamedItem{Kind: s.Kind, Name: s.Name, Line: s.Line, Col: s.SelStart, End: s.SelEnd})
				}
				seen[key] = true
			}
			if s.Kind != "condition" {
				walk(s.Children)
			}
		}
	}
	walk(Outline(lines))
	return out
}

// ---------------------------------------------------------------------------
// Template kinds
// ---------------------------------------------------------------------------

// templateKinds are the root keys that decide where a template can be used.
var templateKinds = []string{"stages", "jobs", "steps", "variables", "extends"}

// TemplateKind returns what a template file provides: "stages", "jobs",
// "steps", "variables" or "extends"; "" when it has none of those root keys
// or more than one.
func TemplateKind(lines []string) string {
	keys := TopLevelKeys(lines)
	kind := ""
	for _, k := range templateKinds {
		if _, ok := keys[k]; ok {
			if kind != "" {
				return ""
			}
			kind = k
		}
	}
	return kind
}

var (
	listKeyRe     = regexp.MustCompile(`^\s*(?:-\s+)?([A-Za-z_]\w*)\s*:\s*$`)
	itemControlRe = regexp.MustCompile(`^\s*(?:-\s+)?\$\{\{.*\}\}\s*:\s*$`)
)

// TemplateContext returns the list a `- template:` item sits in: "stages",
// "jobs", "steps" or "variables". Conditional blocks (`${{ if }}:`) between
// the item and the list key are looked through. It returns "" when the
// reference is not a list item or the list is something else (for example a
// `stepList` parameter with another name).
func TemplateContext(lines []string, refLine int) string {
	if refLine < 0 || refLine >= len(lines) {
		return ""
	}
	l := lines[refLine]
	if !strings.HasPrefix(strings.TrimSpace(l), "-") {
		return ""
	}
	ind := Indent(l)
	for i := refLine - 1; i >= 0; i-- {
		p := lines[i]
		if IsBlank(p) || IsComment(p) {
			continue
		}
		pi := Indent(p)
		isItem := strings.HasPrefix(strings.TrimSpace(p), "-")
		if pi > ind || (pi == ind && isItem) {
			continue // body of an earlier item, or a sibling item
		}
		clean := StripComment(p)
		if itemControlRe.MatchString(clean) {
			// `${{ if }}:` / `- ${{ each }}:` wrap part of the list: keep
			// climbing from the control line.
			ind = pi
			continue
		}
		if m := listKeyRe.FindStringSubmatch(clean); m != nil {
			switch m[1] {
			case "stages", "jobs", "steps", "variables":
				return m[1]
			}
		}
		return ""
	}
	return ""
}

// ---------------------------------------------------------------------------
// Repository checkouts
// ---------------------------------------------------------------------------

var checkoutRe = regexp.MustCompile(`^(\s*)-\s+checkout\s*:[ \t]*(.*)$`)

// FindCheckouts returns `- checkout: <alias>` steps that name a repository
// resource (not self, none, an inline git:// reference or an expression).
func FindCheckouts(lines []string) []NameLoc {
	mask := BlockScalarMask(lines)
	var out []NameLoc
	for i, l := range lines {
		if mask[i] || !strings.Contains(l, "checkout") {
			continue
		}
		clean := StripComment(l)
		m := checkoutRe.FindStringSubmatchIndex(clean)
		if m == nil {
			continue
		}
		raw := strings.TrimSpace(clean[m[4]:m[5]])
		name := Unquote(raw)
		ln := strings.ToLower(name)
		if name == "" || ln == "self" || ln == "none" || strings.Contains(name, "://") || IsExpression(name) {
			continue
		}
		out = append(out, NameLoc{Name: name, Line: i, Col: m[4], End: m[4] + len(raw)})
	}
	return out
}

// ---------------------------------------------------------------------------
// Output variables
// ---------------------------------------------------------------------------

// OutputRef is `outputs['step.var']` (with any job / stage prefix:
// `dependencies.Job.outputs['step.var']`,
// `stageDependencies.Stage.Job.outputs['step.var']`,
// `dependencies.Stage.outputs['Job.step.var']`). Step and Var are the last
// two segments, which name the step that sets the variable and the variable.
type OutputRef struct {
	Step, Var string
	Line      int
	Col, End  int // the quoted 'step.var' text
}

var outputsRe = regexp.MustCompile(`\boutputs\[\s*(['"])([^'"]+)['"]\s*\]`)

// FindOutputRefs returns output-variable references outside script bodies.
func FindOutputRefs(lines []string) []OutputRef {
	var mask []bool
	var out []OutputRef
	for i, l := range lines {
		if !strings.Contains(l, "outputs[") {
			continue
		}
		if mask == nil {
			mask = BlockScalarMask(lines)
		}
		if mask[i] || IsComment(l) {
			continue
		}
		clean := StripComment(l)
		for _, m := range outputsRe.FindAllStringSubmatchIndex(clean, -1) {
			ref := clean[m[4]:m[5]]
			if IsExpression(ref) || strings.Contains(ref, "{{") {
				continue
			}
			segs := strings.Split(ref, ".")
			if len(segs) < 2 {
				continue
			}
			out = append(out, OutputRef{Step: segs[len(segs)-2], Var: segs[len(segs)-1], Line: i, Col: m[2], End: m[5] + 1})
		}
	}
	return out
}

// StepInfo is a step that has a `name:`, which is what output variables are
// addressed by.
type StepInfo struct {
	Name string
	Kind string // script | bash | pwsh | powershell | task | ...
	Line int    // line of the step item
	// Variables the step's script sets with ##vso[task.setvariable]:
	// variable name (lower case) -> whether it was set with isOutput=true.
	Sets map[string]bool
}

var (
	stepItemRe    = regexp.MustCompile(`^(\s*)-\s+(script|bash|pwsh|powershell|task|checkout|download|downloadBuild|publish|getPackage|reviewApp)\s*:`)
	stepNameRe    = regexp.MustCompile(`^\s*name\s*:[ \t]*(.*)$`)
	setVariableRe = regexp.MustCompile(`(?i)##vso\[task\.setvariable\s+([^\]]*)\]`)
)

// FindNamedSteps returns the steps that have a `name:`, with the variables
// their inline scripts set.
func FindNamedSteps(lines []string) []StepInfo {
	mask := BlockScalarMask(lines)
	var out []StepInfo
	for i, l := range lines {
		if mask[i] {
			continue
		}
		clean := StripComment(l)
		m := stepItemRe.FindStringSubmatch(clean)
		if m == nil {
			continue
		}
		var name string
		for _, j := range MappingSiblings(lines, i) {
			if nm := stepNameRe.FindStringSubmatch(StripComment(lines[j])); nm != nil && j != i {
				name = Unquote(strings.TrimSpace(nm[1]))
			}
		}
		if name == "" {
			continue
		}
		last := BlockEnd(lines, i) // the item's keys and its script body
		s := StepInfo{Name: name, Kind: m[2], Line: i, Sets: map[string]bool{}}
		for j := i; j <= last && j < len(lines); j++ {
			for _, sv := range setVariableRe.FindAllStringSubmatch(lines[j], -1) {
				v, isOut := parseSetVariable(sv[1])
				if v == "" || strings.ContainsAny(v, "$%{") {
					continue
				}
				k := strings.ToLower(v)
				s.Sets[k] = s.Sets[k] || isOut
			}
		}
		out = append(out, s)
	}
	return out
}

// parseSetVariable reads `variable=x;isOutput=true` (any order, any case).
func parseSetVariable(props string) (name string, isOutput bool) {
	for _, kv := range strings.Split(props, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(kv), "=")
		if !ok {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "variable":
			name = strings.TrimSpace(v)
		case "isoutput":
			isOutput = strings.EqualFold(strings.TrimSpace(v), "true")
		}
	}
	return name, isOutput
}

// ---------------------------------------------------------------------------
// Triggers
// ---------------------------------------------------------------------------

var triggerNoneRe = regexp.MustCompile(`^trigger\s*:\s*['"]?none['"]?\s*$`)

// RunsAutomatically reports whether a pipeline can start without anyone
// filling in the "Run pipeline" form: a CI trigger (on by default when
// `trigger:` is absent) or a schedule.
func RunsAutomatically(lines []string) bool {
	keys := TopLevelKeys(lines)
	if _, ok := keys["schedules"]; ok {
		return true
	}
	t, ok := keys["trigger"]
	if !ok {
		return true
	}
	return !triggerNoneRe.MatchString(StripComment(lines[t]))
}

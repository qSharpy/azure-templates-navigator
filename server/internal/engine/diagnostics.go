package engine

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/qSharpy/azure-templates-navigator/server/internal/pipeline"
	"github.com/qSharpy/azure-templates-navigator/server/internal/repos"
)

// Severity values match LSP DiagnosticSeverity.
const (
	SevError   = 1
	SevWarning = 2
	SevInfo    = 3
	SevHint    = 4
)

// Diagnostic codes.
const (
	CodeMissingRequired = "missing-required-param"
	CodeUnknownParam    = "unknown-param"
	CodeTypeMismatch    = "type-mismatch"
	CodeValueNotAllowed = "value-not-allowed"
	CodeUnusedParam     = "unused-param"
	CodeTemplateMissing = "template-not-found"
	CodeUnknownAlias    = "unknown-repository-alias"
	CodeRepoUnavailable = "repository-unavailable"
	CodeRepoFetching    = "repository-fetching"
	CodeUndeclaredParam = "undeclared-param"
)

// Source is the diagnostic source shown in IDEs.
const Source = "Azure Templates Navigator"

// Diagnostic is an IDE-independent diagnostic. Columns are byte offsets.
type Diagnostic struct {
	Line     int    `json:"line"`
	StartCol int    `json:"startCol"`
	EndLine  int    `json:"endLine"`
	EndCol   int    `json:"endCol"`
	Severity int    `json:"severity"`
	Code     string `json:"code"`
	Message  string `json:"message"`

	// fix metadata (not serialised to clients directly)
	templateLine int
	param        string
	paramType    string
	alias        string
	values       []string
	suggestions  []string
}

// Diagnose returns diagnostics for one file.
func (e *Engine) Diagnose(path string) []Diagnostic {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ensureLinked()
	return e.diagnose(filepath.Clean(path))
}

// DiagnoseAll returns diagnostics for every workspace file (files without
// problems are included with an empty slice).
func (e *Engine) DiagnoseAll() map[string][]Diagnostic {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ensureLinked()
	out := map[string][]Diagnostic{}
	for _, p := range e.workspaceFiles() {
		out[p] = e.diagnose(p)
	}
	return out
}

func (e *Engine) diagnose(path string) []Diagnostic {
	if !e.settings.Diagnostics.Enabled {
		return nil
	}
	if d, ok := e.diagCache[path]; ok && e.linked {
		return d
	}
	d := e.computeDiagnostics(path)
	if e.diagCache != nil {
		e.diagCache[path] = d
	}
	return d
}

func (e *Engine) computeDiagnostics(path string) []Diagnostic {
	f := e.file(path)
	if f == nil {
		return nil
	}
	var out []Diagnostic
	for _, r := range f.refs {
		out = append(out, e.checkCallSite(f, r)...)
	}
	if e.settings.Diagnostics.UnusedParameters && len(f.params) > 0 {
		out = append(out, checkUnused(f)...)
	}
	out = append(out, checkUndeclared(f)...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Line != out[j].Line {
			return out[i].Line < out[j].Line
		}
		return out[i].StartCol < out[j].StartCol
	})
	return out
}

func refRange(r pipeline.TemplateRef) (int, int) { return r.ValueCol, r.ValueEnd }

func (e *Engine) checkCallSite(f *fileInfo, r pipeline.TemplateRef) []Diagnostic {
	res := e.resolve(r, f)
	vs, ve := refRange(r)
	mk := func(sev int, code, msg string) Diagnostic {
		return Diagnostic{Line: r.Line, StartCol: vs, EndLine: r.Line, EndCol: ve, Severity: sev, Code: code, Message: msg, templateLine: r.Line, alias: res.Alias}
	}
	switch res.Kind {
	case KindDynamic:
		return nil
	case KindUnknownAlias:
		sev := SevError
		if !f.isPipeline {
			// templates get aliases from whichever pipeline consumes them
			sev = SevWarning
		}
		return []Diagnostic{mk(sev, CodeUnknownAlias, fmt.Sprintf("Repository alias '@%s' is not declared in resources.repositories", res.Alias))}
	case KindRepoUnavailable:
		loc := res.RepoLoc
		name := res.Repo.Name
		switch loc.State {
		case repos.StateFetching:
			return []Diagnostic{mk(SevInfo, CodeRepoFetching, fmt.Sprintf("Fetching repository '%s' (%s)…", name, refLabel(res.Repo.Ref)))}
		case repos.StateFailed:
			return []Diagnostic{mk(SevWarning, CodeRepoUnavailable, fmt.Sprintf("Could not fetch repository '%s': %s", name, loc.Error))}
		case repos.StateUnconfigured:
			return []Diagnostic{mk(SevWarning, CodeRepoUnavailable, fmt.Sprintf("Repository '%s' is not available locally: %s", name, loc.Error))}
		default:
			return []Diagnostic{mk(SevWarning, CodeRepoUnavailable, fmt.Sprintf("Repository '%s' (@%s) is not available locally — use the quick fix to fetch it", name, res.Alias))}
		}
	}
	if !res.Exists {
		msg := fmt.Sprintf("Template not found: %s", e.relPath(res.Path))
		if res.Kind == KindExternal {
			msg = fmt.Sprintf("Template '%s' not found in repository '%s' (%s)", r.Path(), res.Repo.Name, refLabel(res.Repo.Ref))
		}
		return []Diagnostic{mk(SevError, CodeTemplateMissing, msg)}
	}

	tf := e.file(res.Path)
	if tf == nil || len(tf.params) == 0 {
		return nil
	}
	cs := pipeline.ParseCallSite(f.lines, r.Line)
	if cs.EachPassthrough {
		return nil
	}
	var out []Diagnostic
	tref := strings.TrimSpace(r.Ref)

	for _, p := range tf.params {
		if !p.Required {
			continue
		}
		if _, ok := cs.Passed[p.Name]; ok {
			continue
		}
		d := mk(SevError, CodeMissingRequired, fmt.Sprintf("Missing required parameter '%s' (type: %s) for template '%s'", p.Name, typeLabel(p.Type), tref))
		d.StartCol = r.KeyCol
		d.param, d.paramType = p.Name, p.Type
		out = append(out, d)
	}

	for _, name := range cs.Order {
		pp := cs.Passed[name]
		decl, ok := pipeline.ParamByName(tf.params, name)
		if !ok {
			d := Diagnostic{Line: pp.Line, StartCol: pp.NameCol, EndLine: pp.Line, EndCol: pp.NameCol + len(name),
				Severity: SevWarning, Code: CodeUnknownParam, templateLine: r.Line, param: name,
				Message: fmt.Sprintf("Unknown parameter '%s' — not declared in template '%s'", name, tref)}
			d.suggestions = suggest(name, tf.params, cs)
			if len(d.suggestions) > 0 {
				d.Message += fmt.Sprintf(". Did you mean '%s'?", d.suggestions[0])
			}
			out = append(out, d)
			continue
		}
		if pp.IsBlock || pp.Value == "" || pipeline.IsExpression(pp.Value) {
			continue
		}
		lineLen := len(strings.TrimRight(f.lines[pp.Line], " \t"))
		if decl.Type != "" {
			if inferred, ok := pipeline.TypeCompatible(decl.Type, pp.Value); !ok {
				out = append(out, Diagnostic{Line: pp.Line, StartCol: pp.ValueCol, EndLine: pp.Line, EndCol: lineLen,
					Severity: SevWarning, Code: CodeTypeMismatch, templateLine: r.Line, param: name, paramType: decl.Type,
					Message: fmt.Sprintf("Type mismatch for parameter '%s': template expects '%s', got value '%s' (inferred as '%s')", name, decl.Type, pp.Value, inferred)})
				continue
			}
		}
		if len(decl.Values) > 0 {
			v := pipeline.Unquote(pp.Value)
			allowed := false
			for _, a := range decl.Values {
				if strings.EqualFold(a, v) {
					allowed = true
					break
				}
			}
			if !allowed {
				out = append(out, Diagnostic{Line: pp.Line, StartCol: pp.ValueCol, EndLine: pp.Line, EndCol: lineLen,
					Severity: SevError, Code: CodeValueNotAllowed, templateLine: r.Line, param: name, values: decl.Values,
					Message: fmt.Sprintf("Value '%s' is not allowed for parameter '%s'. Allowed: %s", v, name, strings.Join(decl.Values, ", "))})
			}
		}
	}
	return out
}

func checkUnused(f *fileInfo) []Diagnostic {
	usage := pipeline.CollectParamUsage(f.lines)
	if usage.UsesAll {
		return nil
	}
	var out []Diagnostic
	for _, p := range f.params {
		if usage.Names[p.Name] {
			continue
		}
		out = append(out, Diagnostic{Line: p.Line, StartCol: p.NameCol, EndLine: p.Line, EndCol: p.NameCol + len(p.Name),
			Severity: SevWarning, Code: CodeUnusedParam, param: p.Name,
			Message: fmt.Sprintf("Parameter '%s' is declared but never referenced in the template body", p.Name)})
	}
	return out
}

// checkUndeclared reports `${{ parameters.x }}` references to parameters the
// file never declares. Azure evaluates those to an empty string instead of
// failing, so a typo in a template body silently changes what the pipeline does.
func checkUndeclared(f *fileInfo) []Diagnostic {
	if pipeline.FindTopLevelParameters(f.lines) >= 0 && len(f.params) == 0 {
		return nil // a parameters block we could not parse: don't guess
	}
	declared := map[string]bool{}
	for _, p := range f.params {
		declared[p.Name] = true
	}
	var out []Diagnostic
	for _, r := range pipeline.FindParamRefs(f.lines) {
		if declared[r.Name] {
			continue
		}
		d := Diagnostic{Line: r.Line, StartCol: r.NameStart, EndLine: r.Line, EndCol: r.NameEnd,
			Severity: SevWarning, Code: CodeUndeclaredParam, param: r.Name}
		if len(f.params) == 0 {
			d.Message = fmt.Sprintf("Parameter '%s' is not declared: this file has no parameters block, so it evaluates to an empty string", r.Name)
		} else {
			d.Message = fmt.Sprintf("Parameter '%s' is not declared in this file's parameters block, so it evaluates to an empty string", r.Name)
			d.suggestions = suggest(r.Name, f.params, pipeline.CallSite{})
			if len(d.suggestions) > 0 {
				d.Message += fmt.Sprintf(". Did you mean '%s'?", d.suggestions[0])
			}
		}
		out = append(out, d)
	}
	return out
}

func refLabel(ref string) string {
	if ref == "" {
		return "default branch"
	}
	return strings.TrimPrefix(ref, "refs/heads/")
}

func typeLabel(t string) string {
	if t == "" {
		return "any"
	}
	return t
}

// suggest returns declared-but-not-passed parameter names close to name.
func suggest(name string, params []pipeline.Param, cs pipeline.CallSite) []string {
	type cand struct {
		n string
		d int
	}
	var cands []cand
	for _, p := range params {
		if _, passed := cs.Passed[p.Name]; passed {
			continue
		}
		d := levenshtein(strings.ToLower(name), strings.ToLower(p.Name))
		limit := len(p.Name)/3 + 1
		if d <= limit || strings.EqualFold(name, p.Name) {
			cands = append(cands, cand{p.Name, d})
		}
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].d < cands[j].d })
	var out []string
	for i, c := range cands {
		if i >= 3 {
			break
		}
		out = append(out, c.n)
	}
	return out
}

func levenshtein(a, b string) int {
	if a == b {
		return 0
	}
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = minInt(minInt(prev[j]+1, cur[j-1]+1), prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

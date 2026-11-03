package engine

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/qSharpy/azure-templates-navigator/server/internal/pipeline"
)

// Span is a single-line range in a file. Columns are byte offsets.
type Span struct {
	Path     string `json:"path"`
	Line     int    `json:"line"`
	StartCol int    `json:"startCol"`
	EndCol   int    `json:"endCol"`
}

// Usage is a reference with what a UI needs to list it.
type Usage struct {
	Span
	RelativePath string `json:"relativePath"`
	Kind         string `json:"kind"` // pipeline | template (kind of the file containing the usage)
	Text         string `json:"text"` // the trimmed source line
	Declaration  bool   `json:"declaration,omitempty"`
}

// UsageResult is what "find usages" found and how to title it.
type UsageResult struct {
	Title  string  `json:"title"`
	Usages []Usage `json:"usages"`
}

// callSite is a `template:` reference in a workspace file that resolves to a
// given template.
type callSite struct {
	path string
	ref  pipeline.TemplateRef
}

// isRootPipeline: a file that looks like a pipeline and that nothing
// references. A template that itself uses `extends:` (a golden-path template
// extending a base template) has pipeline-like top-level keys but is still a
// template.
func (e *Engine) isRootPipeline(path string) bool {
	f := e.file(path)
	return f != nil && f.isPipeline && len(e.reverse[path]) == 0
}

// callSitesOf returns every workspace reference to target, sorted by file and line.
func (e *Engine) callSitesOf(target string) []callSite {
	var out []callSite
	for _, c := range e.callers(target) {
		for _, ed := range e.forward[c] {
			if ed.target == target {
				out = append(out, callSite{path: c, ref: ed.ref})
			}
		}
	}
	return out
}

// paramAt identifies the template parameter under the cursor, if any: a
// `parameters.x` expression, a `- name: x` declaration, or a name passed at
// a call site. It returns the template that declares it.
func (e *Engine) paramAt(f *fileInfo, line, col int) (template string, name string, ok bool) {
	text := f.lines[line]
	if pr, ok := pipeline.ParamRefAt(text, col); ok {
		return f.path, pr.Name, true
	}
	for _, p := range f.params {
		if p.Line == line {
			return f.path, p.Name, true
		}
	}
	if tl, pp, ok := pipeline.PassedParamAt(f.lines, line, col); ok {
		if tr, ok := f.refAt(tl); ok {
			if res := e.resolve(tr, f); res.Exists {
				return res.Path, pp.Name, true
			}
		}
	}
	return "", "", false
}

// References finds usages of whatever is under the cursor:
//   - a template parameter: its declaration, its `${{ parameters.x }}` uses in
//     the template, and every call site that passes it;
//   - a `template:` reference: every call site of that template;
//   - anything else in a file: every call site of the file itself.
func (e *Engine) References(path string, line, col int, includeDecl bool) UsageResult {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ensureLinked()
	path = filepath.Clean(path)
	f := e.file(path)
	if f == nil || line < 0 || line >= len(f.lines) {
		return UsageResult{}
	}
	var out UsageResult
	if tpl, name, ok := e.paramAt(f, line, col); ok {
		out = e.paramUsages(tpl, name, includeDecl)
	} else {
		target := path
		if r, ok := f.refAt(line); ok {
			res := e.resolve(r, f)
			if !res.Exists {
				return UsageResult{}
			}
			target = res.Path
		}
		out = e.templateUsages(target, includeDecl)
	}
	sortUsages(out.Usages)
	return out
}

func (e *Engine) usage(path string, line, start, end int) Usage {
	text := ""
	kind := "template"
	if f := e.file(path); f != nil && line < len(f.lines) {
		text = strings.TrimSpace(f.lines[line])
	}
	if e.isRootPipeline(path) {
		kind = "pipeline"
	}
	return Usage{Span: Span{Path: path, Line: line, StartCol: start, EndCol: end}, RelativePath: e.relPath(path), Kind: kind, Text: text}
}

func (e *Engine) templateUsages(target string, includeDecl bool) UsageResult {
	res := UsageResult{Title: "Usages of " + filepath.Base(target)}
	if includeDecl {
		u := e.usage(target, 0, 0, 0)
		u.Declaration = true
		res.Usages = append(res.Usages, u)
	}
	for _, cs := range e.callSitesOf(target) {
		res.Usages = append(res.Usages, e.usage(cs.path, cs.ref.Line, cs.ref.ValueCol, cs.ref.ValueEnd))
	}
	return res
}

func (e *Engine) paramUsages(tpl, name string, includeDecl bool) UsageResult {
	res := UsageResult{Title: fmt.Sprintf("Usages of parameter '%s' (%s)", name, filepath.Base(tpl))}
	tf := e.file(tpl)
	if tf == nil {
		return res
	}
	if includeDecl {
		if p, ok := pipeline.ParamByName(tf.params, name); ok {
			u := e.usage(tpl, p.Line, p.NameCol, p.NameCol+len(p.Name))
			u.Declaration = true
			res.Usages = append(res.Usages, u)
		}
	}
	for _, r := range pipeline.FindParamRefs(tf.lines) {
		if r.Name == name {
			res.Usages = append(res.Usages, e.usage(tpl, r.Line, r.NameStart, r.NameEnd))
		}
	}
	for _, cs := range e.callSitesOf(tpl) {
		cf := e.file(cs.path)
		if cf == nil {
			continue
		}
		if pp, ok := pipeline.ParseCallSite(cf.lines, cs.ref.Line).Passed[name]; ok {
			res.Usages = append(res.Usages, e.usage(cs.path, pp.Line, pp.NameCol, pp.NameCol+len(name)))
		}
	}
	return res
}

// ---------------------------------------------------------------------------
// Call hierarchy: pipelines and templates are the "functions", template:
// references are the "calls".
// ---------------------------------------------------------------------------

// HierarchyItem is a file in the call hierarchy.
type HierarchyItem struct {
	Path     string `json:"path"`
	Name     string `json:"name"`
	Detail   string `json:"detail"`
	Kind     string `json:"kind"` // pipeline | template
	LastLine int    `json:"lastLine"`
}

// HierarchyCall links an item to the references (in the caller) that make the call.
type HierarchyCall struct {
	Item   HierarchyItem
	Ranges []Span
}

func (e *Engine) hierarchyItem(path string) HierarchyItem {
	it := HierarchyItem{Path: path, Name: filepath.Base(path), Detail: filepath.Dir(e.relPath(path)), Kind: "template"}
	if it.Detail == "." {
		it.Detail = ""
	}
	if f := e.file(path); f != nil {
		it.LastLine = len(f.lines) - 1
		if it.LastLine < 0 {
			it.LastLine = 0
		}
	}
	if e.isRootPipeline(path) {
		it.Kind = "pipeline"
	}
	return it
}

// PrepareHierarchy returns the item at the cursor: the template referenced on
// that line, or the file itself.
func (e *Engine) PrepareHierarchy(path string, line int) (HierarchyItem, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ensureLinked()
	path = filepath.Clean(path)
	f := e.file(path)
	if f == nil {
		return HierarchyItem{}, false
	}
	if line >= 0 && line < len(f.lines) {
		if r, ok := f.refAt(line); ok {
			if res := e.resolve(r, f); res.Exists {
				return e.hierarchyItem(res.Path), true
			}
		}
	}
	return e.hierarchyItem(path), true
}

// IncomingCalls lists the files that reference path.
func (e *Engine) IncomingCalls(path string) []HierarchyCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ensureLinked()
	path = filepath.Clean(path)
	byCaller := map[string][]Span{}
	var order []string
	for _, cs := range e.callSitesOf(path) {
		if _, ok := byCaller[cs.path]; !ok {
			order = append(order, cs.path)
		}
		byCaller[cs.path] = append(byCaller[cs.path], Span{Path: cs.path, Line: cs.ref.Line, StartCol: cs.ref.ValueCol, EndCol: cs.ref.ValueEnd})
	}
	out := make([]HierarchyCall, 0, len(order))
	for _, c := range order {
		out = append(out, HierarchyCall{Item: e.hierarchyItem(c), Ranges: byCaller[c]})
	}
	return out
}

// OutgoingCalls lists the templates path references.
func (e *Engine) OutgoingCalls(path string) []HierarchyCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ensureLinked()
	path = filepath.Clean(path)
	byTarget := map[string][]Span{}
	var order []string
	for _, ed := range e.edgesOf(path) {
		if ed.target == "" {
			continue
		}
		if _, ok := byTarget[ed.target]; !ok {
			order = append(order, ed.target)
		}
		byTarget[ed.target] = append(byTarget[ed.target], Span{Path: path, Line: ed.ref.Line, StartCol: ed.ref.ValueCol, EndCol: ed.ref.ValueEnd})
	}
	out := make([]HierarchyCall, 0, len(order))
	for _, t := range order {
		out = append(out, HierarchyCall{Item: e.hierarchyItem(t), Ranges: byTarget[t]})
	}
	return out
}

// ---------------------------------------------------------------------------
// Code lenses ("Code Vision" in IntelliJ)
// ---------------------------------------------------------------------------

// Lens is a usage count shown above a line.
type Lens struct {
	Line  int    `json:"line"`
	Col   int    `json:"col"`
	Title string `json:"title"`
	Count int    `json:"count"`
}

// CodeLenses returns "N usages" above a template's parameters block (or its
// first line). Root pipelines that nothing references get no lens.
func (e *Engine) CodeLenses(path string) []Lens {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ensureLinked()
	path = filepath.Clean(path)
	f := e.file(path)
	if f == nil || len(f.lines) == 0 {
		return nil
	}
	sites := e.callSitesOf(path)
	if f.isPipeline && len(sites) == 0 {
		return nil
	}
	if !f.isPipeline && len(sites) == 0 && len(f.params) == 0 && len(f.refs) == 0 {
		return nil // not recognisably a template
	}
	line := pipeline.FindTopLevelParameters(f.lines)
	if line < 0 {
		for i, l := range f.lines {
			if !pipeline.IsBlank(l) && !pipeline.IsComment(l) {
				line = i
				break
			}
		}
	}
	if line < 0 {
		return nil
	}
	return []Lens{{Line: line, Title: usageTitle(sites, e), Count: len(sites)}}
}

func usageTitle(sites []callSite, e *Engine) string {
	if len(sites) == 0 {
		return "no usages"
	}
	files := map[string]bool{}
	pipelines := 0
	for _, s := range sites {
		if !files[s.path] {
			files[s.path] = true
			if e.isRootPipeline(s.path) {
				pipelines++
			}
		}
	}
	t := fmt.Sprintf("%d usage%s", len(sites), plural(len(sites)))
	if pipelines > 0 {
		t += fmt.Sprintf(" · %d pipeline%s", pipelines, plural(pipelines))
	}
	return t
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// ---------------------------------------------------------------------------
// Document symbols
// ---------------------------------------------------------------------------

// Symbols returns the outline of a file.
func (e *Engine) Symbols(path string) []*pipeline.Symbol {
	e.mu.Lock()
	defer e.mu.Unlock()
	f := e.file(filepath.Clean(path))
	if f == nil {
		return nil
	}
	return pipeline.Outline(f.lines)
}

// sortUsages orders usages: declaration first, then by file and line.
func sortUsages(us []Usage) {
	sort.SliceStable(us, func(i, j int) bool {
		if us[i].Declaration != us[j].Declaration {
			return us[i].Declaration
		}
		if us[i].Path != us[j].Path {
			return us[i].Path < us[j].Path
		}
		return us[i].Line < us[j].Line
	})
}

package engine

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/qSharpy/azure-templates-navigator/server/internal/pipeline"
)

// Pre-flight checks: problems that make Azure reject a run when it is queued
// (errors) or that make a run misbehave once it is under way (warnings),
// found without running the pipeline.
//
// File-level checks look at one file. Pipeline-level checks look at a root
// pipeline together with every template it reaches, because that is what
// Azure validates: aliases resolve against the root pipeline's resources, and
// dependsOn / output variables name stages, jobs and steps that may live in
// any template. Their findings are reported on the root pipeline, at the
// template call that leads to the offending file.

// Diagnostic codes of the pre-flight checks.
const (
	CodeRepoNotDeclared   = "repository-not-declared"
	CodeUnknownDepend     = "unknown-dependency"
	CodeDuplicateName     = "duplicate-name"
	CodeInvalidName       = "invalid-name"
	CodeTemplateKind      = "template-kind-mismatch"
	CodeDefaultNotValue   = "default-not-allowed"
	CodeTabIndent         = "tab-indentation"
	CodeNoDefault         = "runtime-param-no-default"
	CodeOutputVariable    = "unknown-output-variable"
	CodeOutputNotIsOutput = "output-variable-not-output"
	CodeRepoNoEndpoint    = "repository-missing-endpoint"
)

// ---------------------------------------------------------------------------
// File-level checks
// ---------------------------------------------------------------------------

func (e *Engine) checkStructure(f *fileInfo) []Diagnostic {
	var out []Diagnostic
	for _, l := range pipeline.TabIndentedLines(f.lines) {
		n := pipeline.Indent(f.lines[l])
		out = append(out, Diagnostic{Line: l, StartCol: 0, EndLine: l, EndCol: n, Severity: SevError, Code: CodeTabIndent,
			Message: "Tab in indentation: YAML only allows spaces here, so Azure rejects the file when the run is queued"})
	}
	for _, it := range pipeline.FindNamedItems(f.lines) {
		if !pipeline.ValidName(it.Name) {
			out = append(out, Diagnostic{Line: it.Line, StartCol: it.Col, EndLine: it.Line, EndCol: it.End, Severity: SevError, Code: CodeInvalidName,
				Message: fmt.Sprintf("Invalid %s name '%s': names may only contain letters, digits and '_', and may not start with a digit", kindLabel(it.Kind), it.Name)})
		}
	}
	for _, it := range pipeline.DuplicateNames(f.lines) {
		out = append(out, Diagnostic{Line: it.Line, StartCol: it.Col, EndLine: it.Line, EndCol: it.End, Severity: SevError, Code: CodeDuplicateName,
			Message: fmt.Sprintf("%s '%s' is defined more than once in this list; Azure rejects the run when it is queued", capitalize(kindLabel(it.Kind)), it.Name)})
	}
	for _, p := range f.params {
		if d, ok := checkDefault(p); ok {
			out = append(out, d)
		}
	}
	out = append(out, checkEndpoints(f)...)
	return out
}

// checkEndpoints: a repository on GitHub, GitHub Enterprise or Bitbucket is
// reached through a service connection, so Azure rejects a declaration without
// `endpoint:` when the run is queued. Azure Repos (`type: git`) needs none.
func checkEndpoints(f *fileInfo) []Diagnostic {
	var decls []pipeline.RepoDecl
	for _, d := range f.repos {
		switch d.Type {
		case "github", "githubenterprise", "bitbucket":
			if strings.TrimSpace(d.Endpoint) == "" {
				decls = append(decls, d)
			}
		}
	}
	sort.Slice(decls, func(i, j int) bool { return decls[i].Line < decls[j].Line })
	var out []Diagnostic
	for _, d := range decls {
		line := f.lines[d.Line]
		col, end := 0, len(line)
		if k := strings.Index(line, "repository"); k >= 0 {
			if i := strings.Index(line[k:], d.Alias); i >= 0 {
				col, end = k+i, k+i+len(d.Alias)
			}
		}
		out = append(out, Diagnostic{Line: d.Line, StartCol: col, EndLine: d.Line, EndCol: end, Severity: SevError, Code: CodeRepoNoEndpoint,
			Message: fmt.Sprintf("Repository '%s' (type: %s) has no 'endpoint:'. Azure reaches %s repositories through a service connection, so the run fails when it is queued", d.Alias, d.Type, repoHost(d.Type))})
	}
	return out
}

func repoHost(typ string) string {
	switch typ {
	case "githubenterprise":
		return "GitHub Enterprise"
	case "bitbucket":
		return "Bitbucket"
	}
	return "GitHub"
}

// checkDefault reports a default that is not one of the parameter's
// allowed values: every run (or call) that doesn't pass the parameter fails.
func checkDefault(p pipeline.Param) (Diagnostic, bool) {
	if !p.HasDefault || len(p.Values) == 0 || p.DefaultEnd == p.DefaultCol {
		return Diagnostic{}, false
	}
	v := pipeline.Unquote(p.Default)
	if pipeline.IsExpression(v) || strings.HasPrefix(v, "[") || strings.HasPrefix(v, "{") {
		return Diagnostic{}, false
	}
	for _, a := range p.Values {
		if strings.EqualFold(a, v) {
			return Diagnostic{}, false
		}
	}
	return Diagnostic{Line: p.DefaultLine, StartCol: p.DefaultCol, EndLine: p.DefaultLine, EndCol: p.DefaultEnd,
		Severity: SevError, Code: CodeDefaultNotValue, param: p.Name, values: p.Values,
		Message: fmt.Sprintf("Default '%s' of parameter '%s' is not one of its allowed values (%s), so any run that doesn't set it fails", v, p.Name, strings.Join(p.Values, ", "))}, true
}

// checkRuntimeParams: a pipeline started by a CI trigger or a schedule has
// nobody to fill in the "Run pipeline" form, so a runtime parameter without
// a default (and without values: to pick the first of) fails those runs.
func checkRuntimeParams(f *fileInfo) []Diagnostic {
	if len(f.params) == 0 || !pipeline.RunsAutomatically(f.lines) {
		return nil
	}
	var out []Diagnostic
	for _, p := range f.params {
		if p.HasDefault || len(p.Values) > 0 {
			continue
		}
		out = append(out, Diagnostic{Line: p.Line, StartCol: p.NameCol, EndLine: p.Line, EndCol: p.NameCol + len(p.Name),
			Severity: SevWarning, Code: CodeNoDefault, param: p.Name, paramType: p.Type,
			Message: fmt.Sprintf("Runtime parameter '%s' has no default. Runs started by a trigger or a schedule can't ask for a value, so they fail", p.Name)})
	}
	return out
}

// checkTemplateKind reports a template used in the wrong kind of list, e.g. a
// steps template under `jobs:`.
func checkTemplateKind(f *fileInfo, r pipeline.TemplateRef, target *fileInfo) (Diagnostic, bool) {
	if target == nil || target.kind == "" {
		return Diagnostic{}, false
	}
	ctx := pipeline.TemplateContext(f.lines, r.Line)
	if ctx == "" || ctx == target.kind {
		return Diagnostic{}, false
	}
	what := target.kind + " template"
	if target.kind == "extends" {
		what = "pipeline template (it uses extends:)"
	}
	return Diagnostic{Line: r.Line, StartCol: r.ValueCol, EndLine: r.Line, EndCol: r.ValueEnd, Severity: SevError, Code: CodeTemplateKind,
		templateLine: r.Line,
		Message:      fmt.Sprintf("'%s' is a %s, but it is used in a %s: list; Azure rejects the run when it is queued", strings.TrimSpace(r.Ref), what, ctx)}, true
}

// ---------------------------------------------------------------------------
// Pipeline expansion
// ---------------------------------------------------------------------------

// expansion is a root pipeline and every file it reaches.
type expansion struct {
	root  *fileInfo
	files []*fileInfo // root first, then templates in breadth-first order
	// via maps each reached template to the root pipeline's template
	// reference that leads to it.
	via map[string]pipeline.TemplateRef
	// complete: every template reference was resolved to an existing file,
	// so the names collected below are all the names the run can have.
	complete bool
	dynamic  int // references decided at run time (template: ${{ ... }})
	declared map[string]bool
	// ctxs are the parameter values each file is reached with (one entry
	// per distinct set of values), so ${{ if }} branches can be decided.
	ctxs map[string][]xctx
	rel  func(string) string
}

// xctx is one way a file is reached: the parameter values its call passes,
// and, when the path to it goes through a ${{ if }} whose condition is not
// known, where that branch is ("file:line").
type xctx struct {
	b     pipeline.Bindings
	guard string
}

// maxContexts bounds the distinct parameter values tracked per file; past
// it a file is reached with every value unknown.
const maxContexts = 8

// expand walks the templates reachable from root. References inside
// ${{ if }} branches that are certainly not taken with the values passed
// along the way are not followed. Caller holds e.mu.
func (e *Engine) expand(root *fileInfo) *expansion {
	x := &expansion{root: root, via: map[string]pipeline.TemplateRef{}, complete: true, declared: map[string]bool{"self": true},
		ctxs: map[string][]xctx{}, rel: e.relPath}
	type item struct {
		f *fileInfo
		c xctx
	}
	seenCtx := map[string]map[string]bool{root.path: {"": true}}
	dynamic := map[string]bool{}
	// A root pipeline's own parameters are filled in when the run is
	// queued, so they are unknown.
	start := xctx{b: pipeline.Bindings{}}
	x.ctxs[root.path] = []xctx{start}
	x.files = append(x.files, root)
	queue := []item{{root, start}}
	for len(queue) > 0 {
		it := queue[0]
		queue = queue[1:]
		f := it.f
		// Templates can't declare repositories for the pipeline, but being
		// lenient here only hides problems, never invents them.
		for a := range f.repos {
			x.declared[a] = true
		}
		for _, ed := range e.edgesOf(f.path) {
			live := it.c.live(f, ed.ref.Line)
			if live == pipeline.False {
				continue
			}
			if ed.res.Kind == KindDynamic {
				if k := fmt.Sprintf("%s\x00%d", f.path, ed.ref.Line); !dynamic[k] {
					dynamic[k] = true
					x.dynamic++
				}
				x.complete = false
				continue
			}
			if ed.target == "" {
				x.complete = false
				continue
			}
			tf := e.file(ed.target)
			if tf == nil {
				x.complete = false
				continue
			}
			c := xctx{b: e.bindingsFor(tf, f, ed.ref, it.c.b), guard: it.c.guard}
			if c.guard == "" && live == pipeline.Unknown {
				c.guard = fmt.Sprintf("%s:%d", e.relPath(f.path), f.conditions().Guard(ed.ref.Line, it.c.b)+1)
			}
			seen := seenCtx[tf.path]
			if seen == nil {
				seen = map[string]bool{}
				seenCtx[tf.path] = seen
				x.files = append(x.files, tf)
				if f == root {
					x.via[tf.path] = ed.ref
				} else {
					x.via[tf.path] = x.via[f.path]
				}
			}
			if len(seen) >= maxContexts {
				c.b = pipeline.Bindings{}
			}
			key := c.b.Key()
			if c.guard != "" {
				key += "?"
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			x.ctxs[tf.path] = append(x.ctxs[tf.path], c)
			queue = append(queue, item{tf, c})
		}
	}
	return x
}

// live reports whether a line of f is expanded in this context.
func (c xctx) live(f *fileInfo, line int) pipeline.Tri {
	t := f.conditions().Live(line, c.b)
	if c.guard != "" {
		t = t.And(pipeline.Unknown)
	}
	return t
}

// state reports whether a line of a reached file is expanded in this
// pipeline: True when it is with some of the values the file is reached
// with, False when it is with none, Unknown otherwise. For Unknown, guard
// is the ${{ if }} ("file:line") the answer depends on.
func (x *expansion) state(f *fileInfo, line int) (t pipeline.Tri, guard string) {
	t = pipeline.False
	for _, c := range x.ctxs[f.path] {
		switch c.live(f, line) {
		case pipeline.True:
			return pipeline.True, ""
		case pipeline.Unknown:
			if t == pipeline.False {
				t, guard = pipeline.Unknown, c.guard
				if guard == "" {
					guard = fmt.Sprintf("%s:%d", x.rel(f.path), f.conditions().Guard(line, c.b)+1)
				}
			}
		}
	}
	return t, guard
}

// bindingsFor returns the values of tf's parameters for the call ref in f,
// whose own parameters have the values in outer: literals passed at the
// call, values forwarded as ${{ parameters.x }}, and defaults. Caller holds
// e.mu.
func (e *Engine) bindingsFor(tf, f *fileInfo, ref pipeline.TemplateRef, outer pipeline.Bindings) pipeline.Bindings {
	b := pipeline.Bindings{}
	if len(tf.params) == 0 {
		return b
	}
	cs := pipeline.ParseCallSite(f.lines, ref.Line)
	if cs.Unknowable() {
		return b
	}
	for _, p := range tf.params {
		if p.Type == "" {
			continue // a legacy block: values are untyped
		}
		name := strings.ToLower(p.Name)
		if strings.EqualFold(p.Type, "object") {
			bindObject(b, name, tf, f, cs, p, outer)
			continue
		}
		if pp, passed := cs.Lookup(p.Name); passed {
			if cs.HasConditionals || pp.IsBlock {
				continue // passed under an ${{ if }}, or a structured value
			}
			if from, ok := pipeline.ForwardedParam(pp.Value); ok {
				if v, ok := outer[strings.ToLower(from)]; ok && sameType(v, p.Type) {
					b[name] = v
				}
				continue
			}
			if v, ok := pipeline.ParseLiteral(p.Type, pp.Value); ok {
				b[name] = v
			}
			continue
		}
		if p.HasDefault {
			if v, ok := pipeline.ParseLiteral(p.Type, p.Default); ok {
				b[name] = v
			}
		}
	}
	return b
}

// bindObject binds the scalar members of an object parameter
// (`name.key`): from the value passed at the call (a mapping, or a forwarded
// object of the caller), else from its default.
func bindObject(b pipeline.Bindings, name string, tf, f *fileInfo, cs pipeline.CallSite, p pipeline.Param, outer pipeline.Bindings) {
	if pp, passed := cs.Lookup(p.Name); passed {
		if cs.HasConditionals {
			return
		}
		if from, ok := pipeline.ForwardedParam(pp.Value); ok {
			prefix := strings.ToLower(from) + "."
			for k, v := range outer {
				if strings.HasPrefix(k, prefix) {
					b[name+"."+k[len(prefix):]] = v
				}
			}
			return
		}
		pipeline.ObjectMembers(f.lines, pp.Line, pp.ValueCol, name, b)
		return
	}
	if l := pipeline.DefaultKeyLine(tf.lines, p); l >= 0 {
		col := len(pipeline.StripComment(tf.lines[l]))
		if p.DefaultLine == l && p.DefaultEnd > p.DefaultCol {
			col = p.DefaultCol
		}
		pipeline.ObjectMembers(tf.lines, l, col, name, b)
	}
}

func sameType(v pipeline.Value, typ string) bool {
	switch strings.ToLower(typ) {
	case "boolean":
		return v.Kind == pipeline.KindBool
	case "number":
		return v.Kind == pipeline.KindNumber
	case "string":
		return v.Kind == pipeline.KindString
	}
	return false
}

// at places a finding made in file f (line, columns) on the root pipeline:
// in place when f is the root, otherwise on the call that leads to f, with
// the template location prepended to the message. ok is false when the line
// is never expanded in this pipeline. When it depends on a ${{ if }} whose
// condition is not known, an error becomes a warning that says so.
func (e *Engine) at(x *expansion, f *fileInfo, line, col, end int, sev int, code, msg string) (d Diagnostic, ok bool) {
	st, guard := x.state(f, line)
	if st == pipeline.False {
		return Diagnostic{}, false
	}
	if st == pipeline.Unknown {
		if sev == SevError {
			sev = SevWarning
		}
		msg += fmt.Sprintf(", if the ${{ }} branch at %s is taken (it depends on values known only when the run is queued)", guard)
	}
	if f == x.root {
		return Diagnostic{Line: line, StartCol: col, EndLine: line, EndCol: end, Severity: sev, Code: code, Message: msg}, true
	}
	r := x.via[f.path]
	return Diagnostic{Line: r.Line, StartCol: r.ValueCol, EndLine: r.Line, EndCol: r.ValueEnd, Severity: sev, Code: code, templateLine: r.Line,
		Message: fmt.Sprintf("%s:%d: %s", e.relPath(f.path), line+1, msg)}, true
}

// checkPipeline runs the pipeline-level checks on a root pipeline.
func (e *Engine) checkPipeline(root *fileInfo) []Diagnostic {
	x := e.expand(root)
	var out []Diagnostic
	out = append(out, e.checkAliases(x)...)
	if x.complete {
		out = append(out, e.checkDependencies(x)...)
		out = append(out, e.checkOutputs(x)...)
	}
	return out
}

// checkAliases: Azure resolves every `@alias` and `checkout: alias` in the
// expanded pipeline against the root pipeline's resources.repositories.
func (e *Engine) checkAliases(x *expansion) []Diagnostic {
	var out []Diagnostic
	seen := map[string]bool{}
	report := func(f *fileInfo, line, col, end int, alias, what string) {
		d, ok := e.at(x, f, line, col, end, SevError, CodeRepoNotDeclared,
			fmt.Sprintf("%s repository '%s', which this pipeline does not declare in resources.repositories. Azure looks aliases up in the pipeline's resources, so the run fails when it is queued", what, alias))
		if !ok {
			return
		}
		key := fmt.Sprintf("%s\x00%d", alias, d.Line)
		if seen[key] {
			return
		}
		seen[key] = true
		d.alias = alias
		out = append(out, d)
	}
	for _, f := range x.files {
		for _, ed := range e.edgesOf(f.path) {
			a := ed.res.Alias
			if a == "" || x.declared[a] {
				continue
			}
			if f == x.root && ed.res.Kind == KindUnknownAlias {
				continue // already reported as unknown-repository-alias
			}
			report(f, ed.ref.Line, ed.ref.ValueCol, ed.ref.ValueEnd, a, fmt.Sprintf("'%s' uses", strings.TrimSpace(ed.ref.Ref)))
		}
		for _, c := range pipeline.FindCheckouts(f.lines) {
			if !x.declared[c.Name] {
				report(f, c.Line, c.Col, c.End, c.Name, "This step checks out")
			}
		}
	}
	return out
}

// checkDependencies: every name in a stage's or job's dependsOn must be a
// stage (or job) somewhere in the expanded pipeline. The names are collected
// from every reached file, across all ${{ if }} branches, so a name that is
// missing from all of them cannot exist at run time.
func (e *Engine) checkDependencies(x *expansion) []Diagnostic {
	stages, jobs := pipeline.NewNameMatcher(), pipeline.NewNameMatcher()
	for _, f := range x.files {
		for _, it := range pipeline.FindNamedItems(f.lines) {
			if it.IsJob() {
				jobs.Add(it.Name)
			} else {
				stages.Add(it.Name)
			}
		}
	}
	var out []Diagnostic
	for _, f := range x.files {
		for _, d := range pipeline.FindDependsOn(f.lines) {
			m, what := stages, "stage"
			if d.Owner.IsJob() {
				m, what = jobs, "job"
			}
			for _, n := range d.Names {
				if m.Has(n.Name) {
					continue
				}
				msg := fmt.Sprintf("%s '%s' depends on '%s', but this pipeline has no %s with that name; Azure rejects the run when it is queued",
					capitalize(kindLabel(d.Owner.Kind)), d.Owner.Name, n.Name, what)
				if s := closest(n.Name, m.Literals()); s != "" {
					msg += fmt.Sprintf(". Did you mean '%s'?", s)
				}
				if diag, ok := e.at(x, f, n.Line, n.Col, n.End, SevError, CodeUnknownDepend, msg); ok {
					out = append(out, diag)
				}
			}
		}
	}
	return out
}

// checkOutputs: `dependencies.X.outputs['step.var']` evaluates to an empty
// string when no step is named `step`, or when the step sets `var` without
// isOutput=true. Nothing fails at queue time; the job that reads it gets an
// empty value, so a condition is skipped or a script fails half-way.
func (e *Engine) checkOutputs(x *expansion) []Diagnostic {
	byName := map[string][]pipeline.StepInfo{}
	dynamic := pipeline.NewNameMatcher()
	for _, f := range x.files {
		for _, s := range pipeline.FindNamedSteps(f.lines) {
			if pipeline.IsExpression(s.Name) {
				dynamic.Add(s.Name)
				continue
			}
			k := strings.ToLower(s.Name)
			byName[k] = append(byName[k], s)
		}
	}
	var out []Diagnostic
	for _, f := range x.files {
		for _, r := range pipeline.FindOutputRefs(f.lines) {
			steps := byName[strings.ToLower(r.Step)]
			if len(steps) == 0 {
				if dynamic.Has(r.Step) {
					continue
				}
				if d, ok := e.at(x, f, r.Line, r.Col, r.End, SevWarning, CodeOutputVariable,
					fmt.Sprintf("No step in this pipeline is named '%s', so outputs['%s.%s'] is always empty. Give the step that sets '%s' `name: %s`", r.Step, r.Step, r.Var, r.Var, r.Step)); ok {
					out = append(out, d)
				}
				continue
			}
			setsOutput, setsPlain, opaque := false, false, false
			for _, s := range steps {
				isOut, ok := s.Sets[strings.ToLower(r.Var)]
				switch {
				case s.Kind == "task" || len(s.Sets) == 0:
					opaque = true // a task's outputs, or a script we can't see into
				case ok && isOut:
					setsOutput = true
				case ok:
					setsPlain = true
				}
			}
			if setsOutput || opaque || !setsPlain {
				continue
			}
			if d, ok := e.at(x, f, r.Line, r.Col, r.End, SevWarning, CodeOutputNotIsOutput,
				fmt.Sprintf("Step '%s' sets '%s' without isOutput=true, so outputs['%s.%s'] is always empty. Use ##vso[task.setvariable variable=%s;isOutput=true]", r.Step, r.Var, r.Step, r.Var, r.Var)); ok {
				out = append(out, d)
			}
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Verdict per pipeline
// ---------------------------------------------------------------------------

// Problem is a diagnostic in some file of a pipeline's expansion.
type Problem struct {
	Path         string `json:"path"`
	RelativePath string `json:"relativePath"`
	Diagnostic
	// New: your work added it (Pipelines view, from the last comparison
	// with the base; see changes.go).
	New bool `json:"new,omitempty"`
}

// PipelineReport answers "will this pipeline run?": every error and warning
// in the pipeline and in the templates it reaches.
type PipelineReport struct {
	Path         string    `json:"path"`
	RelativePath string    `json:"relativePath"`
	Errors       int       `json:"errors"`
	Warnings     int       `json:"warnings"`
	Templates    int       `json:"templates"` // templates reached
	Dynamic      int       `json:"dynamic"`   // template references decided at run time (not checked)
	Problems     []Problem `json:"problems"`
	Files        []string  `json:"files"` // the pipeline and every template it reaches
	// Change: what your work changed in it (Pipelines view only).
	Change *PipelineChange `json:"change,omitempty"`
}

// Title is a one-line verdict.
func (r PipelineReport) Title() string {
	switch {
	case r.Errors > 0:
		t := fmt.Sprintf("✗ Will fail when queued: %d error%s", r.Errors, plural(r.Errors))
		if r.Warnings > 0 {
			t += fmt.Sprintf(", %d warning%s", r.Warnings, plural(r.Warnings))
		}
		return t
	case r.Warnings > 0:
		return fmt.Sprintf("⚠ May fail while running: %d warning%s", r.Warnings, plural(r.Warnings))
	case r.Dynamic > 0:
		return fmt.Sprintf("✓ No problems found (%d template reference%s decided at run time not checked)", r.Dynamic, plural(r.Dynamic))
	}
	return "✓ No problems found before running"
}

// Verdict is the outcome in one word: "fail" (errors: Azure rejects the run
// when it is queued), "warn" (warnings: the run may go wrong) or "ok".
func (r PipelineReport) Verdict() string {
	switch {
	case r.Errors > 0:
		return "fail"
	case r.Warnings > 0:
		return "warn"
	}
	return "ok"
}

// notAboutTheRun are codes that say nothing about whether a run succeeds.
var notAboutTheRun = map[string]bool{CodeUnusedParam: true}

// PipelineCheck returns the verdict for a root pipeline; ok is false when
// path is not one (a template, or not a pipeline at all).
func (e *Engine) PipelineCheck(path string) (PipelineReport, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ensureLinked()
	path = filepath.Clean(path)
	if !e.isRootPipeline(path) {
		return PipelineReport{}, false
	}
	return e.pipelineReport(path), true
}

// PipelineChecks returns the verdict of every root pipeline in the workspace.
func (e *Engine) PipelineChecks() []PipelineReport {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ensureLinked()
	var out []PipelineReport
	for _, p := range e.workspaceFiles() {
		if e.isRootPipeline(p) {
			out = append(out, e.pipelineReport(p))
		}
	}
	return out
}

// PipelineOverview returns every root pipeline's verdict for the Pipelines
// view: failing pipelines first, then those with warnings, then the rest,
// by path within each group. ok is false when diagnostics are turned off,
// because there are no verdicts then.
func (e *Engine) PipelineOverview() (reports []PipelineReport, ok bool) {
	if !e.Settings().Diagnostics.Enabled {
		return nil, false
	}
	reports = e.PipelineChecks()
	e.annotate(reports)
	rank := map[string]int{"fail": 0, "warn": 1, "ok": 2}
	sort.SliceStable(reports, func(i, j int) bool {
		a, b := rank[reports[i].Verdict()], rank[reports[j].Verdict()]
		if a != b {
			return a < b
		}
		return reports[i].RelativePath < reports[j].RelativePath
	})
	return reports, true
}

func (e *Engine) pipelineReport(path string) PipelineReport {
	f := e.file(path)
	x := e.expand(f)
	rep := PipelineReport{Path: path, RelativePath: e.relPath(path), Templates: len(x.files) - 1, Dynamic: x.dynamic, Problems: []Problem{}}
	for _, tf := range x.files {
		rep.Files = append(rep.Files, tf.path)
		for _, d := range e.diagnose(tf.path) {
			if d.Severity > SevWarning || notAboutTheRun[d.Code] {
				continue
			}
			if st, _ := x.state(tf, d.Line); st == pipeline.False {
				continue // in a ${{ if }} branch this pipeline never expands
			}
			if d.Severity == SevError {
				rep.Errors++
			} else {
				rep.Warnings++
			}
			rep.Problems = append(rep.Problems, Problem{Path: tf.path, RelativePath: e.relPath(tf.path), Diagnostic: d})
		}
	}
	sort.SliceStable(rep.Problems, func(i, j int) bool { return rep.Problems[i].Severity < rep.Problems[j].Severity })
	return rep
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func kindLabel(kind string) string {
	if kind == "deployment" {
		return "deployment job"
	}
	return kind
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// closest returns the candidate nearest to name, if it is close enough to be
// a likely typo.
func closest(name string, cands []string) string {
	best, bestD := "", 1<<30
	for _, c := range cands {
		d := levenshtein(strings.ToLower(name), strings.ToLower(c))
		if d < bestD && d <= len(c)/3+1 {
			best, bestD = c, d
		}
	}
	return best
}

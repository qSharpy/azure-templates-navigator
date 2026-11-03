package engine

import (
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/qSharpy/azure-templates-navigator/server/internal/pipeline"
)

// applyEdits applies same-document edits (byte columns) to text.
func applyEdits(t *testing.T, text string, edits []TextEdit) string {
	t.Helper()
	lines := pipeline.SplitLines(text)
	offset := func(line, col int) int {
		n := 0
		for i := 0; i < line; i++ {
			n += len(lines[i]) + 1
		}
		return n + col
	}
	sorted := append([]TextEdit(nil), edits...)
	sort.Slice(sorted, func(i, j int) bool {
		return offset(sorted[i].StartLine, sorted[i].StartCol) > offset(sorted[j].StartLine, sorted[j].StartCol)
	})
	for _, ed := range sorted {
		s, e := offset(ed.StartLine, ed.StartCol), offset(ed.EndLine, ed.EndCol)
		text = text[:s] + ed.NewText + text[e:]
	}
	return text
}

func actionByTitle(t *testing.T, acts []CodeAction, title string) CodeAction {
	t.Helper()
	for _, a := range acts {
		if a.Title == title {
			return a
		}
	}
	var titles []string
	for _, a := range acts {
		titles = append(titles, a.Title)
	}
	t.Fatalf("no action %q in %q", title, titles)
	return CodeAction{}
}

func TestTemplateTextInScriptIsNotAReference(t *testing.T) {
	root, e := newWorkspace(t)
	p := write(t, root, "scripted.yml", `
trigger: none
steps:
  - script: |
      echo "usage:"
      template: not-a-real-template.yml
  - bash: >-
      template: folded.yml
  - template: templates/build.yml
    parameters:
      project: app
`)
	e.FileChanged(p)
	if ds := e.Diagnose(p); len(ds) != 0 {
		t.Errorf("diagnostics: %+v", ds)
	}
	if _, ok := e.Resolve(p, 4); ok {
		t.Error("Resolve treated a script line as a template reference")
	}
	if _, ok := e.Definition(p, 4, 18); ok {
		t.Error("Definition on a script line")
	}
	usages := e.References(filepath.Join(root, "templates/build.yml"), 0, 0, false)
	for _, u := range usages.Usages {
		if u.Path == p && u.Line != 7 {
			t.Errorf("usage from script body: %+v", u)
		}
	}
}

const flowPipeline = `
trigger: none
steps:
  - template: templates/build.yml
    parameters: { project: app, configuration: Staging, publsh: true }
  - template: templates/build.yml
    parameters: {}
  - template: templates/build.yml
    parameters: {
      project: app,
      configuration: Debug
    }
`

func TestFlowMappingParameters(t *testing.T) {
	root, e := newWorkspace(t)
	p := write(t, root, "flow.yml", flowPipeline)
	e.FileChanged(p)
	ds := e.Diagnose(p)
	got := map[string][]int{}
	for _, d := range ds {
		got[d.Code] = append(got[d.Code], d.Line)
	}
	want := map[string][]int{
		CodeValueNotAllowed: {3},
		CodeUnknownParam:    {3},
		CodeMissingRequired: {4}, // `parameters: {}` passes nothing
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("diagnostics = %v, want %v (%+v)", got, want, ds)
	}
	for _, d := range ds {
		line := pipeline.SplitLines(flowPipeline[1:])[d.Line]
		if d.Code == CodeValueNotAllowed && line[d.StartCol:d.EndCol] != "Staging" {
			t.Errorf("value range = %q", line[d.StartCol:d.EndCol])
		}
		if d.Code == CodeUnknownParam && !strings.Contains(d.Message, "Did you mean 'publish'") {
			t.Errorf("unknown-param message: %s", d.Message)
		}
	}

	text := mustRead(t, p)
	acts := e.CodeActions(p, 0, 20)
	fix := func(title string) string {
		return applyEdits(t, text, actionByTitle(t, acts, title).Edits)
	}
	lineOf := func(s string, n int) string { return strings.Split(s, "\n")[n] }

	if got := lineOf(fix("Change value to 'Debug'"), 3); got != "    parameters: { project: app, configuration: Debug, publsh: true }" {
		t.Errorf("change value: %q", got)
	}
	if got := lineOf(fix("Rename to 'publish'"), 3); got != "    parameters: { project: app, configuration: Staging, publish: true }" {
		t.Errorf("rename: %q", got)
	}
	if got := lineOf(fix("Remove unknown parameter 'publsh'"), 3); got != "    parameters: { project: app, configuration: Staging }" {
		t.Errorf("remove: %q", got)
	}
	if got := lineOf(fix("Add missing parameter 'project' (string)"), 5); got != "    parameters: { project: '' }" {
		t.Errorf("add into {}: %q", got)
	}
}

func TestFlowMappingAddAfterLastEntry(t *testing.T) {
	root, e := newWorkspace(t)
	p := write(t, root, "flow2.yml", `
trigger: none
steps:
  - template: templates/build.yml
    parameters: { configuration: Debug }
`)
	e.FileChanged(p)
	acts := e.CodeActions(p, 0, 10)
	got := applyEdits(t, mustRead(t, p), actionByTitle(t, acts, "Add missing parameter 'project' (string)").Edits)
	if line := strings.Split(got, "\n")[3]; line != "    parameters: { configuration: Debug, project: '' }" {
		t.Errorf("got %q", line)
	}
}

func TestFlowMappingHoverAndDefinition(t *testing.T) {
	root, e := newWorkspace(t)
	p := write(t, root, "flow.yml", flowPipeline)
	e.FileChanged(p)
	col := strings.Index("    parameters: { project: app, configuration: Staging, publsh: true }", "configuration") + 3
	h, ok := e.HoverAt(p, 3, col, ClientFeatures{})
	if !ok || !strings.Contains(h.Markdown, "configuration") {
		t.Fatalf("hover = %+v, %v", h, ok)
	}
	loc, ok := e.Definition(p, 3, col)
	if !ok || loc.Path != filepath.Join(root, "templates/build.yml") || loc.Line != 3 {
		t.Errorf("definition = %+v, %v", loc, ok)
	}
	// multi-line flow mapping
	h, ok = e.HoverAt(p, 8, 8, ClientFeatures{})
	if !ok || !strings.Contains(h.Markdown, "project") {
		t.Errorf("multi-line hover = %+v, %v", h, ok)
	}
	// find usages of `configuration` includes the flow call sites
	u := e.References(filepath.Join(root, "templates/build.yml"), 3, 12, false)
	n := 0
	for _, x := range u.Usages {
		if x.Path == p {
			n++
		}
	}
	if n != 2 {
		t.Errorf("usages in flow.yml = %d, want 2: %+v", n, u.Usages)
	}
}

func TestForwardedParametersAreNotChecked(t *testing.T) {
	root, e := newWorkspace(t)
	p := write(t, root, "templates/forward.yml", `
parameters:
  - name: project
    type: string
steps:
  - template: build.yml
    parameters: ${{ parameters }}
`)
	e.FileChanged(p)
	if ds := e.Diagnose(p); len(ds) != 0 {
		t.Errorf("diagnostics: %+v", ds)
	}
}

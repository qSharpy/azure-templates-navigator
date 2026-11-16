package engine

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func nodesByLabel(g Graph) map[string]GraphNode {
	m := map[string]GraphNode{}
	for _, n := range g.Nodes {
		m[n.Label] = n
	}
	return m
}

func insightsWorkspace(t *testing.T) (string, *Engine) {
	root, e := newWorkspace(t)
	write(t, root, "templates/stale.yml", "parameters:\n  - name: x\n    default: 1\nsteps:\n  - script: echo ${{ parameters.x }}\n")
	write(t, root, "consumer.yml", "trigger: none\nresources:\n  repositories:\n    - repository: tools\n      type: github\n      name: org/tools\nsteps:\n  - template: x.yml@tools\n  - template: templates/build.yml\n    parameters:\n      project: y\n")
	e.Rescan()
	return root, e
}

func TestGraphDegree(t *testing.T) {
	_, e := insightsWorkspace(t)
	g := e.BuildGraph(GraphRequest{})
	n := nodesByLabel(g)
	build, wrapper, pipe := n["build.yml"], n["wrapper.yml"], n["azure-pipelines.yml"]
	// build.yml: called by the pipeline (twice), wrapper and consumer
	if build.Callers != 3 || build.Usages != 4 || build.Pipelines != 2 || build.Callees != 0 || build.Weight != 3 {
		t.Errorf("build.yml degree: %+v", build)
	}
	if wrapper.Callers != 1 || wrapper.Callees != 1 || wrapper.Weight != 2 {
		t.Errorf("wrapper.yml degree: %+v", wrapper)
	}
	// build, wrapper, nope.yml (missing)
	if pipe.Callers != 0 || pipe.Callees != 3 || pipe.Weight != 3 || pipe.Errors == 0 {
		t.Errorf("pipeline degree: %+v", pipe)
	}
	if !strings.Contains(build.Tooltip, "Used by 3 files in this workspace (4 usages), reached from 2 pipelines") {
		t.Errorf("build.yml tooltip: %q", build.Tooltip)
	}
	if !strings.Contains(n["stale.yml"].Tooltip, "Not referenced in this workspace") {
		t.Errorf("stale.yml tooltip: %q", n["stale.yml"].Tooltip)
	}
	if n["nope.yml"].Broken != "missing" || n["x.yml"].Broken != "repoUnavailable" || n["x.yml"].RepoName != "org/tools" {
		t.Errorf("broken nodes: %+v / %+v", n["nope.yml"], n["x.yml"])
	}
	if g.TotalNodes != len(g.Nodes) || g.TotalEdges != len(g.Edges) {
		t.Errorf("unfiltered totals %d/%d vs %d/%d", g.TotalNodes, g.TotalEdges, len(g.Nodes), len(g.Edges))
	}

	// Degree is workspace-wide: the same in a graph scoped to one file.
	fg := e.BuildGraph(GraphRequest{File: build.FilePath, Depth: 1})
	if fb := nodesByLabel(fg)["build.yml"]; fb.Callers != 3 || fb.Weight != 3 {
		t.Errorf("file-scoped build.yml degree: %+v", fb)
	}
}

func TestGraphFilters(t *testing.T) {
	_, e := insightsWorkspace(t)
	all := e.BuildGraph(GraphRequest{})
	if all.Facets.Kinds["pipeline"] != 2 || all.Facets.Kinds["broken"] != 2 || all.Facets.Kinds["template"] != 3 {
		t.Errorf("kind facets: %+v", all.Facets.Kinds)
	}
	if len(all.Facets.Repos) != 2 || all.Facets.Repos[0].Label != workspaceRepoLabel || all.Facets.Repos[1].Key != "org/tools" {
		t.Errorf("repo facets: %+v", all.Facets.Repos)
	}

	broken := e.BuildGraph(GraphRequest{Kinds: []string{"broken"}})
	if len(broken.Nodes) != 2 || len(broken.Edges) != 0 || broken.TotalNodes != all.TotalNodes {
		t.Errorf("kinds=broken: %d nodes %d edges (total %d)", len(broken.Nodes), len(broken.Edges), broken.TotalNodes)
	}

	// Problems: files with diagnostics and broken targets; build.yml is clean.
	pr := nodesByLabel(e.BuildGraph(GraphRequest{ProblemsOnly: true}))
	for _, want := range []string{"azure-pipelines.yml", "consumer.yml", "nope.yml", "x.yml", "wrapper.yml"} {
		if _, ok := pr[want]; !ok {
			t.Errorf("problems only: %s missing (got %v)", want, keysOf(pr))
		}
	}
	if _, ok := pr["build.yml"]; ok {
		t.Errorf("problems only kept clean build.yml")
	}
	if _, ok := pr["stale.yml"]; ok {
		t.Errorf("problems only kept clean, unused stale.yml")
	}

	// Repository: its nodes, plus their direct callers as context.
	rg := e.BuildGraph(GraphRequest{Repos: []string{"org/tools"}})
	rn := nodesByLabel(rg)
	if len(rg.Nodes) != 2 || rn["x.yml"].Context || !rn["consumer.yml"].Context || len(rg.Edges) != 1 {
		t.Errorf("repo filter: %+v", rg.Nodes)
	}
}

func TestGraphDirection(t *testing.T) {
	root, e := insightsWorkspace(t)
	build := filepath.Join(root, "templates/build.yml")
	wrapper := filepath.Join(root, "templates/wrapper.yml")
	up := e.BuildGraph(GraphRequest{File: build, Depth: 2, Direction: "callers"})
	for _, ed := range up.Edges {
		if ed.Direction != "upstream" {
			t.Errorf("callers graph has %+v", ed)
		}
	}
	if len(up.Edges) != 4 { // pipeline, wrapper, consumer → build; pipeline → wrapper
		t.Errorf("callers edges: %+v", up.Edges)
	}
	down := e.BuildGraph(GraphRequest{File: wrapper, Depth: 2, Direction: "callees"})
	if len(down.Nodes) != 2 || len(down.Edges) != 1 || down.Edges[0].Direction != "downstream" {
		t.Errorf("callees graph: %+v", down)
	}
	// The focal file survives any filter.
	f := e.BuildGraph(GraphRequest{File: build, Depth: 1, Kinds: []string{"external"}})
	if len(f.Nodes) != 1 || !f.Nodes[0].Context || f.Nodes[0].FilePath != build {
		t.Errorf("focal node under a filter: %+v", f.Nodes)
	}
}

func keysOf(m map[string]GraphNode) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func section(t *testing.T, ss []StatSection, id string) StatSection {
	t.Helper()
	for _, s := range ss {
		if s.ID == id {
			if s.Rows == nil {
				t.Errorf("section %s: nil rows", id)
			}
			return s
		}
	}
	t.Fatalf("no section %s", id)
	return StatSection{}
}

func TestStats(t *testing.T) {
	_, e := insightsWorkspace(t)
	ss := e.Stats()

	ov := map[string]int{}
	for _, r := range section(t, ss, "overview").Rows {
		ov[r.Label] = r.Value
	}
	if ov["Pipelines"] != 2 || ov["Templates"] != 3 || ov["Repositories"] != 1 || ov["Broken references"] != 2 || ov["Errors"] == 0 {
		t.Errorf("overview: %+v", ov)
	}

	mu := section(t, ss, "mostUsed")
	if len(mu.Rows) == 0 || mu.Rows[0].Label != "build.yml" || mu.Rows[0].Value != 4 || mu.Rows[0].Focus != "file" ||
		mu.Rows[0].Description != "4 usages · 3 files · reached from 2 pipelines" {
		t.Errorf("most used: %+v", mu.Rows)
	}
	for _, r := range mu.Rows {
		if r.Kind == "missing" {
			t.Errorf("broken target in most used: %+v", r)
		}
	}

	// build.yml call sites: unknown-param, type-mismatch, missing-required,
	// value-not-allowed (all in azure-pipelines.yml).
	mc := section(t, ss, "misCalled")
	if len(mc.Rows) != 1 || mc.Rows[0].Label != "build.yml" || mc.Rows[0].Value != 4 || mc.Rows[0].Description != "4 problems in 1 file" {
		t.Errorf("mis-called: %+v", mc.Rows)
	}

	fo := section(t, ss, "fanOut")
	if len(fo.Rows) != 1 || fo.Rows[0].Label != "wrapper.yml" || fo.Rows[0].Value != 1 {
		t.Errorf("fan-out: %+v", fo.Rows)
	}

	un := section(t, ss, "unused")
	if len(un.Rows) != 1 || un.Rows[0].Label != "stale.yml" || un.Description != "1" {
		t.Errorf("unused: %+v", un.Rows)
	}

	rp := section(t, ss, "repositories")
	if len(rp.Rows) != 1 || rp.Rows[0].Repo != "org/tools" || rp.Rows[0].Focus != "repo" || rp.Rows[0].Label != "org/tools (@tools)" ||
		!strings.Contains(rp.Rows[0].Description, "1 broken reference") {
		t.Errorf("repositories: %+v", rp.Rows)
	}

	// Problems by type: right after the overview, adds up to its errors + warnings,
	// most frequent first, each type listing the files that have it.
	if ss[1].ID != "problemsByType" {
		t.Errorf("section order: %s second", ss[1].ID)
	}
	pt := section(t, ss, "problemsByType")
	sum := 0
	for i, r := range pt.Rows {
		sum += r.Value
		if i > 0 && r.Value > pt.Rows[i-1].Value {
			t.Errorf("not sorted: %+v", pt.Rows)
		}
		files := 0
		for _, c := range r.Children {
			files += c.Value
			if c.FilePath == "" {
				t.Errorf("%s: child without file: %+v", r.Label, c)
			}
		}
		if files != r.Value {
			t.Errorf("%s: children add up to %d, want %d", r.Label, files, r.Value)
		}
	}
	if sum != ov["Errors"]+ov["Warnings"] {
		t.Errorf("problems by type add up to %d, overview says %d errors + %d warnings", sum, ov["Errors"], ov["Warnings"])
	}
	labels := map[string]StatRow{}
	for _, r := range pt.Rows {
		labels[r.Label] = r
	}
	if r := labels["Template not found"]; r.Kind != "error" || len(r.Children) == 0 {
		t.Errorf("template not found: %+v", r)
	}
	// build.yml declares typed parameters, so an unknown one is an error (Azure rejects the run)
	if r := labels["Unknown parameter"]; r.Kind != "error" || r.Children[0].Label != "azure-pipelines.yml" || r.Children[0].Line == 0 {
		t.Errorf("unknown parameter: %+v", r)
	}
}

func TestUniqueSuffixes(t *testing.T) {
	got := uniqueSuffixes([]string{
		"teams/a/build/template.yml", "teams/b/build/template.yml", "deploy/template.yml",
		"pipelines/azure-pipelines.yml", "solo.yml",
	})
	want := map[string]string{
		"teams/a/build/template.yml":    "a/build/template.yml",
		"teams/b/build/template.yml":    "b/build/template.yml",
		"deploy/template.yml":           "deploy/template.yml",
		"pipelines/azure-pipelines.yml": "azure-pipelines.yml",
		"solo.yml":                      "solo.yml",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: got %q, want %q", k, got[k], v)
		}
	}
	// One path is the other's suffix: the shorter keeps its whole path.
	got = uniqueSuffixes([]string{"x/t.yml", "y/x/t.yml"})
	if got["x/t.yml"] != "x/t.yml" || got["y/x/t.yml"] != "y/x/t.yml" {
		t.Errorf("nested: %+v", got)
	}
}

func TestStatsDrillDown(t *testing.T) {
	_, e := insightsWorkspace(t)
	ov := map[string]StatRow{}
	ss := e.Stats()
	for _, r := range section(t, ss, "overview").Rows {
		ov[r.Label] = r
	}
	if r := ov["Pipelines"]; r.View != "pipelines" || r.Focus != "kind" || len(r.Kinds) != 1 || r.Kinds[0] != "pipeline" {
		t.Errorf("pipelines row: %+v", r)
	}
	if r := ov["External templates"]; r.Focus != "kind" || r.Kinds[0] != "external" {
		t.Errorf("external row: %+v", r)
	}
	if r := ov["Repositories"]; r.View != "repositories" {
		t.Errorf("repositories row: %+v", r)
	}
	// Broken references: one child per call site, each opening at it.
	br := ov["Broken references"]
	if len(br.Children) != br.Value || br.Value != 2 {
		t.Fatalf("broken children: %d for %d: %+v", len(br.Children), br.Value, br.Children)
	}
	refs := map[string]StatRow{}
	for _, c := range br.Children {
		refs[c.Label] = c
		if c.FilePath == "" || c.Line == 0 || c.Kind != "missing" {
			t.Errorf("broken child: %+v", c)
		}
	}
	if c := refs["x.yml@tools"]; !strings.HasPrefix(c.Description, "consumer.yml:") || !strings.Contains(c.Tooltip, "org/tools is not available") {
		t.Errorf("x.yml@tools: %+v", c)
	}
	if _, ok := refs["templates/nope.yml"]; !ok {
		t.Errorf("nope.yml missing from %+v", refs)
	}
	// Errors: the files with errors, adding up to the count, at their first one.
	er := ov["Errors"]
	sum := 0
	for _, c := range er.Children {
		sum += c.Value
		if c.FilePath == "" || c.Severity != SevError {
			t.Errorf("errors child: %+v", c)
		}
	}
	if sum != er.Value || len(er.Children) == 0 {
		t.Errorf("errors children add up to %d, want %d", sum, er.Value)
	}
}

func TestRepositoriesListTemplatesAndDeclarations(t *testing.T) {
	root, e := insightsWorkspace(t)
	rs := e.Repositories()
	if len(rs) != 1 {
		t.Fatalf("repositories: %+v", rs)
	}
	r := rs[0]
	consumer := filepath.Join(root, "consumer.yml")
	if len(r.Declarations) != 1 || r.Declarations[0].FilePath != consumer || r.Declarations[0].Line != 3 ||
		r.Declarations[0].RelativePath != "consumer.yml" || len(r.DeclaredIn) != 1 {
		t.Errorf("declarations: %+v", r.Declarations)
	}
	if len(r.Templates) != 1 {
		t.Fatalf("templates: %+v", r.Templates)
	}
	if tp := r.Templates[0]; tp.Label != "x.yml" || !tp.Broken || tp.Usages != 1 || tp.Callers != 1 || tp.RefFile != consumer || tp.RefLine != 7 {
		t.Errorf("template: %+v", tp)
	}
}

func TestRepositoryTemplatesFromLocalRepo(t *testing.T) {
	parent := t.TempDir()
	write(t, parent, "tpl/stages/deploy.yml", "parameters:\n  - name: env\n    default: dev\nstages: []\n")
	write(t, parent, "tpl/steps/build.yml", "steps: []\n")
	root := filepath.Join(parent, "app")
	write(t, root, ".git/HEAD", "")
	res := "resources:\n  repositories:\n    - repository: tpl\n      type: github\n      name: contoso/tpl\n"
	write(t, root, "a.yml", "trigger: none\n"+res+"stages:\n  - template: stages/deploy.yml@tpl\n  - template: stages/deploy.yml@tpl\n")
	write(t, root, "b.yml", "trigger: none\n"+res+"stages:\n  - template: /stages/deploy.yml@tpl\n  - template: steps/build.yml@tpl\n")
	st := DefaultSettings()
	st.Repositories.AutoFetch = false
	e := New([]string{root}, st)
	e.Rescan()
	rs := e.Repositories()
	if len(rs) != 1 || rs[0].Location.State != "ready" {
		t.Fatalf("repositories: %+v", rs)
	}
	ts := rs[0].Templates
	if len(ts) != 2 || ts[0].Label != "stages/deploy.yml" || ts[0].Usages != 3 || ts[0].Callers != 2 || ts[0].FilePath == "" || ts[0].Broken ||
		ts[1].Label != "steps/build.yml" || ts[1].Usages != 1 {
		t.Errorf("templates: %+v", ts)
	}
	if len(rs[0].Declarations) != 2 {
		t.Errorf("declarations: %+v", rs[0].Declarations)
	}
}

func TestGraphLabelsTellSameNamesApart(t *testing.T) {
	root, e := newWorkspace(t)
	write(t, root, "teams/a/template.yml", "steps: []\n")
	write(t, root, "teams/b/template.yml", "steps: []\n")
	write(t, root, "teams/p.yml", "trigger: none\nsteps:\n  - template: a/template.yml\n  - template: b/template.yml\n")
	e.Rescan()
	n := nodesByLabel(e.BuildGraph(GraphRequest{}))
	if _, ok := n["a/template.yml"]; !ok {
		t.Errorf("labels: %v", keysOf(n))
	}
	if _, ok := n["b/template.yml"]; !ok {
		t.Errorf("labels: %v", keysOf(n))
	}
	if _, ok := n["build.yml"]; !ok {
		t.Errorf("unique names keep their file name: %v", keysOf(n))
	}
	// The same label in a file-scoped graph that only holds one of them.
	fg := nodesByLabel(e.BuildGraph(GraphRequest{File: filepath.Join(root, "teams/a/template.yml"), Depth: 1}))
	if _, ok := fg["a/template.yml"]; !ok {
		t.Errorf("file-scoped labels: %v", keysOf(fg))
	}
}

func TestUniqueSuffixesManySameNames(t *testing.T) {
	var paths []string
	for i := 0; i < 3000; i++ {
		paths = append(paths, fmt.Sprintf("teams/t%d/build/template.yml", i))
	}
	paths = append(paths, "teams/t1/deploy/template.yml")
	got := uniqueSuffixes(paths)
	if got["teams/t7/build/template.yml"] != "t7/build/template.yml" || got["teams/t1/deploy/template.yml"] != "deploy/template.yml" {
		t.Errorf("got %q, %q", got["teams/t7/build/template.yml"], got["teams/t1/deploy/template.yml"])
	}
}

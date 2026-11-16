package engine

import (
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
}

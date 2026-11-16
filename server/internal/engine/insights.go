package engine

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// Workspace-wide numbers behind the graph's node sizes and filters and the
// Stats view. "Workspace-wide" means: counted over every indexed file, no
// matter which part of the graph is drawn, so a hub still looks like a hub
// when the graph is scoped to one file.

// nodeDegree is how a graph node (file or broken target) is used.
type nodeDegree struct {
	callers   map[string]bool // distinct workspace files referencing it
	usages    int             // call sites (the number the "N usages" lens shows)
	pipelines int             // pipelines that reach it, directly or through templates
	problems  map[string]int  // call-site diagnostics attributed to it, by code
	problemAt map[string]bool // files holding those call sites
}

// targetID is the graph node id of an edge's target ("" for dynamic refs).
func targetID(ed edge) string {
	res := ed.res
	switch {
	case res.Kind == KindDynamic:
		return ""
	case res.Kind == KindUnknownAlias:
		return "UNKNOWN_ALIAS:" + res.Alias + ":" + ed.ref.Ref
	case res.Kind == KindRepoUnavailable:
		return "REPO:" + res.Alias + ":" + ed.ref.Ref
	case !res.Exists:
		return "MISSING:" + res.Path
	}
	return res.Path
}

// callSiteCodes are the diagnostics a template's interface causes at its
// call sites ("most mis-called").
var callSiteCodes = map[string]bool{
	CodeMissingRequired: true, CodeUnknownParam: true, CodeTypeMismatch: true, CodeValueNotAllowed: true,
}

type degreeIndex struct {
	e     *Engine
	in    map[string]*nodeDegree
	edges map[string][]edge // memoised edgesOf (external files are resolved on demand)
}

func (d *degreeIndex) get(id string) *nodeDegree {
	n := d.in[id]
	if n == nil {
		n = &nodeDegree{callers: map[string]bool{}, problems: map[string]int{}, problemAt: map[string]bool{}}
		d.in[id] = n
	}
	return n
}

func (d *degreeIndex) edgesOf(p string) []edge {
	if ed, ok := d.edges[p]; ok {
		return ed
	}
	ed := d.e.edgesOf(p)
	d.edges[p] = ed
	return ed
}

// callees is the number of distinct targets a file references.
func (d *degreeIndex) callees(p string) int {
	seen := map[string]bool{}
	for _, ed := range d.edgesOf(p) {
		if id := targetID(ed); id != "" {
			seen[id] = true
		}
	}
	return len(seen)
}

// degrees computes the workspace-wide usage of every node. Caller must hold
// e.mu and have linked the index.
func (e *Engine) degrees() *degreeIndex {
	d := &degreeIndex{e: e, in: map[string]*nodeDegree{}, edges: map[string][]edge{}}
	files := e.workspaceFiles()
	for _, p := range files {
		byLine := map[int]string{}
		for _, ed := range e.forward[p] {
			id := targetID(ed)
			if id == "" {
				continue
			}
			n := d.get(id)
			n.callers[p] = true
			n.usages++
			byLine[ed.ref.Line] = id
		}
		for _, dg := range e.diagnose(p) {
			if !callSiteCodes[dg.Code] {
				continue
			}
			if id, ok := byLine[dg.templateLine]; ok {
				n := d.get(id)
				n.problems[dg.Code]++
				n.problemAt[p] = true
			}
		}
	}
	for id, n := range e.pipelineReach(d, files) {
		d.get(id).pipelines = n
	}
	return d
}

// pipelineReach counts, for every node, the pipelines that reach it. It walks
// every pipeline's whole call tree, so the result is cached until the call
// graph changes (e.version).
func (e *Engine) pipelineReach(d *degreeIndex, files []string) map[string]int {
	if e.reach != nil && e.reachVersion == e.version {
		return e.reach
	}
	out := map[string]int{}
	for _, p := range files {
		f := e.file(p)
		if f == nil || !f.isPipeline {
			continue
		}
		seen := map[string]bool{p: true}
		stack := []string{p}
		for len(stack) > 0 {
			cur := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			for _, ed := range d.edgesOf(cur) {
				id := targetID(ed)
				if id == "" || seen[id] {
					continue
				}
				seen[id] = true
				out[id]++
				if ed.target != "" {
					stack = append(stack, ed.target)
				}
			}
		}
	}
	e.reach, e.reachVersion = out, e.version
	return out
}

type problemCount struct{ errors, warnings int }

// problemCounts counts errors and warnings per workspace file.
func (e *Engine) problemCounts() map[string]problemCount {
	out := map[string]problemCount{}
	for _, p := range e.workspaceFiles() {
		var c problemCount
		for _, d := range e.diagnose(p) {
			switch d.Severity {
			case SevError:
				c.errors++
			case SevWarning:
				c.warnings++
			}
		}
		if c.errors+c.warnings > 0 {
			out[p] = c
		}
	}
	return out
}

func count(n int, word string) string {
	return fmt.Sprintf("%d %s%s", n, word, plural(n))
}

// ---------------------------------------------------------------------------
// Graph: degree, tooltips, filters
// ---------------------------------------------------------------------------

// kindClass maps a node kind onto the graph's kind filter.
func kindClass(kind string) string {
	switch kind {
	case "pipeline":
		return "pipeline"
	case "local":
		return "template"
	case "external":
		return "external"
	}
	return "broken"
}

func (n *GraphNode) problem() bool {
	return n.Broken != "" || n.Errors > 0 || n.Warnings > 0
}

// decorate fills in the usage numbers, problems and tooltip of every node.
func (g *graphBuilder) decorate(d *degreeIndex, pc map[string]problemCount) {
	for _, id := range g.order {
		n := g.nodes[id]
		if dg := d.in[id]; dg != nil {
			n.Callers, n.Usages, n.Pipelines = len(dg.callers), dg.usages, dg.pipelines
		}
		if n.Broken == "" && n.FilePath != "" {
			n.Callees = d.callees(n.FilePath)
		}
		n.Weight = n.Callers + n.Callees
		if c, ok := pc[n.FilePath]; ok && n.Broken == "" {
			n.Errors, n.Warnings = c.errors, c.warnings
		}
		n.Tooltip = g.tooltip(n)
	}
}

func (g *graphBuilder) tooltip(n *GraphNode) string {
	lines := []string{n.Label}
	if n.RelativePath != "" {
		lines[0] = n.RelativePath
	}
	switch n.Broken {
	case "missing":
		lines = append(lines, "Template not found")
	case "unknownAlias":
		lines = append(lines, "Repository alias @"+n.Alias+" is not declared in resources.repositories")
	case "repoUnavailable":
		lines = append(lines, "Repository "+n.RepoName+" is not available")
	}
	if n.Kind == "external" {
		lines = append(lines, fmt.Sprintf("From repository %s (@%s)", n.RepoName, n.Alias))
	}
	switch {
	case n.Callers > 0:
		s := fmt.Sprintf("Used by %s in this workspace (%s)", count(n.Callers, "file"), count(n.Usages, "usage"))
		if n.Pipelines > 0 {
			s += ", reached from " + count(n.Pipelines, "pipeline")
		}
		lines = append(lines, s)
	case n.Kind != "pipeline":
		lines = append(lines, "Not referenced in this workspace")
	}
	if n.Callees > 0 {
		lines = append(lines, "Calls "+count(n.Callees, "template"))
	}
	if n.ParamCount > 0 {
		lines = append(lines, fmt.Sprintf("%s, %d required", count(n.ParamCount, "parameter"), n.RequiredCount))
	}
	if n.Errors+n.Warnings > 0 {
		lines = append(lines, count(n.Errors, "error")+", "+count(n.Warnings, "warning"))
	}
	return strings.Join(lines, "\n")
}

const workspaceRepoLabel = "This workspace"

// filter applies the request's filters to a built graph. Nodes that match
// are kept; with a repository or problems filter, their direct callers are
// kept too (marked Context) so you can see who uses them. The focal file of
// a file-scoped graph is always kept.
func filterGraph(g Graph, req GraphRequest, focal string) Graph {
	g.TotalNodes, g.TotalEdges = len(g.Nodes), len(g.Edges)
	g.Facets = GraphFacets{Kinds: map[string]int{}, Repos: []GraphFacet{}}
	repoCount := map[string]int{}
	for _, n := range g.Nodes {
		g.Facets.Kinds[kindClass(n.Kind)]++
		repoCount[n.RepoName]++
		if n.problem() {
			g.Facets.Problems++
		}
	}
	var repoKeys []string
	for k := range repoCount {
		repoKeys = append(repoKeys, k)
	}
	sort.Strings(repoKeys) // "" (this workspace) first
	for _, k := range repoKeys {
		label := k
		if k == "" {
			label = workspaceRepoLabel
		}
		g.Facets.Repos = append(g.Facets.Repos, GraphFacet{Key: k, Label: label, Count: repoCount[k]})
	}

	kinds := setOf(req.Kinds)
	repoSet := setOf(req.Repos)
	byRepo := len(repoSet) > 0
	if !req.ProblemsOnly && len(kinds) == 0 && !byRepo {
		return g
	}
	kindOK := func(n GraphNode) bool { return len(kinds) == 0 || kinds[kindClass(n.Kind)] }
	keep := map[string]bool{}
	for _, n := range g.Nodes {
		if kindOK(n) && (!byRepo || repoSet[n.RepoName]) && (!req.ProblemsOnly || n.problem()) {
			keep[n.ID] = true
		}
	}
	context := map[string]bool{}
	if req.ProblemsOnly || byRepo {
		byID := map[string]GraphNode{}
		for _, n := range g.Nodes {
			byID[n.ID] = n
		}
		for _, ed := range g.Edges {
			if keep[ed.Target] && !keep[ed.Source] && kindOK(byID[ed.Source]) {
				context[ed.Source] = true
			}
		}
	}
	if focal != "" && !keep[focal] {
		context[focal] = true
	}
	nodes := []GraphNode{}
	for _, n := range g.Nodes {
		if keep[n.ID] || context[n.ID] {
			n.Context = context[n.ID]
			nodes = append(nodes, n)
		}
	}
	edges := []GraphEdge{}
	for _, ed := range g.Edges {
		if (keep[ed.Source] || context[ed.Source]) && (keep[ed.Target] || context[ed.Target]) {
			edges = append(edges, ed)
		}
	}
	g.Nodes, g.Edges = nodes, edges
	return g
}

func setOf(xs []string) map[string]bool {
	m := map[string]bool{}
	for _, x := range xs {
		m[x] = true
	}
	return m
}

// ---------------------------------------------------------------------------
// Stats (atn/stats)
// ---------------------------------------------------------------------------

// StatRow is one line of the Stats view. Clients render Label/Description/
// Tooltip as-is and use Focus to decide what "Show in Graph" does:
// "file" (scope the graph to FilePath), "repo" (filter by Repo) or
// "problems" (problems only).
type StatRow struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
	Tooltip     string `json:"tooltip,omitempty"`
	Kind        string `json:"kind"` // pipeline|template|external|missing|repo|error|warning|info
	FilePath    string `json:"filePath,omitempty"`
	Repo        string `json:"repo,omitempty"`
	Focus       string `json:"focus,omitempty"`
	Value       int    `json:"value"`
	Severity    int    `json:"severity,omitempty"`
}

// StatSection is a group of rows ("Most used", ...).
type StatSection struct {
	ID          string    `json:"id"`
	Label       string    `json:"label"`
	Description string    `json:"description,omitempty"`
	Tooltip     string    `json:"tooltip,omitempty"`
	Rows        []StatRow `json:"rows"`
}

const statTop = 10
const statUnusedMax = 100

// Stats computes the Stats view.
func (e *Engine) Stats() []StatSection {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ensureLinked()
	d := e.degrees()
	pc := e.problemCounts()
	sev := e.severityMap()
	files := e.workspaceFiles()

	// Every target id → what it is, from the workspace's edges.
	type target struct {
		id, path, kind, repo string
	}
	targets := map[string]target{}
	repoAlias := map[string]string{}
	var ids []string
	for _, p := range files {
		for _, ed := range e.forward[p] {
			id := targetID(ed)
			if id == "" {
				continue
			}
			prev, seen := targets[id]
			external := ed.res.Repo != nil && ed.res.Kind != KindLocal
			// A file referenced both locally and through an alias counts
			// under its repository, as in the graph.
			if !seen || (prev.repo == "" && external) {
				t := target{id: id, path: ed.res.Path}
				switch {
				case ed.res.Kind == KindUnknownAlias:
					t.kind, t.path = "missing", ""
				case ed.res.Kind == KindRepoUnavailable || !ed.res.Exists:
					t.kind = "missing"
				case ed.res.Kind == KindExternal:
					t.kind = "external"
				default:
					t.kind = "template"
					if f := e.file(ed.res.Path); f != nil && f.isPipeline {
						t.kind = "pipeline"
					}
				}
				if external {
					t.repo = ed.res.Repo.Name
					if repoAlias[t.repo] == "" {
						repoAlias[t.repo] = ed.res.Alias
					}
				}
				targets[id] = t
				if !seen {
					ids = append(ids, id)
				}
			}
		}
	}
	sort.Strings(ids)

	fileRow := func(path, kind string) StatRow {
		r := StatRow{Label: filepath.Base(path), Kind: kind, FilePath: path, Focus: "file", Severity: sev[path]}
		r.Tooltip = e.relPath(path)
		return r
	}

	// ── Overview
	var pipelines, templates int
	var errors, warnings, errFiles, warnFiles int
	for _, p := range files {
		f := e.file(p)
		if f == nil {
			continue
		}
		if f.isPipeline {
			pipelines++
		} else {
			templates++
		}
		if c, ok := pc[p]; ok {
			errors += c.errors
			warnings += c.warnings
			if c.errors > 0 {
				errFiles++
			}
			if c.warnings > 0 {
				warnFiles++
			}
		}
	}
	var external, broken int
	repoSet := map[string]bool{}
	for _, id := range ids {
		t := targets[id]
		switch t.kind {
		case "external":
			external++
		case "missing":
			broken += d.in[id].usages
		}
		if t.repo != "" {
			repoSet[t.repo] = true
		}
	}
	overview := StatSection{ID: "overview", Label: "Overview", Rows: []StatRow{
		{Label: "Pipelines", Description: fmt.Sprint(pipelines), Value: pipelines, Kind: "pipeline"},
		{Label: "Templates", Description: fmt.Sprint(templates), Value: templates, Kind: "template", Tooltip: "Templates in this workspace"},
		{Label: "External templates", Description: fmt.Sprint(external), Value: external, Kind: "external", Tooltip: "Templates used from other repositories"},
		{Label: "Repositories", Description: fmt.Sprint(len(repoSet)), Value: len(repoSet), Kind: "repo", Tooltip: "Repositories templates are used from"},
		{Label: "Errors", Description: fmt.Sprintf("%d in %s", errors, count(errFiles, "file")), Value: errors, Kind: "error", Focus: "problems"},
		{Label: "Warnings", Description: fmt.Sprintf("%d in %s", warnings, count(warnFiles, "file")), Value: warnings, Kind: "warning", Focus: "problems"},
		{Label: "Broken references", Description: fmt.Sprint(broken), Value: broken, Kind: "missing", Focus: "problems",
			Tooltip: "Template references that point to a missing file, an undeclared alias or an unavailable repository"},
	}}

	// ── Most used (by call sites, like the "N usages" code lens)
	var used []string
	for _, id := range ids {
		if t := targets[id]; t.kind != "missing" {
			used = append(used, id)
		}
	}
	sort.SliceStable(used, func(i, j int) bool {
		a, b := d.in[used[i]], d.in[used[j]]
		if a.usages != b.usages {
			return a.usages > b.usages
		}
		return len(a.callers) > len(b.callers)
	})
	mostUsed := StatSection{ID: "mostUsed", Label: "Most used",
		Tooltip: "Templates with the most call sites. A change to their parameters reaches the most files and pipelines.", Rows: []StatRow{}}
	for _, id := range used {
		if len(mostUsed.Rows) == statTop {
			break
		}
		t, dg := targets[id], d.in[id]
		r := fileRow(t.path, t.kind)
		r.Value = dg.usages
		r.Description = count(dg.usages, "usage") + " · " + count(len(dg.callers), "file")
		if dg.pipelines > 0 {
			r.Description += " · reached from " + count(dg.pipelines, "pipeline")
		}
		r.Tooltip += fmt.Sprintf("\nUsed by %s (%s), reached from %s", count(len(dg.callers), "file"), count(dg.usages, "usage"), count(dg.pipelines, "pipeline"))
		mostUsed.Rows = append(mostUsed.Rows, r)
	}

	// ── Most mis-called
	var mis []string
	for _, id := range used {
		if len(d.in[id].problems) > 0 {
			mis = append(mis, id)
		}
	}
	total := func(m map[string]int) int {
		n := 0
		for _, v := range m {
			n += v
		}
		return n
	}
	sort.SliceStable(mis, func(i, j int) bool { return total(d.in[mis[i]].problems) > total(d.in[mis[j]].problems) })
	misCalled := StatSection{ID: "misCalled", Label: "Most mis-called",
		Tooltip: "Templates whose call sites have the most parameter problems (missing required, unknown, wrong type or value). Often a sign of a confusing interface.", Rows: []StatRow{}}
	for _, id := range mis {
		if len(misCalled.Rows) == statTop {
			break
		}
		t, dg := targets[id], d.in[id]
		n := total(dg.problems)
		r := fileRow(t.path, t.kind)
		r.Value = n
		r.Description = count(n, "problem") + " in " + count(len(dg.problemAt), "file")
		var codes []string
		for c := range dg.problems {
			codes = append(codes, c)
		}
		sort.Strings(codes)
		for _, c := range codes {
			r.Tooltip += fmt.Sprintf("\n%s ×%d", c, dg.problems[c])
		}
		misCalled.Rows = append(misCalled.Rows, r)
	}

	// ── Fan-out: templates that call the most others
	type fan struct {
		path         string
		callees, use int
	}
	var fans []fan
	for _, p := range files {
		f := e.file(p)
		if f == nil || f.isPipeline {
			continue
		}
		if c := d.callees(p); c > 0 {
			fans = append(fans, fan{p, c, len(e.forward[p])})
		}
	}
	sort.SliceStable(fans, func(i, j int) bool { return fans[i].callees > fans[j].callees })
	fanOut := StatSection{ID: "fanOut", Label: "Calls the most templates",
		Tooltip: "Templates that reference the most other templates. Deep or wide templates are harder to change and to debug.", Rows: []StatRow{}}
	for _, fn := range fans {
		if len(fanOut.Rows) == statTop {
			break
		}
		r := fileRow(fn.path, "template")
		r.Value = fn.callees
		r.Description = "calls " + count(fn.callees, "template")
		if fn.use != fn.callees {
			r.Description += fmt.Sprintf(" (%s)", count(fn.use, "call"))
		}
		fanOut.Rows = append(fanOut.Rows, r)
	}

	// ── Not referenced in this workspace
	unused := StatSection{ID: "unused", Label: "Not referenced in this workspace",
		Tooltip: "Templates that no pipeline or template in this workspace references. Templates published for other repositories show up here too, so check before deleting.", Rows: []StatRow{}}
	nUnused := 0
	for _, p := range files {
		f := e.file(p)
		if f == nil || f.isPipeline || len(e.reverse[p]) > 0 || !e.underRoots(p) {
			continue
		}
		nUnused++
		if len(unused.Rows) < statUnusedMax {
			r := fileRow(p, "template")
			r.Description = filepath.ToSlash(filepath.Dir(e.relPath(p)))
			if r.Description == "." {
				r.Description = ""
			}
			unused.Rows = append(unused.Rows, r)
		}
	}
	unused.Description = fmt.Sprint(nUnused)

	// ── By repository
	type repoStat struct {
		templates, usages, broken int
		callers                   map[string]bool
	}
	rs := map[string]*repoStat{}
	for _, id := range ids {
		t := targets[id]
		if t.repo == "" {
			continue
		}
		s := rs[t.repo]
		if s == nil {
			s = &repoStat{callers: map[string]bool{}}
			rs[t.repo] = s
		}
		dg := d.in[id]
		if t.kind == "missing" {
			s.broken += dg.usages
		} else {
			s.templates++
		}
		s.usages += dg.usages
		for c := range dg.callers {
			s.callers[c] = true
		}
	}
	var repoNames []string
	for n := range rs {
		repoNames = append(repoNames, n)
	}
	sort.Slice(repoNames, func(i, j int) bool {
		a, b := rs[repoNames[i]], rs[repoNames[j]]
		if a.usages != b.usages {
			return a.usages > b.usages
		}
		return repoNames[i] < repoNames[j]
	})
	byRepo := StatSection{ID: "repositories", Label: "By repository",
		Tooltip: "Templates used from each repository, and who uses them", Rows: []StatRow{}}
	for _, n := range repoNames {
		s := rs[n]
		r := StatRow{Label: fmt.Sprintf("%s (@%s)", n, repoAlias[n]), Kind: "repo", Repo: n, Focus: "repo", Value: s.usages,
			Description: fmt.Sprintf("%s · %s · %s", count(s.templates, "template"), count(s.usages, "usage"), count(len(s.callers), "file"))}
		r.Tooltip = fmt.Sprintf("%s used from %s, %s in %s", count(s.templates, "template"), n, count(s.usages, "usage"), count(len(s.callers), "file"))
		if s.broken > 0 {
			r.Description += " · " + count(s.broken, "broken reference")
			r.Severity = SevError
		}
		byRepo.Rows = append(byRepo.Rows, r)
	}

	for _, sec := range []*StatSection{&mostUsed, &misCalled, &fanOut, &unused} {
		disambiguate(e, sec.Rows)
	}
	mostUsed.Description = fmt.Sprint(len(used))
	misCalled.Description = fmt.Sprint(len(mis))
	fanOut.Description = fmt.Sprint(len(fans))
	byRepo.Description = fmt.Sprint(len(byRepo.Rows))
	return []StatSection{overview, mostUsed, misCalled, fanOut, unused, byRepo}
}

// disambiguate labels rows that share a file name with their relative path.
func disambiguate(e *Engine, rows []StatRow) {
	n := map[string]int{}
	for _, r := range rows {
		n[r.Label]++
	}
	for i, r := range rows {
		if n[r.Label] > 1 && r.FilePath != "" {
			rows[i].Label = filepath.ToSlash(e.relPath(r.FilePath))
		}
	}
}

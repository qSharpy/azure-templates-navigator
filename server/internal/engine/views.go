package engine

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/qSharpy/azure-templates-navigator/server/internal/pipeline"
	"github.com/qSharpy/azure-templates-navigator/server/internal/repos"
)

// The view models below are rendered as-is by every IDE client, so all
// labels, descriptions and tooltips are decided here — once.

// TreeNode is a node of the dependency tree.
type TreeNode struct {
	ID            string     `json:"id"`
	Label         string     `json:"label"`
	Description   string     `json:"description,omitempty"`
	Tooltip       string     `json:"tooltip,omitempty"`
	Kind          string     `json:"kind"` // pipeline|template|external|missing|dynamic|unknownAlias|repoUnavailable|cycle
	FilePath      string     `json:"filePath,omitempty"`
	Line          int        `json:"line"`               // line to reveal in FilePath
	RefFile       string     `json:"refFile,omitempty"`  // file containing the reference
	RefLine       int        `json:"refLine"`            // line of the reference in RefFile
	Severity      int        `json:"severity,omitempty"` // worst diagnostic in FilePath (1 error, 2 warning)
	ParamCount    int        `json:"paramCount"`
	RequiredCount int        `json:"requiredCount"`
	Alias         string     `json:"alias,omitempty"`
	Children      []TreeNode `json:"children,omitempty"`
}

// DependencyTree is the "Is calling" / "Called by" view for one file.
type DependencyTree struct {
	FilePath     string     `json:"filePath"`
	Label        string     `json:"label"`
	RelativePath string     `json:"relativePath"`
	Kind         string     `json:"kind"`
	IsCalling    []TreeNode `json:"isCalling"`
	CalledBy     []TreeNode `json:"calledBy"`
	CallerCount  int        `json:"callerCount"`
	Severity     int        `json:"severity,omitempty"`
}

const maxTreeDepth = 25

// Tree builds the dependency tree for a file.
func (e *Engine) Tree(path string) (DependencyTree, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ensureLinked()
	path = filepath.Clean(path)
	f := e.file(path)
	if f == nil {
		return DependencyTree{}, false
	}
	sev := e.severityMap()
	t := DependencyTree{
		FilePath: path, Label: filepath.Base(path), RelativePath: e.relPath(path),
		Kind: e.kindOf(f), Severity: sev[path],
	}
	t.IsCalling = e.downstream(path, map[string]bool{path: true}, 0, sev)
	t.CalledBy, t.CallerCount = e.upstream(path, sev)
	// Clients index into these; send [] rather than null for leaf/root files.
	if t.IsCalling == nil {
		t.IsCalling = []TreeNode{}
	}
	if t.CalledBy == nil {
		t.CalledBy = []TreeNode{}
	}
	return t, true
}

func (e *Engine) kindOf(f *fileInfo) string {
	if f.isPipeline {
		return "pipeline"
	}
	if !e.underRoots(f.path) {
		return "external"
	}
	return "template"
}

func (e *Engine) severityMap() map[string]int {
	out := map[string]int{}
	for _, p := range e.workspaceFiles() {
		for _, d := range e.diagnose(p) {
			if d.Severity > SevWarning {
				continue
			}
			if cur, ok := out[p]; !ok || d.Severity < cur {
				out[p] = d.Severity
			}
		}
	}
	return out
}

func (e *Engine) fileNode(p string, sev map[string]int) TreeNode {
	n := TreeNode{ID: p, Label: filepath.Base(p), FilePath: p, Description: filepath.ToSlash(filepath.Dir(e.relPath(p))), Severity: sev[p]}
	if n.Description == "." {
		n.Description = ""
	}
	if f := e.file(p); f != nil {
		n.Kind = e.kindOf(f)
		n.ParamCount = len(f.params)
		for _, pp := range f.params {
			if pp.Required {
				n.RequiredCount++
			}
		}
	}
	n.Tooltip = e.relPath(p)
	if n.ParamCount > 0 {
		n.Tooltip += fmt.Sprintf("\n%d parameter(s), %d required", n.ParamCount, n.RequiredCount)
	}
	return n
}

func (e *Engine) downstream(path string, visiting map[string]bool, depth int, sev map[string]int) []TreeNode {
	if depth > maxTreeDepth {
		return nil
	}
	var out []TreeNode
	for i, ed := range e.edgesOf(path) {
		id := fmt.Sprintf("%s#%d", path, i)
		base := TreeNode{ID: id, RefFile: path, RefLine: ed.ref.Line, Label: ed.ref.Ref}
		res := ed.res
		switch {
		case res.Kind == KindDynamic:
			base.Kind, base.Description, base.Tooltip = "dynamic", "resolved at runtime", "Template path contains an expression"
		case res.Kind == KindUnknownAlias:
			base.Kind, base.Description = "unknownAlias", "unknown alias @"+res.Alias
			base.Tooltip = "Repository alias not declared in resources.repositories"
		case res.Kind == KindRepoUnavailable:
			base.Kind, base.Alias = "repoUnavailable", res.Alias
			base.Description = string(res.RepoLoc.State)
			base.Tooltip = fmt.Sprintf("Repository %s is %s", res.Repo.Name, res.RepoLoc.State)
			if res.RepoLoc.Error != "" {
				base.Tooltip += ": " + res.RepoLoc.Error
			}
		case !res.Exists:
			base.Kind, base.Description, base.FilePath = "missing", "not found", res.Path
			base.Tooltip = "Template not found: " + res.Path
		default:
			n := e.fileNode(res.Path, sev)
			n.ID, n.RefFile, n.RefLine = id, path, ed.ref.Line
			if res.Kind == KindExternal {
				n.Kind, n.Alias = "external", res.Alias
				n.Description = "@" + res.Alias + " · " + res.Repo.Name
				n.Tooltip = fmt.Sprintf("%s\nfrom repository %s (%s)", ed.ref.Ref, res.Repo.Name, refLabel(res.Repo.Ref))
			}
			if visiting[res.Path] {
				n.Kind = "cycle"
				n.Description = "↻ cycle"
			} else {
				visiting[res.Path] = true
				n.Children = e.downstream(res.Path, visiting, depth+1, sev)
				delete(visiting, res.Path)
			}
			out = append(out, n)
			continue
		}
		out = append(out, base)
	}
	return out
}

// upstream builds the "Called by" view: top-level callers (usually
// pipelines) at the root, expanding down through intermediate templates
// toward the target.
func (e *Engine) upstream(target string, sev map[string]int) ([]TreeNode, int) {
	direct := e.callers(target)
	parentMap := map[string]map[string]bool{} // caller -> files it calls on the way to target
	visited := map[string]bool{target: true}
	frontier := []string{target}
	for len(frontier) > 0 {
		var next []string
		for _, node := range frontier {
			for _, c := range e.callers(node) {
				if parentMap[c] == nil {
					parentMap[c] = map[string]bool{}
				}
				parentMap[c][node] = true
				if !visited[c] {
					visited[c] = true
					next = append(next, c)
				}
			}
		}
		frontier = next
	}
	called := map[string]bool{}
	for _, children := range parentMap {
		for c := range children {
			called[c] = true
		}
	}
	var roots []string
	for c := range parentMap {
		if !called[c] {
			roots = append(roots, c)
		}
	}
	if len(roots) == 0 { // everything is in a cycle
		roots = direct
	}
	sort.Strings(roots)

	var build func(p string, seen map[string]bool, depth int) TreeNode
	build = func(p string, seen map[string]bool, depth int) TreeNode {
		n := e.fileNode(p, sev)
		n.ID = "up:" + p
		for _, ed := range e.forward[p] {
			if parentMap[p][ed.target] {
				n.RefFile, n.RefLine = p, ed.ref.Line
				n.Line = ed.ref.Line
				break
			}
		}
		if seen[p] || depth > maxTreeDepth {
			n.Kind, n.Description = "cycle", "↻ cycle"
			return n
		}
		seen[p] = true
		var kids []string
		for c := range parentMap[p] {
			if c != target {
				kids = append(kids, c)
			}
		}
		sort.Strings(kids)
		for _, c := range kids {
			child := build(c, seen, depth+1)
			child.ID = n.ID + ">" + c
			n.Children = append(n.Children, child)
		}
		delete(seen, p)
		return n
	}
	var out []TreeNode
	for _, r := range roots {
		out = append(out, build(r, map[string]bool{}, 0))
	}
	return out, len(direct)
}

// ---------------------------------------------------------------------------
// Graph
// ---------------------------------------------------------------------------

// GraphNode / GraphEdge match the shape consumed by the shared graph webview.
type GraphNode struct {
	ID            string `json:"id"`
	Label         string `json:"label"`
	RelativePath  string `json:"relativePath,omitempty"`
	Kind          string `json:"kind"` // pipeline|local|external|missing|unknown
	FilePath      string `json:"filePath,omitempty"`
	RepoName      string `json:"repoName,omitempty"`
	Alias         string `json:"alias,omitempty"`
	ParamCount    int    `json:"paramCount"`
	RequiredCount int    `json:"requiredCount"`
	IsScope       bool   `json:"isScope,omitempty"`
}

type GraphEdge struct {
	Source    string `json:"source"`
	Target    string `json:"target"`
	Label     string `json:"label,omitempty"`
	Direction string `json:"direction,omitempty"`
}

type Graph struct {
	Nodes []GraphNode `json:"nodes"`
	Edges []GraphEdge `json:"edges"`
}

// GraphRequest selects workspace-wide or file-scoped graphs.
type GraphRequest struct {
	File     string `json:"file"`     // when set: file-scoped graph
	Depth    int    `json:"depth"`    // BFS depth for file scope (1-10)
	RootPath string `json:"rootPath"` // sub-folder for workspace graph
}

type graphBuilder struct {
	e     *Engine
	nodes map[string]*GraphNode
	order []string
	edges []GraphEdge
	seen  map[string]bool
}

func (g *graphBuilder) node(n GraphNode) *GraphNode {
	if ex, ok := g.nodes[n.ID]; ok {
		return ex
	}
	g.nodes[n.ID] = &n
	g.order = append(g.order, n.ID)
	return &n
}

func (g *graphBuilder) fileNode(p string) *GraphNode {
	n := GraphNode{ID: p, Label: filepath.Base(p), RelativePath: g.e.relPath(p), FilePath: p, Kind: "local"}
	if f := g.e.file(p); f != nil {
		if f.isPipeline {
			n.Kind = "pipeline"
		}
		n.ParamCount = len(f.params)
		for _, pp := range f.params {
			if pp.Required {
				n.RequiredCount++
			}
		}
	}
	return g.node(n)
}

func (g *graphBuilder) edge(src, dst, label, dir string) {
	k := src + "→" + dst
	if g.seen[k] {
		return
	}
	g.seen[k] = true
	g.edges = append(g.edges, GraphEdge{Source: src, Target: dst, Label: label, Direction: dir})
}

// target returns the node id for an edge target, creating the node.
func (g *graphBuilder) target(ed edge) (string, bool) {
	res := ed.res
	switch {
	case res.Kind == KindDynamic:
		return "", false
	case res.Kind == KindUnknownAlias:
		id := "UNKNOWN_ALIAS:" + res.Alias + ":" + ed.ref.Ref
		g.node(GraphNode{ID: id, Label: filepath.Base(ed.ref.Path()), Kind: "unknown", Alias: res.Alias})
		return id, false
	case res.Kind == KindRepoUnavailable:
		id := "REPO:" + res.Alias + ":" + ed.ref.Ref
		g.node(GraphNode{ID: id, Label: filepath.Base(ed.ref.Path()), Kind: "missing", Alias: res.Alias, RepoName: res.Repo.Name,
			RelativePath: fmt.Sprintf("%s (repository %s)", ed.ref.Path(), res.RepoLoc.State)})
		return id, false
	case !res.Exists:
		id := "MISSING:" + res.Path
		n := GraphNode{ID: id, Label: filepath.Base(res.Path), Kind: "missing", FilePath: res.Path, RelativePath: g.e.relPath(res.Path)}
		if res.Repo != nil {
			n.RepoName = res.Repo.Name
		}
		g.node(n)
		return id, false
	}
	n := g.fileNode(res.Path)
	if res.Kind == KindExternal {
		n.Kind, n.RepoName, n.Alias = "external", res.Repo.Name, res.Alias
	}
	return res.Path, true
}

func edgeLabel(ed edge) string {
	if ed.res.Alias != "" && ed.res.Alias != "self" {
		return "@" + ed.res.Alias
	}
	return ""
}

// BuildGraph builds graph data for the webview.
func (e *Engine) BuildGraph(req GraphRequest) Graph {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ensureLinked()
	g := &graphBuilder{e: e, nodes: map[string]*GraphNode{}, seen: map[string]bool{}}

	if req.File == "" {
		scope := ""
		rp := strings.Trim(strings.TrimSpace(req.RootPath), `/\`)
		for _, p := range e.workspaceFiles() {
			if rp != "" {
				rel := e.relPath(p)
				if rel != rp && !strings.HasPrefix(rel, rp+"/") {
					continue
				}
			}
			_ = scope
			g.fileNode(p)
			for _, ed := range e.forward[p] {
				if id, _ := g.target(ed); id != "" {
					g.edge(p, id, edgeLabel(ed), "")
				}
			}
		}
		return g.result()
	}

	depth := req.Depth
	if depth < 1 {
		depth = 1
	}
	if depth > 10 {
		depth = 10
	}
	root := filepath.Clean(req.File)
	g.fileNode(root).IsScope = true

	type item struct {
		p string
		d int
	}
	queue := []item{{root, 0}}
	visited := map[string]bool{root: true}
	for len(queue) > 0 {
		it := queue[0]
		queue = queue[1:]
		if it.d >= depth {
			continue
		}
		for _, ed := range e.edgesOf(it.p) {
			id, real := g.target(ed)
			if id == "" {
				continue
			}
			g.edge(it.p, id, edgeLabel(ed), "downstream")
			if real && !visited[id] {
				visited[id] = true
				queue = append(queue, item{id, it.d + 1})
			}
		}
	}
	queue = []item{{root, 0}}
	upVisited := map[string]bool{root: true}
	for len(queue) > 0 {
		it := queue[0]
		queue = queue[1:]
		if it.d >= depth {
			continue
		}
		for _, c := range e.callers(it.p) {
			g.fileNode(c)
			label := ""
			for _, ed := range e.forward[c] {
				if ed.target == it.p {
					label = edgeLabel(ed)
					break
				}
			}
			g.edge(c, it.p, label, "upstream")
			if !upVisited[c] {
				upVisited[c] = true
				queue = append(queue, item{c, it.d + 1})
			}
		}
	}
	return g.result()
}

func (g *graphBuilder) result() Graph {
	out := Graph{Nodes: []GraphNode{}, Edges: g.edges}
	for _, id := range g.order {
		out.Nodes = append(out.Nodes, *g.nodes[id])
	}
	if out.Edges == nil {
		out.Edges = []GraphEdge{}
	}
	return out
}

// ---------------------------------------------------------------------------
// Lists: templates, diagnostics, repositories
// ---------------------------------------------------------------------------

// TemplateInfo is an entry for template search / quick open.
type TemplateInfo struct {
	FilePath      string `json:"filePath"`
	RelativePath  string `json:"relativePath"`
	Label         string `json:"label"`
	Kind          string `json:"kind"`
	ParamCount    int    `json:"paramCount"`
	RequiredCount int    `json:"requiredCount"`
	CallerCount   int    `json:"callerCount"`
}

// ListTemplates lists every workspace YAML file with summary info.
func (e *Engine) ListTemplates() []TemplateInfo {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ensureLinked()
	var out []TemplateInfo
	for _, p := range e.workspaceFiles() {
		f := e.file(p)
		if f == nil {
			continue
		}
		ti := TemplateInfo{FilePath: p, RelativePath: e.relPath(p), Label: filepath.Base(p), Kind: e.kindOf(f),
			ParamCount: len(f.params), CallerCount: len(e.reverse[p])}
		for _, pp := range f.params {
			if pp.Required {
				ti.RequiredCount++
			}
		}
		out = append(out, ti)
	}
	return out
}

// FileDiagnostics groups diagnostics for one file.
type FileDiagnostics struct {
	FilePath     string       `json:"filePath"`
	RelativePath string       `json:"relativePath"`
	Diagnostics  []Diagnostic `json:"diagnostics"`
	Errors       int          `json:"errors"`
	Warnings     int          `json:"warnings"`
}

// WorkspaceDiagnostics returns files with problems, worst first.
func (e *Engine) WorkspaceDiagnostics() []FileDiagnostics {
	all := e.DiagnoseAll()
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []FileDiagnostics
	for p, ds := range all {
		fd := FileDiagnostics{FilePath: p, RelativePath: e.relPath(p)}
		for _, d := range ds {
			if d.Severity > SevWarning {
				continue
			}
			fd.Diagnostics = append(fd.Diagnostics, d)
			if d.Severity == SevError {
				fd.Errors++
			} else {
				fd.Warnings++
			}
		}
		if len(fd.Diagnostics) > 0 {
			out = append(out, fd)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Errors != out[j].Errors {
			return out[i].Errors > out[j].Errors
		}
		return out[i].RelativePath < out[j].RelativePath
	})
	return out
}

// RepoInfo describes an external repository used by the workspace.
type RepoInfo struct {
	Alias      string         `json:"alias"`
	Name       string         `json:"name"`
	Type       string         `json:"type"`
	Ref        string         `json:"ref"`
	DeclaredIn []string       `json:"declaredIn"`
	Location   repos.Location `json:"location"`
	Label      string         `json:"label"`
	Detail     string         `json:"detail"`
}

// Repositories lists every repository declared in the workspace.
func (e *Engine) Repositories() []RepoInfo {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ensureLinked()
	byKey := map[string]*RepoInfo{}
	var keys []string
	for _, p := range e.workspaceFiles() {
		f := e.file(p)
		if f == nil {
			continue
		}
		for _, d := range f.repos {
			k := d.Type + "|" + d.Name + "|" + d.Ref
			ri, ok := byKey[k]
			if !ok {
				repoRoot := e.repoRoot(filepath.Dir(p))
				parents := []string{filepath.Dir(repoRoot)}
				loc := e.Repos.Locate(d, parents, e.remoteFor(p))
				ri = &RepoInfo{Alias: d.Alias, Name: d.Name, Type: d.Type, Ref: d.Ref, Location: loc}
				byKey[k] = ri
				keys = append(keys, k)
			}
			ri.DeclaredIn = append(ri.DeclaredIn, e.relPath(p))
		}
	}
	sort.Strings(keys)
	var out []RepoInfo
	for _, k := range keys {
		ri := byKey[k]
		ri.Label = fmt.Sprintf("%s (@%s)", ri.Name, ri.Alias)
		state := string(ri.Location.State)
		switch ri.Location.Source {
		case repos.SourceSibling:
			state = "local clone"
		case repos.SourceLocal:
			state = "local path"
		case repos.SourceCache:
			if ri.Location.State == repos.StateReady && !ri.Location.FetchedAt.IsZero() {
				state = "cached " + ago(ri.Location.FetchedAt)
			}
		}
		ri.Detail = fmt.Sprintf("%s · %s · %s", ri.Type, refLabel(ri.Ref), state)
		out = append(out, *ri)
	}
	return out
}

// RepoDecls returns all distinct repository declarations with the remote to
// use for each (used by `atn fetch`).
func (e *Engine) RepoDecls() []struct {
	Decl   pipeline.RepoDecl
	Remote string
	File   string
} {
	e.mu.Lock()
	defer e.mu.Unlock()
	type rd = struct {
		Decl   pipeline.RepoDecl
		Remote string
		File   string
	}
	seen := map[string]bool{}
	var out []rd
	for _, p := range e.workspaceFiles() {
		f := e.file(p)
		if f == nil {
			continue
		}
		var aliases []string
		for a := range f.repos {
			aliases = append(aliases, a)
		}
		sort.Strings(aliases)
		for _, a := range aliases {
			d := f.repos[a]
			k := d.Type + "|" + d.Name + "|" + d.Ref
			if seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, rd{Decl: d, Remote: e.remoteFor(p), File: p})
		}
	}
	return out
}

// ParamInfo is one declared template parameter (atn/parameters).
type ParamInfo struct {
	Name     string   `json:"name"`
	Type     string   `json:"type"`
	Default  string   `json:"default,omitempty"`
	Required bool     `json:"required"`
	Values   []string `json:"values,omitempty"`
	Line     int      `json:"line"`
}

// TemplateParameters lists the parameters of a template. When line >= 0 and
// that line of path is a `template:` reference, the referenced template is
// used; otherwise path itself is treated as the template.
func (e *Engine) TemplateParameters(path string, line int) (string, []ParamInfo) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ensureLinked()
	target := path
	if f := e.file(path); f != nil && line >= 0 && line < len(f.lines) {
		if r, ok := f.refAt(line); ok {
			res := e.resolve(r, f)
			if !res.Exists {
				return "", nil
			}
			target = res.Path
		}
	}
	tf := e.file(target)
	if tf == nil {
		return "", nil
	}
	out := make([]ParamInfo, 0, len(tf.params))
	for _, p := range tf.params {
		out = append(out, ParamInfo{Name: p.Name, Type: typeLabel(p.Type), Default: p.Default, Required: p.Required, Values: p.Values, Line: p.Line})
	}
	return target, out
}

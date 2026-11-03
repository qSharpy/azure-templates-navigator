// Package engine holds the workspace model (files, index, resolution) and
// every IDE-independent feature: diagnostics, hover, completion, definition,
// quick fixes, dependency tree and graph data. Both the LSP server and the
// CLI drive this package; IDE plugins only render its results.
package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/qSharpy/azure-templates-navigator/server/internal/pipeline"
	"github.com/qSharpy/azure-templates-navigator/server/internal/repos"
)

// Settings are shared by every client. Keys mirror the IDE settings.
type Settings struct {
	RequiredParameterColor string `json:"requiredParameterColor"`
	Diagnostics            struct {
		Enabled          bool `json:"enabled"`
		UnusedParameters bool `json:"unusedParameters"`
	} `json:"diagnostics"`
	Graph struct {
		RootPath string `json:"rootPath"`
	} `json:"graph"`
	Repositories repos.Config `json:"repositories"`
}

// DefaultSettings returns defaults.
func DefaultSettings() Settings {
	var s Settings
	s.RequiredParameterColor = "#c92d35"
	s.Diagnostics.Enabled = true
	s.Diagnostics.UnusedParameters = true
	s.Repositories = repos.DefaultConfig()
	return s
}

// ParseSettings decodes settings JSON on top of defaults. It accepts either
// the bare object or one wrapped as {"azure-templates-navigator": {...}}.
func ParseSettings(raw json.RawMessage) Settings {
	s := DefaultSettings()
	if len(raw) == 0 || string(raw) == "null" {
		return s
	}
	var wrapped map[string]json.RawMessage
	if json.Unmarshal(raw, &wrapped) == nil {
		if inner, ok := wrapped["azure-templates-navigator"]; ok {
			raw = inner
		}
	}
	_ = json.Unmarshal(raw, &s)
	if s.RequiredParameterColor == "" {
		s.RequiredParameterColor = "#c92d35"
	}
	return s
}

// ProjectConfigFile is an optional, committable per-repository config file.
const ProjectConfigFile = ".azure-templates-navigator.json"

// fileInfo is the parsed view of one YAML file.
type fileInfo struct {
	path       string
	text       string
	lines      []string
	params     []pipeline.Param
	refs       []pipeline.TemplateRef
	repos      map[string]pipeline.RepoDecl
	isPipeline bool
	modTime    time.Time
	size       int64
	fromDisk   bool
}

// Engine is the workspace model. All exported methods are safe for
// concurrent use.
type Engine struct {
	mu        sync.Mutex
	roots     []string
	settings  Settings
	ideRepos  repos.Config // repositories config as provided by the IDE
	Repos     *repos.Manager
	overlay   map[string]string    // open documents
	files     map[string]*fileInfo // parse cache for any file (workspace or not)
	workspace map[string]bool      // YAML files under the roots
	forward   map[string][]edge
	reverse   map[string]map[string]bool
	aliases   map[string]pipeline.RepoDecl // workspace-wide alias registry
	aliasFrom map[string]string            // alias -> declaring file
	linked    bool
	Log       func(string)

	rootCache   map[string]string
	originCache map[string]string
	diagCache   map[string][]Diagnostic
}

type edge struct {
	ref    pipeline.TemplateRef
	target string // resolved path ("" if unresolved)
	res    Resolution
}

// New creates an engine for the given workspace roots.
func New(roots []string, s Settings) *Engine {
	e := &Engine{
		overlay:   map[string]string{},
		files:     map[string]*fileInfo{},
		workspace: map[string]bool{},
		Log:       func(string) {},
	}
	for _, r := range roots {
		if abs, err := filepath.Abs(r); err == nil {
			e.roots = append(e.roots, abs)
		}
	}
	e.Repos = repos.NewManager(s.Repositories)
	e.Repos.SetLogger(func(m string) { e.Log(m) })
	e.applySettings(s)
	return e
}

// Roots returns the workspace roots.
func (e *Engine) Roots() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.roots...)
}

// Settings returns the effective settings.
func (e *Engine) Settings() Settings {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.settings
}

// SetSettings replaces the settings.
func (e *Engine) SetSettings(s Settings) {
	e.mu.Lock()
	e.applySettings(s)
	e.linked = false
	e.mu.Unlock()
}

func (e *Engine) applySettings(s Settings) {
	e.ideRepos = s.Repositories
	s.Repositories = mergeProjectConfig(s.Repositories, e.roots)
	e.settings = s
	e.Repos.SetConfig(s.Repositories)
}

// mergeProjectConfig layers .azure-templates-navigator.json (committed in the
// repository) under the IDE settings so a team can share URL mappings.
func mergeProjectConfig(c repos.Config, roots []string) repos.Config {
	for _, root := range roots {
		b, err := os.ReadFile(filepath.Join(root, ProjectConfigFile))
		if err != nil {
			continue
		}
		var pc struct {
			Repositories repos.Config `json:"repositories"`
		}
		if json.Unmarshal(b, &pc) != nil {
			continue
		}
		p := pc.Repositories
		urls := map[string]string{}
		for k, v := range p.URLs {
			urls[k] = v
		}
		for k, v := range c.URLs {
			urls[k] = v
		}
		c.URLs = urls
		lp := map[string]string{}
		for k, v := range p.LocalPaths {
			if !filepath.IsAbs(v) && !strings.HasPrefix(v, "~") {
				v = filepath.Join(root, v)
			}
			lp[k] = v
		}
		for k, v := range c.LocalPaths {
			lp[k] = v
		}
		c.LocalPaths = lp
		if c.AzureDevOpsOrganization == "" {
			c.AzureDevOpsOrganization = p.AzureDevOpsOrganization
		}
		if c.AzureDevOpsProject == "" {
			c.AzureDevOpsProject = p.AzureDevOpsProject
		}
		if c.GitHubEnterpriseHost == "" {
			c.GitHubEnterpriseHost = p.GitHubEnterpriseHost
		}
	}
	return c
}

// ---------------------------------------------------------------------------
// Documents & files
// ---------------------------------------------------------------------------

// OpenDocument sets the in-memory content of an open editor document.
func (e *Engine) OpenDocument(path, text string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	path = filepath.Clean(path)
	e.overlay[path] = text
	delete(e.files, path)
	if isYAML(path) && e.underRoots(path) {
		e.workspace[path] = true
	}
	e.linked = false
}

// CloseDocument drops the in-memory content; the file is re-read from disk.
func (e *Engine) CloseDocument(path string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	path = filepath.Clean(path)
	delete(e.overlay, path)
	delete(e.files, path)
	if _, err := os.Stat(path); err != nil {
		delete(e.workspace, path)
	}
	e.linked = false
}

// FileChanged notifies the engine that a file changed on disk (created,
// modified or deleted).
func (e *Engine) FileChanged(path string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	path = filepath.Clean(path)
	delete(e.files, path)
	if filepath.Base(path) == ProjectConfigFile {
		e.applySettings(e.settingsWithIDERepos())
	}
	if isYAML(path) && e.underRoots(path) {
		if _, err := os.Stat(path); err == nil {
			e.workspace[path] = true
		} else if _, open := e.overlay[path]; !open {
			delete(e.workspace, path)
		}
	}
	e.linked = false
}

func (e *Engine) settingsWithIDERepos() Settings {
	s := e.settings
	s.Repositories = e.ideRepos
	return s
}

// Invalidate marks the index stale (e.g. after a repository fetch).
func (e *Engine) Invalidate() {
	e.mu.Lock()
	e.linked = false
	e.rootCache = nil
	// cached repository files may have been replaced
	for p, f := range e.files {
		if f.fromDisk && !e.workspace[p] {
			delete(e.files, p)
		}
	}
	e.mu.Unlock()
}

// Rescan rebuilds the workspace file list from disk.
func (e *Engine) Rescan() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.workspace = map[string]bool{}
	e.files = map[string]*fileInfo{}
	e.rootCache, e.originCache = nil, nil
	for _, root := range e.roots {
		for _, f := range collectYAML(root) {
			e.workspace[f] = true
		}
	}
	for p := range e.overlay {
		if isYAML(p) && e.underRoots(p) {
			e.workspace[p] = true
		}
	}
	e.linked = false
}

var skipDirs = map[string]bool{
	".git": true, "node_modules": true, ".vscode": true, ".idea": true, "dist": true, "out": true,
	"build": true, "target": true, "bin": true, "obj": true, ".terraform": true, ".venv": true,
	"venv": true, "__pycache__": true, ".gradle": true, ".intellijPlatform": true, ".vscode-test": true,
}

func collectYAML(root string) []string {
	var out []string
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			if p != root && (skipDirs[info.Name()] || strings.HasSuffix(info.Name(), ".partial")) {
				return filepath.SkipDir
			}
			return nil
		}
		if isYAML(p) {
			out = append(out, filepath.Clean(p))
		}
		return nil
	})
	sort.Strings(out)
	return out
}

func isYAML(p string) bool {
	ext := strings.ToLower(filepath.Ext(p))
	return ext == ".yml" || ext == ".yaml"
}

func (e *Engine) underRoots(p string) bool {
	for _, r := range e.roots {
		if p == r || strings.HasPrefix(p, r+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// file returns the parsed file (from overlay or disk), or nil if unreadable.
// Caller must hold e.mu.
func (e *Engine) file(path string) *fileInfo {
	path = filepath.Clean(path)
	if text, ok := e.overlay[path]; ok {
		if f, ok := e.files[path]; ok && !f.fromDisk && f.text == text {
			return f
		}
		f := parseFile(path, text)
		e.files[path] = f
		return f
	}
	st, err := os.Stat(path)
	if err != nil || st.IsDir() {
		delete(e.files, path)
		return nil
	}
	if f, ok := e.files[path]; ok && f.fromDisk && f.modTime.Equal(st.ModTime()) && f.size == st.Size() {
		return f
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	f := parseFile(path, string(b))
	f.fromDisk, f.modTime, f.size = true, st.ModTime(), st.Size()
	e.files[path] = f
	return f
}

func parseFile(path, text string) *fileInfo {
	lines := pipeline.SplitLines(text)
	return &fileInfo{
		path:       path,
		text:       text,
		lines:      lines,
		params:     pipeline.ParseParameters(lines),
		refs:       pipeline.FindTemplateRefs(lines),
		repos:      pipeline.ParseRepositories(lines),
		isPipeline: pipeline.IsPipelineRoot(lines),
	}
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// ---------------------------------------------------------------------------
// Index (call graph)
// ---------------------------------------------------------------------------

// ensureLinked (re)builds the alias registry and the call graph if stale.
// Caller must hold e.mu.
func (e *Engine) ensureLinked() {
	if e.linked {
		return
	}
	e.aliases = map[string]pipeline.RepoDecl{}
	e.aliasFrom = map[string]string{}
	e.diagCache = map[string][]Diagnostic{}
	paths := e.workspaceFiles()
	for _, p := range paths {
		f := e.file(p)
		if f == nil {
			continue
		}
		for a, d := range f.repos {
			if _, ok := e.aliases[a]; !ok {
				e.aliases[a] = d
				e.aliasFrom[a] = p
			}
		}
	}
	e.forward = map[string][]edge{}
	e.reverse = map[string]map[string]bool{}
	for _, p := range paths {
		f := e.file(p)
		if f == nil {
			continue
		}
		e.linkFile(f)
	}
	e.linked = true
}

func (e *Engine) linkFile(f *fileInfo) {
	var edges []edge
	for _, r := range f.refs {
		res := e.resolve(r, f)
		ed := edge{ref: r, res: res}
		if res.Path != "" && res.Exists {
			ed.target = res.Path
			if e.reverse[res.Path] == nil {
				e.reverse[res.Path] = map[string]bool{}
			}
			e.reverse[res.Path][f.path] = true
		}
		edges = append(edges, ed)
	}
	e.forward[f.path] = edges
}

// edgesOf returns the outgoing references of any file (workspace or not).
func (e *Engine) edgesOf(path string) []edge {
	if ed, ok := e.forward[path]; ok {
		return ed
	}
	f := e.file(path)
	if f == nil {
		return nil
	}
	var edges []edge
	for _, r := range f.refs {
		res := e.resolve(r, f)
		ed := edge{ref: r, res: res}
		if res.Path != "" && res.Exists {
			ed.target = res.Path
		}
		edges = append(edges, ed)
	}
	return edges
}

func (e *Engine) workspaceFiles() []string {
	out := make([]string, 0, len(e.workspace))
	for p := range e.workspace {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// WorkspaceFiles returns all indexed YAML files.
func (e *Engine) WorkspaceFiles() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.workspaceFiles()
}

func (e *Engine) callers(path string) []string {
	var out []string
	for c := range e.reverse[path] {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// relPath returns a display path relative to the closest root.
func (e *Engine) relPath(p string) string {
	best := ""
	for _, r := range e.roots {
		if strings.HasPrefix(p, r+string(filepath.Separator)) && len(r) > len(best) {
			best = r
		}
	}
	if best != "" {
		if rel, err := filepath.Rel(best, p); err == nil {
			return filepath.ToSlash(rel)
		}
	}
	cache := e.settings.Repositories.CacheDir
	if cache != "" && strings.HasPrefix(p, cache+string(filepath.Separator)) {
		rel, _ := filepath.Rel(cache, p)
		return "⬡ " + filepath.ToSlash(rel)
	}
	return filepath.ToSlash(p)
}

// LineText returns one line of a file ("" when unavailable).
func (e *Engine) LineText(path string, line int) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	f := e.file(path)
	if f == nil || line < 0 || line >= len(f.lines) {
		return ""
	}
	return f.lines[line]
}

// SetRoots replaces the workspace roots (call Rescan afterwards).
func (e *Engine) SetRoots(roots []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.roots = nil
	for _, r := range roots {
		if abs, err := filepath.Abs(r); err == nil {
			e.roots = append(e.roots, abs)
		}
	}
	e.applySettings(e.settingsWithIDERepos())
	e.linked = false
}

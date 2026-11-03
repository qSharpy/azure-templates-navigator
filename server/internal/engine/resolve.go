package engine

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/qSharpy/azure-templates-navigator/server/internal/pipeline"
	"github.com/qSharpy/azure-templates-navigator/server/internal/repos"
)

// ResolutionKind classifies a template reference.
type ResolutionKind string

const (
	KindLocal           ResolutionKind = "local"           // same repository
	KindExternal        ResolutionKind = "external"        // @alias, repository available
	KindRepoUnavailable ResolutionKind = "repoUnavailable" // @alias, repository not available (yet)
	KindUnknownAlias    ResolutionKind = "unknownAlias"    // @alias not declared anywhere
	KindDynamic         ResolutionKind = "dynamic"         // contains an expression
)

// Resolution is the result of resolving a template reference.
type Resolution struct {
	Kind     ResolutionKind
	Ref      string
	Path     string // absolute target path (may not exist)
	Exists   bool
	Alias    string
	Repo     *pipeline.RepoDecl
	RepoLoc  *repos.Location
	AliasSrc string // file declaring the alias (when not the current file)
}

// FindRepoRoot walks up from dir to the nearest directory containing .git.
// Falls back to dir.
func FindRepoRoot(dir string) string {
	d := dir
	for {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		parent := filepath.Dir(d)
		if parent == d {
			return dir
		}
		d = parent
	}
}

// resolve resolves a template reference made from file f. Caller holds e.mu.
func (e *Engine) resolve(r pipeline.TemplateRef, f *fileInfo) Resolution {
	res := Resolution{Ref: r.Ref}
	if r.IsDynamic() {
		res.Kind = KindDynamic
		return res
	}
	path, alias := pipeline.SplitRef(r.Ref)
	res.Alias = alias
	if alias == "" || alias == "self" {
		res.Kind = KindLocal
		res.Path = resolveLocal(path, f.path)
		res.Exists = fileExists(res.Path)
		return res
	}

	decl, ok := f.repos[alias]
	if !ok {
		e.ensureAliases()
		if d, found := e.aliases[alias]; found {
			decl, ok = d, true
			res.AliasSrc = e.aliasFrom[alias]
		}
	}
	if !ok {
		res.Kind = KindUnknownAlias
		return res
	}
	res.Repo = &decl

	repoRoot := e.repoRoot(filepath.Dir(f.path))
	parents := []string{filepath.Dir(repoRoot)}
	for _, root := range e.roots {
		parents = append(parents, filepath.Dir(e.repoRoot(root)))
	}
	loc := e.Repos.Locate(decl, parents, e.remoteFor(f.path))
	res.RepoLoc = &loc
	if loc.State != repos.StateReady {
		res.Kind = KindRepoUnavailable
		return res
	}
	res.Kind = KindExternal
	res.Path = filepath.Join(loc.Dir, filepath.FromSlash(strings.TrimLeft(path, "/")))
	res.Exists = fileExists(res.Path)
	return res
}

// ensureAliases builds the alias registry without building the full call
// graph (used while linking, when the registry may be partially built).
func (e *Engine) ensureAliases() {
	if e.aliases != nil {
		return
	}
	e.aliases = map[string]pipeline.RepoDecl{}
	e.aliasFrom = map[string]string{}
	for _, p := range e.workspaceFiles() {
		if f := e.file(p); f != nil {
			for a, d := range f.repos {
				if _, ok := e.aliases[a]; !ok {
					e.aliases[a] = d
					e.aliasFrom[a] = p
				}
			}
		}
	}
}

// remoteFor returns the origin URL of the workspace repository relevant to
// a file, used to infer hosts / organizations for clone URLs.
func (e *Engine) remoteFor(path string) string {
	if e.underRoots(path) {
		if u := e.origin(e.repoRoot(filepath.Dir(path))); u != "" {
			return u
		}
	}
	for _, r := range e.roots {
		if u := e.origin(e.repoRoot(r)); u != "" {
			return u
		}
	}
	return ""
}

// repoRoot is a memoised FindRepoRoot. Caller holds e.mu.
func (e *Engine) repoRoot(dir string) string {
	if e.rootCache == nil {
		e.rootCache = map[string]string{}
	}
	if r, ok := e.rootCache[dir]; ok {
		return r
	}
	r := FindRepoRoot(dir)
	e.rootCache[dir] = r
	return r
}

// origin is a memoised repos.OriginURL. Caller holds e.mu.
func (e *Engine) origin(repoRoot string) string {
	if e.originCache == nil {
		e.originCache = map[string]string{}
	}
	if u, ok := e.originCache[repoRoot]; ok {
		return u
	}
	u := repos.OriginURL(repoRoot)
	e.originCache[repoRoot] = u
	return u
}

func resolveLocal(ref, currentFile string) string {
	if strings.HasPrefix(ref, "/") {
		root := FindRepoRoot(filepath.Dir(currentFile))
		return filepath.Join(root, filepath.FromSlash(strings.TrimLeft(ref, "/")))
	}
	return filepath.Join(filepath.Dir(currentFile), filepath.FromSlash(ref))
}

// Resolve resolves the template reference on a given line of a file.
func (e *Engine) Resolve(path string, line int) (Resolution, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	f := e.file(path)
	if f == nil || line < 0 || line >= len(f.lines) {
		return Resolution{}, false
	}
	r, ok := pipeline.TemplateRefOnLine(f.lines[line], line)
	if !ok {
		return Resolution{}, false
	}
	return e.resolve(r, f), true
}

// RepoRequest identifies a repository to fetch.
type RepoRequest struct {
	Alias string `json:"alias"`
	File  string `json:"file"` // file whose resources declare the alias (optional)
}

// RepoDeclFor finds the declaration for an alias as seen from a file.
func (e *Engine) RepoDeclFor(req RepoRequest) (pipeline.RepoDecl, string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if req.File != "" {
		if f := e.file(req.File); f != nil {
			if d, ok := f.repos[req.Alias]; ok {
				return d, e.remoteFor(req.File), true
			}
		}
	}
	e.ensureLinked()
	d, ok := e.aliases[req.Alias]
	if !ok {
		return pipeline.RepoDecl{}, "", false
	}
	return d, e.remoteFor(e.aliasFrom[req.Alias]), true
}

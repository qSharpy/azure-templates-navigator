// Package repos makes templates from other repositories (`template: x@alias`)
// available locally without the user cloning anything by hand.
//
// A repository declared under `resources.repositories` is located, in order:
//  1. an explicit local path from settings (repositories.localPaths)
//  2. a sibling clone next to the current repository (legacy behaviour,
//     when repositories.preferLocalClones is on) — your working copy wins
//  3. the shared cache (repositories.cacheDir), populated by a shallow
//     `git fetch` that uses the user's normal git credentials
package repos

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/qSharpy/azure-templates-navigator/server/internal/pipeline"
)

// Config controls repository resolution and fetching.
type Config struct {
	CacheDir                string            `json:"cacheDir"`
	AutoFetch               bool              `json:"autoFetch"`
	RefreshIntervalHours    float64           `json:"refreshIntervalHours"`
	PreferLocalClones       bool              `json:"preferLocalClones"`
	URLs                    map[string]string `json:"urls"`       // alias | name | repoName -> clone URL
	LocalPaths              map[string]string `json:"localPaths"` // alias | name | repoName -> directory
	AzureDevOpsOrganization string            `json:"azureDevOpsOrganization"`
	AzureDevOpsProject      string            `json:"azureDevOpsProject"`
	GitHubEnterpriseHost    string            `json:"githubEnterpriseHost"`
	GitPath                 string            `json:"gitPath"`
	FetchTimeoutSeconds     int               `json:"fetchTimeoutSeconds"`
}

// DefaultConfig returns the default configuration.
func DefaultConfig() Config {
	return Config{
		CacheDir:             DefaultCacheDir(),
		AutoFetch:            true,
		RefreshIntervalHours: 12,
		PreferLocalClones:    true,
		URLs:                 map[string]string{},
		LocalPaths:           map[string]string{},
		GitPath:              "git",
		FetchTimeoutSeconds:  120,
	}
}

// DefaultCacheDir is ~/.azure-templates-navigator/repos (overridable with
// the ATN_CACHE_DIR environment variable).
func DefaultCacheDir() string {
	if d := os.Getenv("ATN_CACHE_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "azure-templates-navigator", "repos")
	}
	return filepath.Join(home, ".azure-templates-navigator", "repos")
}

// Source says where a repository's files came from.
type Source string

const (
	SourceNone    Source = ""
	SourceLocal   Source = "local"   // configured local path
	SourceSibling Source = "sibling" // clone next to the workspace repo
	SourceCache   Source = "cache"   // fetched into the shared cache
)

// State is the availability of a repository.
type State string

const (
	StateReady        State = "ready"
	StateMissing      State = "missing" // not fetched yet
	StateFetching     State = "fetching"
	StateFailed       State = "failed"
	StateUnconfigured State = "unconfigured" // clone URL can't be derived
)

// Location is the result of locating a repository.
type Location struct {
	Dir       string    `json:"dir,omitempty"`
	Source    Source    `json:"source,omitempty"`
	State     State     `json:"state"`
	URL       string    `json:"url,omitempty"`
	Error     string    `json:"error,omitempty"`
	FetchedAt time.Time `json:"fetchedAt,omitempty"`
	Commit    string    `json:"commit,omitempty"`
}

type meta struct {
	URL       string    `json:"url"`
	Ref       string    `json:"ref"`
	Commit    string    `json:"commit"`
	FetchedAt time.Time `json:"fetchedAt"`
}

type entry struct {
	fetching bool
	err      string
	lastTry  time.Time
}

// Manager locates and fetches repositories. It is safe for concurrent use.
type Manager struct {
	mu       sync.Mutex
	cfg      Config
	entries  map[string]*entry // cache dir -> state
	onChange func()
	log      func(string)
}

// NewManager creates a Manager.
func NewManager(cfg Config) *Manager {
	return &Manager{cfg: normalise(cfg), entries: map[string]*entry{}, onChange: func() {}, log: func(string) {}}
}

// SetConfig replaces the configuration.
func (m *Manager) SetConfig(cfg Config) {
	m.mu.Lock()
	m.cfg = normalise(cfg)
	m.mu.Unlock()
}

// Config returns the current configuration.
func (m *Manager) Config() Config {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg
}

// OnChange registers a callback fired (from a background goroutine) whenever
// a fetch finishes or fails.
func (m *Manager) OnChange(f func()) { m.onChange = f }

// SetLogger registers a log sink.
func (m *Manager) SetLogger(f func(string)) { m.log = f }

func normalise(c Config) Config {
	if c.CacheDir == "" {
		c.CacheDir = DefaultCacheDir()
	}
	c.CacheDir = expandHome(c.CacheDir)
	if c.URLs == nil {
		c.URLs = map[string]string{}
	}
	if c.LocalPaths == nil {
		c.LocalPaths = map[string]string{}
	}
	if c.GitPath == "" {
		c.GitPath = "git"
	}
	if c.FetchTimeoutSeconds <= 0 {
		c.FetchTimeoutSeconds = 120
	}
	return c
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[1:])
		}
	}
	return p
}

var unsafeChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// CacheDirFor returns the cache directory for a clone URL + ref.
func CacheDirFor(cacheRoot, cloneURL, ref string) string {
	r, ok := ParseRemote(cloneURL)
	var parts []string
	if ok && r.Host != "" {
		parts = append(parts, r.Host)
		for _, p := range []string{r.Org, r.Project, r.Repo} {
			if p != "" {
				parts = append(parts, p)
			}
		}
	} else {
		parts = append(parts, "other", strings.TrimSuffix(filepath.Base(cloneURL), ".git"))
	}
	ref = strings.TrimPrefix(strings.TrimPrefix(ref, "refs/heads/"), "refs/")
	if ref == "" {
		ref = "_default"
	}
	parts = append(parts, ref)
	for i, p := range parts {
		if u, err := url.PathUnescape(p); err == nil {
			p = u
		}
		parts[i] = strings.Trim(unsafeChars.ReplaceAllString(p, "_"), "_")
		if parts[i] == "" {
			parts[i] = "_"
		}
	}
	return filepath.Join(append([]string{cacheRoot}, parts...)...)
}

// Locate finds where a repository's files are. searchParents are directories
// in which a sibling clone named after the repository may exist.
// When the repository is not available and autoFetch is enabled a background
// fetch is started; OnChange fires when it completes.
func (m *Manager) Locate(decl pipeline.RepoDecl, searchParents []string, workspaceRemote string) Location {
	cfg := m.Config()

	for _, k := range []string{decl.Alias, decl.Name, decl.RepoName()} {
		if p, ok := cfg.LocalPaths[k]; ok && p != "" {
			p = expandHome(p)
			if isDir(p) {
				return Location{Dir: p, Source: SourceLocal, State: StateReady}
			}
			return Location{Dir: p, Source: SourceLocal, State: StateFailed, Error: fmt.Sprintf("configured local path does not exist: %s", p)}
		}
	}

	if cfg.PreferLocalClones {
		seen := map[string]bool{}
		for _, parent := range searchParents {
			if parent == "" || seen[parent] {
				continue
			}
			seen[parent] = true
			for _, name := range []string{decl.RepoName(), decl.Alias} {
				if name == "" {
					continue
				}
				d := filepath.Join(parent, name)
				if isDir(d) && !strings.HasPrefix(d, cfg.CacheDir) {
					return Location{Dir: d, Source: SourceSibling, State: StateReady}
				}
			}
		}
	}

	cloneURL, err := CloneURL(decl, cfg, workspaceRemote)
	if err != nil {
		return Location{State: StateUnconfigured, Error: err.Error()}
	}
	dir := CacheDirFor(cfg.CacheDir, cloneURL, decl.Ref)
	loc := Location{Dir: dir, Source: SourceCache, URL: cloneURL}

	m.mu.Lock()
	e := m.entries[dir]
	if e == nil {
		e = &entry{}
		m.entries[dir] = e
	}
	fetching, lastErr := e.fetching, e.err
	m.mu.Unlock()

	if isDir(filepath.Join(dir, ".git")) {
		loc.State = StateReady
		if md, ok := readMeta(dir); ok {
			loc.FetchedAt, loc.Commit = md.FetchedAt, md.Commit
			if cfg.AutoFetch && cfg.RefreshIntervalHours > 0 && !fetching &&
				time.Since(md.FetchedAt) > time.Duration(cfg.RefreshIntervalHours*float64(time.Hour)) {
				m.startFetch(decl, cloneURL, dir, false)
			}
		}
		return loc
	}

	switch {
	case fetching:
		loc.State = StateFetching
	case lastErr != "":
		loc.State = StateFailed
		loc.Error = lastErr
	default:
		loc.State = StateMissing
		if cfg.AutoFetch {
			if m.startFetch(decl, cloneURL, dir, false) {
				loc.State = StateFetching
			}
		}
	}
	return loc
}

// Fetch starts (or restarts) a background fetch. Returns false when one is
// already running.
func (m *Manager) Fetch(decl pipeline.RepoDecl, workspaceRemote string) (Location, error) {
	cfg := m.Config()
	cloneURL, err := CloneURL(decl, cfg, workspaceRemote)
	if err != nil {
		return Location{State: StateUnconfigured, Error: err.Error()}, err
	}
	dir := CacheDirFor(cfg.CacheDir, cloneURL, decl.Ref)
	m.mu.Lock()
	if e := m.entries[dir]; e != nil {
		e.err = "" // explicit retry clears the failure
	}
	m.mu.Unlock()
	m.startFetch(decl, cloneURL, dir, true)
	return Location{Dir: dir, Source: SourceCache, URL: cloneURL, State: StateFetching}, nil
}

// FetchSync fetches a repository and waits for completion.
func (m *Manager) FetchSync(decl pipeline.RepoDecl, workspaceRemote string) (Location, error) {
	cfg := m.Config()
	cloneURL, err := CloneURL(decl, cfg, workspaceRemote)
	if err != nil {
		return Location{State: StateUnconfigured, Error: err.Error()}, err
	}
	dir := CacheDirFor(cfg.CacheDir, cloneURL, decl.Ref)
	commit, err := m.gitFetch(cfg, cloneURL, decl.Ref, dir)
	if err != nil {
		return Location{Dir: dir, Source: SourceCache, URL: cloneURL, State: StateFailed, Error: err.Error()}, err
	}
	return Location{Dir: dir, Source: SourceCache, URL: cloneURL, State: StateReady, Commit: commit, FetchedAt: time.Now()}, nil
}

func (m *Manager) startFetch(decl pipeline.RepoDecl, cloneURL, dir string, explicit bool) bool {
	m.mu.Lock()
	e := m.entries[dir]
	if e == nil {
		e = &entry{}
		m.entries[dir] = e
	}
	if e.fetching {
		m.mu.Unlock()
		return false
	}
	// Don't hammer a failing remote with automatic retries.
	if !explicit && e.err != "" && time.Since(e.lastTry) < 10*time.Minute {
		m.mu.Unlock()
		return false
	}
	e.fetching = true
	e.lastTry = time.Now()
	cfg := m.cfg
	m.mu.Unlock()

	go func() {
		m.log(fmt.Sprintf("fetching %s (%s) -> %s", cloneURL, refOrDefault(decl.Ref), dir))
		_, err := m.gitFetch(cfg, cloneURL, decl.Ref, dir)
		m.mu.Lock()
		e.fetching = false
		if err != nil {
			e.err = err.Error()
			m.log("fetch failed: " + e.err)
		} else {
			e.err = ""
			m.log("fetched " + cloneURL)
		}
		m.mu.Unlock()
		m.onChange()
	}()
	return true
}

func refOrDefault(ref string) string {
	if ref == "" {
		return "default branch"
	}
	return ref
}

// gitFetch performs a shallow fetch of ref into a fresh directory and swaps
// it into place atomically-ish, so readers never see a half-written tree.
func (m *Manager) gitFetch(cfg Config, cloneURL, ref, dir string) (string, error) {
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return "", err
	}
	tmp := dir + ".partial"
	_ = os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.FetchTimeoutSeconds)*time.Second)
	defer cancel()

	run := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, cfg.GitPath, args...)
		cmd.Dir = tmp
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=", "SSH_ASKPASS=", "GIT_SSH_COMMAND="+sshCommand())
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		if err := cmd.Run(); err != nil {
			msg := strings.TrimSpace(errb.String())
			if ctx.Err() == context.DeadlineExceeded {
				msg = "timed out"
			}
			if msg == "" {
				msg = err.Error()
			}
			return "", fmt.Errorf("git %s: %s", args[0], explainGitError(msg, cloneURL))
		}
		return strings.TrimSpace(out.String()), nil
	}

	fetchRef := ref
	if fetchRef == "" {
		fetchRef = "HEAD"
	}
	steps := [][]string{
		{"init", "-q"},
		{"fetch", "--depth", "1", "--no-tags", "-q", cloneURL, fetchRef},
		{"-c", "advice.detachedHead=false", "checkout", "-q", "FETCH_HEAD"},
	}
	for _, s := range steps {
		if _, err := run(s...); err != nil {
			_ = os.RemoveAll(tmp)
			return "", err
		}
	}
	commit, _ := run("rev-parse", "HEAD")
	b, _ := json.MarshalIndent(meta{URL: cloneURL, Ref: ref, Commit: commit, FetchedAt: time.Now().UTC()}, "", "  ")
	_ = os.WriteFile(filepath.Join(tmp, ".git", "atn-meta.json"), b, 0o644)

	old := dir + ".old"
	_ = os.RemoveAll(old)
	if isDir(dir) {
		if err := os.Rename(dir, old); err != nil {
			_ = os.RemoveAll(dir)
		}
	}
	if err := os.Rename(tmp, dir); err != nil {
		return "", err
	}
	_ = os.RemoveAll(old)
	return commit, nil
}

func sshCommand() string {
	if v := os.Getenv("GIT_SSH_COMMAND"); v != "" {
		return v
	}
	// Never block on an interactive host-key / passphrase prompt.
	return "ssh -o BatchMode=yes"
}

func explainGitError(msg, cloneURL string) string {
	low := strings.ToLower(msg)
	switch {
	case strings.Contains(low, "authentication failed"), strings.Contains(low, "could not read username"),
		strings.Contains(low, "terminal prompts disabled"), strings.Contains(low, "permission denied"),
		strings.Contains(low, "403"), strings.Contains(low, "401"):
		return fmt.Sprintf("%s — git has no credentials for %s. Run `git clone %s` once in a terminal (or `gh auth setup-git` / sign in with Git Credential Manager), then retry.", firstLine(msg), cloneURL, cloneURL)
	case strings.Contains(low, "repository not found"), strings.Contains(low, "not found"), strings.Contains(low, "does not exist"):
		return fmt.Sprintf("%s — check the repository name, or that your account can access %s.", firstLine(msg), cloneURL)
	case strings.Contains(low, "couldn't find remote ref"):
		return fmt.Sprintf("%s — the ref does not exist in %s.", firstLine(msg), cloneURL)
	case strings.Contains(low, "executable file not found"):
		return "git is not installed or not on PATH (set repositories.gitPath)"
	}
	return firstLine(msg)
}

func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "hint:") {
			return l
		}
	}
	return s
}

func readMeta(dir string) (meta, bool) {
	b, err := os.ReadFile(filepath.Join(dir, ".git", "atn-meta.json"))
	if err != nil {
		return meta{}, false
	}
	var md meta
	if json.Unmarshal(b, &md) != nil {
		return meta{}, false
	}
	return md, true
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// CachedRepo is an entry found in the cache directory.
type CachedRepo struct {
	Dir       string    `json:"dir"`
	URL       string    `json:"url"`
	Ref       string    `json:"ref"`
	Commit    string    `json:"commit"`
	FetchedAt time.Time `json:"fetchedAt"`
}

// ListCache lists repositories present in the cache.
func (m *Manager) ListCache() []CachedRepo {
	root := m.Config().CacheDir
	var out []CachedRepo
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || !info.IsDir() {
			return nil
		}
		if isDir(filepath.Join(p, ".git")) {
			if md, ok := readMeta(p); ok {
				out = append(out, CachedRepo{Dir: p, URL: md.URL, Ref: md.Ref, Commit: md.Commit, FetchedAt: md.FetchedAt})
			}
			return filepath.SkipDir
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Dir < out[j].Dir })
	return out
}

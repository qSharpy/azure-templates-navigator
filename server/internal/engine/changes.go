package engine

// Changes: "what did my work change?". The workspace (with open, unsaved
// documents) is compared against the commit the work started from (see
// package gitbase for how that is found). That commit's YAML is exported
// once and indexed by a second, read-only engine. Every root pipeline's
// problems are then compared between the two, ignoring line numbers, so a
// pipeline that already had 40 errors shows the 2 your change added.

import (
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/qSharpy/azure-templates-navigator/server/internal/gitbase"
)

// ChangedFile is a workspace file that differs from the base.
type ChangedFile struct {
	Path         string `json:"path"`
	RelativePath string `json:"relativePath"`
	Status       string `json:"status"` // added | modified | deleted
	// Pipelines is the number of root pipelines that reach the file now (or
	// reached it at the base, when it was deleted).
	Pipelines int `json:"pipelines"`
	// The file's own problems (errors and warnings) that are new or gone.
	New   []Problem `json:"new"`
	Fixed []Problem `json:"fixed"`
}

// PipelineVerdict is a pipeline's outcome at one point in time.
type PipelineVerdict struct {
	Exists   bool   `json:"exists"`
	Verdict  string `json:"verdict"` // fail | warn | ok ("" when it didn't exist)
	Errors   int    `json:"errors"`
	Warnings int    `json:"warnings"`
}

// PipelineChange is a root pipeline your work touched: it reaches a changed
// file, or its problems differ from the base.
type PipelineChange struct {
	Path         string `json:"path"`
	RelativePath string `json:"relativePath"`
	// Status: "added" (a new pipeline), "removed" (gone or no longer a
	// pipeline), "changed" (it reaches a file you changed) or "affected"
	// (it doesn't, but its problems changed, e.g. through an alias
	// declared elsewhere).
	Status string          `json:"status"`
	Now    PipelineVerdict `json:"now"`
	Before PipelineVerdict `json:"before"`
	// Problems that are new, gone, or in both (Existing, a count).
	New      []Problem `json:"new"`
	Fixed    []Problem `json:"fixed"`
	Existing int       `json:"existing"`
	// Changed files it reaches (now, or at the base), relative paths.
	Changed []string `json:"changed"`
}

// Title is a one-line summary, e.g. "+2 new · 1 fixed · 38 already there".
func (c PipelineChange) Title() string {
	var parts []string
	if len(c.New) > 0 {
		parts = append(parts, fmt.Sprintf("+%d new", len(c.New)))
	}
	if len(c.Fixed) > 0 {
		parts = append(parts, fmt.Sprintf("%d fixed", len(c.Fixed)))
	}
	switch c.Status {
	case "added":
		if len(parts) == 0 {
			parts = append(parts, "new pipeline, no problems")
		} else {
			parts = append([]string{"new pipeline"}, parts...)
		}
	case "removed":
		return "removed"
	}
	if len(parts) == 0 {
		parts = append(parts, "no new problems")
	}
	if c.Existing > 0 {
		parts = append(parts, fmt.Sprintf("%d already there", c.Existing))
	}
	return strings.Join(parts, " · ")
}

// ChangeBase is the base of one git repository in the workspace.
type ChangeBase struct {
	gitbase.Base
	Repo  string `json:"repo"`  // repository folder name
	Top   string `json:"top"`   // repository top-level directory
	Short string `json:"short"` // abbreviated commit id
	Error string `json:"error,omitempty"`
}

// ChangesReport answers "what did my work change?".
type ChangesReport struct {
	// Available is false outside git (Reason says why); nothing else is set.
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`

	Bases     []ChangeBase     `json:"bases"`
	Label     string           `json:"label"` // "feature/story @ 3f2a1c4"
	Files     []ChangedFile    `json:"files"`
	Pipelines []PipelineChange `json:"pipelines"` // most new problems first

	// Totals. Problems are counted once even when several pipelines
	// reach the template they are in.
	NewErrors     int `json:"newErrors"`
	NewWarnings   int `json:"newWarnings"`
	FixedProblems int `json:"fixedProblems"`
	NowFailing    int `json:"nowFailing"`   // pipelines that fail now but didn't at the base
	NoLonger      int `json:"noLonger"`     // pipelines that failed at the base but don't now
	AllPipelines  int `json:"allPipelines"` // root pipelines in the workspace
	AllFailing    int `json:"allFailing"`   // of which fail

	gen int // head diagnostics generation it was computed for
}

// Summary is a one-line summary of the totals.
func (r ChangesReport) Summary() string {
	if !r.Available {
		return r.Reason
	}
	if len(r.Files) == 0 {
		return "No changes"
	}
	var parts []string
	parts = append(parts, fmt.Sprintf("%d file%s changed", len(r.Files), plural(len(r.Files))))
	parts = append(parts, fmt.Sprintf("%d pipeline%s affected", len(r.Pipelines), plural(len(r.Pipelines))))
	switch n := r.NewErrors + r.NewWarnings; {
	case n == 0:
		parts = append(parts, "no new problems")
	case r.NewWarnings == 0:
		parts = append(parts, fmt.Sprintf("%d new error%s", r.NewErrors, plural(r.NewErrors)))
	case r.NewErrors == 0:
		parts = append(parts, fmt.Sprintf("%d new warning%s", r.NewWarnings, plural(r.NewWarnings)))
	default:
		parts = append(parts, fmt.Sprintf("%d new error%s, %d new warning%s", r.NewErrors, plural(r.NewErrors), r.NewWarnings, plural(r.NewWarnings)))
	}
	if r.FixedProblems > 0 {
		parts = append(parts, fmt.Sprintf("%d fixed", r.FixedProblems))
	}
	return strings.Join(parts, " · ")
}

// Change returns a pipeline's entry, if your work touched it.
func (r *ChangesReport) Change(path string) (PipelineChange, bool) {
	if r == nil {
		return PipelineChange{}, false
	}
	for _, c := range r.Pipelines {
		if c.Path == path {
			return c, true
		}
	}
	return PipelineChange{}, false
}

// BaseChoice is one entry of the "compare against" picker.
type BaseChoice struct {
	Ref    string `json:"ref"` // "" = automatic, "HEAD" = uncommitted only
	Label  string `json:"label"`
	Detail string `json:"detail"`
	Picked bool   `json:"picked"`
}

// ---------------------------------------------------------------------------
// state
// ---------------------------------------------------------------------------

// baseResolveTTL is how long a resolved base is trusted before git is asked
// again (a commit or a checkout can move it).
const baseResolveTTL = 10 * time.Second

type repoBase struct {
	repo   *gitbase.Repo
	base   gitbase.Base
	err    error
	export string // exported top-level directory ("" when not exported)
}

type changeState struct {
	repos      []*repoBase
	resolvedAt time.Time
	nonGit     []string // roots outside git
	key        string   // what the baseline engine was built for
	base       *Engine  // baseline engine
}

// baselineRoot is where base commits are exported: BaselineDir, else
// $ATN_BASELINE_DIR, else the user cache directory.
func (e *Engine) baselineRoot() string {
	if e.BaselineDir != "" {
		return e.BaselineDir
	}
	if d := os.Getenv("ATN_BASELINE_DIR"); d != "" {
		return d
	}
	if d, err := os.UserCacheDir(); err == nil {
		return filepath.Join(d, "azure-templates-navigator", "baselines")
	}
	return filepath.Join(os.TempDir(), "azure-templates-navigator-baselines")
}

// Changes compares the workspace with the base chosen or guessed for each
// git repository in it.
func (e *Engine) Changes() ChangesReport {
	r, _ := e.changes("", false)
	return r
}

// ChangesSince compares the workspace with ref ("auto" or "" guesses, as
// Changes; "HEAD" compares with the last commit) without storing it. Unlike
// Changes it fails when ref can't be used (outside git, an unknown ref, a
// shallow clone without the common commit), so a CI gate never passes by
// comparing with the wrong commit.
func (e *Engine) ChangesSince(ref string) (ChangesReport, error) {
	if ref == "auto" {
		ref = ""
	}
	return e.changes(ref, true)
}

// LastChanges is the report computed most recently ("nil" when none). It
// never runs git, so it is safe where the engine lock is held.
func (e *Engine) LastChanges() *ChangesReport {
	e.lastMu.Lock()
	defer e.lastMu.Unlock()
	return e.lastCh
}

func (e *Engine) changes(override string, explicit bool) (ChangesReport, error) {
	e.chMu.Lock()
	defer e.chMu.Unlock()
	s := e.Settings()
	off := ""
	switch {
	case !s.Diagnostics.Enabled:
		off = "Diagnostics are turned off"
	case !s.Changes.Enabled && !explicit:
		off = "Changes are turned off"
	}
	if off != "" {
		r := ChangesReport{Reason: off}
		e.storeChanges(&r, explicit)
		return r, fmt.Errorf("%s", off)
	}
	e.mu.Lock()
	e.ensureLinked()
	gen := e.diagGen
	e.mu.Unlock()

	if !explicit && e.ch != nil && time.Since(e.ch.resolvedAt) < baseResolveTTL {
		if last := e.LastChanges(); last != nil && last.gen == gen {
			return *last, nil
		}
	}
	st := e.resolveBases(override, s)
	if explicit {
		if len(st.repos) == 0 {
			return ChangesReport{}, fmt.Errorf("not a git repository")
		}
		for _, rb := range st.repos {
			if rb.err != nil {
				return ChangesReport{}, rb.err
			}
			if override == "" && rb.base.Source == "head" {
				return ChangesReport{}, fmt.Errorf("couldn't tell where this branch started (%s). Name the base instead: a branch such as origin/main, or HEAD for uncommitted changes only", strings.ToLower(rb.base.Detail[:1])+rb.base.Detail[1:])
			}
		}
	}
	if !explicit && e.ch != nil && st.baseKey(s) == e.ch.key {
		if last := e.LastChanges(); last != nil && last.gen == gen {
			e.ch.resolvedAt = st.resolvedAt
			return *last, nil
		}
	}
	if len(st.repos) == 0 {
		r := ChangesReport{Reason: "Not a git repository: there is no commit to compare with"}
		e.storeChanges(&r, explicit)
		return r, fmt.Errorf("not a git repository")
	}
	if err := e.ensureBaseline(st, s); err != nil {
		r := ChangesReport{Reason: "Couldn't read the base commit: " + err.Error()}
		e.storeChanges(&r, explicit)
		return r, err
	}
	if !explicit {
		e.ch = st
	}
	r := e.compare(st)
	r.gen = gen
	e.storeChanges(&r, explicit)
	return r, nil
}

func (e *Engine) storeChanges(r *ChangesReport, explicit bool) {
	if explicit {
		return
	}
	e.lastMu.Lock()
	e.lastCh = r
	e.lastSt = nil
	if e.ch != nil {
		e.baseEng = e.ch.base
		if r.Available {
			e.lastSt = e.ch
		}
	}
	e.lastMu.Unlock()
}

// resolveBases opens each root's repository and finds its base. A previous
// state is reused while its bases are unchanged, so the baseline engine is
// kept.
func (e *Engine) resolveBases(override string, s Settings) *changeState {
	st := &changeState{resolvedAt: time.Now()}
	byTop := map[string]*repoBase{}
	for _, root := range e.Roots() {
		r, err := gitbase.Open(root, s.Repositories.GitPath)
		if err != nil {
			st.nonGit = append(st.nonGit, root)
			continue
		}
		if byTop[r.Top] != nil {
			continue
		}
		rb := &repoBase{repo: r}
		choice := override
		if choice == "" && r.Branch() == "" {
			choice = e.detachedChoice
		}
		rb.base, rb.err = r.ResolveBase(choice)
		if rb.err != nil && override == "" {
			// the chosen base is gone: say so, and guess meanwhile
			rb.err = fmt.Errorf("the base chosen for this branch can't be used (%v), so it is guessed", rb.err)
			if g, err := r.Guess(); err == nil {
				rb.base = g
			}
		}
		byTop[r.Top] = rb
		st.repos = append(st.repos, rb)
	}
	if e.ch != nil {
		st.base, st.key = e.ch.base, e.ch.key
	}
	return st
}

// ensureBaseline exports each base commit and (re)builds the baseline
// engine when the bases or the settings changed.
func (e *Engine) ensureBaseline(st *changeState, s Settings) error {
	key := st.baseKey(s)

	for _, rb := range st.repos {
		if rb.base.SHA == "" {
			rb.export = ""
			continue
		}
		dir := filepath.Join(e.baselineRoot(), fmt.Sprintf("%x", sha1.Sum([]byte(rb.repo.Top)))[:12], rb.base.SHA)
		if err := rb.repo.Export(rb.base.SHA, dir, keepForBaseline); err != nil {
			return err
		}
		rb.export = dir
		now := time.Now()
		_ = os.Chtimes(dir, now, now) // in use: the newest, so never pruned
		pruneExports(filepath.Dir(dir), 3)
	}
	if st.base != nil && st.key == key {
		return nil
	}

	// The baseline engine's roots are the exported copies of the roots.
	// A root whose repository has no commits gets an empty directory: all
	// of its files are new.
	var roots []string
	mirror := map[string]string{}
	for _, root := range e.Roots() {
		rb := st.repoOf(root)
		if rb == nil {
			continue
		}
		top := rb.export
		if top == "" {
			top = filepath.Join(e.baselineRoot(), "empty")
			_ = os.MkdirAll(top, 0o755)
		}
		rel, _ := filepath.Rel(rb.repo.Top, root)
		roots = append(roots, filepath.Join(top, rel))
		mirror[top] = rb.repo.Top
	}
	bs := e.settingsForBaseline()
	bs.Repositories.AutoFetch = false // the workspace engine fetches; the base reads the same cache
	b := &Engine{
		overlay:   map[string]string{},
		files:     map[string]*fileInfo{},
		workspace: map[string]bool{},
		dirty:     map[string]bool{},
		diagDirty: map[string]bool{},
		Log:       func(string) {},
		mirror:    mirror,
		Repos:     e.Repos, // same cache, same fetch state
		shared:    true,
	}
	b.roots = roots
	b.applySettings(bs)
	b.Rescan()
	st.base, st.key = b, key
	return nil
}

// baseKey identifies what a baseline engine is built from: each
// repository's base commit and the settings.
func (st *changeState) baseKey(s Settings) string {
	var parts []string
	for _, rb := range st.repos {
		parts = append(parts, rb.repo.Top+"="+rb.base.SHA)
	}
	sb, _ := json.Marshal(s)
	return strings.Join(parts, "\n") + "\n" + fmt.Sprintf("%x", sha1.Sum(sb))
}

func (e *Engine) settingsForBaseline() Settings {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.settingsWithIDERepos()
}

func (st *changeState) repoOf(path string) *repoBase {
	var best *repoBase
	for _, rb := range st.repos {
		t := rb.repo.Top
		if (path == t || strings.HasPrefix(path, t+string(filepath.Separator))) && (best == nil || len(t) > len(best.repo.Top)) {
			best = rb
		}
	}
	return best
}

// toBase maps a workspace path into the baseline ("" when it is in no
// exported repository). Paths outside the repositories (cached external
// repositories) are the same on both sides.
func (st *changeState) toBase(p string) string {
	rb := st.repoOf(p)
	if rb == nil {
		return p
	}
	if rb.export == "" {
		return ""
	}
	rel, _ := filepath.Rel(rb.repo.Top, p)
	return filepath.Join(rb.export, rel)
}

// toHead maps a baseline path back into the workspace.
func (st *changeState) toHead(p string) string {
	for _, rb := range st.repos {
		if rb.export != "" && (p == rb.export || strings.HasPrefix(p, rb.export+string(filepath.Separator))) {
			rel, _ := filepath.Rel(rb.export, p)
			return filepath.Join(rb.repo.Top, rel)
		}
	}
	return p
}

func keepForBaseline(path string) bool {
	return isYAML(path) || filepath.Base(path) == ProjectConfigFile
}

// pruneExports keeps the newest n exports in dir.
func pruneExports(dir string, n int) {
	ents, err := os.ReadDir(dir)
	if err != nil || len(ents) <= n {
		return
	}
	type ex struct {
		path string
		mod  time.Time
	}
	var all []ex
	for _, d := range ents {
		if info, err := d.Info(); err == nil && d.IsDir() {
			all = append(all, ex{filepath.Join(dir, d.Name()), info.ModTime()})
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].mod.After(all[j].mod) })
	for i := n; i < len(all); i++ {
		_ = os.RemoveAll(all[i].path)
	}
}

// ---------------------------------------------------------------------------
// comparison
// ---------------------------------------------------------------------------

var crlf = strings.NewReplacer("\r\n", "\n")

func fileOrDirExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

var (
	msgLocation = regexp.MustCompile(`^(.+?):\d+: `)
	msgSuggest  = regexp.MustCompile(`\. Did you mean '[^']*'\?`)
)

// problemKey identifies a problem without its position: the file it is in,
// its code and its message without line numbers or suggestions (both
// change when unrelated lines do).
func problemKey(relPath, code, msg string) string {
	msg = msgLocation.ReplaceAllString(msg, "$1: ")
	msg = msgSuggest.ReplaceAllString(msg, "")
	return relPath + "\x00" + code + "\x00" + msg
}

// diffProblems splits two problem lists into new (only in now), fixed
// (only in before) and the number in both, counting duplicates.
func diffProblems(now, before []Problem) (added, fixed []Problem, both int) {
	count := map[string]int{}
	for _, p := range before {
		count[problemKey(p.RelativePath, p.Code, p.Message)]++
	}
	for _, p := range now {
		k := problemKey(p.RelativePath, p.Code, p.Message)
		if count[k] > 0 {
			count[k]--
			both++
			continue
		}
		added = append(added, p)
	}
	left := map[string]int{}
	for _, p := range now {
		left[problemKey(p.RelativePath, p.Code, p.Message)]++
	}
	for _, p := range before {
		k := problemKey(p.RelativePath, p.Code, p.Message)
		if left[k] > 0 {
			left[k]--
			continue
		}
		fixed = append(fixed, p)
	}
	return added, fixed, both
}

func verdictOf(r *PipelineReport) PipelineVerdict {
	if r == nil {
		return PipelineVerdict{}
	}
	return PipelineVerdict{Exists: true, Verdict: r.Verdict(), Errors: r.Errors, Warnings: r.Warnings}
}

func (e *Engine) compare(st *changeState) ChangesReport {
	out := ChangesReport{Available: true, Files: []ChangedFile{}, Pipelines: []PipelineChange{}}
	for _, rb := range st.repos {
		cb := ChangeBase{Base: rb.base, Repo: filepath.Base(rb.repo.Top), Top: rb.repo.Top, Short: rb.base.Short()}
		if rb.err != nil {
			cb.Error = rb.err.Error()
		}
		out.Bases = append(out.Bases, cb)
	}
	if len(out.Bases) > 0 {
		b := out.Bases[0]
		switch {
		case b.SHA == "":
			out.Label = "no commits yet"
		case b.Ref == "HEAD":
			out.Label = "uncommitted changes (HEAD @ " + b.Short + ")"
		case b.Ref == b.Short:
			out.Label = b.Short
		default:
			out.Label = b.Ref + " @ " + b.Short
		}
		if len(out.Bases) > 1 {
			out.Label += fmt.Sprintf(" (+%d more repositor%s)", len(out.Bases)-1, map[bool]string{true: "y", false: "ies"}[len(out.Bases) == 2])
		}
	}
	base := st.base
	// a file counts when it is in one of the repositories, not in a
	// submodule or another checkout nested inside it
	nested := map[string]bool{}
	inGit := func(p string) bool {
		rb := st.repoOf(p)
		if rb == nil {
			return false
		}
		d := filepath.Dir(p)
		v, ok := nested[d]
		if !ok {
			v = FindRepoRoot(d) != rb.repo.Top && fileOrDirExists(filepath.Join(FindRepoRoot(d), ".git"))
			nested[d] = v
		}
		return !v
	}

	// changed files: compare contents path by path
	headText := e.workspaceTexts()
	// A nested checkout (a submodule, a clone inside the repository) isn't
	// in the commit, so the base sees it as it is now.
	nestedNow := map[string]bool{}
	for p, t := range headText {
		if st.repoOf(p) != nil && !inGit(p) {
			if bp := st.toBase(p); bp != "" {
				nestedNow[bp] = true
				base.OpenDocument(bp, t)
			}
		}
	}
	for _, bp := range base.openDocuments() {
		if !nestedNow[bp] {
			base.CloseDocument(bp)
		}
	}
	baseText := map[string]string{}
	for p, t := range base.workspaceTexts() {
		baseText[st.toHead(p)] = t
	}
	// pipelines
	headReports := map[string]*PipelineReport{}
	for _, r := range e.PipelineChecks() {
		r := r
		if inGit(r.Path) {
			headReports[r.Path] = &r
		}
		out.AllPipelines++
		if r.Verdict() == "fail" {
			out.AllFailing++
		}
	}
	baseReports := map[string]*PipelineReport{}
	for _, r := range base.PipelineChecks() {
		r := r
		r.Path = st.toHead(r.Path)
		for i := range r.Files {
			r.Files[i] = st.toHead(r.Files[i])
		}
		for i := range r.Problems {
			r.Problems[i].Path = st.toHead(r.Problems[i].Path)
		}
		if !inGit(r.Path) {
			continue // a nested checkout's pipeline: not in this repository
		}
		baseReports[r.Path] = &r
	}
	var added []string
	changed := map[string]string{} // head path -> status
	for p, t := range headText {
		if !inGit(p) {
			continue
		}
		bt, ok := baseText[p]
		if !ok {
			// not indexed at the base (a skipped folder like build/): the
			// file may still be there
			if bp := st.toBase(p); bp != "" {
				bt, ok = base.fileText(bp)
			}
		}
		if !ok {
			added = append(added, p)
		} else if crlf.Replace(bt) != crlf.Replace(t) {
			changed[p] = "modified" // core.autocrlf checks out CRLF; git stores LF
		}
	}
	for p, bt := range baseText {
		if _, ok := headText[p]; ok || !inGit(p) {
			continue
		}
		if ht, ok := e.fileText(p); !ok {
			changed[p] = "deleted"
		} else if crlf.Replace(bt) != crlf.Replace(ht) {
			changed[p] = "modified"
		}
	}
	// templates the index skips (under build/, out/, …) that a pipeline
	// reaches: compare those too
	extra := map[string]bool{}
	for _, reps := range []map[string]*PipelineReport{headReports, baseReports} {
		for _, r := range reps {
			for _, f := range r.Files {
				if _, ok := headText[f]; !ok && inGit(f) {
					if _, ok := baseText[f]; !ok {
						extra[f] = true
					}
				}
			}
		}
	}
	for f := range extra {
		ht, hok := e.fileText(f)
		bt, bok := "", false
		if bp := st.toBase(f); bp != "" {
			bt, bok = base.fileText(bp)
		}
		switch {
		case hok && bok && crlf.Replace(ht) != crlf.Replace(bt):
			changed[f] = "modified"
		case hok && !bok:
			added = append(added, f)
		case !hok && bok:
			changed[f] = "deleted"
		}
	}
	// a new file git ignores (build output, a local copy) isn't your work
	byRepo := map[*repoBase][]string{}
	for _, p := range added {
		rb := st.repoOf(p)
		rel, _ := filepath.Rel(rb.repo.Top, p)
		byRepo[rb] = append(byRepo[rb], filepath.ToSlash(rel))
	}
	for rb, rels := range byRepo {
		ign := rb.repo.Ignored(rels)
		for _, rel := range rels {
			if !ign[rel] {
				changed[filepath.Join(rb.repo.Top, filepath.FromSlash(rel))] = "added"
			}
		}
	}

	paths := map[string]bool{}
	for p := range headReports {
		paths[p] = true
	}
	for p := range baseReports {
		paths[p] = true
	}
	// totals: a template's problem reaches several pipelines but counts
	// once; two identical problems in one file count twice
	maxNew, maxFixed := map[string]int{}, map[string]int{}
	sevNew := map[string]int{}
	for p := range paths {
		now, before := headReports[p], baseReports[p]
		var touched []string
		seen := map[string]bool{}
		for _, r := range []*PipelineReport{now, before} {
			if r == nil {
				continue
			}
			for _, f := range r.Files {
				if _, ok := changed[f]; ok && !seen[f] {
					seen[f] = true
					touched = append(touched, e.relPathLocked(f))
				}
			}
		}
		var np, bp []Problem
		if now != nil {
			np = now.Problems
		}
		if before != nil {
			bp = before.Problems
		}
		add, fix, both := diffProblems(np, bp)
		c := PipelineChange{Path: p, Now: verdictOf(now), Before: verdictOf(before), New: nonNil(add), Fixed: nonNil(fix), Existing: both, Changed: touched}
		switch {
		case now != nil && before == nil && changed[p] == "":
			continue // a file git ignores
		case now != nil && before == nil:
			c.RelativePath, c.Status = now.RelativePath, "added"
		case now == nil:
			c.RelativePath, c.Status = before.RelativePath, "removed"
		case len(touched) > 0:
			c.RelativePath, c.Status = now.RelativePath, "changed"
		case len(add) > 0 || len(fix) > 0:
			c.RelativePath, c.Status = now.RelativePath, "affected"
		default:
			continue
		}
		sort.Strings(c.Changed)
		if c.Changed == nil {
			c.Changed = []string{}
		}
		out.Pipelines = append(out.Pipelines, c)
		maxInto(maxNew, add, sevNew)
		maxInto(maxFixed, fix, nil)
		if c.Now.Verdict == "fail" && c.Before.Verdict != "fail" {
			out.NowFailing++
		}
		if c.Before.Verdict == "fail" && c.Now.Verdict != "fail" {
			out.NoLonger++
		}
	}
	for k, n := range maxNew {
		if sevNew[k] == SevError {
			out.NewErrors += n
		} else {
			out.NewWarnings += n
		}
	}
	for _, n := range maxFixed {
		out.FixedProblems += n
	}
	sort.SliceStable(out.Pipelines, func(i, j int) bool {
		a, b := out.Pipelines[i], out.Pipelines[j]
		if (a.Status == "removed") != (b.Status == "removed") {
			return b.Status == "removed"
		}
		if ae, be := newErrors(a), newErrors(b); ae != be {
			return ae > be
		}
		if len(a.New) != len(b.New) {
			return len(a.New) > len(b.New)
		}
		return a.RelativePath < b.RelativePath
	})

	// files, with the pipelines reaching them and their own new and fixed
	// problems
	reaches := map[string]int{}
	for _, r := range headReports {
		for _, f := range r.Files {
			if changed[f] != "" {
				reaches[f]++
			}
		}
	}
	for _, r := range baseReports {
		for _, f := range r.Files {
			if changed[f] == "deleted" {
				reaches[f]++
			}
		}
	}
	var cps []string
	for p := range changed {
		cps = append(cps, p)
	}
	sort.Strings(cps)
	for _, p := range cps {
		cf := ChangedFile{Path: p, RelativePath: e.relPathLocked(p), Status: changed[p], Pipelines: reaches[p], New: []Problem{}, Fixed: []Problem{}}
		var now, before []Problem
		if cf.Status != "deleted" {
			now = toProblems(p, cf.RelativePath, e.Diagnose(p))
		}
		if bp := st.toBase(p); bp != "" && cf.Status != "added" {
			before = toProblems(p, cf.RelativePath, base.Diagnose(bp))
		}
		add, fix, _ := diffProblems(now, before)
		cf.New, cf.Fixed = nonNil(add), nonNil(fix)
		out.Files = append(out.Files, cf)
	}
	return out
}

// ExistingID identifies the comparison Existing uses: it changes when the
// base does, so the caller re-sends what it dimmed. "" when there is none.
func (e *Engine) ExistingID() string {
	e.lastMu.Lock()
	defer e.lastMu.Unlock()
	if e.lastSt == nil || e.lastCh == nil {
		return ""
	}
	return e.lastSt.key + "\n" + e.lastCh.Label
}

// Existing marks which of a file's errors and warnings were already there
// at the base (same code and message, line numbers aside, counting
// duplicates), and names that base ("origin/main"). It uses the last
// comparison and never runs git, so a caller that publishes diagnostics
// can afford it. marks is nil when there is nothing to compare with:
// before the first comparison, and for files outside the repositories.
func (e *Engine) Existing(path string, ds []Diagnostic) (marks []bool, base string) {
	e.lastMu.Lock()
	st := e.lastSt
	e.lastMu.Unlock()
	if st == nil || st.base == nil {
		return nil, ""
	}
	rb := st.repoOf(path)
	if rb == nil {
		return nil, "" // a cached external repository: not your work either way
	}
	marks = make([]bool, len(ds))
	base = rb.base.Ref
	if base == "" {
		base = rb.base.Short()
	}
	if rb.export == "" {
		return marks, base // no commits yet: everything is new
	}
	before := st.base.Diagnose(st.toBase(path))
	count := map[string]int{}
	for _, d := range before {
		count[problemKey("", d.Code, d.Message)]++
	}
	for i, d := range ds {
		if d.Severity > SevWarning {
			continue
		}
		if k := problemKey("", d.Code, d.Message); count[k] > 0 {
			count[k]--
			marks[i] = true
		}
	}
	return marks, base
}

// maxInto records, per problem, the most times one pipeline has it.
func maxInto(max map[string]int, ps []Problem, sev map[string]int) {
	n := map[string]int{}
	for _, p := range ps {
		k := problemKey(p.RelativePath, p.Code, p.Message) + "\x00" + fmt.Sprint(p.Severity)
		n[k]++
		if sev != nil {
			sev[k] = p.Severity
		}
	}
	for k, c := range n {
		if c > max[k] {
			max[k] = c
		}
	}
}

func newErrors(c PipelineChange) int {
	n := 0
	for _, p := range c.New {
		if p.Severity == SevError {
			n++
		}
	}
	return n
}

func toProblems(path, rel string, ds []Diagnostic) []Problem {
	var out []Problem
	for _, d := range ds {
		if d.Severity > SevWarning || notAboutTheRun[d.Code] {
			continue
		}
		out = append(out, Problem{Path: path, RelativePath: rel, Diagnostic: d})
	}
	return out
}

func nonNil(ps []Problem) []Problem {
	if ps == nil {
		return []Problem{}
	}
	return ps
}

// fileText returns a file's content (open document or disk).
func (e *Engine) fileText(p string) (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if f := e.file(p); f != nil {
		return f.text, true
	}
	return "", false
}

// openDocuments lists the documents given with OpenDocument.
func (e *Engine) openDocuments() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for p := range e.overlay {
		out = append(out, p)
	}
	return out
}

// workspaceTexts returns the content of every indexed workspace file.
func (e *Engine) workspaceTexts() map[string]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := map[string]string{}
	for _, p := range e.workspaceFiles() {
		if f := e.file(p); f != nil {
			out[p] = f.text
		}
	}
	return out
}

func (e *Engine) relPathLocked(p string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.relPath(p)
}

// ---------------------------------------------------------------------------
// choosing the base
// ---------------------------------------------------------------------------

// ChangeBases lists what the workspace can be compared against: automatic,
// uncommitted only, and recent branches of the first repository.
func (e *Engine) ChangeBases() []BaseChoice {
	s := e.Settings()
	var repo *gitbase.Repo
	for _, root := range e.Roots() {
		if r, err := gitbase.Open(root, s.Repositories.GitPath); err == nil {
			repo = r
			break
		}
	}
	if repo == nil {
		return []BaseChoice{}
	}
	chosen := repo.Choice()
	if repo.Branch() == "" {
		e.chMu.Lock()
		chosen = e.detachedChoice
		e.chMu.Unlock()
	}
	auto := "Guess where this branch started"
	if b, err := repo.Guess(); err == nil {
		label := b.Ref
		if b.Ref != b.Short() && b.Ref != "HEAD" {
			label += " @ " + b.Short()
		}
		auto = label + ": " + b.Detail
	}
	out := []BaseChoice{
		{Ref: "", Label: "Automatic", Detail: auto, Picked: chosen == ""},
		{Ref: gitbase.Uncommitted, Label: "Uncommitted changes only", Detail: "Compare with the last commit (HEAD)", Picked: chosen == gitbase.Uncommitted},
	}
	for _, b := range repo.Branches(30) {
		out = append(out, BaseChoice{Ref: b, Label: b, Detail: "Where this branch splits from " + b, Picked: chosen == b})
	}
	if chosen != "" && chosen != gitbase.Uncommitted {
		found := false
		for _, c := range out {
			found = found || c.Ref == chosen
		}
		if !found {
			out = append(out, BaseChoice{Ref: chosen, Label: chosen, Detail: "Chosen for this branch", Picked: true})
		}
	}
	return out
}

// SetChangesBase stores what to compare against for the current branch of
// every repository in the workspace ("" = automatic, "HEAD" = uncommitted
// only). On a detached HEAD the choice lasts until the server stops.
func (e *Engine) SetChangesBase(ref string) error {
	s := e.Settings()
	e.chMu.Lock()
	defer e.chMu.Unlock()
	var firstErr error
	ok := false
	seen := map[string]bool{}
	for _, root := range e.Roots() {
		r, err := gitbase.Open(root, s.Repositories.GitPath)
		if err != nil || seen[r.Top] {
			continue
		}
		seen[r.Top] = true
		if r.Branch() == "" {
			if ref != "" && ref != gitbase.Uncommitted && r.Commit(ref) == "" {
				firstErr = fmt.Errorf("'%s' is not a branch, tag or commit in this repository", ref)
				continue
			}
			e.detachedChoice = ref
			ok = true
			continue
		}
		if err := r.SetChoice(ref); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		ok = true
	}
	e.ch = nil // resolve again
	if !ok && firstErr == nil {
		firstErr = fmt.Errorf("not a git repository")
	}
	if ok {
		return nil
	}
	return firstErr
}

// changeNote is what the verdict lens adds for a pipeline your work
// touched, e.g. " · +2 new since feature/story".
// Caller holds e.mu.
func (e *Engine) changeNote(path string) string {
	last := e.LastChanges()
	if last == nil || last.gen != e.diagGen {
		return "" // out of date: the server refreshes the lens when it isn't
	}
	c, ok := last.Change(path)
	if !ok || (len(c.New) == 0 && len(c.Fixed) == 0) {
		return ""
	}
	var parts []string
	if len(c.New) > 0 {
		parts = append(parts, fmt.Sprintf("+%d new", len(c.New)))
	}
	if len(c.Fixed) > 0 {
		parts = append(parts, fmt.Sprintf("%d fixed", len(c.Fixed)))
	}
	since := ""
	if len(last.Bases) > 0 {
		if b := last.Bases[0]; b.Ref == "HEAD" {
			since = " since the last commit"
		} else if b.Ref != "" {
			since = " since " + b.Ref
		}
	}
	return " · " + strings.Join(parts, ", ") + since
}

// annotate marks, in the reports of the Pipelines view, the problems your
// work added (from the last comparison).
func (e *Engine) annotate(reports []PipelineReport) {
	last := e.LastChanges()
	e.mu.Lock()
	gen := e.diagGen
	e.mu.Unlock()
	if last == nil || !last.Available || last.gen != gen {
		return
	}
	for i := range reports {
		c, ok := last.Change(reports[i].Path)
		if !ok {
			continue
		}
		reports[i].Change = &c
		want := map[string]int{}
		for _, p := range c.New {
			want[problemKey(p.RelativePath, p.Code, p.Message)]++
		}
		for j := range reports[i].Problems {
			p := &reports[i].Problems[j]
			k := problemKey(p.RelativePath, p.Code, p.Message)
			if want[k] > 0 {
				want[k]--
				p.New = true
			}
		}
	}
}

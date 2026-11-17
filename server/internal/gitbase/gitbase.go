// Package gitbase answers "where did my work start?" for a git checkout and
// exports the YAML of that commit, so the engine can compare the workspace
// against it (engine/changes.go).
//
// Git doesn't record which branch a branch was created from, so the start
// point is a best guess unless the user picked one. The order:
//
//  1. a base the user chose for this branch (git config branch.<b>.atnBase)
//  2. on the default branch: its upstream (your unpushed commits)
//  3. the branch it was created from, read from the reflogs: where HEAD
//     splits from that branch now, which stays right after a rebase
//  4. the commit the branch was created at, while it is still an ancestor
//  5. where HEAD splits from the default branch
//  6. HEAD itself: only uncommitted work counts
//
// It shells out to git with prompts disabled and never writes to the
// repository except for SetChoice (git config).
package gitbase

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ConfigKey is the per-branch git config key holding a chosen base.
const ConfigKey = "atnBase"

// Uncommitted is the choice that compares against HEAD (only uncommitted
// work counts).
const Uncommitted = "HEAD"

// Repo is a git working tree.
type Repo struct {
	Top string // top-level directory
	Git string // git executable
}

// Open finds the repository containing dir. It fails when dir is not in a
// git working tree or git is not installed.
func Open(dir, gitPath string) (*Repo, error) {
	if gitPath == "" {
		gitPath = "git"
	}
	r := &Repo{Top: dir, Git: gitPath}
	// Top keeps the caller's spelling of dir (git prints the real path,
	// /private/var for /var on macOS), so paths can be mapped by prefix.
	prefix, err := r.run("rev-parse", "--show-prefix")
	if err != nil {
		return nil, err
	}
	top := filepath.Clean(dir)
	if p := strings.TrimSuffix(strings.TrimSpace(prefix), "/"); p != "" {
		for range strings.Split(p, "/") {
			top = filepath.Dir(top)
		}
	}
	r.Top = top
	return r, nil
}

func (r *Repo) run(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.Git, args...)
	cmd.Dir = r.Top
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0", "LC_ALL=C")
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return out.String(), errors.New(firstLine(msg))
	}
	return out.String(), nil
}

func (r *Repo) line(args ...string) string {
	out, err := r.run(args...)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// Branch returns the checked-out branch ("" when HEAD is detached).
func (r *Repo) Branch() string {
	return r.line("symbolic-ref", "--short", "-q", "HEAD")
}

// Head returns the commit HEAD points to ("" in a repository with no commits).
func (r *Repo) Head() string {
	return r.line("rev-parse", "-q", "--verify", "HEAD^{commit}")
}

// Commit resolves ref to a commit id ("" when it doesn't name one).
func (r *Repo) Commit(ref string) string {
	if ref == "" || strings.HasPrefix(ref, "-") {
		return ""
	}
	return r.line("rev-parse", "-q", "--verify", ref+"^{commit}")
}

func (r *Repo) mergeBase(a, b string) string {
	return r.line("merge-base", a, b)
}

func (r *Repo) isAncestor(a, b string) bool {
	_, err := r.run("merge-base", "--is-ancestor", a, b)
	return err == nil
}

// Choice returns the base chosen for the current branch ("" = automatic).
func (r *Repo) Choice() string {
	b := r.Branch()
	if b == "" {
		return ""
	}
	return r.line("config", "--get", "branch."+b+"."+ConfigKey)
}

// SetChoice stores the base for the current branch; "" goes back to
// automatic. It fails on a detached HEAD (no branch to remember it for) and
// when ref doesn't name a commit.
func (r *Repo) SetChoice(ref string) error {
	b := r.Branch()
	if b == "" {
		return errors.New("HEAD is detached: there is no branch to remember the base for")
	}
	key := "branch." + b + "." + ConfigKey
	if ref == "" {
		_, err := r.run("config", "--unset", key)
		if err != nil && r.line("config", "--get", key) != "" {
			return err
		}
		return nil
	}
	if r.Commit(ref) == "" {
		return fmt.Errorf("'%s' is not a branch, tag or commit in this repository", ref)
	}
	_, err := r.run("config", key, ref)
	return err
}

// Base is the commit the workspace is compared against.
type Base struct {
	SHA string `json:"sha"` // "" when the repository has no commits
	// Ref names the base for display: a branch ("feature/story"), "HEAD"
	// (uncommitted work only) or a short commit id.
	Ref string `json:"ref"`
	// Source says how it was found: "chosen", "upstream", "parent",
	// "created", "default", "head".
	Source string `json:"source"`
	Branch string `json:"branch"` // checked-out branch ("" when detached)
	// Detail explains the choice in one sentence.
	Detail string `json:"detail"`
	// Commits made since the base (on HEAD's side).
	Commits int `json:"commits"`
}

// Short is the base's abbreviated commit id.
func (b Base) Short() string {
	if len(b.SHA) > 7 {
		return b.SHA[:7]
	}
	return b.SHA
}

// ResolveBase finds the base: choice when not empty (a ref, or Uncommitted),
// else the stored choice for the branch, else the best guess.
func (r *Repo) ResolveBase(choice string) (Base, error) {
	if choice == "" {
		choice = r.Choice()
	}
	return r.resolve(choice)
}

// Guess is the automatic base, ignoring any stored choice.
func (r *Repo) Guess() (Base, error) { return r.resolve("") }

func (r *Repo) resolve(choice string) (Base, error) {
	head := r.Head()
	branch := r.Branch()
	b := Base{Branch: branch}
	if head == "" {
		b.Ref, b.Source, b.Detail = "HEAD", "head", "The repository has no commits yet, so every file counts as new"
		return b, nil
	}
	if choice != "" {
		if choice == Uncommitted {
			return r.finish(Base{SHA: head, Ref: "HEAD", Source: "chosen", Branch: branch,
				Detail: "Only uncommitted changes count (chosen for this branch)"}, head), nil
		}
		sha := r.Commit(choice)
		if sha == "" {
			return b, fmt.Errorf("'%s' is not a branch, tag or commit in this repository", choice)
		}
		// a branch means "where I split from it", a commit means itself
		if r.isBranch(choice) {
			mb := r.mergeBase(head, sha)
			if mb == "" {
				return b, fmt.Errorf("HEAD and '%s' have no commit in common (in a shallow clone, fetch more history)", choice)
			}
			sha = mb
		}
		return r.finish(Base{SHA: sha, Ref: choice, Source: "chosen", Branch: branch,
			Detail: fmt.Sprintf("Compared with %s (chosen for this branch)", choice)}, head), nil
	}

	def := r.DefaultBranch()
	if branch == "" {
		if def != "" {
			if mb := r.mergeBase(head, def); mb != "" {
				return r.finish(Base{SHA: mb, Ref: def, Source: "default", Detail: fmt.Sprintf("HEAD is detached: compared with where it splits from %s", def)}, head), nil
			}
		}
		return r.finish(Base{SHA: head, Ref: "HEAD", Source: "head", Detail: "HEAD is detached: only uncommitted changes count"}, head), nil
	}

	if def != "" && shortName(def) == branch {
		if up := r.line("rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}"); up != "" {
			if mb := r.mergeBase(head, up); mb != "" {
				return r.finish(Base{SHA: mb, Ref: up, Source: "upstream", Branch: branch,
					Detail: fmt.Sprintf("On %s: your unpushed commits and uncommitted changes, compared with %s", branch, up)}, head), nil
			}
		}
		return r.finish(Base{SHA: head, Ref: "HEAD", Source: "head", Branch: branch,
			Detail: fmt.Sprintf("On %s with no upstream: only uncommitted changes count", branch)}, head), nil
	}

	created, parent := r.creation(branch)
	if parent != "" && r.isOwnRemote(branch, parent) {
		// checked out from its own remote (e.g. a branch created in Azure
		// DevOps): neither that nor the checkout commit is where the work
		// started, and the delta would empty on every push
		created, parent = "", ""
	}
	if parent != "" && parent != branch {
		if sha, ref := r.splitFrom(head, parent); sha != "" {
			return r.finish(Base{SHA: sha, Ref: ref, Source: "parent", Branch: branch,
				Detail: fmt.Sprintf("Where %s split from %s, the branch it was created from", branch, ref)}, head), nil
		}
	}
	if created != "" && r.isAncestor(created, head) {
		return r.finish(Base{SHA: created, Ref: shortSHA(created), Source: "created", Branch: branch,
			Detail: fmt.Sprintf("The commit %s was created at", branch)}, head), nil
	}
	if def != "" {
		if mb := r.mergeBase(head, def); mb != "" {
			return r.finish(Base{SHA: mb, Ref: def, Source: "default", Branch: branch,
				Detail: fmt.Sprintf("Where %s splits from %s", branch, def)}, head), nil
		}
	}
	return r.finish(Base{SHA: head, Ref: "HEAD", Source: "head", Branch: branch,
		Detail: "No start point found: only uncommitted changes count"}, head), nil
}

func (r *Repo) finish(b Base, head string) Base {
	if b.SHA != "" && b.SHA != head {
		if n, err := strconv.Atoi(r.line("rev-list", "--count", b.SHA+".."+head)); err == nil {
			b.Commits = n
		}
	}
	return b
}

// isOwnRemote: parent is branch's upstream or a remote branch of the same
// name.
func (r *Repo) isOwnRemote(branch, parent string) bool {
	if up := r.line("rev-parse", "--abbrev-ref", "--symbolic-full-name", "refs/heads/"+branch+"@{upstream}"); up != "" && up == parent {
		return true
	}
	if i := strings.IndexByte(parent, '/'); i > 0 && parent[i+1:] == branch && r.line("rev-parse", "-q", "--verify", "refs/remotes/"+parent) != "" {
		return true
	}
	return false
}

func (r *Repo) isBranch(ref string) bool {
	for _, p := range []string{"refs/heads/", "refs/remotes/"} {
		if r.line("rev-parse", "-q", "--verify", p+ref) != "" {
			return true
		}
	}
	return false
}

// splitFrom is where head splits from branch name (local or its remote
// counterpart, whichever is further along), with the name used.
func (r *Repo) splitFrom(head, name string) (string, string) {
	var cands []string
	if r.isBranch(name) {
		cands = append(cands, name)
	}
	if r.line("rev-parse", "-q", "--verify", "refs/heads/"+name) != "" && r.line("rev-parse", "-q", "--verify", "refs/remotes/origin/"+name) != "" {
		cands = append(cands, "origin/"+name)
	}
	best, bestRef := "", ""
	for _, c := range cands {
		mb := r.mergeBase(head, c)
		if mb == "" {
			continue
		}
		if best == "" || (mb != best && r.isAncestor(best, mb)) {
			best, bestRef = mb, c
		}
	}
	return best, bestRef
}

var (
	createdFrom = regexp.MustCompile(`^branch: Created from (.+)$`)
	movingFrom  = regexp.MustCompile(`^checkout: moving from (.+) to (.+)$`)
	hexID       = regexp.MustCompile(`^[0-9a-f]{7,64}$`)
)

// creation reads the reflogs: the commit branch was created at, and the
// branch it was created from when git recorded one ("" otherwise).
func (r *Repo) creation(branch string) (created, parent string) {
	out, err := r.run("reflog", "show", "--format=%H %gs", "refs/heads/"+branch, "--")
	if err != nil {
		return "", ""
	}
	entries := reflog(out)
	if len(entries) == 0 {
		return "", ""
	}
	first := entries[len(entries)-1] // oldest
	if m := createdFrom.FindStringSubmatch(first.msg); m != nil {
		created = first.sha
		if from := m[1]; from != "HEAD" && !hexID.MatchString(from) {
			from = strings.TrimPrefix(from, "refs/heads/")
			from = strings.TrimPrefix(from, "refs/remotes/")
			return created, from
		}
	} else {
		return "", "" // the reflog doesn't go back to the branch's creation
	}
	// `git switch -c b` records "Created from HEAD"; HEAD's reflog says
	// which branch HEAD was on at that moment.
	out, err = r.run("reflog", "show", "--format=%H %gs", "HEAD", "--")
	if err != nil {
		return created, ""
	}
	hs := reflog(out)
	for i := len(hs) - 1; i >= 0; i-- {
		m := movingFrom.FindStringSubmatch(hs[i].msg)
		if m == nil || m[2] != branch || hs[i].sha != created {
			continue
		}
		if from := m[1]; !hexID.MatchString(from) && from != branch {
			return created, from
		}
		break
	}
	return created, ""
}

type reflogEntry struct{ sha, msg string }

func reflog(out string) []reflogEntry {
	var es []reflogEntry
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		if i := strings.IndexByte(l, ' '); i > 0 {
			es = append(es, reflogEntry{l[:i], l[i+1:]})
		}
	}
	return es
}

// DefaultBranch is the repository's default branch: origin's HEAD, else the
// first of origin/main, origin/master, main, master that exists.
func (r *Repo) DefaultBranch() string {
	if s := r.line("symbolic-ref", "-q", "--short", "refs/remotes/origin/HEAD"); s != "" && r.Commit(s) != "" {
		return s
	}
	for _, c := range []string{"origin/main", "origin/master", "main", "master"} {
		if r.Commit(c) != "" {
			return c
		}
	}
	return ""
}

func shortName(ref string) string {
	if i := strings.IndexByte(ref, '/'); i >= 0 && strings.HasPrefix(ref, "origin/") {
		return ref[i+1:]
	}
	return ref
}

func shortSHA(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}

// Branches lists local and remote branches, most recently committed first,
// without the checked-out one and origin/HEAD.
func (r *Repo) Branches(limit int) []string {
	out, err := r.run("for-each-ref", "--sort=-committerdate", "--format=%(refname:short)", "refs/heads", "refs/remotes")
	if err != nil {
		return nil
	}
	cur := r.Branch()
	var bs []string
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || l == cur || strings.HasSuffix(l, "/HEAD") || l == "origin" {
			continue
		}
		bs = append(bs, l)
		if limit > 0 && len(bs) >= limit {
			break
		}
	}
	return bs
}

// Ignored returns the paths (relative to Top, slash-separated) that git
// ignores.
func (r *Repo) Ignored(paths []string) map[string]bool {
	out := map[string]bool{}
	if len(paths) == 0 {
		return out
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.Git, "check-ignore", "--stdin")
	cmd.Dir = r.Top
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0")
	cmd.Stdin = strings.NewReader(strings.Join(paths, "\n") + "\n")
	b, _ := cmd.Output() // exits 1 when nothing is ignored
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out[filepath.ToSlash(l)] = true
		}
	}
	return out
}

// Export writes the files of commit sha for which keep(path) is true under
// dir, keeping their paths. A finished export is marked, so an existing one
// is reused. It is written next to dir and renamed into place, so two
// processes exporting at once never see half of one.
func (r *Repo) Export(sha, dir string, keep func(path string) bool) error {
	marker := func(d string) string { return filepath.Join(d, ".atn-export") }
	done := func() bool {
		b, err := os.ReadFile(marker(dir))
		return err == nil && strings.TrimSpace(string(b)) == sha
	}
	if done() {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dir), ".export-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	out, err := r.run("ls-tree", "-r", "-z", "--full-tree", sha)
	if err != nil {
		return err
	}
	var blobs []blob
	for _, rec := range strings.Split(out, "\x00") {
		tab := strings.IndexByte(rec, '\t')
		if tab < 0 {
			continue
		}
		meta := strings.Fields(rec[:tab])
		path := rec[tab+1:]
		// regular files only: no symlinks (120000) or submodules (commit)
		if len(meta) != 3 || meta[1] != "blob" || meta[0] == "120000" || !keep(path) {
			continue
		}
		blobs = append(blobs, blob{meta[2], path})
	}
	if len(blobs) > 0 {
		if err := r.writeBlobs(tmp, blobs); err != nil {
			return err
		}
	}
	if err := os.WriteFile(marker(tmp), []byte(sha+"\n"), 0o644); err != nil {
		return err
	}
	if done() {
		return nil // another process finished first
	}
	_ = os.RemoveAll(dir) // an unfinished export
	if err := os.Rename(tmp, dir); err != nil {
		if done() {
			return nil
		}
		return err
	}
	return nil
}

type blob struct{ id, path string }

func (r *Repo) writeBlobs(dir string, blobs []blob) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.Git, "cat-file", "--batch")
	cmd.Dir = r.Top
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0")
	var in bytes.Buffer
	for _, b := range blobs {
		in.WriteString(b.id + "\n")
	}
	cmd.Stdin = &in
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	rd := bufio.NewReader(stdout)
	var werr error
	for _, b := range blobs {
		header, err := rd.ReadString('\n')
		if err != nil {
			werr = err
			break
		}
		f := strings.Fields(header)
		if len(f) != 3 {
			werr = fmt.Errorf("unexpected git cat-file output: %q", header)
			break
		}
		size, err := strconv.Atoi(f[2])
		if err != nil {
			werr = err
			break
		}
		data := make([]byte, size+1) // content + trailing newline
		if _, err := io.ReadFull(rd, data); err != nil {
			werr = err
			break
		}
		dst := filepath.Join(dir, filepath.FromSlash(b.path))
		if !strings.HasPrefix(dst, filepath.Clean(dir)+string(filepath.Separator)) {
			continue // a path escaping the export (shouldn't happen)
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			werr = err
			break
		}
		if err := os.WriteFile(dst, data[:size], 0o644); err != nil {
			werr = err
			break
		}
	}
	_, _ = io.Copy(io.Discard, rd)
	if err := cmd.Wait(); err != nil && werr == nil {
		werr = err
	}
	return werr
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

package gitbase

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// repo is a scratch repository with one commit on main.
func repo(t *testing.T) *Repo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "config", "user.email", "t@example.com")
	git(t, dir, "config", "user.name", "t")
	git(t, dir, "config", "commit.gpgsign", "false")
	commit(t, dir, "a.yml", "a: 1\n")
	r, err := Open(dir, "git")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func commit(t *testing.T, dir, file, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, file)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", file)
	return git(t, dir, "rev-parse", "HEAD")
}

func resolve(t *testing.T, r *Repo) Base {
	t.Helper()
	b, err := r.ResolveBase("")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestOpenOutsideGit(t *testing.T) {
	if _, err := Open(t.TempDir(), "git"); err == nil {
		t.Fatal("a plain directory is not a repository")
	}
}

func TestOpenSubdirKeepsSpelling(t *testing.T) {
	r := repo(t)
	sub := filepath.Join(r.Top, "x", "y")
	_ = os.MkdirAll(sub, 0o755)
	r2, err := Open(sub, "git")
	if err != nil {
		t.Fatal(err)
	}
	if r2.Top != r.Top {
		t.Fatalf("top %q, want %q", r2.Top, r.Top)
	}
}

// git switch -c records "Created from HEAD"; HEAD's reflog names the branch.
func TestBranchCreatedFromCurrentBranch(t *testing.T) {
	r := repo(t)
	git(t, r.Top, "switch", "-q", "-c", "feature/story")
	fork := commit(t, r.Top, "b.yml", "b: 1\n")
	git(t, r.Top, "switch", "-q", "-c", "mywork")
	commit(t, r.Top, "c.yml", "c: 1\n")
	commit(t, r.Top, "d.yml", "d: 1\n")
	b := resolve(t, r)
	if b.SHA != fork || b.Ref != "feature/story" || b.Source != "parent" || b.Commits != 2 {
		t.Fatalf("got %+v, want feature/story @ %s with 2 commits", b, fork)
	}
	// the parent moves on: the base stays where mywork split from it
	git(t, r.Top, "switch", "-q", "feature/story")
	commit(t, r.Top, "e.yml", "e: 1\n")
	git(t, r.Top, "switch", "-q", "mywork")
	if b := resolve(t, r); b.SHA != fork {
		t.Fatalf("base moved to %s", b.SHA)
	}
	// rebase onto the parent: now the base is its new tip
	tip := git(t, r.Top, "rev-parse", "feature/story")
	git(t, r.Top, "rebase", "-q", "feature/story")
	if b := resolve(t, r); b.SHA != tip || b.Ref != "feature/story" {
		t.Fatalf("after rebase got %+v, want %s", b, tip)
	}
}

func TestBranchCreatedFromNamedStart(t *testing.T) {
	r := repo(t)
	git(t, r.Top, "switch", "-q", "-c", "feature/story")
	fork := commit(t, r.Top, "b.yml", "b: 1\n")
	git(t, r.Top, "switch", "-q", "main")
	git(t, r.Top, "checkout", "-q", "-b", "mywork", "feature/story")
	commit(t, r.Top, "c.yml", "c: 1\n")
	if b := resolve(t, r); b.SHA != fork || b.Ref != "feature/story" {
		t.Fatalf("got %+v", b)
	}
}

func TestOnDefaultBranchWithoutUpstream(t *testing.T) {
	r := repo(t)
	head := git(t, r.Top, "rev-parse", "HEAD")
	if b := resolve(t, r); b.SHA != head || b.Ref != "HEAD" {
		t.Fatalf("got %+v", b)
	}
}

func TestOnDefaultBranchWithUpstream(t *testing.T) {
	r := repo(t)
	bare := filepath.Join(t.TempDir(), "remote.git")
	git(t, r.Top, "init", "-q", "--bare", bare)
	git(t, r.Top, "remote", "add", "origin", bare)
	git(t, r.Top, "push", "-q", "-u", "origin", "main")
	pushed := git(t, r.Top, "rev-parse", "HEAD")
	commit(t, r.Top, "b.yml", "b: 1\n")
	b := resolve(t, r)
	if b.SHA != pushed || b.Ref != "origin/main" || b.Source != "upstream" || b.Commits != 1 {
		t.Fatalf("got %+v", b)
	}
}

func TestChoiceIsRememberedPerBranch(t *testing.T) {
	r := repo(t)
	first := git(t, r.Top, "rev-parse", "HEAD")
	git(t, r.Top, "switch", "-q", "-c", "mywork")
	commit(t, r.Top, "b.yml", "b: 1\n")
	if err := r.SetChoice("HEAD"); err != nil {
		t.Fatal(err)
	}
	head := git(t, r.Top, "rev-parse", "HEAD")
	if b := resolve(t, r); b.SHA != head || b.Source != "chosen" {
		t.Fatalf("uncommitted only: got %+v", b)
	}
	if err := r.SetChoice(first); err != nil {
		t.Fatal(err)
	}
	if b := resolve(t, r); b.SHA != first || b.Source != "chosen" {
		t.Fatalf("commit: got %+v", b)
	}
	if err := r.SetChoice("no-such-branch"); err == nil {
		t.Fatal("an unknown ref must be refused")
	}
	if err := r.SetChoice(""); err != nil {
		t.Fatal(err)
	}
	if b := resolve(t, r); b.Source == "chosen" {
		t.Fatalf("back to automatic: got %+v", b)
	}
}

func TestDetachedHeadComparesWithDefaultBranch(t *testing.T) {
	r := repo(t)
	main := git(t, r.Top, "rev-parse", "HEAD")
	git(t, r.Top, "switch", "-q", "-c", "x")
	c := commit(t, r.Top, "b.yml", "b: 1\n")
	git(t, r.Top, "checkout", "-q", "--detach", c)
	if b := resolve(t, r); b.SHA != main || b.Ref != "main" || b.Branch != "" {
		t.Fatalf("got %+v", b)
	}
}

func TestExportKeepsOnlyWantedFiles(t *testing.T) {
	r := repo(t)
	sha := commit(t, r.Top, "dir/t.yaml", "steps: []\n")
	commit(t, r.Top, "readme.md", "x")
	commit(t, r.Top, "dir/t.yaml", "changed: true\n")
	dir := filepath.Join(t.TempDir(), "export")
	keep := func(p string) bool { return strings.HasSuffix(p, ".yml") || strings.HasSuffix(p, ".yaml") }
	if err := r.Export(sha, dir, keep); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "dir", "t.yaml"))
	if err != nil || string(b) != "steps: []\n" {
		t.Fatalf("t.yaml = %q, %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "readme.md")); err == nil {
		t.Fatal("readme.md exported")
	}
	// reused when complete
	_ = os.WriteFile(filepath.Join(dir, "a.yml"), []byte("touched"), 0o644)
	if err := r.Export(sha, dir, keep); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "a.yml")); string(b) != "touched" {
		t.Fatal("a finished export should be reused")
	}
}

// A branch checked out from the remote (created in Azure DevOps, or by a
// colleague) records "Created from refs/remotes/origin/<it>". That is not
// where the work started, and pushing must not empty the delta.
func TestBranchCheckedOutFromItsRemote(t *testing.T) {
	r := repo(t)
	start := git(t, r.Top, "rev-parse", "HEAD")
	bare := filepath.Join(t.TempDir(), "remote.git")
	git(t, r.Top, "init", "-q", "--bare", bare)
	git(t, r.Top, "remote", "add", "origin", bare)
	git(t, r.Top, "push", "-q", "-u", "origin", "main")
	git(t, r.Top, "switch", "-q", "-c", "feature/x")
	commit(t, r.Top, "b.yml", "b: 1\n")
	git(t, r.Top, "push", "-q", "origin", "feature/x")

	clone := filepath.Join(t.TempDir(), "clone")
	git(t, r.Top, "clone", "-q", bare, clone)
	git(t, clone, "config", "user.email", "t@example.com")
	git(t, clone, "config", "user.name", "t")
	git(t, clone, "config", "commit.gpgsign", "false")
	git(t, clone, "switch", "-q", "feature/x")
	commit(t, clone, "c.yml", "c: 1\n")
	git(t, clone, "push", "-q", "origin", "feature/x")
	c, err := Open(clone, "git")
	if err != nil {
		t.Fatal(err)
	}
	b := resolve(t, c)
	if b.SHA != start || b.Ref != "origin/main" || b.Commits != 2 {
		t.Fatalf("got %+v, want where feature/x splits from origin/main (%s)", b, start)
	}
}

func TestChosenBranchWithoutCommonHistory(t *testing.T) {
	r := repo(t)
	git(t, r.Top, "switch", "-q", "--orphan", "other")
	commit(t, r.Top, "z.yml", "z: 1\n")
	git(t, r.Top, "switch", "-q", "main")
	if _, err := r.ResolveBase("other"); err == nil || !strings.Contains(err.Error(), "no commit in common") {
		t.Fatalf("err = %v", err)
	}
}

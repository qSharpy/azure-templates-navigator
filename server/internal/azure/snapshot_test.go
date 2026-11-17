package azure

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestSnapshot builds a snapshot of a modified, an added (untracked) and a
// deleted file plus an unsaved text, pushes it to a custom ref of a bare
// "server" repository, overwrites and deletes it, and checks that the user's
// repository is untouched.
func TestSnapshot(t *testing.T) {
	dir := t.TempDir()
	work, bare := filepath.Join(dir, "work"), filepath.Join(dir, "server.git")
	git(t, dir, "init", "-q", "--bare", bare)
	git(t, dir, "init", "-q", "-b", "main", work)
	write(t, filepath.Join(work, "azure-pipelines.yml"), "steps:\n- template: local/steps.yml\n")
	write(t, filepath.Join(work, "local/steps.yml"), "steps:\n- script: echo v1\n")
	write(t, filepath.Join(work, "old.yml"), "steps: []\n")
	write(t, filepath.Join(work, "src/main.go"), "package main\n")
	git(t, work, "add", "-A")
	git(t, work, "commit", "-q", "-m", "base")
	git(t, work, "remote", "add", "origin", bare)
	git(t, work, "push", "-q", "origin", "main")
	base := git(t, work, "rev-parse", "HEAD")

	// local work: a committed change, uncommitted changes, an untracked file,
	// a staged deletion, and non-YAML work that must stay local
	write(t, filepath.Join(work, "local/steps.yml"), "steps:\n- script: echo on disk\n")
	write(t, filepath.Join(work, "local/new.yml"), "steps:\n- script: echo new\n")
	write(t, filepath.Join(work, "src/main.go"), "package main // secret work\n")
	git(t, work, "rm", "-q", "old.yml")
	write(t, filepath.Join(work, "notes.txt"), "stash me\n")
	git(t, work, "stash", "push", "-q", "--include-untracked", "--", "notes.txt")
	statusBefore := git(t, work, "status", "--porcelain")
	stashBefore := git(t, work, "stash", "list")

	g := Git{Dir: work}
	ctx := context.Background()
	snap, err := g.Snapshot(ctx, base, []FileChange{
		{Path: "local/steps.yml", Text: "steps:\n- script: echo unsaved\n"}, // an unsaved buffer wins over the disk
		{Path: "local/new.yml", Text: "steps:\n- script: echo new\n"},
		{Path: "old.yml", Deleted: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if p := git(t, work, "rev-parse", snap+"^"); p != base {
		t.Fatalf("parent = %s, want the base %s", p, base)
	}
	if got := git(t, work, "ls-tree", "-r", "--name-only", snap); got != "azure-pipelines.yml\nlocal/new.yml\nlocal/steps.yml\nsrc/main.go" {
		t.Fatalf("tree:\n%s", got)
	}
	if got := git(t, work, "show", snap+":local/steps.yml"); !strings.Contains(got, "unsaved") {
		t.Fatalf("steps.yml = %q", got)
	}
	if got := git(t, work, "show", snap+":src/main.go"); strings.Contains(got, "secret") {
		t.Fatal("non-YAML work was snapshotted")
	}
	if msg := git(t, work, "log", "-1", "--format=%s", snap); !strings.Contains(msg, "[skip ci]") {
		t.Fatalf("message = %q", msg)
	}
	// nothing of the user's changed
	if s := git(t, work, "status", "--porcelain"); s != statusBefore {
		t.Fatalf("status changed:\n%s\nwas:\n%s", s, statusBefore)
	}
	if s := git(t, work, "stash", "list"); s != stashBefore {
		t.Fatal("stash changed")
	}
	if h := git(t, work, "rev-parse", "HEAD"); h != base {
		t.Fatal("HEAD moved")
	}

	ref := RefName("3f2a9c1e", "main")
	if !g.OnRemote(ctx, base) {
		t.Error("base should be on the remote")
	}
	if err := g.Push(ctx, bare, "", "", snap+":"+ref); err != nil {
		t.Fatal(err)
	}
	if got := git(t, bare, "rev-parse", ref); got != snap {
		t.Fatalf("server has %s at %s", got, ref)
	}
	if b := git(t, bare, "branch", "--list"); b != "* main" && b != "main" {
		t.Fatalf("a branch was created: %q", b)
	}
	// overwrite (a later run), then delete
	snap2, err := g.Snapshot(ctx, base, []FileChange{{Path: "local/steps.yml", Text: "steps:\n- script: echo v3\n"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Push(ctx, bare, "", "", snap2+":"+ref); err != nil {
		t.Fatal(err)
	}
	if got := git(t, bare, "rev-parse", ref); got != snap2 {
		t.Fatal("not overwritten")
	}
	if err := g.Push(ctx, bare, "", "", ":"+ref); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", bare, "show-ref", ref).CombinedOutput(); err == nil {
		t.Fatalf("ref still there: %s", out)
	}
	// a bad token push falls back to the user's own credentials (a local path here)
	if err := g.Push(ctx, "https://127.0.0.1:1/x/_git/y", "tok", bare, snap+":"+ref); err != nil {
		t.Fatalf("fallback push: %v", err)
	}
}

func TestTokenEnvKeepsTokenOffTheCommandLine(t *testing.T) {
	env := strings.Join(tokenEnv("https://org@dev.azure.com/org/p/_git/r", "SECRET"), "\n")
	if !strings.Contains(env, "GIT_CONFIG_KEY_1=http.https://dev.azure.com/.extraHeader") || !strings.Contains(env, "Authorization: Bearer SECRET") {
		t.Fatalf("env:\n%s", env)
	}
	if stripUser("https://org@dev.azure.com/org/p/_git/r") != "https://dev.azure.com/org/p/_git/r" {
		t.Fatal(stripUser("https://org@dev.azure.com/org/p/_git/r"))
	}
}

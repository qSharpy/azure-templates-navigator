package azure

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An Azure DevOps token is never handed to git for a remote that isn't
// Azure DevOps: a stand-in git records its environment and arguments.
func TestPushNeverSendsTheTokenOutsideAzure(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "log")
	fake := filepath.Join(dir, "git")
	write(t, fake, "#!/bin/sh\n{ echo \"ARGS $*\"; env | grep '^GIT_CONFIG' ; } >> "+log+"\nexit 1\n")
	if err := os.Chmod(fake, 0o755); err != nil {
		t.Fatal(err)
	}
	g := Git{Path: fake, Dir: dir}
	for _, u := range []string{"https://github.com/contoso/payments-api.git", "https://user@github.com/contoso/x.git", "file:///tmp/x.git"} {
		_ = g.Push(context.Background(), u, "SECRET", "", "abc:refs/atn/preview/x")
	}
	b, _ := os.ReadFile(log)
	if strings.Contains(string(b), "SECRET") || strings.Contains(string(b), "extraHeader") {
		t.Fatalf("token reached a non-Azure remote:\n%s", b)
	}
	_ = g.Push(context.Background(), "https://org@dev.azure.com/org/p/_git/r", "SECRET", "", "abc:refs/atn/preview/x")
	b, _ = os.ReadFile(log)
	if !strings.Contains(string(b), "Authorization: Bearer SECRET") {
		t.Fatalf("Azure push without the token:\n%s", b)
	}
	if !IsAzureURL("https://fabrikam.visualstudio.com/p/_git/r") || IsAzureURL("https://dev.azure.com.evil.example/x") || IsAzureURL("git@github.com:o/r.git") {
		t.Error("IsAzureURL")
	}
}

func TestRemoteBranch(t *testing.T) {
	dir := t.TempDir()
	work := filepath.Join(dir, "work")
	git(t, dir, "init", "-q", "-b", "main", work)
	write(t, filepath.Join(work, "a.yml"), "steps: []\n")
	git(t, work, "add", "-A")
	git(t, work, "commit", "-qm", "one")
	first := git(t, work, "rev-parse", "HEAD")
	git(t, work, "update-ref", "refs/remotes/origin/develop", first)
	write(t, filepath.Join(work, "a.yml"), "steps: [x]\n")
	git(t, work, "commit", "-qam", "two")
	second := git(t, work, "rev-parse", "HEAD")
	git(t, work, "update-ref", "refs/remotes/origin/main", second)
	git(t, work, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	g := Git{Dir: work}
	ctx := context.Background()
	if got := g.RemoteBranch(ctx, first, "origin"); got != "refs/heads/main" {
		t.Errorf("first: %q", got) // on both: the default branch wins
	}
	write(t, filepath.Join(work, "a.yml"), "steps: [y]\n")
	git(t, work, "commit", "-qam", "three")
	if got := g.RemoteBranch(ctx, git(t, work, "rev-parse", "HEAD"), "origin"); got != "" {
		t.Errorf("unpushed: %q", got)
	}
}

// Azure's answers for a GitHub repository at a ref it can't use (recorded on
// pipeline 4, 2026-11-18): no verdict, not a rejection.
func TestPreviewGitHubRefErrors(t *testing.T) {
	s, c := fakeClient(t)
	for _, msg := range []string{
		"Unable to resolve the reference 'refs/atn/preview/test/nope' to a specific version. Verify the reference exists in the source repository.",
		`/azure-pipelines.yml: Could not get the latest source version for repository qSharpy/atn-gh-templates hosted on https://github.com/ using ref refs/atn/preview/test/nope. GitHub reported the error, "No commit found for SHA: refs/atn/preview/test/nope"`,
	} {
		msg := msg
		s.Preview = func(string, int, map[string]interface{}) (int, []byte) {
			return 400, []byte(fmt.Sprintf(`{"message":%q,"typeKey":"PipelineValidationException"}`, msg))
		}
		if r, err := c.Preview(context.Background(), "personal", 2, PreviewRequest{}); err == nil || r.Rejected {
			t.Errorf("%q: %+v %v", msg, r, err)
		}
	}
}

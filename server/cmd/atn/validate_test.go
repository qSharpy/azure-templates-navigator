package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/qSharpy/azure-templates-navigator/server/internal/azure/azuretest"
)

func gitT(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.email=t@example.com", "-c", "user.name=t", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// `atn validate` exit codes against the fake Azure DevOps: 0 when Azure
// compiles your change, 1 when your change makes Azure reject it, 2 when it
// can't ask (no --yes without a terminal, or a sign-in failure).
func TestValidateExitCodes(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("ATN_BASELINE_DIR", t.TempDir())
	t.Setenv("ATN_AZURE_TOKEN", "tok")
	fake := azuretest.New()
	defer fake.Close()
	t.Setenv("ATN_AZURE_URL", fake.URL)

	root := filepath.Join(t.TempDir(), "atn-consumer")
	_ = os.MkdirAll(root, 0o755)
	_ = os.WriteFile(filepath.Join(root, "t.yml"), []byte("steps:\n  - script: echo hi\n"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "azure-pipelines.yml"), []byte("trigger: none\nsteps:\n  - template: t.yml\n"), 0o644)
	gitT(t, root, "init", "-q", "-b", "main")
	gitT(t, root, "add", "-A")
	gitT(t, root, "commit", "-q", "-m", "start")
	bare := filepath.Join(t.TempDir(), "server.git")
	gitT(t, root, "init", "-q", "--bare", bare)
	gitT(t, root, "push", "-q", bare, "main")
	gitT(t, root, "remote", "add", "origin", "https://dev.azure.com/fabrikam/personal/_git/atn-consumer")
	for i := range fake.Repos {
		fake.Repos[i].RemoteURL = "file://" + bare
	}
	_ = os.WriteFile(filepath.Join(root, "t.yml"), []byte("steps:\n  - script: echo changed\n"), 0o644)

	stdout := os.Stdout
	devnull, _ := os.Open(os.DevNull)
	os.Stdout = devnull
	defer func() { os.Stdout = stdout }()

	args := []string{"--root", root, "--since", "HEAD", "--no-fetch", "--format", "json"}
	if code := runValidate(args); code != 2 {
		t.Fatalf("without --yes and no terminal: exit %d, want 2", code)
	}
	if n := len(fake.Previews()); n != 0 {
		t.Fatalf("%d previews without consent", n)
	}
	if code := runValidate(append(args, "--yes")); code != 0 {
		t.Fatalf("compiles: exit %d", code)
	}
	// the change breaks it: the base compiles, the snapshot doesn't
	fake.Preview = func(_ string, _ int, body map[string]interface{}) (int, []byte) {
		self := body["resources"].(map[string]interface{})["repositories"].(map[string]interface{})["self"].(map[string]interface{})
		head, _ := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
		if self["version"] == string(head[:len(head)-1]) {
			return azuretest.PreviewFixture("A_empty")
		}
		return azuretest.PreviewFixture("L_override_bad_param")
	}
	if code := runValidate(append(args, "--yes")); code != 1 {
		t.Fatalf("rejected: exit %d, want 1", code)
	}
	fake.Token = "other"
	if code := runValidate(append(args, "--yes")); code != 2 {
		t.Fatalf("sign-in failure: exit %d, want 2", code)
	}
}

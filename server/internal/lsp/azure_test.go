package lsp

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qSharpy/azure-templates-navigator/server/internal/azure/azuretest"
)

// atn/azurePlan → atn/azureValidate → atn/azureProgress → atn/didChange →
// atn/azureResults, against the fake Azure DevOps.
func TestAzureRequests(t *testing.T) {
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
	pipe := "trigger: none\nsteps:\n  - template: t.yml\n"
	pipePath := filepath.Join(root, "azure-pipelines.yml")
	_ = os.WriteFile(pipePath, []byte(pipe), 0o644)
	gitRun(t, root, "init", "-q", "-b", "main")
	gitRun(t, root, "-c", "user.email=t@example.com", "-c", "user.name=t", "-c", "commit.gpgsign=false", "add", "-A")
	gitRun(t, root, "-c", "user.email=t@example.com", "-c", "user.name=t", "-c", "commit.gpgsign=false", "commit", "-q", "-m", "start")
	bare := filepath.Join(t.TempDir(), "server.git")
	gitRun(t, root, "init", "-q", "--bare", bare)
	gitRun(t, root, "push", "-q", bare, "main")
	gitRun(t, root, "remote", "add", "origin", "https://dev.azure.com/fabrikam/personal/_git/atn-consumer")
	gitRun(t, root, "update-ref", "refs/remotes/origin/main", "HEAD")
	gitRun(t, root, "switch", "-q", "-c", "mywork")
	for i := range fake.Repos {
		fake.Repos[i].RemoteURL = "file://" + bare
	}

	cl := startServer(t)
	cl.call("initialize", map[string]interface{}{
		"rootUri": pathToURI(root),
		"initializationOptions": map[string]interface{}{
			"settings": map[string]interface{}{"repositories": map[string]interface{}{"autoFetch": false}, "azure": map[string]interface{}{"enabled": true}},
		},
	}, nil)
	_ = cl.c.notify("initialized", map[string]interface{}{})
	_ = cl.c.notify("textDocument/didOpen", map[string]interface{}{"textDocument": map[string]interface{}{
		"uri": pathToURI(filepath.Join(root, "t.yml")), "languageId": "yaml", "version": 1, "text": "steps:\n  - script: echo changed\n"}})
	cl.waitNote("atn/didChange", nil)

	var plan struct {
		ID, Organization, Summary, Error string
		Checkable                        int
		Pipelines                        []struct {
			URI        string
			Definition *struct{ ID int }
		}
		Snapshots []struct {
			Ref   string
			Files []struct{ RelativePath string }
		}
	}
	cl.call("atn/azurePlan", map[string]interface{}{}, &plan)
	if plan.Error != "" || plan.ID == "" || plan.Organization != "fabrikam" || plan.Checkable != 1 || plan.Pipelines[0].Definition.ID != 2 ||
		plan.Pipelines[0].URI != pathToURI(pipePath) || len(plan.Snapshots) != 1 || plan.Snapshots[0].Ref != "refs/atn/preview/3f2a9c1e/mywork" {
		t.Fatalf("plan %+v", plan)
	}

	var res map[string]interface{}
	cl.call("atn/azureValidate", map[string]interface{}{"planId": "p-not-this-one"}, &res)
	if res["error"] == nil {
		t.Fatalf("started without the right plan: %v", res)
	}
	cl.call("atn/azureValidate", map[string]interface{}{"planId": plan.ID}, &res)
	if res["started"] != true {
		t.Fatalf("validate: %v", res)
	}
	cl.waitNote("atn/azureProgress", func(m *message) bool { return strings.Contains(string(m.Params), `"finished":true`) })
	cl.waitNote("atn/didChange", func(m *message) bool { return strings.Contains(string(m.Params), "azure") })

	var rep struct {
		State, Summary string
		Pipelines      []struct {
			URI, Verdict, Before, Agreement string
			Expansion                       *struct{ Changed bool }
		}
	}
	cl.call("atn/azureResults", nil, &rep)
	if rep.State != "done" || len(rep.Pipelines) != 1 || rep.Pipelines[0].Verdict != "compiles" || rep.Pipelines[0].Agreement != "agree" ||
		rep.Pipelines[0].Expansion == nil || rep.Pipelines[0].Expansion.Changed {
		t.Fatalf("results %+v", rep)
	}
	var all []struct {
		URI   string
		Azure *struct{ Verdict string }
	}
	cl.call("atn/pipelineCheck", map[string]string{}, &all)
	if len(all) != 1 || all[0].Azure == nil || all[0].Azure.Verdict != "compiles" {
		raw, _ := json.Marshal(all)
		t.Fatalf("pipelineCheck %s", raw)
	}
	var ex struct{ Base, Change, Title string }
	cl.call("atn/azureExpansion", map[string]string{"uri": pathToURI(pipePath)}, &ex)
	if ex.Base == "" || ex.Base != ex.Change || ex.Title != "azure-pipelines.yml" {
		t.Fatalf("expansion %+v", ex)
	}
	out, _ := exec.Command("git", "-C", bare, "for-each-ref", "refs/atn").CombinedOutput()
	if len(strings.TrimSpace(string(out))) > 0 {
		t.Fatalf("snapshot ref left: %s", out)
	}
	if n := len(fake.Previews()); n != 2 {
		t.Fatalf("%d previews", n)
	}
}

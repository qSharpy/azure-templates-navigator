package engine

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qSharpy/azure-templates-navigator/server/internal/azure"
	"github.com/qSharpy/azure-templates-navigator/server/internal/azure/azuretest"
)

// Issue #10's setup: an Azure DevOps pipeline (payments-api-ci, project
// Payments) whose YAML lives on GitHub (contoso/payments-api,
// /ci/azure-pipelines.yml), with a template of its own.
const (
	ghRoot = `trigger: none
stages:
- template: /stages/build.yml
  parameters:
    appName: payments-api
`
	ghStages = `parameters:
- { name: appName, type: string }
stages:
- stage: build
  jobs:
  - job: build
    steps:
    - script: echo build ${{ parameters.appName }}
`
)

type ghWorkspace struct {
	repo, bare string
	fake       *azuretest.Server
	e          *Engine
}

// newGHWorkspace: origin is on github.com (fetches never happen), and its
// pushurl is a local bare repository standing in for GitHub.
func newGHWorkspace(t *testing.T, org, project string) *ghWorkspace {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	w := &ghWorkspace{repo: filepath.Join(dir, "payments-api"), bare: filepath.Join(t.TempDir(), "payments-api.git")}
	gitIn(t, dir, "init", "-q", "-b", "main", w.repo)
	gitIn(t, w.repo, "config", "user.email", "t@example.com")
	gitIn(t, w.repo, "config", "user.name", "t")
	gitIn(t, w.repo, "config", "commit.gpgsign", "false")
	write(t, w.repo, "ci/azure-pipelines.yml", ghRoot)
	write(t, w.repo, "stages/build.yml", ghStages)
	commitAll(t, w.repo, "start")
	gitIn(t, dir, "init", "-q", "--bare", w.bare)
	gitIn(t, w.repo, "push", "-q", w.bare, "main")
	gitIn(t, w.repo, "remote", "add", "origin", "https://github.com/contoso/payments-api.git")
	gitIn(t, w.repo, "remote", "set-url", "--push", "origin", w.bare)
	gitIn(t, w.repo, "update-ref", "refs/remotes/origin/main", "HEAD")

	w.fake = azuretest.New()
	t.Cleanup(w.fake.Close)
	t.Setenv("ATN_AZURE_URL", w.fake.URL)
	w.fake.Defs = append(w.fake.Defs, azuretest.Def{ID: 7, Name: "payments-api-ci", Project: "Payments", YAMLFilename: "/ci/azure-pipelines.yml",
		RepoID: "contoso/payments-api", RepoType: "GitHub"})
	w.fake.Preview = w.preview(t)

	s := DefaultSettings()
	s.Repositories.AutoFetch = false
	s.Repositories.CacheDir = filepath.Join(t.TempDir(), "cache")
	s.Repositories.AzureDevOpsOrganization = org
	s.Repositories.AzureDevOpsProject = project
	s.Azure.Enabled = true
	w.e = New([]string{w.repo}, s)
	w.e.BaselineDir = t.TempDir()
	w.e.SetAzureTokens(azure.Static("tok"))
	w.e.Rescan()
	return w
}

// preview reads self at the version asked for from the bare "GitHub" (main
// when none), the root from yamlOverride when sent, and rejects `scriptt:`.
func (w *ghWorkspace) preview(t *testing.T) azuretest.PreviewFunc {
	return func(project string, def int, body map[string]interface{}) (int, []byte) {
		v := "main"
		res, _ := body["resources"].(map[string]interface{})
		rs, _ := res["repositories"].(map[string]interface{})
		if o, ok := rs["self"].(map[string]interface{}); ok {
			if s, _ := o["version"].(string); s != "" {
				v = s
			}
		}
		show := func(path string) string {
			out, _ := exec.Command("git", "-C", w.bare, "show", v+":"+path).CombinedOutput()
			return string(out)
		}
		root, _ := body["yamlOverride"].(string)
		if root == "" {
			root = show("ci/azure-pipelines.yml")
		}
		stages := show("stages/build.yml")
		for i, l := range strings.Split(stages, "\n") {
			if c := strings.Index(l, "scriptt"); c >= 0 {
				msg := fmt.Sprintf("/stages/build.yml (Line: %d, Col: %d): Unexpected value 'scriptt'", i+1, c+1)
				return 400, []byte(fmt.Sprintf(`{"message":%q,"typeKey":"PipelineValidationException"}`, msg))
			}
		}
		return 200, []byte(fmt.Sprintf(`{"finalYaml":%q}`, root+stages))
	}
}

func (w *ghWorkspace) plan(t *testing.T) *AzurePlan {
	t.Helper()
	plan, err := w.e.AzurePlan(context.Background(), AzureOptions{Since: "HEAD"})
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func ghSelf(p azuretest.Call) map[string]interface{} {
	res, _ := p.Body["resources"].(map[string]interface{})
	rs, _ := res["repositories"].(map[string]interface{})
	o, _ := rs["self"].(map[string]interface{})
	return o
}

// Only the root changed: nothing is pushed to GitHub. Both sides pin self to
// the base on main, and the change side sends the edited root. The project
// is found by searching the organization.
func TestAzureGitHubRootOnly(t *testing.T) {
	w := newGHWorkspace(t, "fabrikam", "")
	base := gitIn(t, w.repo, "rev-parse", "HEAD")
	write(t, w.repo, "ci/azure-pipelines.yml", strings.Replace(ghRoot, "appName: payments-api", "appName: payments-api-v2", 1))
	w.e.FileChanged(filepath.Join(w.repo, "ci/azure-pipelines.yml"))

	plan := w.plan(t)
	if plan.Organization != "fabrikam" || plan.Project != "Payments" || len(plan.Snapshots) != 0 || plan.Checkable() != 1 {
		t.Fatalf("plan: %+v", plan)
	}
	if p := plan.Pipelines[0]; !p.YAMLOverride || p.Definition == nil || p.Definition.ID != 7 {
		t.Fatalf("pipeline: %+v", p)
	}
	if !strings.Contains(plan.Summary, "end 1 changed pipeline file in the preview request") || strings.Contains(plan.Summary, "changed YAML file") {
		t.Errorf("summary: %s", plan.Summary)
	}
	rep := w.e.AzureRun(context.Background(), plan, nil)
	if rep.State != "done" {
		t.Fatalf("%s: %s", rep.State, rep.Error)
	}
	r := rep.Pipelines[0]
	if r.Now.Verdict != "compiles" || r.Before.Verdict != "compiles" || r.Expansion == nil || !r.Expansion.Changed {
		t.Fatalf("result: %+v", r)
	}
	previews := w.fake.Previews()
	if len(previews) != 2 {
		t.Fatalf("%d previews", len(previews))
	}
	for _, p := range previews {
		if !strings.Contains(p.Path, "/Payments/_apis/pipelines/7/") {
			t.Errorf("path %s", p.Path)
		}
		if s := ghSelf(p); s["refName"] != "refs/heads/main" || s["version"] != base {
			t.Errorf("self: %v", s)
		}
	}
	if previews[0].Body["yamlOverride"] == previews[1].Body["yamlOverride"] {
		t.Error("one side should send the changed root")
	}
	if refs := refsOn(t, w.bare); refs != "" {
		t.Errorf("pushed to GitHub: %s", refs)
	}
	var searched bool
	for _, c := range w.fake.Calls() {
		searched = searched || strings.HasSuffix(c.Path, "/_apis/projects")
		if strings.Contains(c.Path, "/build/definitions") && strings.Contains(c.Query, "repositoryType=GitHub") &&
			!strings.Contains(c.Query, "repositoryId=contoso%2Fpayments-api") {
			t.Errorf("definitions query: %s", c.Query)
		}
	}
	if !searched {
		t.Error("no project set: the organization's projects should be searched")
	}
}

// A template changed too: the snapshot is pushed to GitHub (with the user's
// git setup: here the pushurl) and deleted afterwards; Azure's error is
// placed on the template.
func TestAzureGitHubTemplateChange(t *testing.T) {
	w := newGHWorkspace(t, "fabrikam", "Payments")
	write(t, w.repo, "stages/build.yml", strings.Replace(ghStages, "- script:", "- scriptt:", 1))
	w.e.FileChanged(filepath.Join(w.repo, "stages/build.yml"))

	plan := w.plan(t)
	if len(plan.Snapshots) != 1 || !plan.Snapshots[0].GitHub || plan.Snapshots[0].Repo != "contoso/payments-api" ||
		plan.Snapshots[0].RemoteURL != "https://github.com/contoso/payments-api.git" || plan.Pipelines[0].YAMLOverride {
		t.Fatalf("plan: %+v", plan)
	}
	if !strings.Contains(plan.Summary, "GitHub, with your own git credentials") {
		t.Errorf("summary: %s", plan.Summary)
	}
	rep := w.e.AzureRun(context.Background(), plan, nil)
	if rep.State != "done" {
		t.Fatalf("%s: %s", rep.State, rep.Error)
	}
	r := rep.Pipelines[0]
	if r.Before.Verdict != "compiles" || r.Now.Verdict != "rejected" || len(r.New) != 1 ||
		r.New[0].Path != filepath.Join(w.repo, "stages/build.yml") || r.New[0].Line != 7 {
		t.Fatalf("result: %+v", r)
	}
	for _, p := range w.fake.Previews() {
		if s := ghSelf(p); !strings.HasPrefix(fmt.Sprint(s["refName"]), "refs/atn/preview/") {
			t.Errorf("self: %v", s)
		}
	}
	for _, c := range w.fake.Calls() {
		if strings.HasSuffix(c.Path, "/_apis/projects") {
			t.Error("the project is set: no search")
		}
	}
	if refs := refsOn(t, w.bare); refs != "" {
		t.Errorf("snapshot ref left behind: %s", refs)
	}
}

// GitHub refuses the push: the reason says what atn needs.
func TestAzureGitHubRefusedPush(t *testing.T) {
	w := newGHWorkspace(t, "fabrikam", "Payments")
	if err := os.WriteFile(filepath.Join(w.bare, "hooks", "pre-receive"), []byte("#!/bin/sh\necho 'protected' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, w.repo, "stages/build.yml", ghStages+"    - script: echo more\n")
	w.e.FileChanged(filepath.Join(w.repo, "stages/build.yml"))
	rep := w.e.AzureRun(context.Background(), w.plan(t), nil)
	if rep.State != "failed" || !strings.Contains(rep.Error, "contoso/payments-api on GitHub") || !strings.Contains(rep.Error, "write access") {
		t.Fatalf("%s: %s", rep.State, rep.Error)
	}
}

// No organization in the settings and nothing on Azure Repos to take it
// from: the error names the setting.
func TestAzureGitHubNeedsOrganization(t *testing.T) {
	w := newGHWorkspace(t, "", "")
	write(t, w.repo, "ci/azure-pipelines.yml", ghRoot+"# edit\n")
	w.e.FileChanged(filepath.Join(w.repo, "ci/azure-pipelines.yml"))
	_, err := w.e.AzurePlan(context.Background(), AzureOptions{Since: "HEAD"})
	if err == nil || !strings.Contains(err.Error(), "repositories.azureDevOpsOrganization") || !strings.Contains(err.Error(), "contoso/payments-api") {
		t.Fatalf("err: %v", err)
	}
}

// The layout of issue #10: the pipeline's YAML is on GitHub, its templates in
// an Azure Repos repository (alias `templates`). A template change is pushed
// to Azure Repos, self is left alone, the preview goes to the GitHub
// pipeline's project, and with no setting the organization comes from the
// Azure Repos repository.
func TestAzureGitHubRootWithAzureReposTemplates(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir, server := t.TempDir(), t.TempDir()
	app, tpl := filepath.Join(dir, "payments-api"), filepath.Join(dir, "pipeline-templates")
	appBare, tplBare := filepath.Join(server, "payments-api.git"), filepath.Join(server, "pipeline-templates.git")
	for _, r := range []struct {
		root, bare, origin string
		files              map[string]string
	}{
		{app, appBare, "https://github.com/contoso/payments-api.git", map[string]string{"ci/azure-pipelines.yml": `trigger: none
resources:
  repositories:
  - repository: templates
    type: git
    name: Platform/pipeline-templates
stages:
- template: stages/build.yml@templates
  parameters:
    appName: payments-api
`}},
		{tpl, tplBare, "https://fabrikam@dev.azure.com/fabrikam/Platform/_git/pipeline-templates", map[string]string{"stages/build.yml": ghStages}},
	} {
		gitIn(t, dir, "init", "-q", "-b", "main", r.root)
		gitIn(t, r.root, "config", "user.email", "t@example.com")
		gitIn(t, r.root, "config", "user.name", "t")
		gitIn(t, r.root, "config", "commit.gpgsign", "false")
		for rel, text := range r.files {
			write(t, r.root, rel, text)
		}
		commitAll(t, r.root, "start")
		gitIn(t, server, "init", "-q", "--bare", r.bare)
		gitIn(t, r.root, "push", "-q", r.bare, "main")
		gitIn(t, r.root, "remote", "add", "origin", r.origin)
		gitIn(t, r.root, "update-ref", "refs/remotes/origin/main", "HEAD")
	}
	gitIn(t, app, "remote", "set-url", "--push", "origin", appBare)

	fake := azuretest.New()
	t.Cleanup(fake.Close)
	t.Setenv("ATN_AZURE_URL", fake.URL)
	fake.Repos = append(fake.Repos, azuretest.Repo{ID: "9d1c", Name: "pipeline-templates", Project: "Platform", RemoteURL: "file://" + tplBare})
	fake.Defs = append(fake.Defs, azuretest.Def{ID: 7, Name: "payments-api-ci", Project: "Payments", YAMLFilename: "ci/azure-pipelines.yml",
		RepoID: "contoso/payments-api", RepoType: "GitHub"})
	fake.Preview = func(project string, def int, body map[string]interface{}) (int, []byte) {
		v := "main"
		res, _ := body["resources"].(map[string]interface{})
		rs, _ := res["repositories"].(map[string]interface{})
		if o, ok := rs["templates"].(map[string]interface{}); ok {
			v, _ = o["version"].(string)
		}
		out, _ := exec.Command("git", "-C", tplBare, "show", v+":stages/build.yml").CombinedOutput()
		return 200, []byte(fmt.Sprintf(`{"finalYaml":%q}`, string(out)))
	}

	s := DefaultSettings()
	s.Repositories.AutoFetch = false
	s.Repositories.CacheDir = filepath.Join(t.TempDir(), "cache")
	s.Azure.Enabled = true
	e := New([]string{app, tpl}, s)
	e.BaselineDir = t.TempDir()
	e.SetAzureTokens(azure.Static("tok"))
	e.Rescan()

	write(t, tpl, "stages/build.yml", ghStages+"    - script: echo more\n")
	e.FileChanged(filepath.Join(tpl, "stages/build.yml"))
	plan, err := e.AzurePlan(context.Background(), AzureOptions{Since: "HEAD"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Organization != "fabrikam" || plan.Project != "Payments" || plan.Checkable() != 1 || len(plan.Snapshots) != 1 ||
		plan.Snapshots[0].Repo != "pipeline-templates" || plan.Snapshots[0].GitHub {
		t.Fatalf("plan: %+v %+v", plan, plan.Pipelines)
	}
	rep := e.AzureRun(context.Background(), plan, nil)
	if rep.State != "done" || rep.Pipelines[0].Now.Verdict != "compiles" || rep.Pipelines[0].Expansion == nil || !rep.Pipelines[0].Expansion.Changed {
		t.Fatalf("%s %s %+v", rep.State, rep.Error, rep.Pipelines)
	}
	for _, p := range fake.Previews() {
		if !strings.Contains(p.Path, "/Payments/_apis/pipelines/7/") || ghSelf(p) != nil || p.Body["yamlOverride"] != nil {
			t.Errorf("preview %s %v", p.Path, p.Body)
		}
	}
	if refs := refsOn(t, appBare); refs != "" {
		t.Errorf("pushed to GitHub: %s", refs)
	}
	if refs := refsOn(t, tplBare); refs != "" {
		t.Errorf("snapshot ref left behind: %s", refs)
	}
}

// Everything on GitHub: the app's pipeline and a shared templates repository
// (`type: github` alias), with only the definition in Azure DevOps. Both the
// root and a shared template change: the root goes in the request (nothing
// pushed to the app repository), the template is pushed to the templates
// repository with the user's credentials and passed through the alias.
func TestAzureGitHubTemplatesRepository(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir, server := t.TempDir(), t.TempDir()
	app, tpl := filepath.Join(dir, "payments-api"), filepath.Join(dir, "pipeline-templates")
	appBare, tplBare := filepath.Join(server, "payments-api.git"), filepath.Join(server, "pipeline-templates.git")
	root := `trigger: none
resources:
  repositories:
  - repository: templates
    type: github
    name: contoso/pipeline-templates
    endpoint: contoso-github
stages:
- template: stages/build.yml@templates
  parameters:
    appName: payments-api
`
	for _, r := range []struct {
		root, bare, origin string
		files              map[string]string
	}{
		{app, appBare, "https://github.com/contoso/payments-api.git", map[string]string{"azure-pipelines.yml": root}},
		{tpl, tplBare, "git@github.com:contoso/pipeline-templates.git", map[string]string{"stages/build.yml": ghStages}},
	} {
		gitIn(t, dir, "init", "-q", "-b", "main", r.root)
		gitIn(t, r.root, "config", "user.email", "t@example.com")
		gitIn(t, r.root, "config", "user.name", "t")
		gitIn(t, r.root, "config", "commit.gpgsign", "false")
		for rel, text := range r.files {
			write(t, r.root, rel, text)
		}
		commitAll(t, r.root, "start")
		gitIn(t, server, "init", "-q", "--bare", r.bare)
		gitIn(t, r.root, "push", "-q", r.bare, "main")
		gitIn(t, r.root, "remote", "add", "origin", r.origin)
		gitIn(t, r.root, "remote", "set-url", "--push", "origin", r.bare)
		gitIn(t, r.root, "update-ref", "refs/remotes/origin/main", "HEAD")
	}
	appBase := gitIn(t, app, "rev-parse", "HEAD")

	fake := azuretest.New()
	t.Cleanup(fake.Close)
	t.Setenv("ATN_AZURE_URL", fake.URL)
	fake.Defs = append(fake.Defs, azuretest.Def{ID: 7, Name: "payments-api-ci", Project: "Payments", YAMLFilename: "azure-pipelines.yml",
		RepoID: "contoso/payments-api", RepoType: "GitHub"})
	fake.Preview = func(project string, def int, body map[string]interface{}) (int, []byte) {
		v := "main"
		res, _ := body["resources"].(map[string]interface{})
		rs, _ := res["repositories"].(map[string]interface{})
		if o, ok := rs["templates"].(map[string]interface{}); ok {
			v, _ = o["version"].(string)
		}
		out, _ := exec.Command("git", "-C", tplBare, "show", v+":stages/build.yml").CombinedOutput()
		y, _ := body["yamlOverride"].(string)
		return 200, []byte(fmt.Sprintf(`{"finalYaml":%q}`, y+string(out)))
	}

	s := DefaultSettings()
	s.Repositories.AutoFetch = false
	s.Repositories.CacheDir = filepath.Join(t.TempDir(), "cache")
	s.Repositories.AzureDevOpsOrganization = "fabrikam"
	s.Repositories.AzureDevOpsProject = "Payments"
	s.Azure.Enabled = true
	e := New([]string{app, tpl}, s)
	e.BaselineDir = t.TempDir()
	e.SetAzureTokens(azure.Static("tok"))
	e.Rescan()

	write(t, app, "azure-pipelines.yml", strings.Replace(root, "appName: payments-api", "appName: payments-api-v2", 1))
	e.FileChanged(filepath.Join(app, "azure-pipelines.yml"))
	write(t, tpl, "stages/build.yml", ghStages+"    - script: echo shared change\n")
	e.FileChanged(filepath.Join(tpl, "stages/build.yml"))

	plan, err := e.AzurePlan(context.Background(), AzureOptions{Since: "HEAD"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Checkable() != 1 || !plan.Pipelines[0].YAMLOverride || len(plan.Snapshots) != 1 || !plan.Snapshots[0].GitHub ||
		plan.Snapshots[0].Repo != "contoso/pipeline-templates" {
		t.Fatalf("plan: %+v %+v", plan.Snapshots, plan.Pipelines)
	}
	rep := e.AzureRun(context.Background(), plan, nil)
	if rep.State != "done" || rep.Pipelines[0].Now.Verdict != "compiles" || rep.Pipelines[0].Expansion == nil || !rep.Pipelines[0].Expansion.Changed {
		t.Fatalf("%s %s %+v", rep.State, rep.Error, rep.Pipelines)
	}
	_, change, _ := e.AzureExpansion(rep.Pipelines[0].Path)
	if !strings.Contains(change, "payments-api-v2") || !strings.Contains(change, "shared change") {
		t.Errorf("change side should have both edits:\n%s", change)
	}
	for _, p := range fake.Previews() {
		rs := p.Body["resources"].(map[string]interface{})["repositories"].(map[string]interface{})
		if s := ghSelf(p); s["refName"] != "refs/heads/main" || s["version"] != appBase {
			t.Errorf("self: %v", s)
		}
		if a, _ := rs["templates"].(map[string]interface{}); !strings.HasPrefix(fmt.Sprint(a["refName"]), "refs/atn/preview/") {
			t.Errorf("templates alias: %v", rs)
		}
	}
	if refs := refsOn(t, appBare); refs != "" {
		t.Errorf("pushed to the app repository: %s", refs)
	}
	if refs := refsOn(t, tplBare); refs != "" {
		t.Errorf("snapshot ref left behind: %s", refs)
	}
}

package engine

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qSharpy/azure-templates-navigator/server/internal/azure"
	"github.com/qSharpy/azure-templates-navigator/server/internal/azure/azuretest"
	"github.com/qSharpy/azure-templates-navigator/server/internal/pipeline"
)

// The spike's repositories (docs/design/phase2-spike/azure-repos): a
// consumer pipeline calling a template in another repository through an
// alias, and a template of its own.
const (
	azConsumer = `trigger: none

resources:
  repositories:
  - repository: templates
    type: git
    name: personal/atn-templates
    ref: refs/heads/main

stages:
- template: templates/stages.yml@templates
  parameters:
    env: dev
- stage: local
  jobs:
  - job: local
    steps:
    - template: local/steps.yml
`
	azLocalSteps = `steps:
- script: echo local
  displayName: 'local-version: v1'
`
	azStages = `parameters:
- name: env
  type: string
  default: dev
  values: [dev, prod]

stages:
- stage: tpl
  jobs:
  - job: tpl
    steps:
    - script: echo v1
      displayName: 'tpl-version: v1'
`
)

// azWorkspace is the consumer and template repositories side by side (both
// workspace roots), each with an origin on dev.azure.com/fabrikam and a bare
// repository standing in for Azure's git server, and a fake of Azure DevOps
// whose previews compile what was really pushed.
type azWorkspace struct {
	consumer, templates string
	servers             map[string]string // repository name -> bare repository
	fake                *azuretest.Server
	e                   *Engine
}

func newAzWorkspace(t *testing.T) *azWorkspace {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	w := &azWorkspace{consumer: filepath.Join(dir, "atn-consumer"), templates: filepath.Join(dir, "atn-templates"), servers: map[string]string{}}
	server := t.TempDir()
	for name, files := range map[string]map[string]string{
		"atn-consumer":  {"azure-pipelines.yml": azConsumer, "local/steps.yml": azLocalSteps},
		"atn-templates": {"templates/stages.yml": azStages},
	} {
		root := filepath.Join(dir, name)
		gitIn(t, dir, "init", "-q", "-b", "main", root)
		gitIn(t, root, "config", "user.email", "t@example.com")
		gitIn(t, root, "config", "user.name", "t")
		gitIn(t, root, "config", "commit.gpgsign", "false")
		for rel, content := range files {
			write(t, root, rel, content)
		}
		commitAll(t, root, "start")
		bare := filepath.Join(server, name+".git")
		gitIn(t, server, "init", "-q", "--bare", bare)
		gitIn(t, root, "push", "-q", bare, "main")
		gitIn(t, root, "remote", "add", "origin", "https://fabrikam@dev.azure.com/fabrikam/personal/_git/"+name)
		gitIn(t, root, "update-ref", "refs/remotes/origin/main", "HEAD")
		w.servers[name] = bare
	}

	w.fake = azuretest.New()
	t.Cleanup(w.fake.Close)
	t.Setenv("ATN_AZURE_URL", w.fake.URL)
	for i := range w.fake.Repos {
		w.fake.Repos[i].RemoteURL = "file://" + w.servers[w.fake.Repos[i].Name]
	}
	w.fake.Preview = w.preview(t)

	s := DefaultSettings()
	s.Repositories.AutoFetch = false
	s.Repositories.CacheDir = filepath.Join(t.TempDir(), "cache")
	s.Azure.Enabled = true
	w.e = New([]string{w.consumer, w.templates}, s)
	w.e.BaselineDir = t.TempDir()
	w.e.SetAzureTokens(azure.Static("tok"))
	w.e.Rescan()
	return w
}

// preview compiles pipeline 2 like Azure would, enough for the tests: it
// reads the root and the templates from the bare repositories at the
// versions the request asks for, rejects `scriptt:` with Azure's message,
// and returns the files' text as the expansion.
func (w *azWorkspace) preview(t *testing.T) azuretest.PreviewFunc {
	return func(project string, def int, body map[string]interface{}) (int, []byte) {
		version := func(alias, repo string) string {
			res, _ := body["resources"].(map[string]interface{})
			rs, _ := res["repositories"].(map[string]interface{})
			if o, ok := rs[alias].(map[string]interface{}); ok {
				if v, _ := o["version"].(string); v != "" {
					return v
				}
			}
			return "main"
		}
		show := func(repo, v, path string) string {
			out, err := exec.Command("git", "-C", w.servers[repo], "show", v+":"+path).CombinedOutput()
			if err != nil {
				return ""
			}
			return string(out)
		}
		self, tpl := version("self", "atn-consumer"), version("templates", "atn-templates")
		root := show("atn-consumer", self, "azure-pipelines.yml")
		stages := show("atn-templates", tpl, "templates/stages.yml")
		steps := show("atn-consumer", self, "local/steps.yml")
		if stages == "" {
			return 400, []byte(`{"message":"/templates/stages.yml@templates: File /templates/stages.yml not found in repository","typeKey":"PipelineValidationException"}`)
		}
		for i, l := range strings.Split(stages, "\n") {
			if strings.Contains(l, "scriptt:") {
				msg := fmt.Sprintf("/templates/stages.yml@templates (Line: %d, Col: %d): Unexpected value 'scriptt'", i+1, strings.Index(l, "scriptt")+1)
				return 400, []byte(fmt.Sprintf(`{"message":%q,"typeKey":"PipelineValidationException"}`, msg))
			}
		}
		final := "# root\n" + root + "# stages\n" + stages + "# steps\n" + steps
		return 200, []byte(fmt.Sprintf(`{"finalYaml":%q}`, final))
	}
}

func (w *azWorkspace) check(t *testing.T) (*AzurePlan, *AzureReport) {
	t.Helper()
	plan, err := w.e.AzurePlan(context.Background(), AzureOptions{Since: "HEAD"})
	if err != nil {
		t.Fatal(err)
	}
	rep := w.e.AzureRun(context.Background(), plan, nil)
	if rep.State != "done" {
		t.Fatalf("state %s: %s", rep.State, rep.Error)
	}
	return plan, rep
}

func refsOn(t *testing.T, bare string) string {
	out, _ := exec.Command("git", "-C", bare, "for-each-ref", "refs/atn").CombinedOutput()
	return strings.TrimSpace(string(out))
}

// A template change in another repository: only that repository gets a
// snapshot, the consumer stays on its branch, and the expansion diff shows
// the new step.
func TestAzureTemplateRepoChange(t *testing.T) {
	w := newAzWorkspace(t)
	write(t, w.templates, "templates/stages.yml", azStages+"    - script: echo new\n      displayName: added\n")
	w.e.FileChanged(filepath.Join(w.templates, "templates/stages.yml"))

	plan, rep := w.check(t)
	if plan.Organization != "fabrikam" || plan.Project != "personal" || plan.User != "Test User" {
		t.Fatalf("plan: %+v", plan)
	}
	if len(plan.Snapshots) != 1 || plan.Snapshots[0].Repo != "atn-templates" || plan.Snapshots[0].Ref != "refs/atn/preview/3f2a9c1e/main" ||
		len(plan.Snapshots[0].Files) != 1 || plan.Snapshots[0].Files[0].RelativePath != "templates/stages.yml" || plan.Snapshots[0].UnpushedBase {
		t.Fatalf("snapshots: %+v", plan.Snapshots)
	}
	if len(plan.Pipelines) != 1 || plan.Pipelines[0].Definition == nil || plan.Pipelines[0].Definition.ID != 2 {
		t.Fatalf("pipelines: %+v", plan.Pipelines)
	}
	if !strings.Contains(plan.Summary, "push 1 changed YAML file") && !strings.Contains(plan.Summary, "Push 1 changed YAML file") {
		t.Errorf("summary: %s", plan.Summary)
	}

	previews := w.fake.Previews()
	if len(previews) != 2 {
		t.Fatalf("%d previews", len(previews))
	}
	for _, p := range previews {
		rs := p.Body["resources"].(map[string]interface{})["repositories"].(map[string]interface{})
		if _, ok := rs["self"]; ok {
			t.Errorf("the consumer has no changes, but self was overridden: %v", rs)
		}
		if rs["templates"].(map[string]interface{})["refName"] != "refs/atn/preview/3f2a9c1e/main" {
			t.Errorf("templates: %v", rs)
		}
	}
	r := rep.Pipelines[0]
	if r.Now.Verdict != "compiles" || r.Before.Verdict != "compiles" || r.Agreement != "agree" {
		t.Fatalf("result: %+v", r)
	}
	if r.Expansion == nil || !r.Expansion.Changed || r.Expansion.Added != 2 || r.Expansion.Removed != 0 {
		t.Fatalf("expansion: %+v", r.Expansion)
	}
	base, change, ok := w.e.AzureExpansion(r.Path)
	if !ok || strings.Contains(base, "echo new") || !strings.Contains(change, "echo new") {
		t.Fatal("expansion texts")
	}
	if rep.Totals.ChangedExpansions != 1 || rep.Totals.Compiles != 1 {
		t.Errorf("totals: %+v", rep.Totals)
	}
	if refs := refsOn(t, w.servers["atn-templates"]); refs != "" {
		t.Errorf("snapshot ref left behind: %s", refs)
	}
	if !strings.Contains(r.Title(), "atn agrees") || !strings.Contains(r.Title(), "expansion +2 −0") {
		t.Errorf("title: %s", r.Title())
	}
}

// An unsaved edit that breaks a template: Azure rejects the pipeline now and
// not at the base, on the template's line, and atn didn't see it.
func TestAzureRejectsUnsavedChange(t *testing.T) {
	w := newAzWorkspace(t)
	broken := strings.Replace(azStages, "    - script: echo v1", "    - scriptt: echo v1", 1)
	path := filepath.Join(w.templates, "templates/stages.yml")
	w.e.OpenDocument(path, broken) // not saved

	_, rep := w.check(t)
	r := rep.Pipelines[0]
	if r.Now.Verdict != "rejected" || r.Before.Verdict != "compiles" || len(r.New) != 1 || r.Existing != 0 {
		t.Fatalf("result: %+v", r)
	}
	p := r.New[0]
	if p.Path != path || p.Line != 11 || p.Col != 6 || p.Message != "Unexpected value 'scriptt'" || p.AzurePath != "/templates/stages.yml@templates" {
		t.Fatalf("problem: %+v", p)
	}
	if r.Agreement != "azureOnly" || rep.Totals.NewlyRejected != 1 || rep.Totals.Disagree != 1 || r.Expansion != nil {
		t.Fatalf("agreement %q, totals %+v", r.Agreement, rep.Totals)
	}
	if !strings.Contains(rep.Summary(), "rejected because of your changes") {
		t.Errorf("summary: %s", rep.Summary())
	}
}

// A change in the pipeline's own repository: self is previewed on the
// snapshot ref on both sides (same Build.SourceBranch), pinned to the base
// and to the snapshot.
func TestAzureSelfChangeAndNewPipeline(t *testing.T) {
	w := newAzWorkspace(t)
	write(t, w.consumer, "local/steps.yml", strings.Replace(azLocalSteps, "v1", "v2", 1))
	write(t, w.consumer, "other.yml", "trigger: none\nsteps:\n- template: local/steps.yml\n")
	w.e.Rescan()

	plan, rep := w.check(t)
	var skipped []string
	for _, p := range plan.Pipelines {
		if p.Skip != "" {
			skipped = append(skipped, p.RelativePath+": "+p.Skip)
		}
	}
	if len(skipped) != 1 || !strings.Contains(skipped[0], "other.yml: not registered") {
		t.Fatalf("skipped: %v", skipped)
	}
	base := gitIn(t, w.consumer, "rev-parse", "HEAD")
	var versions []string
	for _, p := range w.fake.Previews() {
		self := p.Body["resources"].(map[string]interface{})["repositories"].(map[string]interface{})["self"].(map[string]interface{})
		if self["refName"] != "refs/atn/preview/3f2a9c1e/main" {
			t.Fatalf("self: %v", self)
		}
		versions = append(versions, self["version"].(string))
	}
	if len(versions) != 2 || (versions[0] != base && versions[1] != base) || versions[0] == versions[1] {
		t.Fatalf("versions %v, base %s", versions, base)
	}
	var r AzurePipelineResult
	for _, x := range rep.Pipelines {
		if x.Skip == "" {
			r = x
		}
	}
	if r.Expansion == nil || r.Expansion.Added != 1 || r.Expansion.Removed != 1 {
		t.Fatalf("expansion: %+v", r.Expansion)
	}
	if rep.Totals.Skipped != 1 {
		t.Errorf("totals: %+v", rep.Totals)
	}
}

// Consent: a run only starts for the plan the user saw; a change made since
// gets a new plan id.
func TestAzureStartChecksThePlan(t *testing.T) {
	w := newAzWorkspace(t)
	path := filepath.Join(w.templates, "templates/stages.yml")
	w.e.OpenDocument(path, azStages+"# one\n")
	plan, err := w.e.AzurePlan(context.Background(), AzureOptions{Since: "HEAD"})
	if err != nil {
		t.Fatal(err)
	}
	w.e.OpenDocument(path, azStages+"# two\n")
	if err := w.e.AzureStart(plan.ID, AzureOptions{Since: "HEAD"}, nil, nil); err == nil || !strings.Contains(err.Error(), "changed since you confirmed") {
		t.Fatalf("err = %v", err)
	}
	if len(w.fake.Previews()) != 0 {
		t.Fatal("previewed without consent")
	}
	plan, _ = w.e.AzurePlan(context.Background(), AzureOptions{Since: "HEAD"})
	// the user reads the dialog: the same plan must still be accepted later
	// (nothing time-dependent, such as a commit date, goes into its id)
	time.Sleep(1100 * time.Millisecond)
	done := make(chan *AzureReport, 1)
	if err := w.e.AzureStart(plan.ID, AzureOptions{Since: "HEAD"}, nil, func(r *AzureReport) { done <- r }); err != nil {
		t.Fatal(err)
	}
	rep := <-done
	if rep.State != "done" || w.e.AzureLast() == nil || w.e.AzureLast().Stale {
		t.Fatalf("report %+v", rep)
	}
	w.e.OpenDocument(path, azStages+"# three\n")
	if !w.e.AzureLast().Stale {
		t.Error("not stale after an edit")
	}
}

// A repository refuses the snapshot push (a branch policy or a missing
// permission): the pipelines that need it say why they weren't checked, the
// others are still checked, and the refs that were pushed are deleted.
func TestAzureRefusedPush(t *testing.T) {
	w := newAzWorkspace(t)
	// pipeline 3 (ci-default.yml) uses only the consumer's own steps
	write(t, w.consumer, "ci-default.yml", "trigger: none\nsteps:\n- template: local/steps.yml\n")
	commitAll(t, w.consumer, "ci")
	gitIn(t, w.consumer, "push", "-q", w.servers["atn-consumer"], "main")
	gitIn(t, w.consumer, "update-ref", "refs/remotes/origin/main", "HEAD")
	hook := filepath.Join(w.servers["atn-templates"], "hooks", "pre-receive")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\necho 'refused' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// the fallback (your own credentials on origin) goes to the same server
	gitIn(t, w.templates, "remote", "set-url", "--push", "origin", w.servers["atn-templates"])
	w.e.Rescan()

	write(t, w.consumer, "local/steps.yml", azLocalSteps+"- script: echo more\n")
	w.e.FileChanged(filepath.Join(w.consumer, "local/steps.yml"))
	write(t, w.templates, "templates/stages.yml", azStages+"    - script: echo new\n")
	w.e.FileChanged(filepath.Join(w.templates, "templates/stages.yml"))

	plan, rep := w.check(t)
	if len(plan.Snapshots) != 2 || plan.Checkable() != 2 {
		t.Fatalf("plan: %+v", plan)
	}
	byDef := map[int]AzurePipelineResult{}
	for _, r := range rep.Pipelines {
		byDef[r.Definition.ID] = r
	}
	if r := byDef[2]; r.Now.Verdict != "unknown" || !strings.Contains(r.Now.Error, "refused it: pre-receive hook declined") || !strings.Contains(r.Now.Error, "atn-templates") ||
		strings.Contains(r.Now.Error, "own git credentials") {
		t.Errorf("pipeline 2 needs the refused repository: %+v", r.Now)
	}
	if r := byDef[3]; r.Now.Verdict != "compiles" {
		t.Errorf("pipeline 3 doesn't need it: %+v", r.Now)
	}
	if rep.Totals.Unknown != 1 || rep.Totals.Compiles != 1 || len(rep.Warnings) != 1 {
		t.Errorf("totals %+v, warnings %v", rep.Totals, rep.Warnings)
	}
	for _, p := range w.fake.Previews() {
		if strings.Contains(p.Path, "/pipelines/2/") {
			t.Error("previewed pipeline 2 without its snapshot")
		}
	}
	if refs := refsOn(t, w.servers["atn-consumer"]); refs != "" {
		t.Errorf("snapshot ref left behind: %s", refs)
	}

	// when nothing could be pushed, the check fails with the reason
	write(t, w.consumer, "local/steps.yml", azLocalSteps)
	w.e.FileChanged(filepath.Join(w.consumer, "local/steps.yml"))
	plan, err := w.e.AzurePlan(context.Background(), AzureOptions{Since: "HEAD"})
	if err != nil {
		t.Fatal(err)
	}
	if rep := w.e.AzureRun(context.Background(), plan, nil); rep.State != "failed" || !strings.Contains(rep.Error, "pre-receive hook declined") {
		t.Errorf("all refused: %s %s", rep.State, rep.Error)
	}
}

func TestAzureAuthError(t *testing.T) {
	w := newAzWorkspace(t)
	w.fake.Token = "other"
	write(t, w.templates, "templates/stages.yml", azStages+"# x\n")
	w.e.Rescan()
	_, err := w.e.AzurePlan(context.Background(), AzureOptions{Since: "HEAD"})
	if err == nil || !strings.Contains(err.Error(), "rejected the token") || !strings.Contains(err.Error(), "az login") {
		t.Fatalf("err = %v", err)
	}
}

func TestRuntimeParams(t *testing.T) {
	ps := pipeline.ParseParameters(strings.Split("parameters:\n- name: a\n  type: boolean\n- name: b\n  values: [x, y]\n- name: c\n  default: z\n- name: d\n  type: object\n", "\n"))
	got := runtimeParams(ps)
	if got["a"] != false || got["b"] != "x" || got["c"] != nil || fmt.Sprint(got["d"]) != "map[]" {
		t.Fatalf("%v", got)
	}
}

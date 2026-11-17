package engine

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// gitWorkspace is a repository whose first commit (on main) holds files.
func gitWorkspace(t *testing.T, files map[string]string) (string, *Engine) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	gitIn(t, root, "init", "-q", "-b", "main")
	gitIn(t, root, "config", "user.email", "t@example.com")
	gitIn(t, root, "config", "user.name", "t")
	gitIn(t, root, "config", "commit.gpgsign", "false")
	for rel, content := range files {
		write(t, root, rel, content)
	}
	gitIn(t, root, "add", "-A", "-f") // -f: the fixtures, whatever the global gitignore says
	gitIn(t, root, "commit", "-q", "-m", "start")
	s := DefaultSettings()
	s.Repositories.AutoFetch = false
	s.Repositories.CacheDir = filepath.Join(t.TempDir(), "cache")
	e := New([]string{root}, s)
	e.BaselineDir = t.TempDir()
	e.Rescan()
	return root, e
}

func commitAll(t *testing.T, root, msg string) {
	t.Helper()
	gitIn(t, root, "add", "-A")
	gitIn(t, root, "commit", "-q", "-m", msg)
}

const (
	changesPipeline = `
trigger: none
steps:
  - template: build.yml
    parameters:
      name: app
`
	changesTemplate = `
parameters:
  - name: name
    type: string
steps:
  - script: echo ${{ parameters.name }}
`
	// an existing problem: an unknown parameter that is already on main
	changesOther = `
trigger: none
steps:
  - template: build.yml
    parameters:
      name: app
      color: red
`
)

func changeOf(t *testing.T, r ChangesReport, rel string) PipelineChange {
	t.Helper()
	for _, c := range r.Pipelines {
		if c.RelativePath == rel {
			return c
		}
	}
	t.Fatalf("%s not among the changed pipelines: %+v", rel, r.Pipelines)
	return PipelineChange{}
}

func TestChangesOnlyShowWhatTheBranchAdded(t *testing.T) {
	root, e := gitWorkspace(t, map[string]string{
		"azure-pipelines.yml": changesPipeline,
		"other.yml":           changesOther,
		"build.yml":           changesTemplate,
		"lib/unused.yml":      "steps:\n  - script: echo\n",
	})
	// nothing changed yet
	r := e.Changes()
	if !r.Available || len(r.Files) != 0 || len(r.Pipelines) != 0 {
		t.Fatalf("clean checkout: %+v", r)
	}
	if r.AllPipelines != 2 || r.AllFailing != 1 {
		t.Fatalf("big picture: %d pipelines, %d failing", r.AllPipelines, r.AllFailing)
	}

	// a committed change on a branch: rename the template's parameter
	gitIn(t, root, "switch", "-q", "-c", "mywork")
	write(t, root, "build.yml", strings.Replace(changesTemplate, "name: name", "name: title", 1))
	commitAll(t, root, "rename")
	e.FileChanged(filepath.Join(root, "build.yml"))

	r = e.Changes()
	if len(r.Bases) != 1 || r.Bases[0].Ref != "main" || r.Bases[0].Commits != 1 {
		t.Fatalf("base: %+v", r.Bases)
	}
	if len(r.Files) != 1 || r.Files[0].RelativePath != "build.yml" || r.Files[0].Status != "modified" || r.Files[0].Pipelines != 2 {
		t.Fatalf("files: %+v", r.Files)
	}
	main := changeOf(t, r, "azure-pipelines.yml")
	if main.Status != "changed" || main.Before.Verdict != "ok" || main.Now.Verdict != "fail" || len(main.New) == 0 || len(main.Fixed) != 0 {
		t.Fatalf("azure-pipelines.yml: %+v", main)
	}
	other := changeOf(t, r, "other.yml")
	// other.yml's `color` error was already there; only the rename is new
	if other.Existing != 1 || len(other.New) == 0 {
		t.Fatalf("other.yml: %+v", other)
	}
	for _, p := range other.New {
		if strings.Contains(p.Message, "'color'") {
			t.Fatalf("the existing problem is reported as new: %s", p.Message)
		}
	}
	if r.NowFailing != 1 || r.NewErrors == 0 {
		t.Fatalf("totals: %+v", r)
	}
	// 2 call-site errors in each pipeline, and one warning in build.yml
	// that both pipelines reach: counted once
	if r.NewErrors != 4 || r.NewWarnings != 1 {
		t.Fatalf("new: %d errors, %d warnings; want 4 and 1", r.NewErrors, r.NewWarnings)
	}

	// an unsaved edit that puts it back: nothing new any more
	e.OpenDocument(filepath.Join(root, "build.yml"), strings.TrimPrefix(changesTemplate, "\n"))
	r = e.Changes()
	if len(r.Files) != 0 || len(r.Pipelines) != 0 {
		t.Fatalf("after reverting in the editor: %+v", r)
	}
}

func TestChangesLineShiftsAreNotNewProblems(t *testing.T) {
	root, e := gitWorkspace(t, map[string]string{
		"other.yml": changesOther,
		"build.yml": changesTemplate,
	})
	gitIn(t, root, "switch", "-q", "-c", "mywork")
	// lines move, the problem stays
	e.OpenDocument(filepath.Join(root, "other.yml"), "# a comment\n# another\n"+strings.TrimPrefix(changesOther, "\n"))
	r := e.Changes()
	c := changeOf(t, r, "other.yml")
	if len(c.New) != 0 || len(c.Fixed) != 0 || c.Existing != 1 {
		t.Fatalf("other.yml: %+v", c)
	}
	if r.NewErrors != 0 || r.Summary() != "1 file changed · 1 pipeline affected · no new problems" {
		t.Fatalf("summary %q", r.Summary())
	}
}

func TestChangesNewAndRemovedFiles(t *testing.T) {
	root, e := gitWorkspace(t, map[string]string{
		"azure-pipelines.yml": changesPipeline,
		"build.yml":           changesTemplate,
		"old.yml":             "trigger: none\nsteps:\n  - script: echo\n",
	})
	gitIn(t, root, "switch", "-q", "-c", "mywork")
	// a new template, used by the existing pipeline, with a problem in the call
	write(t, root, "deploy.yml", "parameters:\n  - name: env\n    type: string\nsteps:\n  - script: echo ${{ parameters.env }}\n")
	write(t, root, "azure-pipelines.yml", changesPipeline+"  - template: deploy.yml\n    parameters:\n      environment: prod\n")
	_ = os.Remove(filepath.Join(root, "old.yml"))
	// ignored output isn't the user's work
	write(t, root, ".gitignore", "gen/\n")
	write(t, root, "gen/generated.yml", "trigger: none\nsteps:\n  - script: echo\n")
	e.Rescan()

	r := e.Changes()
	status := map[string]string{}
	for _, f := range r.Files {
		status[f.RelativePath] = f.Status
	}
	want := map[string]string{"deploy.yml": "added", "azure-pipelines.yml": "modified", "old.yml": "deleted"}
	for k, v := range want {
		if status[k] != v {
			t.Fatalf("files %v, want %v", status, want)
		}
	}
	if _, ok := status["gen/generated.yml"]; ok {
		t.Fatal("an ignored file is not a change")
	}
	main := changeOf(t, r, "azure-pipelines.yml")
	if len(main.New) == 0 || main.Before.Verdict != "ok" || main.Now.Verdict != "fail" {
		t.Fatalf("azure-pipelines.yml: %+v", main)
	}
	if c := changeOf(t, r, "old.yml"); c.Status != "removed" {
		t.Fatalf("old.yml: %+v", c)
	}
	for _, c := range r.Pipelines {
		if c.RelativePath == "gen/generated.yml" {
			t.Fatal("an ignored pipeline is not a change")
		}
	}
}

func TestChangesBaseChoice(t *testing.T) {
	root, e := gitWorkspace(t, map[string]string{
		"azure-pipelines.yml": changesPipeline,
		"build.yml":           changesTemplate,
	})
	gitIn(t, root, "switch", "-q", "-c", "mywork")
	write(t, root, "build.yml", strings.Replace(changesTemplate, "name: name", "name: title", 1))
	commitAll(t, root, "rename")
	e.FileChanged(filepath.Join(root, "build.yml"))
	if r := e.Changes(); len(r.Files) != 1 {
		t.Fatalf("since main: %+v", r.Files)
	}
	// uncommitted only: the committed rename is not "mine" any more
	if err := e.SetChangesBase("HEAD"); err != nil {
		t.Fatal(err)
	}
	r := e.Changes()
	if len(r.Files) != 0 || r.Bases[0].Source != "chosen" || !strings.HasPrefix(r.Label, "uncommitted changes") {
		t.Fatalf("uncommitted only: %+v", r)
	}
	var picked []string
	for _, c := range e.ChangeBases() {
		if c.Picked {
			picked = append(picked, c.Ref)
		}
	}
	if len(picked) != 1 || picked[0] != "HEAD" {
		t.Fatalf("picked %v", picked)
	}
	if err := e.SetChangesBase("nope"); err == nil {
		t.Fatal("an unknown ref must be refused")
	}
	if err := e.SetChangesBase(""); err != nil {
		t.Fatal(err)
	}
	if r := e.Changes(); len(r.Files) != 1 {
		t.Fatalf("back to automatic: %+v", r.Files)
	}
	// the CLI form doesn't store anything
	if r, err := e.ChangesSince("HEAD"); err != nil || len(r.Files) != 0 {
		t.Fatalf("--since HEAD: %+v %v", r.Files, err)
	}
	if _, err := e.ChangesSince("nope"); err == nil {
		t.Fatal("--since an unknown ref must fail")
	}
	if r := e.Changes(); len(r.Files) != 1 {
		t.Fatal("ChangesSince must not change the stored base")
	}
}

func TestChangesMarkNewProblemsInOverviewAndLens(t *testing.T) {
	root, e := gitWorkspace(t, map[string]string{
		"other.yml": changesOther,
		"build.yml": changesTemplate,
	})
	gitIn(t, root, "switch", "-q", "-c", "mywork")
	e.OpenDocument(filepath.Join(root, "other.yml"), strings.TrimPrefix(changesOther, "\n")+"      size: big\n")
	e.Changes()
	reports, _ := e.PipelineOverview()
	if len(reports) != 1 || reports[0].Change == nil {
		t.Fatalf("overview: %+v", reports)
	}
	news := 0
	for _, p := range reports[0].Problems {
		if p.New {
			news++
			if !strings.Contains(p.Message, "'size'") {
				t.Fatalf("marked as new: %s", p.Message)
			}
		}
	}
	if news != 1 {
		t.Fatalf("%d problems marked new, want 1", news)
	}
	lens := e.CodeLenses(filepath.Join(root, "other.yml"))
	if len(lens) != 1 || !strings.HasSuffix(lens[0].Title, " · +1 new since main") {
		t.Fatalf("lens: %+v", lens)
	}
}

func TestChangesOutsideGit(t *testing.T) {
	_, e := preflightWorkspace(t, map[string]string{"a.yml": "trigger: none\nsteps: []\n"})
	r := e.Changes()
	if r.Available || !strings.Contains(r.Reason, "git") {
		t.Fatalf("%+v", r)
	}
}

func TestChangesIgnoreLineEndings(t *testing.T) {
	root, e := gitWorkspace(t, map[string]string{
		"azure-pipelines.yml": changesPipeline,
		"build.yml":           changesTemplate,
	})
	// what core.autocrlf=true checks out on Windows
	crlfText := strings.ReplaceAll(strings.TrimPrefix(changesTemplate, "\n"), "\n", "\r\n")
	e.OpenDocument(filepath.Join(root, "build.yml"), crlfText)
	if r := e.Changes(); len(r.Files) != 0 || len(r.Pipelines) != 0 {
		t.Fatalf("CRLF only: %+v", r)
	}
}

func TestChangesSkipNestedRepositories(t *testing.T) {
	root, e := gitWorkspace(t, map[string]string{
		"azure-pipelines.yml": changesPipeline,
		"build.yml":           changesTemplate,
	})
	nested := filepath.Join(root, "vendor-templates")
	write(t, nested, "x.yml", "trigger: none\nsteps:\n  - script: echo\n")
	gitIn(t, nested, "init", "-q")
	e.Rescan()
	if r := e.Changes(); len(r.Files) != 0 || len(r.Pipelines) != 0 {
		t.Fatalf("a nested checkout is not a change: %+v", r)
	}
}

// The baseline engine reads an export of the commit. With nothing changed,
// it must resolve exactly like the workspace engine: same templates, same
// repositories (siblings, cache, local paths), same problems.
func TestChangesSamplesUnchangedIsEmpty(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	samples, err := filepath.Abs(filepath.Join("..", "..", "..", "samples"))
	if err != nil || !fileOrDirExists(samples) {
		t.Skip("samples not found")
	}
	root := filepath.Join(t.TempDir(), "samples")
	copyTree(t, samples, root)
	gitIn(t, root, "init", "-q", "-b", "main")
	gitIn(t, root, "config", "user.email", "t@example.com")
	gitIn(t, root, "config", "user.name", "t")
	gitIn(t, root, "config", "commit.gpgsign", "false")
	commitAll(t, root, "samples")
	s := DefaultSettings()
	s.Repositories.AutoFetch = false
	s.Repositories.CacheDir = filepath.Join(t.TempDir(), "cache")
	e := New([]string{root}, s)
	e.BaselineDir = t.TempDir()
	e.Rescan()
	r := e.Changes()
	if !r.Available || len(r.Files) != 0 || len(r.Pipelines) != 0 {
		t.Fatalf("unchanged samples: %d files, %d pipelines: %+v", len(r.Files), len(r.Pipelines), r.Pipelines)
	}
	if r.AllPipelines == 0 || r.AllFailing == 0 {
		t.Fatalf("expected the samples' failing pipelines in the big picture: %+v", r)
	}
}

func copyTree(t *testing.T, from, to string) {
	t.Helper()
	err := filepath.Walk(from, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, p)
		dst := filepath.Join(to, rel)
		if info.IsDir() {
			if info.Name() == ".git" {
				return filepath.SkipDir
			}
			return os.MkdirAll(dst, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A local path outside the repository (../sibling) and a template in a nested
// checkout resolve the same at the base as in the workspace.
func TestChangesUnchangedWithOutsideAndNestedTemplates(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	parent := t.TempDir()
	write(t, parent, "sibling/steps/lint.yml", "parameters:\n  - name: level\n    type: string\nsteps:\n  - script: echo ${{ parameters.level }}\n")
	root := filepath.Join(parent, "app")
	write(t, root, ".azure-templates-navigator.json", `{"repositories": {"localPaths": {"shared": "../sibling"}}}`)
	write(t, root, "azure-pipelines.yml", `
trigger: none
resources:
  repositories:
    - repository: shared
      type: git
      name: Team/shared
steps:
  - template: steps/lint.yml@shared
    parameters:
      level: high
  - template: vendor/steps/scan.yml
    parameters:
      depth: 2
`)
	gitIn(t, root, "init", "-q", "-b", "main")
	gitIn(t, root, "config", "user.email", "t@example.com")
	gitIn(t, root, "config", "user.name", "t")
	gitIn(t, root, "config", "commit.gpgsign", "false")
	write(t, root, "vendor/steps/scan.yml", "parameters:\n  - name: depth\n    type: number\nsteps:\n  - script: echo ${{ parameters.depth }}\n")
	gitIn(t, filepath.Join(root, "vendor"), "init", "-q")
	write(t, root, ".gitignore", "vendor/\n")
	commitAll(t, root, "start")
	s := DefaultSettings()
	s.Repositories.AutoFetch = false
	s.Repositories.CacheDir = filepath.Join(t.TempDir(), "cache")
	e := New([]string{root}, s)
	e.BaselineDir = t.TempDir()
	e.Rescan()
	if rep, _ := e.PipelineCheck(filepath.Join(root, "azure-pipelines.yml")); rep.Errors+rep.Warnings != 0 {
		t.Fatalf("the fixture should have no problems: %+v", rep.Problems)
	}
	r := e.Changes()
	if !r.Available || len(r.Files) != 0 || len(r.Pipelines) != 0 {
		t.Fatalf("unchanged: %+v", r.Pipelines)
	}
}

// A template under a folder the index skips (build/) is still compared when
// a pipeline reaches it.
func TestChangesTemplateInSkippedFolder(t *testing.T) {
	root, e := gitWorkspace(t, map[string]string{
		"azure-pipelines.yml": "trigger: none\nsteps:\n  - template: build/steps.yml\n    parameters:\n      name: app\n",
		"build/steps.yml":     strings.TrimPrefix(changesTemplate, "\n"),
	})
	gitIn(t, root, "switch", "-q", "-c", "mywork")
	write(t, root, "build/steps.yml", strings.Replace(changesTemplate, "name: name", "name: title", 1))
	e.FileChanged(filepath.Join(root, "build", "steps.yml"))
	r := e.Changes()
	if len(r.Files) != 1 || r.Files[0].RelativePath != "build/steps.yml" || r.Files[0].Status != "modified" {
		t.Fatalf("files %+v", r.Files)
	}
	if c := changeOf(t, r, "azure-pipelines.yml"); c.Status != "changed" || len(c.New) == 0 {
		t.Fatalf("%+v", c)
	}
}

// --since auto fails instead of quietly comparing with HEAD when it can't
// tell where the branch started.
func TestChangesSinceAutoWithoutStartPoint(t *testing.T) {
	_, e := gitWorkspace(t, map[string]string{"azure-pipelines.yml": changesPipeline, "build.yml": changesTemplate})
	// main without an upstream: only uncommitted changes, which the IDE shows
	if r := e.Changes(); !r.Available {
		t.Fatalf("%+v", r)
	}
	if _, err := e.ChangesSince("auto"); err == nil || !strings.Contains(err.Error(), "couldn't tell where this branch started") {
		t.Fatalf("err = %v", err)
	}
	if _, err := e.ChangesSince("HEAD"); err != nil {
		t.Fatal(err)
	}
}

package engine

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qSharpy/azure-templates-navigator/server/internal/pipeline"
	"github.com/qSharpy/azure-templates-navigator/server/internal/repos"
)

func write(t *testing.T, root, rel, content string) string {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(strings.TrimPrefix(content, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const buildTemplate = `
parameters:
  - name: project
    type: string
  - name: configuration
    type: string
    default: Release
    values: [Debug, Release]
  - name: publish
    type: boolean
    default: false

steps:
  - script: dotnet build ${{ parameters.project }} -c ${{ parameters.configuration }}
  - ${{ if parameters.publish }}:
    - script: echo publish
`

func newWorkspace(t *testing.T) (string, *Engine) {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, root, "templates/build.yml", buildTemplate)
	write(t, root, "templates/wrapper.yml", `
parameters:
  - name: project
    type: string
  - name: unused
    default: x
steps:
  - template: build.yml
    parameters:
      project: ${{ parameters.project }}
`)
	write(t, root, "azure-pipelines.yml", `
trigger: none
steps:
  - template: templates/build.yml
    parameters:
      configuraton: Debug
      publish: maybe
  - template: /templates/wrapper.yml
    parameters:
      project: app.csproj
  - template: templates/nope.yml
  - template: templates/build.yml
    parameters:
      project: x
      configuration: Staging
`)
	s := DefaultSettings()
	s.Repositories.AutoFetch = false
	s.Repositories.CacheDir = filepath.Join(t.TempDir(), "cache")
	e := New([]string{root}, s)
	e.Rescan()
	return root, e
}

func codes(ds []Diagnostic) map[string]int {
	m := map[string]int{}
	for _, d := range ds {
		m[d.Code]++
	}
	return m
}

func TestDiagnostics(t *testing.T) {
	root, e := newWorkspace(t)
	ds := e.Diagnose(filepath.Join(root, "azure-pipelines.yml"))
	got := codes(ds)
	want := map[string]int{CodeMissingRequired: 1, CodeUnknownParam: 1, CodeTypeMismatch: 1, CodeTemplateMissing: 1, CodeValueNotAllowed: 1}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: want %d got %d (%+v)", k, v, got[k], ds)
		}
	}
	for _, d := range ds {
		if d.Code == CodeUnknownParam && !strings.Contains(d.Message, "Did you mean 'configuration'") {
			t.Errorf("expected suggestion: %s", d.Message)
		}
	}
	wd := codes(e.Diagnose(filepath.Join(root, "templates/wrapper.yml")))
	if wd[CodeUnusedParam] != 1 || len(wd) != 1 {
		t.Errorf("wrapper: %+v", wd)
	}
}

func TestCodeActions(t *testing.T) {
	root, e := newWorkspace(t)
	p := filepath.Join(root, "azure-pipelines.yml")
	acts := e.CodeActions(p, 0, 100)
	titles := map[string]CodeAction{}
	for _, a := range acts {
		titles[a.Title] = a
	}
	add, ok := titles["Add missing parameter 'project' (string)"]
	if !ok {
		t.Fatalf("no add action in %v", keys(titles))
	}
	if add.Edits[0].StartLine != 5 || add.Edits[0].NewText != "\n      project: ''" {
		t.Errorf("add edit: %+v", add.Edits[0])
	}
	if _, ok := titles["Rename to 'configuration'"]; !ok {
		t.Errorf("no rename action in %v", keys(titles))
	}
	if _, ok := titles["Change value to 'Debug'"]; !ok {
		t.Errorf("no value action in %v", keys(titles))
	}
}

func keys(m map[string]CodeAction) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestAddParamsCreatesBlock(t *testing.T) {
	root, e := newWorkspace(t)
	p := write(t, root, "p2.yml", `
jobs:
  - job: j
    steps:
      - template: templates/build.yml
`)
	e.FileChanged(p)
	acts := e.CodeActions(p, 0, 10)
	if len(acts) == 0 {
		t.Fatal("no actions")
	}
	if got := acts[0].Edits[0].NewText; got != "\n        parameters:\n          project: ''" {
		t.Errorf("edit text %q", got)
	}
}

func TestHoverAndDefinition(t *testing.T) {
	root, e := newWorkspace(t)
	p := filepath.Join(root, "azure-pipelines.yml")
	h, ok := e.HoverAt(p, 2, 10, ClientFeatures{})
	if !ok || !strings.Contains(h.Markdown, "**Parameters** (3, 1 required)") || !strings.Contains(h.Markdown, "one of `Debug`, `Release`") {
		t.Errorf("hover: %v %s", ok, h.Markdown)
	}
	loc, ok := e.Definition(p, 2, 10)
	if !ok || loc.Path != filepath.Join(root, "templates/build.yml") {
		t.Errorf("definition: %+v", loc)
	}
	// parameter key -> declaration line
	loc, ok = e.Definition(p, 12, 7)
	if !ok || loc.Line != 1 {
		t.Errorf("param definition: %+v %v", loc, ok)
	}
	// parameters.x reference inside a template
	tp := filepath.Join(root, "templates/build.yml")
	h, ok = e.HoverAt(tp, 12, 40, ClientFeatures{})
	if !ok || !strings.Contains(h.Markdown, "`project`") {
		t.Errorf("param ref hover: %s", h.Markdown)
	}
}

func TestCompletion(t *testing.T) {
	root, e := newWorkspace(t)
	p := write(t, root, "c.yml", `
steps:
  - template: templates/build.yml
    parameters:
      publish: true

`)
	e.FileChanged(p)
	items := e.Complete(p, 4, 0) // truly empty line
	if len(items) != 3 {
		t.Fatalf("empty line items: %+v", items)
	}
	e.OpenDocument(p, strings.Replace(mustRead(t, p), "true\n\n", "true\n      \n", 1))
	items = e.Complete(p, 4, 6) // auto-indented line
	if len(items) != 3 || items[0].Label == "" {
		t.Fatalf("items: %+v", items)
	}
	var proj CompletionItem
	for _, it := range items {
		if it.Label == "project" {
			proj = it
		}
	}
	if proj.SortText != "0_project" || proj.InsertText != "project: " {
		t.Errorf("project item: %+v", proj)
	}
	e.OpenDocument(p, strings.Replace(mustRead(t, p), "publish: true", "publish: ", 1))
	vals := e.Complete(p, 3, 15)
	if len(vals) != 2 || vals[0].Label != "true" {
		t.Errorf("value completion: %+v", vals)
	}
	e.OpenDocument(p, "steps:\n  - template: \n")
	files := e.Complete(p, 1, 14)
	if len(files) == 0 {
		t.Errorf("no template path completions")
	}
}

func mustRead(t *testing.T, p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestTreeAndGraph(t *testing.T) {
	root, e := newWorkspace(t)
	build := filepath.Join(root, "templates/build.yml")
	tree, ok := e.Tree(build)
	if !ok || tree.CallerCount != 2 {
		t.Fatalf("tree: %+v", tree)
	}
	// azure-pipelines.yml is the top-level caller; wrapper sits underneath it
	if len(tree.CalledBy) != 1 || tree.CalledBy[0].Label != "azure-pipelines.yml" || len(tree.CalledBy[0].Children) != 1 {
		t.Errorf("calledBy: %+v", tree.CalledBy)
	}
	pt, _ := e.Tree(filepath.Join(root, "azure-pipelines.yml"))
	kinds := map[string]int{}
	for _, n := range pt.IsCalling {
		kinds[n.Kind]++
	}
	if kinds["missing"] != 1 || kinds["template"] != 3 {
		t.Errorf("isCalling kinds: %+v", kinds)
	}
	// a pipeline has no callers: must serialize as [], not null (clients read .length)
	if pt.CalledBy == nil || tree.IsCalling == nil {
		t.Errorf("nil slices in tree: calledBy=%v isCalling=%v", pt.CalledBy == nil, tree.IsCalling == nil)
	}
	g := e.BuildGraph(GraphRequest{})
	if len(g.Nodes) < 4 || len(g.Edges) < 4 {
		t.Errorf("graph: %d nodes %d edges", len(g.Nodes), len(g.Edges))
	}
	fg := e.BuildGraph(GraphRequest{File: build, Depth: 2})
	if len(fg.Edges) != 3 {
		t.Errorf("file graph edges: %+v", fg.Edges)
	}
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// End-to-end: a pipeline references a template in another repository that
// is not cloned locally; the engine fetches it into the cache and the
// diagnostics then validate against the fetched template.
func TestRemoteRepositoryFetch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	remote := t.TempDir()
	git(t, remote, "init", "-q", "-b", "main")
	write(t, remote, "stages/deploy.yml", `
parameters:
  - name: env
    type: string
stages:
  - stage: deploy_${{ parameters.env }}
`)
	git(t, remote, "add", ".")
	git(t, remote, "commit", "-q", "-m", "init")

	root := t.TempDir()
	_ = os.Mkdir(filepath.Join(root, ".git"), 0o755)
	p := write(t, root, "pipeline.yml", `
trigger: none
resources:
  repositories:
    - repository: shared
      type: github
      name: contoso/pipeline-templates
      ref: refs/heads/main
stages:
  - template: stages/deploy.yml@shared
    parameters:
      environment: prod
`)
	s := DefaultSettings()
	s.Repositories.CacheDir = filepath.Join(t.TempDir(), "cache")
	s.Repositories.URLs = map[string]string{"contoso/pipeline-templates": "file://" + filepath.ToSlash(remote)}
	e := New([]string{root}, s)
	done := make(chan struct{}, 1)
	e.Repos.OnChange(func() { e.Invalidate(); done <- struct{}{} })
	e.Rescan()

	first := codes(e.Diagnose(p))
	if first[CodeRepoFetching] != 1 {
		t.Fatalf("expected fetching diagnostic, got %+v", first)
	}
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("fetch timed out")
	}
	after := codes(e.Diagnose(p))
	if after[CodeMissingRequired] != 1 || after[CodeUnknownParam] != 1 || len(after) != 2 {
		t.Fatalf("after fetch: %+v", after)
	}
	h, _ := e.HoverAt(p, 8, 10, ClientFeatures{})
	if !strings.Contains(h.Markdown, "cached, fetched") || !strings.Contains(h.Markdown, "**env**") {
		t.Errorf("hover after fetch: %s", h.Markdown)
	}
	list := e.Repos.ListCache()
	if len(list) != 1 || list[0].Commit == "" {
		t.Errorf("cache list: %+v", list)
	}
	infos := e.Repositories()
	if len(infos) != 1 || infos[0].Location.State != repos.StateReady {
		t.Errorf("repositories: %+v", infos)
	}
}

func TestSiblingCloneWins(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "app")
	_ = os.MkdirAll(filepath.Join(root, ".git"), 0o755)
	write(t, parent, "pipeline-templates/t.yml", "parameters:\n  - name: a\nsteps: []\n")
	p := write(t, root, "p.yml", `
resources:
  repositories:
    - repository: tpl
      type: github
      name: contoso/pipeline-templates
steps:
  - template: t.yml@tpl
`)
	s := DefaultSettings()
	s.Repositories.AutoFetch = false
	e := New([]string{root}, s)
	e.Rescan()
	ds := e.Diagnose(p)
	if c := codes(ds); c[CodeMissingRequired] != 1 {
		t.Errorf("sibling resolution: %+v", ds)
	}
}

func TestAliasFromOtherPipeline(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "app")
	_ = os.MkdirAll(filepath.Join(root, ".git"), 0o755)
	write(t, parent, "shared/t.yml", "steps: []\n")
	write(t, root, "pipeline.yml", `
resources:
  repositories:
    - repository: tpl
      name: proj/shared
steps:
  - template: templates/local.yml
`)
	local := write(t, root, "templates/local.yml", "steps:\n  - template: t.yml@tpl\n")
	s := DefaultSettings()
	s.Repositories.AutoFetch = false
	e := New([]string{root}, s)
	e.Rescan()
	if ds := e.Diagnose(local); len(ds) != 0 {
		t.Errorf("template should resolve alias declared by its pipeline: %+v", ds)
	}
}

func TestCloneURLs(t *testing.T) {
	cfg := repos.DefaultConfig()
	cases := []struct {
		typ, name, remote, want string
	}{
		{"github", "org/repo", "", "https://github.com/org/repo.git"},
		{"github", "org/repo", "git@github.com:me/app.git", "git@github.com:org/repo.git"},
		{"git", "Shared/templates", "https://contoso@dev.azure.com/contoso/App/_git/app", "https://dev.azure.com/contoso/Shared/_git/templates"},
		{"git", "templates", "git@ssh.dev.azure.com:v3/contoso/App/app", "git@ssh.dev.azure.com:v3/contoso/App/templates"},
		{"git", "My Project/templates", "https://contoso.visualstudio.com/App/_git/app", "https://dev.azure.com/contoso/My%20Project/_git/templates"},
		{"bitbucket", "team/repo", "", "https://bitbucket.org/team/repo.git"},
	}
	for _, c := range cases {
		got, err := repos.CloneURL(declOf(c.typ, c.name), cfg, c.remote)
		if err != nil || got != c.want {
			t.Errorf("%s %s (%s): want %s got %s (%v)", c.typ, c.name, c.remote, c.want, got, err)
		}
	}
	if _, err := repos.CloneURL(declOf("git", "Shared/templates"), cfg, "https://github.com/me/app.git"); err == nil {
		t.Errorf("expected error when the Azure DevOps organization is unknown")
	}
}

func declOf(typ, name string) pipeline.RepoDecl {
	return pipeline.RepoDecl{Alias: "a", Type: typ, Name: name}
}

func TestTemplateParametersFromCallSite(t *testing.T) {
	root, e := newWorkspace(t)
	p := write(t, root, "params-probe.yml", "steps:\n- template: templates/build.yml\n")
	e.Rescan()
	file, ps := e.TemplateParameters(p, 1)
	if file != filepath.Join(root, "templates", "build.yml") || len(ps) != 3 || !ps[0].Required || ps[0].Line != 1 {
		t.Fatalf("got %s %+v", file, ps)
	}
	if _, ps := e.TemplateParameters(file, -1); len(ps) != 3 {
		t.Fatalf("self: %+v", ps)
	}
}

func TestUndeclaredParameter(t *testing.T) {
	root, e := newWorkspace(t)
	p := write(t, root, "templates/deploy.yml", `
parameters:
  - name: environment
    type: string
steps:
  - script: echo ${{ parameters.enviroment }} ${{ parameters.environment }}
  - ${{ each s in parameters.extraSteps }}:
    - ${{ s }}
`)
	e.FileChanged(p)
	var und []Diagnostic
	for _, d := range e.Diagnose(p) {
		if d.Code == CodeUndeclaredParam {
			und = append(und, d)
		}
	}
	if len(und) != 2 || und[0].param != "enviroment" || und[1].param != "extraSteps" {
		t.Fatalf("undeclared: %+v", und)
	}
	if !strings.Contains(und[0].Message, "Did you mean 'environment'") {
		t.Errorf("message: %s", und[0].Message)
	}
	titles := map[string]CodeAction{}
	for _, a := range e.CodeActions(p, 0, 10) {
		titles[a.Title] = a
	}
	if a, ok := titles["Change to 'environment'"]; !ok || a.Edits[0].NewText != "environment" || a.Edits[0].StartCol != 32 {
		t.Errorf("rename action: %+v in %v", a, keys(titles))
	}
	decl, ok := titles["Declare parameter 'extraSteps'"]
	if !ok {
		t.Fatalf("no declare action in %v", keys(titles))
	}
	if ed := decl.Edits[0]; ed.StartLine != 2 || ed.NewText != "\n  - name: extraSteps\n    type: string\n    default: ''" {
		t.Errorf("declare edit: %+v", ed)
	}

	// no parameters block at all: declare creates it
	q := write(t, root, "templates/bare.yml", `
steps:
  - script: echo ${{ parameters.name }}
`)
	e.FileChanged(q)
	for _, a := range e.CodeActions(q, 0, 10) {
		if a.Title == "Declare parameter 'name'" && a.Edits[0].NewText != "parameters:\n  - name: name\n    type: string\n    default: ''\n\n" {
			t.Errorf("create block: %q", a.Edits[0].NewText)
		}
	}
}

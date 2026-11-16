package engine

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// preflightWorkspace is an empty repository with the given files.
func preflightWorkspace(t *testing.T, files map[string]string) (string, *Engine) {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	for rel, content := range files {
		write(t, root, rel, content)
	}
	s := DefaultSettings()
	s.Repositories.AutoFetch = false
	s.Repositories.CacheDir = filepath.Join(t.TempDir(), "cache")
	e := New([]string{root}, s)
	e.Rescan()
	return root, e
}

func withCode(ds []Diagnostic, code string) []Diagnostic {
	var out []Diagnostic
	for _, d := range ds {
		if d.Code == code {
			out = append(out, d)
		}
	}
	return out
}

// otherPipeline declares @security and @tools, so those aliases resolve in
// the workspace (and are not "unknown"), but a pipeline that doesn't declare
// them itself still fails when queued.
const otherPipeline = `
trigger: none
resources:
  repositories:
    - repository: security
      type: git
      name: Security/security-templates
    - repository: tools
      type: github
      name: org/tools
steps:
  - script: echo
`

func TestRepositoryNotDeclaredInRootPipeline(t *testing.T) {
	root, e := preflightWorkspace(t, map[string]string{
		"other.yml": otherPipeline,
		"azure-pipelines.yml": `
trigger: none
stages:
  - template: stages/base.yml
  - stage: Direct
    jobs:
      - job: J
        steps:
          - template: x.yml@tools
`,
		"stages/base.yml": `
stages:
  - stage: Scan
    jobs:
      - job: Scan
        steps:
          - checkout: self
          - checkout: tools
          - template: ../steps/scan.yml
`,
		"steps/scan.yml": `
steps:
  - template: credscan.yml@security
`,
	})
	pipe := filepath.Join(root, "azure-pipelines.yml")
	ds := withCode(e.Diagnose(pipe), CodeRepoNotDeclared)
	if len(ds) != 3 {
		t.Fatalf("want 3 repository-not-declared, got %+v", ds)
	}
	// findings in templates are placed on the call that leads to them
	if ds[0].Line != 2 || ds[1].Line != 2 || ds[2].Line != 7 {
		t.Errorf("lines: %d %d %d", ds[0].Line, ds[1].Line, ds[2].Line)
	}
	msgs := ds[0].Message + "\n" + ds[1].Message
	if !strings.Contains(msgs, "stages/base.yml:7: This step checks out repository 'tools'") ||
		!strings.Contains(msgs, "steps/scan.yml:2: 'credscan.yml@security' uses repository 'security'") {
		t.Errorf("messages:\n%s", msgs)
	}
	// templates are not root pipelines: no pipeline-level checks on them
	if got := withCode(e.Diagnose(filepath.Join(root, "steps/scan.yml")), CodeRepoNotDeclared); len(got) != 0 {
		t.Errorf("template got %+v", got)
	}
	// the other pipeline declares both, and the quick fix copies its declaration
	if got := withCode(e.Diagnose(filepath.Join(root, "other.yml")), CodeRepoNotDeclared); len(got) != 0 {
		t.Errorf("other.yml: %+v", got)
	}
	acts := e.CodeActions(pipe, 0, 100)
	a := actionByTitle(t, acts, "Declare repository 'security' in resources.repositories")
	text := applyEdits(t, mustRead(t, pipe), a.Edits)
	want := "trigger: none\nresources:\n  repositories:\n    - repository: security\n      type: git\n      name: Security/security-templates\nstages:\n"
	if !strings.HasPrefix(text, want) {
		t.Errorf("fix result:\n%s", text)
	}
}

func TestDeclareRepositoryAppendsToExistingList(t *testing.T) {
	root, e := preflightWorkspace(t, map[string]string{
		"other.yml": otherPipeline,
		"azure-pipelines.yml": `
trigger: none
resources:
  repositories:
  - repository: platform
    type: git
    name: Platform/templates
    ref: refs/tags/v1
  pipelines:
  - pipeline: upstream
    source: Upstream
steps:
  - template: x.yml@tools
`,
	})
	pipe := filepath.Join(root, "azure-pipelines.yml")
	a := actionByTitle(t, e.CodeActions(pipe, 0, 100), "Declare repository 'tools' in resources.repositories")
	text := applyEdits(t, mustRead(t, pipe), a.Edits)
	want := "    ref: refs/tags/v1\n  - repository: tools\n    type: github\n    name: org/tools\n  pipelines:\n"
	if !strings.Contains(text, want) {
		t.Errorf("fix result:\n%s", text)
	}
}

func TestUnknownDependency(t *testing.T) {
	root, e := preflightWorkspace(t, map[string]string{
		"azure-pipelines.yml": `
trigger: none
stages:
  - stage: Build
    jobs:
      - job: Compile
      - job: Test
        dependsOn: Compil
  - template: stages/deploy.yml
    parameters:
      env: prod
  - stage: Smoke
    dependsOn: [Build, Deploy_prod]
  - stage: Report
    dependsOn:
      - Bulid
      - Smoke
`,
		"stages/deploy.yml": `
parameters:
  - name: env
    type: string
stages:
  - stage: Deploy_${{ parameters.env }}
    dependsOn: Biuld
    jobs:
      - deployment: Web
        environment: ${{ parameters.env }}
`,
	})
	ds := withCode(e.Diagnose(filepath.Join(root, "azure-pipelines.yml")), CodeUnknownDepend)
	var got []string
	for _, d := range ds {
		got = append(got, d.Message)
	}
	want := []string{
		"Job 'Test' depends on 'Compil', but this pipeline has no job with that name; Azure rejects the run when it is queued. Did you mean 'Compile'?",
		"stages/deploy.yml:6: Stage 'Deploy_${{ parameters.env }}' depends on 'Biuld', but this pipeline has no stage with that name; Azure rejects the run when it is queued. Did you mean 'Build'?",
		"Stage 'Report' depends on 'Bulid', but this pipeline has no stage with that name; Azure rejects the run when it is queued. Did you mean 'Build'?",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got\n%s", strings.Join(got, "\n"))
	}
	if ds[0].Line != 6 || ds[1].Line != 7 || ds[2].Line != 14 {
		t.Errorf("lines %d %d %d", ds[0].Line, ds[1].Line, ds[2].Line)
	}
}

func TestDependenciesNotCheckedWhenExpansionIsIncomplete(t *testing.T) {
	root, e := preflightWorkspace(t, map[string]string{
		"azure-pipelines.yml": `
parameters:
  - name: flavour
    default: a
trigger: none
stages:
  - template: stages/${{ parameters.flavour }}.yml
  - stage: After
    dependsOn: FromTheDynamicTemplate
`,
	})
	pipe := filepath.Join(root, "azure-pipelines.yml")
	if ds := withCode(e.Diagnose(pipe), CodeUnknownDepend); len(ds) != 0 {
		t.Fatalf("got %+v", ds)
	}
	rep, ok := e.PipelineCheck(pipe)
	if !ok || rep.Dynamic != 1 || !strings.Contains(rep.Title(), "decided at run time") {
		t.Errorf("report %+v %q", rep, rep.Title())
	}
}

func TestOutputVariables(t *testing.T) {
	root, e := preflightWorkspace(t, map[string]string{
		"azure-pipelines.yml": `
trigger: none
jobs:
  - job: A
    steps:
      - bash: |
          echo "##vso[task.setvariable variable=version;isOutput=true]1.2"
          echo "##vso[task.setvariable variable=plain]x"
        name: setvars
      - script: ./compute.sh
        name: opaque
      - task: AzureCLI@2
        name: cli
  - job: B
    dependsOn: A
    variables:
      ok: $[ dependencies.A.outputs['setvars.version'] ]
      notOutput: $[ dependencies.A.outputs['setvars.plain'] ]
      typo: $[ dependencies.A.outputs['setvar.version'] ]
      fromScript: $[ dependencies.A.outputs['opaque.anything'] ]
      fromTask: $[ dependencies.A.outputs['cli.result'] ]
    steps:
      - script: echo $(ok)
`,
	})
	ds := e.Diagnose(filepath.Join(root, "azure-pipelines.yml"))
	no := withCode(ds, CodeOutputNotIsOutput)
	unk := withCode(ds, CodeOutputVariable)
	if len(no) != 1 || no[0].Line != 16 || !strings.Contains(no[0].Message, "sets 'plain' without isOutput=true") {
		t.Errorf("not-output: %+v", no)
	}
	if len(unk) != 1 || unk[0].Line != 17 || !strings.Contains(unk[0].Message, "named 'setvar'") {
		t.Errorf("unknown: %+v", unk)
	}
	for _, d := range append(no, unk...) {
		if d.Severity != SevWarning {
			t.Errorf("%s should be a warning", d.Code)
		}
	}
}

func TestStructureChecks(t *testing.T) {
	root, e := preflightWorkspace(t, map[string]string{
		"azure-pipelines.yml": "" +
			"name: $(Date:yyyyMMdd)$(Rev:.r)\n" + // a pipeline: CI trigger on by default
			"parameters:\n" +
			"  - name: region\n" + // no default, CI trigger on by default
			"    type: string\n" +
			"  - name: size\n" +
			"    default: huge\n" +
			"    values: [small, large]\n" +
			"  - name: pick\n" + // values without default: Azure uses the first
			"    values: [a, b]\n" +
			"stages:\n" +
			"  - stage: Build-App\n" +
			"    jobs:\n" +
			"      - job: A\n" +
			"      - job: A\n" +
			"      - job: B\n" +
			"        steps:\n" +
			"\t\t  - script: echo\n" +
			"  - template: steps.yml\n",
		"steps.yml": "steps:\n  - script: echo\n",
	})
	ds := e.Diagnose(filepath.Join(root, "azure-pipelines.yml"))
	got := codes(ds)
	want := map[string]int{CodeNoDefault: 1, CodeDefaultNotValue: 1, CodeInvalidName: 1, CodeDuplicateName: 1, CodeTabIndent: 1, CodeTemplateKind: 1, CodeUnusedParam: 3}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("codes %v\n%+v", got, ds)
	}
	for _, d := range ds {
		line := strings.Split(mustRead(t, filepath.Join(root, "azure-pipelines.yml")), "\n")[d.Line]
		switch d.Code {
		case CodeDefaultNotValue:
			if line[d.StartCol:d.EndCol] != "huge" {
				t.Errorf("default range %q", line[d.StartCol:d.EndCol])
			}
		case CodeTemplateKind:
			if !strings.Contains(d.Message, "is a steps template, but it is used in a stages: list") {
				t.Errorf("kind message %q", d.Message)
			}
		case CodeNoDefault:
			if d.Severity != SevWarning || line[d.StartCol:d.EndCol] != "region" {
				t.Errorf("no default: %+v", d)
			}
		}
	}

	// a manual pipeline can ask for the value
	_, e2 := preflightWorkspace(t, map[string]string{"p.yml": "trigger: none\nparameters:\n  - name: region\n    type: string\nsteps:\n  - script: echo\n"})
	if ds := withCode(e2.Diagnose(filepath.Join(e2.Roots()[0], "p.yml")), CodeNoDefault); len(ds) != 0 {
		t.Errorf("manual pipeline: %+v", ds)
	}
	// quick fix: pick an allowed default
	a := actionByTitle(t, e.CodeActions(filepath.Join(root, "azure-pipelines.yml"), 0, 100), "Change default to 'small'")
	if a.Edits[0].NewText != "small" {
		t.Errorf("edit %+v", a.Edits)
	}
}

func TestParametersPassedToTemplateWithoutParameters(t *testing.T) {
	root, e := preflightWorkspace(t, map[string]string{
		"azure-pipelines.yml": `
trigger: none
steps:
  - template: plain.yml
    parameters:
      name: x
  - template: plain.yml
`,
		"plain.yml": "steps:\n  - script: echo\n",
	})
	ds := withCode(e.Diagnose(filepath.Join(root, "azure-pipelines.yml")), CodeUnknownParam)
	if len(ds) != 1 || ds[0].Severity != SevWarning || !strings.Contains(ds[0].Message, "declares no parameters") {
		t.Fatalf("got %+v", ds)
	}
}

// Typed parameters reject unknown names when the run is queued; a legacy
// map-form block accepts them (real-world templates rely on that).
func TestUnknownParamSeverityDependsOnParameterForm(t *testing.T) {
	root, e := preflightWorkspace(t, map[string]string{
		"azure-pipelines.yml": `
trigger: none
steps:
  - template: typed.yml
    parameters:
      extra: 1
  - template: legacy.yml
    parameters:
      extra: 1
`,
		"typed.yml":  "parameters:\n  - name: a\n    default: x\nsteps:\n  - script: echo ${{ parameters.a }}\n",
		"legacy.yml": "parameters:\n  a: x\nsteps:\n  - script: echo ${{ parameters.a }} ${{ parameters.extra }}\n",
	})
	ds := withCode(e.Diagnose(filepath.Join(root, "azure-pipelines.yml")), CodeUnknownParam)
	if len(ds) != 2 || ds[0].Severity != SevError || ds[1].Severity != SevInfo {
		t.Fatalf("got %+v", ds)
	}
	// the legacy body reads `extra`, which a caller passes: not undeclared
	legacy := filepath.Join(root, "legacy.yml")
	if got := withCode(e.Diagnose(legacy), CodeUndeclaredParam); len(got) != 0 {
		t.Fatalf("legacy template: %+v", got)
	}
	// once no caller passes it, it is; the template is re-checked when only
	// its caller changed
	pipe := filepath.Join(root, "azure-pipelines.yml")
	open := map[string]string{pipe: "trigger: none\nsteps:\n  - template: legacy.yml\n"}
	e.OpenDocument(pipe, open[pipe])
	if got := withCode(e.Diagnose(legacy), CodeUndeclaredParam); len(got) != 1 {
		t.Fatalf("legacy template after edit: %+v", got)
	}
	if got, want := stateOf(e), stateOf(fresh(t, root, open)); !reflect.DeepEqual(got, want) {
		t.Fatalf("incremental differs from full rebuild:\n got  %+v\n want %+v", got, want)
	}
}

func TestPipelineReport(t *testing.T) {
	root, e := preflightWorkspace(t, map[string]string{
		"azure-pipelines.yml": `
trigger: none
steps:
  - template: t/a.yml
`,
		"t/a.yml": `
parameters:
  - name: unusedButHarmless
    default: x
steps:
  - template: b.yml
`,
		"t/b.yml": `
parameters:
  - name: required
steps:
  - script: echo ${{ parameters.typo }}
  - template: missing.yml
`,
	})
	pipe := filepath.Join(root, "azure-pipelines.yml")
	rep, ok := e.PipelineCheck(pipe)
	if !ok {
		t.Fatal("not a root pipeline")
	}
	if rep.Errors != 2 || rep.Warnings != 1 || rep.Templates != 2 {
		t.Fatalf("report %+v", rep)
	}
	if rep.Problems[0].RelativePath != "t/a.yml" || rep.Problems[0].Code != CodeMissingRequired {
		t.Errorf("first problem %+v", rep.Problems[0])
	}
	if rep.Title() != "✗ Will fail when queued: 2 errors, 1 warning" {
		t.Errorf("title %q", rep.Title())
	}
	if _, ok := e.PipelineCheck(filepath.Join(root, "t/a.yml")); ok {
		t.Error("a template is not a pipeline")
	}
	lenses := e.CodeLenses(pipe)
	if len(lenses) != 1 || lenses[0].Kind != "pipeline" || lenses[0].Title != rep.Title() || lenses[0].Line != 0 {
		t.Errorf("lens %+v", lenses)
	}
	if all := e.PipelineChecks(); len(all) != 1 || all[0].Path != pipe {
		t.Errorf("all %+v", all)
	}
}

// Editing a template two levels down must update the root pipeline's
// diagnostics, exactly as a full rebuild would.
func TestDeepTemplateEditUpdatesRootPipeline(t *testing.T) {
	files := map[string]string{
		"other.yml":           otherPipeline,
		"azure-pipelines.yml": "trigger: none\nsteps:\n  - template: t/a.yml\n",
		"t/a.yml":             "steps:\n  - template: b.yml\n",
		"t/b.yml":             "steps:\n  - checkout: self\n",
	}
	root, e := preflightWorkspace(t, files)
	pipe := filepath.Join(root, "azure-pipelines.yml")
	if ds := e.Diagnose(pipe); len(ds) != 0 {
		t.Fatalf("clean pipeline: %+v", ds)
	}
	_ = e.ChangedDiagnostics()
	b := filepath.Join(root, "t/b.yml")
	open := map[string]string{b: "steps:\n  - checkout: tools\n"}
	e.OpenDocument(b, open[b])
	delta := e.ChangedDiagnostics()
	if _, ok := delta.Files[pipe]; !ok {
		t.Fatalf("root pipeline not republished: %+v", delta.Files)
	}
	if got := withCode(delta.Files[pipe], CodeRepoNotDeclared); len(got) != 1 {
		t.Fatalf("root diagnostics %+v", delta.Files[pipe])
	}
	if got, want := stateOf(e), stateOf(fresh(t, root, open)); !reflect.DeepEqual(got, want) {
		t.Fatalf("incremental differs from full rebuild:\n got  %+v\n want %+v", got, want)
	}

	// a.yml stops being reached from... nowhere: it gains a second caller,
	// and the pipeline that called it becomes a template of another one
	write(t, root, "outer.yml", "trigger: none\nstages:\n  - stage: S\n    jobs:\n      - job: J\n        steps:\n          - template: azure-pipelines.yml\n")
	e.FileChanged(filepath.Join(root, "outer.yml"))
	if got, want := stateOf(e), stateOf(fresh(t, root, open)); !reflect.DeepEqual(got, want) {
		t.Fatalf("after outer.yml:\n got  %+v\n want %+v", got, want)
	}
	if _, ok := e.PipelineCheck(pipe); ok {
		t.Error("azure-pipelines.yml is now a template")
	}
}

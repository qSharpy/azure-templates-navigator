package engine

import (
	"path/filepath"
	"strings"
	"testing"
)

// The reproductions from issue #9: each was reported as an error although
// Azure runs the pipeline.

const issue9Child = `
parameters:
- name: props
  type: object
- name: appID
  type: string
- name: meta
  type: object
  default: {}
steps:
- script: echo ${{ parameters.props.type }} ${{ parameters.appID }} ${{ parameters.meta.name }}
`

func TestMultiLineFlowValueIsOneParameter(t *testing.T) {
	root, e := preflightWorkspace(t, map[string]string{
		"child.yml": issue9Child,
		"flow.yml": `
trigger: none
steps:
- template: child.yml
  parameters:
    appID: a
    props: {
      type: 'artifact',
      name: 'a'
    }
    extra: [
      1, 2
    ]
`,
	})
	p := filepath.Join(root, "flow.yml")
	ds := e.Diagnose(p)
	unknown := withCode(ds, CodeUnknownParam)
	if len(unknown) != 1 || unknown[0].param != "extra" || len(ds) != 1 {
		t.Fatalf("want only unknown-param 'extra', got %+v", ds)
	}
	fix := actionByTitle(t, e.CodeActions(p, unknown[0].Line, unknown[0].Line), "Remove unknown parameter 'extra'")
	got := applyEdits(t, mustRead(t, p), fix.Edits)
	if strings.Contains(got, "extra") || strings.Contains(got, "1, 2") || !strings.HasSuffix(got, "    }\n") {
		t.Errorf("removing a multi-line flow value left:\n%s", got)
	}
}

func TestTemplateKeyInParameterValueIsData(t *testing.T) {
	_, e := preflightWorkspace(t, map[string]string{
		"child.yml": issue9Child,
		"data.yml": `
trigger: none
steps:
- template: child.yml
  parameters:
    appID: a
    props: {}
    meta:
      name: 'someParam'
      template: 'some/path/'
`,
	})
	for p, ds := range e.DiagnoseAll() {
		if len(ds) > 0 {
			t.Errorf("%s: %+v", p, ds)
		}
	}
}

func TestParameterNamesIgnoreCase(t *testing.T) {
	root, e := preflightWorkspace(t, map[string]string{
		"child.yml": strings.Replace(issue9Child, "parameters.appID", "parameters.appId", 1),
		"casing.yml": `
trigger: none
steps:
- template: child.yml
  parameters:
    appId: a
    props: {}
`,
	})
	call := e.Diagnose(filepath.Join(root, "casing.yml"))
	if len(call) != 1 || call[0].Code != CodeParamCasing || call[0].Severity != SevInfo || call[0].Line != 4 {
		t.Fatalf("call site: want one param-casing info on line 5, got %+v", call)
	}
	fix := actionByTitle(t, e.CodeActions(filepath.Join(root, "casing.yml"), 4, 4), "Change to 'appID'")
	if got := applyEdits(t, mustRead(t, filepath.Join(root, "casing.yml")), fix.Edits); !strings.Contains(got, "    appID: a\n") {
		t.Errorf("fix gave:\n%s", got)
	}

	body := e.Diagnose(filepath.Join(root, "child.yml"))
	if len(body) != 1 || body[0].Code != CodeParamCasing || body[0].Severity != SevInfo {
		t.Fatalf("template body: want one param-casing info (no undeclared-param, no unused-param), got %+v", body)
	}

	rep, _ := e.PipelineCheck(filepath.Join(root, "casing.yml"))
	if rep.Verdict() != "ok" {
		t.Errorf("verdict %q: %+v", rep.Verdict(), rep.Problems)
	}
	if u := e.paramUsagesLocked(filepath.Join(root, "child.yml"), "appID"); len(u.Usages) != 3 {
		t.Errorf("usages of appID (declaration, body, call): %+v", u.Usages)
	}
}

func (e *Engine) paramUsagesLocked(tpl, name string) UsageResult {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ensureLinked()
	return e.paramUsages(tpl, name, true)
}

// A template's undeclared alias is the pipeline's problem once a pipeline
// reaches it: repository-not-declared reports it there.
func TestTemplateAliasLeftToThePipeline(t *testing.T) {
	root, e := preflightWorkspace(t, map[string]string{
		"lib/inner.yml":  "steps:\n- template: steps/extra.yml@other\n",
		"lib/outer.yml":  "steps:\n- template: inner.yml\n",
		"lib/orphan.yml": "steps:\n- template: steps/extra.yml@other\n",
		"pipe.yml":       "trigger: none\nsteps:\n- template: lib/outer.yml\n",
	})
	if ds := withCode(e.Diagnose(filepath.Join(root, "lib/inner.yml")), CodeUnknownAlias); len(ds) != 0 {
		t.Errorf("reached template: %+v", ds)
	}
	if ds := withCode(e.Diagnose(filepath.Join(root, "pipe.yml")), CodeRepoNotDeclared); len(ds) != 1 {
		t.Errorf("pipeline should report the alias: %+v", e.Diagnose(filepath.Join(root, "pipe.yml")))
	}
	if ds := withCode(e.Diagnose(filepath.Join(root, "lib/orphan.yml")), CodeUnknownAlias); len(ds) != 1 || ds[0].Severity != SevWarning {
		t.Errorf("template nothing runs: %+v", ds)
	}
}

package engine

import "testing"

// Issue #10: parameters declared inline (`- { name: x, ... }`) are declared.
func TestInlineParametersAreDeclared(t *testing.T) {
	root, e := newWorkspace(t)
	p := write(t, root, "azure-pipelines.yml", `
trigger: none

parameters:
- { name: environment, type: string, default: 'dev', values: ['dev', 'test', 'prod'] }
- { name: runTests, type: boolean, default: true }
- name: region
  type: string
  default: 'westeurope'

stages:
- stage: Build
  jobs:
  - job: build
    pool:
      vmImage: ubuntu-latest
    steps:
    - script: echo "Deploying to ${{ parameters.environment }} in ${{ parameters.region }}"
    - ${{ if eq(parameters.runTests, true) }}:
      - script: echo "Running tests"
`)
	e.FileChanged(p)
	for _, d := range e.Diagnose(p) {
		if d.Severity <= SevWarning {
			t.Errorf("unexpected problem: %d:%d %s [%s]", d.Line, d.StartCol, d.Message, d.Code)
		}
	}
}

func TestInlineParametersChecksAndFixes(t *testing.T) {
	root, e := newWorkspace(t)
	p := write(t, root, "templates/t.yml", `
parameters:
  - { name: tier, type: string, default: 'gold', values: [basic, premium] }
  - { name: unused, type: string, default: '' }
steps:
  - script: echo ${{ parameters.tier }} ${{ parameters.extra }}
`)
	e.FileChanged(p)
	codes := map[string]Diagnostic{}
	for _, d := range e.Diagnose(p) {
		codes[d.Code] = d
	}
	if d, ok := codes[CodeDefaultNotValue]; !ok || d.Line != 1 || d.StartCol != 41 || d.EndCol != 47 {
		t.Errorf("default-not-allowed on the inline default: %+v", d)
	}
	if d, ok := codes[CodeUnusedParam]; !ok || d.Line != 2 {
		t.Errorf("unused-param: %+v", d)
	}
	seen := 0
	for _, a := range e.CodeActions(p, 0, 10) {
		switch a.Title {
		case "Declare parameter 'extra'":
			seen++
			if ed := a.Edits[0]; ed.StartLine != 2 || ed.NewText != "\n  - { name: extra, type: string, default: '' }" {
				t.Errorf("declare edit: %+v", ed)
			}
		case "Remove unused parameter declaration 'unused'":
			seen++
			if ed := a.Edits[0]; ed.StartLine != 2 || ed.EndLine != 3 {
				t.Errorf("remove edit: %+v", ed)
			}
		}
	}
	if seen != 2 {
		t.Errorf("want the declare and remove fixes, saw %d", seen)
	}
}

// An inline entry that names the parameter on its second line is removed whole.
func TestInlineParameterNamedOnLaterLine(t *testing.T) {
	root, e := newWorkspace(t)
	p := write(t, root, "templates/t.yml", `
parameters:
  - { type: string, default: 'x',
      name: late }
  - { name: used, type: string }
steps:
  - script: echo ${{ parameters.used }}
`)
	e.FileChanged(p)
	found := false
	for _, a := range e.CodeActions(p, 0, 10) {
		if a.Title == "Remove unused parameter declaration 'late'" {
			found = true
			if ed := a.Edits[0]; ed.StartLine != 1 || ed.EndLine != 3 || ed.StartCol != 0 || ed.EndCol != 0 {
				t.Errorf("remove edit: %+v", ed)
			}
		}
	}
	if !found {
		t.Error("no remove fix for 'late'")
	}
}

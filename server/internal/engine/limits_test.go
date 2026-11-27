package engine

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// The expectations here were checked against Azure DevOps' preview API
// (2026-11-27): which of these it rejects when the run is queued.

func TestTemplateCycle(t *testing.T) {
	root, e := preflightWorkspace(t, map[string]string{
		"p.yml":        "trigger: none\nsteps:\n- template: lim/a.yml\n",
		"lim/a.yml":    "steps:\n- template: b.yml\n",
		"lim/b.yml":    "steps:\n- template: a.yml\n",
		"self.yml":     "trigger: none\nsteps:\n- template: lim/self.yml\n",
		"lim/self.yml": "steps:\n- template: self.yml\n",
	})
	ds := withCode(e.Diagnose(filepath.Join(root, "p.yml")), CodeTemplateCycle)
	if len(ds) != 1 || ds[0].Severity != SevError || ds[0].Line != 2 ||
		!strings.Contains(ds[0].Message, "lim/a.yml → lim/b.yml → lim/a.yml") {
		t.Fatalf("got %+v", ds)
	}
	ds = withCode(e.Diagnose(filepath.Join(root, "self.yml")), CodeTemplateCycle)
	if len(ds) != 1 || ds[0].Severity != SevError || !strings.Contains(ds[0].Message, "includes itself") {
		t.Fatalf("got %+v", ds)
	}
	rep, _ := e.PipelineCheck(filepath.Join(root, "p.yml"))
	if rep.Verdict() != "fail" {
		t.Fatalf("verdict %s", rep.Verdict())
	}
}

func TestTemplateRecursionThatEnds(t *testing.T) {
	root, e := preflightWorkspace(t, map[string]string{
		// Passes a literal that turns the branch false one level down.
		"p.yml": "trigger: none\nsteps:\n- template: g.yml\n- template: r.yml\n  parameters:\n    on: true\n- template: tree.yml\n  parameters:\n    children: [{children: []}]\n",
		"g.yml": `parameters:
- name: depth
  type: number
  default: 0
steps:
- script: echo ${{ parameters.depth }}
- ${{ if eq(parameters.depth, 0) }}:
  - template: g.yml
    parameters:
      depth: 1
`,
		"r.yml": `parameters:
- name: on
  type: boolean
  default: false
steps:
- script: echo
- ${{ if eq(parameters.on, true) }}:
  - template: r.yml
    parameters:
      on: false
`,
		"tree.yml": `parameters:
- name: children
  type: object
  default: []
steps:
- ${{ each c in parameters.children }}:
  - template: tree.yml
    parameters:
      children: ${{ c.children }}
`,
	})
	if ds := withCode(e.Diagnose(filepath.Join(root, "p.yml")), CodeTemplateCycle); len(ds) != 0 {
		t.Fatalf("got %+v", ds)
	}
}

func TestTemplateCycleThroughUnknownBranchIsAWarning(t *testing.T) {
	root, e := preflightWorkspace(t, map[string]string{
		"p.yml": "trigger: none\nparameters:\n- name: deep\n  type: boolean\n  default: false\nsteps:\n- template: u.yml\n  parameters:\n    on: ${{ parameters.deep }}\n",
		"u.yml": `parameters:
- name: on
  type: boolean
  default: false
steps:
- ${{ if eq(parameters.on, true) }}:
  - template: u.yml
    parameters:
      on: ${{ parameters.on }}
`,
	})
	ds := withCode(e.Diagnose(filepath.Join(root, "p.yml")), CodeTemplateCycle)
	if len(ds) != 1 || ds[0].Severity != SevWarning || !strings.Contains(ds[0].Message, "u.yml:6") {
		t.Fatalf("got %+v", ds)
	}
}

func TestTooManyFiles(t *testing.T) {
	files := map[string]string{}
	var calls99, calls100 strings.Builder
	for i := 1; i <= 100; i++ {
		files[fmt.Sprintf("f/f%d.yml", i)] = fmt.Sprintf("steps:\n- script: echo %d\n", i)
		line := fmt.Sprintf("- template: f/f%d.yml\n", i)
		if i < 100 {
			calls99.WriteString(line)
		}
		calls100.WriteString(line)
	}
	files["ok.yml"] = "trigger: none\nsteps:\n" + calls99.String() + strings.Repeat("- template: f/f1.yml\n", 50)
	files["over.yml"] = "trigger: none\nsteps:\n" + calls100.String()
	files["loop.yml"] = "trigger: none\nparameters:\n- name: xs\n  type: object\n  default: []\nsteps:\n" + calls99.String() +
		"- ${{ each x in parameters.xs }}:\n  - template: f/f100.yml\n"
	files["maybe.yml"] = "trigger: none\nparameters:\n- name: x\n  type: boolean\n  default: false\nsteps:\n" + calls99.String() +
		"- ${{ if eq(parameters.x, true) }}:\n  - template: f/f100.yml\n"
	root, e := preflightWorkspace(t, files)
	if ds := withCode(e.Diagnose(filepath.Join(root, "ok.yml")), CodeTooManyFiles); len(ds) != 0 {
		t.Fatalf("100 files: got %+v", ds)
	}
	ds := withCode(e.Diagnose(filepath.Join(root, "over.yml")), CodeTooManyFiles)
	if len(ds) != 1 || ds[0].Severity != SevError || ds[0].Line != 0 || !strings.Contains(ds[0].Message, "101") {
		t.Fatalf("101 files: got %+v", ds)
	}
	for _, p := range []string{"loop.yml", "maybe.yml"} {
		ds = withCode(e.Diagnose(filepath.Join(root, p)), CodeTooManyFiles)
		if len(ds) != 1 || ds[0].Severity != SevWarning {
			t.Fatalf("%s: got %+v", p, ds)
		}
	}
	ds = withCode(e.Diagnose(filepath.Join(root, "maybe.yml")), CodeTooManyFiles)
	if len(ds) != 1 || ds[0].Severity != SevWarning {
		t.Fatalf("maybe 101 files: got %+v", ds)
	}
}

func TestDefaultTypeMismatch(t *testing.T) {
	// type, default, rejected by Azure (checked with the preview API).
	cases := []struct {
		typ, def string
		bad      bool
	}{
		{"number", "abc", true}, {"number", "0x10", true}, {"number", "1e3", true}, {"number", "true", true}, {"number", "[1]", true},
		{"number", ".5", false}, {"number", "+1", false}, {"number", "-3", false}, {"number", "'1.5'", false}, {"number", "' 2 '", false}, {"number", "1,000", false},
		{"boolean", "maybe", true}, {"boolean", "yes", true}, {"boolean", "on", true}, {"boolean", "1", true},
		{"boolean", "'true'", false}, {"boolean", "True", false}, {"boolean", "FALSE", false},
		{"string", "[a, b]", true}, {"string", "{}", true}, {"string", "{a: 1}", true}, {"string", "123", false}, {"string", "true", false}, {"string", "''", false},
		{"object", "abc", false},
		{"step", "abc", true}, {"step", "{}", true}, {"job", "{}", true}, {"stage", "abc", true}, {"deployment", "abc", true},
		{"stepList", "abc", true}, {"stepList", "{}", true}, {"jobList", "abc", true}, {"stageList", "abc", true}, {"deploymentList", "abc", true},
		{"stepList", "[]", false},
		{"number", "${{ variables.x }}", false},
	}
	var b strings.Builder
	b.WriteString("parameters:\n")
	for i, c := range cases {
		fmt.Fprintf(&b, "- name: p%d\n  type: %s\n  default: %s\n", i, c.typ, c.def)
	}
	b.WriteString("- name: untyped\n  default: [a, b]\nsteps:\n- script: echo ${{ parameters }}\n")
	root, e := preflightWorkspace(t, map[string]string{"t.yml": b.String()})
	bad := map[string]bool{}
	for _, d := range withCode(e.Diagnose(filepath.Join(root, "t.yml")), CodeDefaultType) {
		if d.Severity != SevError {
			t.Fatalf("severity %+v", d)
		}
		bad[d.param] = true
	}
	for i, c := range cases {
		if got := bad[fmt.Sprintf("p%d", i)]; got != c.bad {
			t.Errorf("%s default %s: reported %v, want %v", c.typ, c.def, got, c.bad)
		}
	}
	if bad["untyped"] {
		t.Errorf("a parameter without type: is not checked")
	}
}

func TestDependencyCycleAndUnknownFunction(t *testing.T) {
	root, e := preflightWorkspace(t, map[string]string{
		"p.yml": `trigger: none
stages:
- stage: A
  jobs:
  - job: a
    steps:
    - script: echo ${{ lenght(parameters) }}
    - ${{ if eq(1, 2) }}:
      - script: echo ${{ add(1, 2) }}
    - ${{ if eq(parameters.x, 2) }}:
      - script: echo ${{ ad(1, 2) }}
- stage: B
  dependsOn: [A, C]
  jobs:
  - job: b
    steps:
    - script: echo
- stage: C
  dependsOn: B
  jobs:
  - job: c
    steps:
    - script: echo
`,
	})
	ds := e.Diagnose(filepath.Join(root, "p.yml"))
	if c := withCode(ds, CodeDependencyCycle); len(c) != 2 || c[0].Line != 12 || c[1].Line != 18 {
		t.Fatalf("cycles: %+v", c)
	}
	f := withCode(ds, CodeUnknownFunction)
	if len(f) != 2 || f[0].Line != 6 || f[0].Severity != SevError || !strings.Contains(f[0].Message, "Did you mean 'length'?") ||
		f[1].Line != 10 || f[1].Severity != SevWarning || !strings.Contains(f[1].Message, "branch at line 10") {
		t.Fatalf("functions: %+v", f)
	}
}

func TestProblemKeyIgnoresBranchLines(t *testing.T) {
	a := problemKey("p.yml", CodeTemplateCycle, "u.yml:7: Template cycle: x, if the ${{ }} branch at u.yml:6 is taken")
	b := problemKey("p.yml", CodeTemplateCycle, "u.yml:9: Template cycle: x, if the ${{ }} branch at u.yml:8 is taken")
	c := problemKey("t.yml", CodeUnknownFunction, "Unknown function 'ad', if the ${{ }} branch at line 10 is taken")
	d := problemKey("t.yml", CodeUnknownFunction, "Unknown function 'ad', if the ${{ }} branch at line 14 is taken")
	if a != b || c != d {
		t.Fatalf("keys differ:\n%q\n%q\n%q\n%q", a, b, c, d)
	}
}

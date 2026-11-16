package pipeline

import (
	"reflect"
	"strings"
	"testing"
)

func lns(s string) []string { return SplitLines(strings.TrimPrefix(s, "\n")) }

func TestTabIndentedLines(t *testing.T) {
	lines := lns("steps:\n\t- script: x\n  - script: |\n      \techo tab inside a script\n  - bash: y\n")
	if got := TabIndentedLines(lines); !reflect.DeepEqual(got, []int{1}) {
		t.Fatalf("got %v", got)
	}
}

func TestFindNamedItemsAndValidName(t *testing.T) {
	lines := lns(`
stages:
  - stage: Build   # comment
    jobs:
      - job: 'Compile'
      - deployment: Deploy_${{ parameters.env }}
  - stage: Build-App
    jobs:
      - job: Test
        steps:
          - script: |
              - job: NotAJob
`)
	items := FindNamedItems(lines)
	var names []string
	for _, it := range items {
		names = append(names, it.Kind+":"+it.Name)
	}
	want := []string{"stage:Build", "job:Compile", "deployment:Deploy_${{ parameters.env }}", "stage:Build-App", "job:Test"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("got %v", names)
	}
	if c := items[1]; lines[c.Line][c.Col:c.End] != "'Compile'" {
		t.Fatalf("range %q", lines[c.Line][c.Col:c.End])
	}
	for name, ok := range map[string]bool{
		"Build": true, "_x9": true, "Deploy_${{ parameters.env }}": true, "${{ parameters.name }}": true,
		"Build-App": false, "9lives": false, "Build App": false, "Deploy-${{ parameters.env }}": false, "$(name)": true,
	} {
		if ValidName(name) != ok {
			t.Errorf("ValidName(%q) = %v", name, !ok)
		}
	}
}

func TestNameMatcher(t *testing.T) {
	m := NewNameMatcher()
	m.Add("Build")
	m.Add("Deploy_${{ parameters.environment }}")
	for name, ok := range map[string]bool{"build": true, "Deploy_staging": true, "Deploy_": true, "Test": false, "Predeploy_x": false} {
		if m.Has(name) != ok {
			t.Errorf("Has(%q) = %v", name, !ok)
		}
	}
	if !reflect.DeepEqual(m.Literals(), []string{"Build"}) {
		t.Fatalf("literals %v", m.Literals())
	}
}

func TestFindDependsOn(t *testing.T) {
	lines := lns(`
stages:
  - stage: A
  - stage: B
    dependsOn: A
  - stage: C
    displayName: c
    dependsOn: [A, 'B', ${{ parameters.x }}]
    jobs:
      - job: J1
      - job: J2
        dependsOn:
          - J1
          - $(skip)
        steps:
          - script: echo
  - template: t.yml
    parameters:
      dependsOn: [Nope]
  - stage: D
    dependsOn: []
`)
	ds := FindDependsOn(lines)
	type got struct {
		owner string
		names []string
	}
	var out []got
	for _, d := range ds {
		g := got{owner: d.Owner.Kind + ":" + d.Owner.Name}
		for _, n := range d.Names {
			if lines[n.Line][n.Col:n.End] != n.Name && Unquote(lines[n.Line][n.Col:n.End]) != n.Name {
				t.Errorf("range of %s is %q", n.Name, lines[n.Line][n.Col:n.End])
			}
			g.names = append(g.names, n.Name)
		}
		out = append(out, g)
	}
	want := []got{
		{"stage:B", []string{"A"}},
		{"stage:C", []string{"A", "B"}},
		{"job:J2", []string{"J1"}},
		{"stage:D", nil},
	}
	if !reflect.DeepEqual(out, want) {
		t.Fatalf("got %+v", out)
	}
}

func TestDuplicateNames(t *testing.T) {
	lines := lns(`
stages:
  - stage: Build
    jobs:
      - job: A
      - job: a
  - stage: Test
    jobs:
      - job: A
  - ${{ if true }}:
    - stage: Build
  - ${{ else }}:
    - stage: Build
  - stage: build
`)
	var got []string
	for _, d := range DuplicateNames(lines) {
		got = append(got, d.Kind+":"+d.Name)
	}
	if !reflect.DeepEqual(got, []string{"job:a", "stage:build"}) {
		t.Fatalf("got %v", got)
	}
}

func TestTemplateKindAndContext(t *testing.T) {
	if k := TemplateKind(lns("parameters:\n  - name: a\nsteps:\n  - script: x\n")); k != "steps" {
		t.Fatalf("kind %q", k)
	}
	if k := TemplateKind(lns("jobs: []\nsteps: []\n")); k != "" {
		t.Fatalf("mixed kind %q", k)
	}
	lines := lns(`
stages:
- stage: A
  jobs:
  - job: J
    steps:
    - template: s1.yml
    - ${{ if true }}:
      - template: s2.yml
  - template: j1.yml
- template: st1.yml
variables:
  - template: v.yml
extends:
  template: base.yml
  parameters:
    preSteps:
      - template: s3.yml
`)
	want := map[string]string{"s1.yml": "steps", "s2.yml": "steps", "j1.yml": "jobs", "st1.yml": "stages", "v.yml": "variables", "base.yml": "", "s3.yml": ""}
	for _, r := range FindTemplateRefs(lines) {
		if got := TemplateContext(lines, r.Line); got != want[r.Ref] {
			t.Errorf("%s: context %q, want %q", r.Ref, got, want[r.Ref])
		}
	}
}

func TestFindCheckouts(t *testing.T) {
	lines := lns(`
steps:
  - checkout: self
  - checkout: tools
  - checkout: none
  - checkout: git://Proj/Repo@main
  - checkout: ${{ parameters.repo }}
  - checkout: 'infra'   # quoted
`)
	var got []string
	for _, c := range FindCheckouts(lines) {
		got = append(got, c.Name)
	}
	if !reflect.DeepEqual(got, []string{"tools", "infra"}) {
		t.Fatalf("got %v", got)
	}
}

func TestOutputRefsAndNamedSteps(t *testing.T) {
	lines := lns(`
jobs:
  - job: A
    steps:
      - bash: |
          echo "##vso[task.setvariable variable=version;isOutput=true]1.0"
          echo "##vso[task.setvariable variable=plain]x"
        name: setvars
      - pwsh: Write-Host "##vso[task.setvariable variable=Other;isoutput=true]y"
        name: 'second'
      - task: AzureCLI@2
        name: cli
      - script: echo no name
  - job: B
    dependsOn: A
    variables:
      v: $[ dependencies.A.outputs['setvars.version'] ]
      w: $[ stageDependencies.S.A.outputs["setvars.plain"] ]
    condition: eq(dependencies.A.outputs['A.second.other'], 'y')
`)
	refs := FindOutputRefs(lines)
	var got []string
	for _, r := range refs {
		got = append(got, r.Step+"."+r.Var)
		if q := lines[r.Line][r.Col:r.End]; !strings.Contains(q, r.Step+"."+r.Var) {
			t.Errorf("range %q", q)
		}
	}
	if !reflect.DeepEqual(got, []string{"setvars.version", "setvars.plain", "second.other"}) {
		t.Fatalf("refs %v", got)
	}
	steps := FindNamedSteps(lines)
	if len(steps) != 3 {
		t.Fatalf("steps %+v", steps)
	}
	if s := steps[0]; s.Name != "setvars" || s.Kind != "bash" || !reflect.DeepEqual(s.Sets, map[string]bool{"version": true, "plain": false}) {
		t.Fatalf("setvars %+v", s)
	}
	if s := steps[1]; s.Name != "second" || !s.Sets["other"] {
		t.Fatalf("second %+v", s)
	}
	if s := steps[2]; s.Name != "cli" || s.Kind != "task" || len(s.Sets) != 0 {
		t.Fatalf("cli %+v", s)
	}
}

func TestRunsAutomatically(t *testing.T) {
	cases := map[string]bool{
		"steps: []\n":                                      true,
		"trigger: none\nsteps: []\n":                       false,
		"trigger: 'none' # manual\nsteps: []\n":            false,
		"trigger:\n  branches:\n    include: [main]\n":     true,
		"trigger: none\nschedules:\n  - cron: '0 0 * * *'": true,
	}
	for src, want := range cases {
		if got := RunsAutomatically(lns(src)); got != want {
			t.Errorf("%q: %v", src, got)
		}
	}
}

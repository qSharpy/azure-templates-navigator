package pipeline

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func renderOutline(syms []*Symbol, depth int, b *strings.Builder) {
	for _, s := range syms {
		fmt.Fprintf(b, "%s%s %s", strings.Repeat("  ", depth), s.Kind, s.Name)
		if s.Detail != "" {
			fmt.Fprintf(b, " (%s)", s.Detail)
		}
		fmt.Fprintf(b, " [%d-%d]\n", s.Line, s.EndLine)
		renderOutline(s.Children, depth+1, b)
	}
}

func TestOutline(t *testing.T) {
	ls := lines(`
parameters:
  - name: env
    type: string
  - name: dryRun
    type: boolean
    default: false
trigger:
  branches:
    include: [main]
resources:
  repositories:
    - repository: platform
      type: git
      name: Platform/platform-templates
variables:
  - name: a
    value: one
  - group: shared
stages:
  - stage: Build
    displayName: Build it
    jobs:
      - job: Compile
        steps:
          - task: UseDotNet@2
            displayName: Install .NET
          - script: |
              dotnet build
          - template: steps/publish.yml@platform
            parameters:
              x: 1
  - ${{ if eq(parameters.env, 'prod') }}:
    - stage: Deploy
      jobs:
        - deployment: Web
          environment: prod
`)
	var b strings.Builder
	renderOutline(Outline(ls), 0, &b)
	want := `section parameters [0-5]
  parameter env (string · required) [1-2]
  parameter dryRun (boolean = false) [3-5]
section trigger [6-8]
section resources [9-13]
  repository platform (Platform/platform-templates) [11-13]
section variables [14-17]
  variable a (one) [15-16]
  variable shared (variable group) [17-17]
section stages [18-35]
  stage Build (Build it) [19-30]
    job Compile [22-30]
      step Install .NET (UseDotNet@2) [24-25]
      step script: dotnet build [26-27]
      template steps/publish.yml@platform [28-30]
  condition if eq(parameters.env, 'prod') [31-35]
    stage Deploy [32-35]
      deployment Web [34-35]
`
	if b.String() != want {
		t.Errorf("outline:\n%s\nwant:\n%s", b.String(), want)
	}
}

func TestOutlineExtends(t *testing.T) {
	ls := lines(`
trigger: none
extends:
  template: pipelines/secure-base.yml@platform
  parameters:
    serviceName: x
    stages:
      - stage: Build
        jobs:
          - job: A
            steps:
              - script: echo hi
`)
	var b strings.Builder
	renderOutline(Outline(ls), 0, &b)
	want := `section trigger (none) [0-0]
extends extends [1-10]
  template pipelines/secure-base.yml@platform [2-10]
    stage Build [6-10]
      job A [8-10]
        step script: echo hi [10-10]
`
	if b.String() != want {
		t.Errorf("outline:\n%s\nwant:\n%s", b.String(), want)
	}
}

// checkOutline asserts what LSP clients require of every symbol: the
// selection lies on the first line and inside the range (issue #7: VS Code
// rejects the whole response otherwise), and children lie inside the parent.
func checkOutline(t *testing.T, name string, ls []string, syms []*Symbol, parent *Symbol) {
	t.Helper()
	for _, s := range syms {
		switch {
		case s.EndLine < s.Line:
			t.Errorf("%s: %s %q ends (%d) before it starts (%d)", name, s.Kind, s.Name, s.EndLine, s.Line)
		case s.Line < 0 || s.EndLine >= len(ls):
			t.Errorf("%s: %s %q lines %d-%d outside the file", name, s.Kind, s.Name, s.Line, s.EndLine)
		case s.SelStart < 0 || s.SelEnd < s.SelStart || s.SelEnd > len(ls[s.Line]):
			t.Errorf("%s: %s %q selection %d-%d outside line %d", name, s.Kind, s.Name, s.SelStart, s.SelEnd, s.Line)
		case parent != nil && (s.Line < parent.Line || s.EndLine > parent.EndLine):
			t.Errorf("%s: %s %q [%d-%d] outside parent %q [%d-%d]", name, s.Kind, s.Name, s.Line, s.EndLine, parent.Name, parent.Line, parent.EndLine)
		}
		checkOutline(t, name, ls, s.Children, s)
	}
}

func TestOutlineSectionsWithGaps(t *testing.T) {
	cases := []struct{ name, src, want string }{
		{"blank line between compact items", "parameters:\n- name: a\n  type: string\n\n- name: b\n  type: string\nsteps:\n- script: x\n",
			"section parameters [0-5]\n  parameter a (string · required) [1-2]\n  parameter b (string · required) [4-5]\nsection steps [6-7]\n  step script: x [7-7]\n"},
		{"column-0 comment in an indented list", "parameters:\n  - name: a\n    type: string\n# - name: old\n  - name: b\n    type: string\nsteps:\n- script: x\n",
			"section parameters [0-5]\n  parameter a (string · required) [1-2]\n  parameter b (string · required) [4-5]\nsection steps [6-7]\n  step script: x [7-7]\n"},
		{"column-0 comment in a compact list", "parameters:\n- name: a\n# old\n- name: b\n",
			"section parameters [0-3]\n  parameter a (string · required) [1-1]\n  parameter b (string · required) [3-3]\n"},
		{"map form", "parameters:\n  a: 1\n# c\n  b: 2\n",
			"section parameters [0-3]\n  parameter a (any = 1) [1-1]\n  parameter b (any = 2) [3-3]\n"},
		{"variables", "variables:\n- name: a\n  value: 1\n# c\n- name: b\n  value: 2\n",
			"section variables [0-5]\n  variable a (1) [1-2]\n  variable b (2) [4-5]\n"},
	}
	for _, c := range cases {
		ls := strings.Split(strings.TrimSuffix(c.src, "\n"), "\n")
		syms := Outline(ls)
		checkOutline(t, c.name, ls, syms, nil)
		var b strings.Builder
		renderOutline(syms, 0, &b)
		if b.String() != c.want {
			t.Errorf("%s:\n%s\nwant:\n%s", c.name, b.String(), c.want)
		}
	}
}

func TestOutlineInvariantsOnSamples(t *testing.T) {
	n := 0
	err := filepath.Walk(filepath.Join("..", "..", "..", "samples"), func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || (filepath.Ext(p) != ".yml" && filepath.Ext(p) != ".yaml") {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		ls := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
		checkOutline(t, p, ls, Outline(ls), nil)
		n++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("no sample YAML found")
	}
}

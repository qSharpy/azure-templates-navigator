package pipeline

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func lines(s string) []string { return SplitLines(strings.TrimPrefix(s, "\n")) }

func TestParseParametersListForm(t *testing.T) {
	ps := ParseParameters(lines(`
parameters:
  # REQUIRED
  - name: environment   # comment
    type: string
  - name: vmImage
    type: string
    default: 'ubuntu-latest'
  - name: flag
    type: boolean
    default: false
  - name: tier
    type: string
    default: dev
    values:
      - dev
      - prod
  - name: inline
    values: [a, 'b']
  - name: steps
    type: stepList
    default:
      - script: echo hi

steps:
  - script: echo ${{ parameters.environment }}
`))
	if len(ps) != 6 {
		t.Fatalf("want 6 params, got %d: %+v", len(ps), ps)
	}
	if ps[0].Name != "environment" || !ps[0].Required || ps[0].Type != "string" || ps[0].Line != 2 {
		t.Errorf("bad environment: %+v", ps[0])
	}
	if ps[1].Required || ps[1].Default != "'ubuntu-latest'" {
		t.Errorf("bad vmImage: %+v", ps[1])
	}
	if !reflect.DeepEqual(ps[3].Values, []string{"dev", "prod"}) {
		t.Errorf("bad values: %+v", ps[3].Values)
	}
	if !reflect.DeepEqual(ps[4].Values, []string{"a", "b"}) || !ps[4].Required {
		t.Errorf("bad inline values: %+v", ps[4])
	}
	if ps[5].Required || !ps[5].HasDefault {
		t.Errorf("block default should not be required: %+v", ps[5])
	}
}

func TestParseParametersColumnZeroAndMapForm(t *testing.T) {
	ps := ParseParameters(lines(`
parameters:
- name: a
  type: number
# a comment at column 0
- name: b
  default: 1
jobs: []
`))
	if len(ps) != 2 || ps[0].Type != "number" || ps[1].Required {
		t.Fatalf("column-0 list form: %+v", ps)
	}
	ps = ParseParameters(lines(`
parameters:
  foo: bar
  baz:
    nested: 1
steps: []
`))
	if len(ps) != 2 || ps[0].Name != "foo" || ps[0].Required || ps[0].Type != "" {
		t.Fatalf("map form: %+v", ps)
	}
}

func TestTemplateRefOnLine(t *testing.T) {
	cases := map[string]string{
		"  - template: templates/a.yml":                "templates/a.yml",
		"  - template: 'templates/a.yml@tools' # note": "templates/a.yml@tools",
		"  template: /x/y.yml":                         "/x/y.yml",
		"    - script: echo template: nope":            "",
		"  # - template: commented.yml":                "",
		"  - template:":                                "",
	}
	for in, want := range cases {
		r, ok := TemplateRefOnLine(in, 0)
		if want == "" {
			if ok {
				t.Errorf("%q: expected no match, got %q", in, r.Ref)
			}
			continue
		}
		if !ok || r.Ref != want {
			t.Errorf("%q: want %q, got %q (ok=%v)", in, want, r.Ref, ok)
		}
	}
	r, _ := TemplateRefOnLine("  - template: a.yml@x", 3)
	if r.Path() != "a.yml" || r.Alias() != "x" || r.KeyCol != 4 || r.ValueCol != 14 || r.Line != 3 {
		t.Errorf("bad positions: %+v", r)
	}
}

func TestParseCallSite(t *testing.T) {
	ls := lines(`
steps:
  - template: t.yml
    parameters:
      a: 1
      obj:
        nested: x
        list:
          - k: v
      script: |
        not: a param
      ${{ if eq(variables.x, 'y') }}:
        cond: true
      ${{ else }}:
        cond: false
      quoted: 'hi' # trailing
  - script: echo
`)
	cs := ParseCallSite(ls, 1)
	want := []string{"a", "obj", "script", "cond", "quoted"}
	if !reflect.DeepEqual(cs.Order, want) {
		t.Fatalf("order: want %v got %v", want, cs.Order)
	}
	if cs.Passed["quoted"].Value != "'hi'" || cs.Passed["obj"].IsBlock != true || !cs.HasConditionals {
		t.Errorf("bad call site: %+v", cs)
	}
	if cs.LastLine != 14 {
		t.Errorf("last line: %d", cs.LastLine)
	}

	each := ParseCallSite(lines(`
- template: t.yml
  parameters:
    ${{ each p in parameters }}:
      ${{ p.key }}: ${{ p.value }}
`), 0)
	if !each.EachPassthrough || len(each.Passed) != 0 {
		t.Errorf("each passthrough: %+v", each)
	}
}

func TestFindCallSite(t *testing.T) {
	ls := lines(`
jobs:
  - template: t.yml
    parameters:
      a: 1
      ${{ if true }}:
        b: 2
      c:
        d: 3
  - job: x
    steps:
      - script: hi
`)
	// 3: direct param, 5: param inside ${{ if }}, 7: key nested in an object
	// value (not a parameter position), 10: unrelated job
	for line, want := range map[int]bool{3: true, 5: true, 7: false, 10: false} {
		ctx, ok := FindCallSite(ls, line)
		got := ok && ctx.InParameters && ctx.TemplateLine == 1
		if got != want {
			t.Errorf("line %d: want %v, got %+v ok=%v", line, want, ctx, ok)
		}
	}
}

func TestParseRepositories(t *testing.T) {
	repos := ParseRepositories(lines(`
resources:
  pipelines:
    - pipeline: up
      source: x
  repositories:
    - repository: templates   # alias
      type: github
      name: myorg/pipeline-templates
      ref: refs/tags/v2
      endpoint: gh-conn
    - repository: ado
      name: Shared/infra
trigger: none
`))
	if len(repos) != 2 {
		t.Fatalf("want 2 got %+v", repos)
	}
	tpl := repos["templates"]
	if tpl.Type != "github" || tpl.Name != "myorg/pipeline-templates" || tpl.Ref != "refs/tags/v2" || tpl.Endpoint != "gh-conn" || tpl.RepoName() != "pipeline-templates" {
		t.Errorf("bad templates repo: %+v", tpl)
	}
	if repos["ado"].Type != "git" {
		t.Errorf("default type should be git: %+v", repos["ado"])
	}
}

func TestParseVariables(t *testing.T) {
	vs := ParseVariables(lines(`
variables:
  a: 1
  b: 'two'
stages:
  - stage: s
    variables:
      - name: c
        value: three
      - group: my-group
`))
	if vs.Vars["a"].Value != "1" || vs.Vars["b"].Value != "two" || vs.Vars["c"].Value != "three" {
		t.Errorf("vars: %+v", vs.Vars)
	}
	if len(vs.Groups) != 1 || vs.Groups[0].Name != "my-group" {
		t.Errorf("groups: %+v", vs.Groups)
	}
	if r, ok := VariableRefAt("echo $(Build.BuildId) ${{ variables.a }}", 8); !ok || r.Name != "Build.BuildId" {
		t.Errorf("var ref: %+v", r)
	}
}

func TestCollectParamUsage(t *testing.T) {
	u := CollectParamUsage(lines(`
parameters:
  - name: a
  - name: b
  - name: c
steps:
  - script: echo ${{ parameters.a }}
    condition: eq(parameters['b'], 'x')
`))
	if !u.Names["a"] || !u.Names["b"] || u.Names["c"] || u.UsesAll {
		t.Errorf("usage: %+v", u)
	}
	u = CollectParamUsage(lines(`
parameters:
  - name: a
steps:
  - script: echo '${{ convertToJson(parameters) }}'
`))
	if !u.UsesAll {
		t.Errorf("convertToJson(parameters) should mark all used")
	}
}

func TestTypeCompatible(t *testing.T) {
	cases := []struct {
		typ, val string
		ok       bool
	}{
		{"boolean", "true", true},
		{"boolean", "'true'", false},
		{"boolean", "maybe", false},
		{"number", "42", true},
		{"number", "'42'", true},
		{"number", "abc", false},
		{"string", "true", true},
		{"stepList", "[]", true},
		{"stepList", "hello", false},
		{"customType", "anything", true},
	}
	for _, c := range cases {
		if _, ok := TypeCompatible(c.typ, c.val); ok != c.ok {
			t.Errorf("%s=%s: want %v", c.typ, c.val, c.ok)
		}
	}
}

func TestStripComment(t *testing.T) {
	cases := map[string]string{
		"a: b # c":           "a: b",
		"a: 'b # c'":         "a: 'b # c'",
		"a: \"x#y\" # z":     "a: \"x#y\"",
		"# full":             "",
		"url: http://x#frag": "url: http://x#frag",
	}
	for in, want := range cases {
		if got := StripComment(in); got != want {
			t.Errorf("%q: want %q got %q", in, want, got)
		}
	}
}

func TestParseCallSiteExtends(t *testing.T) {
	// extends: template and parameters are siblings at the same indent, and a
	// block value may itself contain nested template calls.
	ls := lines(`
trigger: none
extends:
  template: base.yml@platform
  parameters:
    serviceConnection: prod
    stages:
      - stage: Build
        jobs:
          - job: A
            steps:
              - template: step.yml
                parameters:
                  foo: 1
    ${{ if eq(variables.x, 'y') }}:
      flag: true
variables:
  a: b
`)
	cs := ParseCallSite(ls, 2)
	if want := []string{"serviceConnection", "stages", "flag"}; !reflect.DeepEqual(cs.Order, want) {
		t.Fatalf("order: want %v got %v", want, cs.Order)
	}
	if cs.ParamsLine != 3 || cs.LastLine != 14 {
		t.Errorf("params line %d, last line %d", cs.ParamsLine, cs.LastLine)
	}
	inner := ParseCallSite(ls, 10)
	if want := []string{"foo"}; !reflect.DeepEqual(inner.Order, want) {
		t.Errorf("inner order: want %v got %v", want, inner.Order)
	}

	// parameters before template, and a list item whose first key is parameters
	before := lines(`
extends:
  parameters:
    a: 1
  template: base.yml
`)
	if cs := ParseCallSite(before, 3); !reflect.DeepEqual(cs.Order, []string{"a"}) {
		t.Errorf("parameters before template: %+v", cs)
	}
	item := lines(`
steps:
  - parameters:
      a: 1
    template: t.yml
  - script: echo
`)
	if cs := ParseCallSite(item, 3); !reflect.DeepEqual(cs.Order, []string{"a"}) {
		t.Errorf("- parameters: item: %+v", cs)
	}

	// no parameters block: a sibling template in the *next* list item must not leak
	none := lines(`
steps:
  - template: a.yml
  - template: b.yml
    parameters:
      x: 1
`)
	if cs := ParseCallSite(none, 1); cs.ParamsLine != -1 {
		t.Errorf("leaked parameters from next item: %+v", cs)
	}
}

func TestFindCallSiteExtends(t *testing.T) {
	ls := lines(`
extends:
  template: base.yml
  parameters:
    a: 1
    obj:
      nested: x
`)
	for line, want := range map[int]bool{3: true, 5: false} {
		ctx, ok := FindCallSite(ls, line)
		got := ok && ctx.InParameters && ctx.TemplateLine == 1
		if got != want {
			t.Errorf("line %d: want %v, got %+v ok=%v", line, want, ctx, ok)
		}
	}
	before := lines(`
extends:
  parameters:
    a: 1
  template: base.yml
`)
	if ctx, ok := FindCallSite(before, 2); !ok || ctx.TemplateLine != 3 {
		t.Errorf("parameters before template: %+v ok=%v", ctx, ok)
	}
}

func TestFindParamRefs(t *testing.T) {
	ls := lines(`
parameters:
  - name: env
    default: ${{ parameters.notARef }}
steps:
  - script: echo ${{ parameters.env }} ${{ parameters['dash-name'] }}
  - ${{ each s in parameters.steps }}:
    - ${{ s.parameters.inner }}
  - script: echo parameters.plainText $[ parameters.runtime ]
  # ${{ parameters.commented }}
  - ${{ if eq(parameters.sdl.credscan, true) }}:
    - script: x
`)
	var got []string
	for _, r := range FindParamRefs(ls) {
		if ls[r.Line][r.NameStart:r.NameEnd] != r.Name {
			t.Errorf("range of %q slices %q", r.Name, ls[r.Line][r.NameStart:r.NameEnd])
		}
		got = append(got, fmt.Sprintf("%s@%d", r.Name, r.Line))
	}
	want := []string{"env@4", "dash-name@4", "steps@5", "sdl@9"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("want %v got %v", want, got)
	}
}

package pipeline

import (
	"reflect"
	"testing"
)

func TestParseParametersInlineOnly(t *testing.T) {
	ls := lines(`
parameters:
- { name: environment, type: string, default: 'dev', values: ['dev', 'test', 'prod'] }
- { name: runTests, type: boolean, default: true }  # trailing comment
- { name: 'label', displayName: "Build label" }
steps: []
`)
	ps := ParseParameters(ls)
	if len(ps) != 3 {
		t.Fatalf("want 3 params, got %+v", ps)
	}
	env := ps[0]
	if env.Name != "environment" || env.Type != "string" || env.Required || env.Default != "'dev'" || !env.Inline ||
		env.Line != 1 || env.NameCol != 10 {
		t.Errorf("environment: %+v", env)
	}
	if !reflect.DeepEqual(env.Values, []string{"dev", "test", "prod"}) {
		t.Errorf("values: %v", env.Values)
	}
	if got := ls[1][env.DefaultCol:env.DefaultEnd]; env.DefaultLine != 1 || got != "'dev'" {
		t.Errorf("default position %d:%d-%d = %q", env.DefaultLine, env.DefaultCol, env.DefaultEnd, got)
	}
	if ps[1].Name != "runTests" || ps[1].Type != "boolean" || ps[1].Default != "true" {
		t.Errorf("runTests: %+v", ps[1])
	}
	if l := ps[2]; l.Name != "label" || ls[3][l.NameCol:l.NameCol+len(l.Name)] != "label" || !l.Required ||
		l.DisplayName != "Build label" || l.Type != "string" {
		t.Errorf("label: %+v", l)
	}
	if DefaultKeyLine(ls, ps[2]) != -1 || DefaultKeyLine(ls, ps[1]) != 2 {
		t.Errorf("default key lines: %d %d", DefaultKeyLine(ls, ps[2]), DefaultKeyLine(ls, ps[1]))
	}
}

func TestParseParametersInlineAndBlockMixed(t *testing.T) {
	ps := ParseParameters(lines(`
parameters:
- { name: environment, type: string, default: 'dev', values: ['dev', 'test', 'prod'] }
- { name: runTests, type: boolean, default: true }
- name: region
  type: string
  default: 'westeurope'
- {name: last, type: number}
`))
	var names []string
	for _, p := range ps {
		names = append(names, p.Name)
	}
	if !reflect.DeepEqual(names, []string{"environment", "runTests", "region", "last"}) {
		t.Fatalf("names: %v", names)
	}
	if ps[2].Inline || ps[2].Default != "'westeurope'" || !ps[3].Inline || !ps[3].Required || ps[3].Type != "number" {
		t.Errorf("params: %+v", ps)
	}
}

func TestParseParametersInlineSpanningLines(t *testing.T) {
	ls := lines(`
parameters:
  - { name: tier, type: string,
      default: 'basic', values: ['basic',
                                 'premium'] }
  - { name: settings, type: object, default: { size: 3, zone: 'a' } }
  - { name: after, type: string }
steps: []
`)
	ps := ParseParameters(ls)
	if len(ps) != 3 {
		t.Fatalf("want 3 params, got %+v", ps)
	}
	tier := ps[0]
	if tier.Name != "tier" || tier.Line != 1 || tier.Default != "'basic'" || tier.DefaultLine != 2 ||
		ls[2][tier.DefaultCol:tier.DefaultEnd] != "'basic'" || !reflect.DeepEqual(tier.Values, []string{"basic", "premium"}) {
		t.Errorf("tier: %+v", tier)
	}
	s := ps[1]
	if s.Type != "object" || s.Default != "{ size: 3, zone: 'a' }" || DefaultKeyLine(ls, s) != 4 || ls[4][s.DefaultCol] != '{' {
		t.Errorf("settings: %+v", s)
	}
	b := Bindings{}
	ObjectMembers(ls, DefaultKeyLine(ls, s), s.DefaultCol, "settings", b)
	if len(b) != 2 {
		t.Errorf("object default members: %v", b)
	}
	if ps[2].Name != "after" || ps[2].Line != 5 {
		t.Errorf("after: %+v", ps[2])
	}
}

package pipeline

import "testing"

func TestEvalCondition(t *testing.T) {
	b := Bindings{"enableextra": BoolValue(false), "env": StringValue("Prod"), "n": NumberValue(2), "empty": StringValue("")}
	cases := map[string]Tri{
		"eq(parameters.enableExtra, true)":            False,
		"eq(parameters['enableExtra'], false)":        True,
		"parameters.enableExtra":                      False,
		"not(parameters.enableExtra)":                 True,
		"eq(parameters.env, 'prod')":                  True,
		"ne(parameters.env, 'prod')":                  False,
		"in(parameters.env, 'dev', 'PROD')":           True,
		"notIn(parameters.env, 'dev', 'test')":        True,
		"startsWith(parameters.env, 'pr')":            True,
		"eq(parameters.n, 2)":                         True,
		"parameters.empty":                            False,
		"and(parameters.enableExtra, variables.x)":    False,
		"or(parameters.enableExtra, variables.x)":     Unknown,
		"or(eq(parameters.env, 'prod'), variables.x)": True,
		"eq(variables['Build.Reason'], 'Manual')":     Unknown,
		"eq(parameters.missing, true)":                Unknown,
		"eq(parameters.enableExtra, 'false')":         Unknown, // a string converted to a boolean: not guessed
		"coalesce(parameters.env, 'x')":               Unknown,
		"eq(parameters.env, 'it''s')":                 False,
		"eq(parameters.env":                           Unknown,
		"true":                                        True,
	}
	for expr, want := range cases {
		if got := EvalCondition(expr, b); got != want {
			t.Errorf("%s: got %v, want %v", expr, got, want)
		}
	}
	if n, ok := ForwardedParam("${{ parameters.enableExtra }}"); !ok || n != "enableExtra" {
		t.Errorf("forwarded: %q %v", n, ok)
	}
	if _, ok := ForwardedParam("${{ parameters.a.b }}"); ok {
		t.Error("a property is not the parameter")
	}
	if v, ok := ParseLiteral("boolean", "False"); !ok || v != BoolValue(false) {
		t.Errorf("literal: %+v", v)
	}
	if _, ok := ParseLiteral("boolean", "${{ parameters.x }}"); ok {
		t.Error("an expression is not a literal")
	}
}

func TestConditionsLive(t *testing.T) {
	ls := lines(`
steps:
- ${{ if eq(parameters.a, true) }}:
  - script: a
- ${{ elseif eq(parameters.b, true) }}:
  - script: b
- ${{ else }}:
  - script: c
  - ${{ if parameters.d }}:
    - script: d
- script: always
- script: |
    ${{ if false }}:
      text
`)
	c := FindConditions(ls)
	if len(c.Branches) != 4 {
		t.Fatalf("branches: %+v", c.Branches)
	}
	type want struct{ a, b, c, d, always Tri }
	check := func(b Bindings, w want) {
		t.Helper()
		got := want{c.Live(2, b), c.Live(4, b), c.Live(6, b), c.Live(8, b), c.Live(9, b)}
		if got != w {
			t.Errorf("%v: got %+v, want %+v", b, got, w)
		}
	}
	T, F, U := True, False, Unknown
	check(Bindings{"a": BoolValue(true)}, want{T, F, F, F, T})
	check(Bindings{"a": BoolValue(false), "b": BoolValue(true)}, want{F, T, F, F, T})
	check(Bindings{"a": BoolValue(false), "b": BoolValue(false), "d": BoolValue(true)}, want{F, F, T, T, T})
	check(Bindings{"a": BoolValue(false)}, want{F, U, U, U, T})
	check(Bindings{}, want{U, U, U, U, T})
	if g := c.Guard(8, Bindings{"a": BoolValue(false), "b": BoolValue(false)}); g != 7 {
		t.Errorf("guard: %d", g)
	}
	if c.Live(12, Bindings{}) != True {
		t.Error("a script body is text")
	}

	mapping := FindConditions(lines(`
extends:
  ${{ if eq(parameters.x, 'a') }}:
    template: a.yml
  ${{ else }}:
    template: b.yml
`))
	if l := mapping.Live(4, Bindings{"x": StringValue("a")}); l != False {
		t.Errorf("else of a taken if: %v", l)
	}
}

func TestObjectMembers(t *testing.T) {
	ls := lines(`
sdl:
  credscan: true
  level: 3
  name: 'x' # comment
  nested:
    deep: false
  list:
    - a
  ${{ if true }}:
    cond: true
flow: { a: 1, b: { c: 'two' }, d: ${{ variables.x }} }
`)
	b := Bindings{}
	ObjectMembers(ls, 0, len(ls[0]), "sdl", b)
	ObjectMembers(ls, 10, 6, "flow", b)
	want := Bindings{"sdl.credscan": BoolValue(true), "sdl.level": NumberValue(3), "sdl.name": StringValue("x"),
		"sdl.nested.deep": BoolValue(false), "flow.a": NumberValue(1), "flow.b.c": StringValue("two")}
	if b.Key() != want.Key() {
		t.Errorf("got %s\nwant %s", b.Key(), want.Key())
	}
	if EvalCondition("eq(parameters.sdl.nested.deep, false)", b) != True || EvalCondition("parameters.sdl.cond", b) != Unknown {
		t.Error("member paths")
	}
}

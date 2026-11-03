package pipeline

import (
	"reflect"
	"testing"
)

func TestBlockScalarMask(t *testing.T) {
	lines := SplitLines(`steps:
  - script: |
      echo hi
      template: not-a-template.yml

      echo done
  - bash: >-
      template: folded.yml
  - template: real.yml
  - powershell: |2
      parameters:
  -   |
    template: in-a-list-item.yml
  - script: echo a | grep b
  - template: also-real.yml`)
	mask := BlockScalarMask(lines)
	want := map[int]bool{2: true, 3: true, 4: true, 5: true, 7: true, 10: true, 12: true}
	for i := range lines {
		if mask[i] != want[i] {
			t.Errorf("line %d %q: mask=%v, want %v", i, lines[i], mask[i], want[i])
		}
	}
}

func TestIsBlockIndicator(t *testing.T) {
	for _, s := range []string{"|", ">", "|-", "|+", ">-", "|2", "|-2", "|2-", ">+1"} {
		if !isBlockIndicator(s) {
			t.Errorf("%q should be a block indicator", s)
		}
	}
	for _, s := range []string{"", "||", "|x", "|0", "|--", "a", "|-2-"} {
		if isBlockIndicator(s) {
			t.Errorf("%q should not be a block indicator", s)
		}
	}
}

func TestFindTemplateRefsSkipsScriptBodies(t *testing.T) {
	lines := SplitLines(`steps:
  - script: |
      echo hi
      template: not-a-template.yml
  - template: real.yml`)
	refs := FindTemplateRefs(lines)
	if len(refs) != 1 || refs[0].Ref != "real.yml" || refs[0].Line != 4 {
		t.Fatalf("refs = %+v", refs)
	}
}

func TestOutlineAndVariablesSkipScriptBodies(t *testing.T) {
	lines := SplitLines(`steps:
  - script: |
      variables:
        fake: 1
      - task: Fake@1
  - task: Real@1`)
	syms := Outline(lines)
	if len(syms) != 1 || len(syms[0].Children) != 2 {
		t.Fatalf("outline = %s", dumpSymbols(syms, 0))
	}
	if got := syms[0].Children[0].EndLine; got != 4 {
		t.Errorf("script step EndLine = %d, want 4 (covers its body)", got)
	}
	if vs := ParseVariables(lines); len(vs.Order) != 0 {
		t.Errorf("variables parsed from a script body: %v", vs.Order)
	}
}

func TestParseFlowMapping(t *testing.T) {
	lines := SplitLines(`  parameters: { env: prod, count: 3, 'quoted key': "a, b: c", list: [a, b], expr: ${{ coalesce(parameters.a, 'x') }}, empty: }`)
	col := len("  parameters: ")
	fm := ParseFlowMapping(lines, 0, col, 2)
	if !fm.Closed {
		t.Fatal("not closed")
	}
	var got [][2]string
	for _, e := range fm.Entries {
		got = append(got, [2]string{e.Key, e.Value})
	}
	want := [][2]string{{"env", "prod"}, {"count", "3"}, {"quoted key", `"a, b: c"`}, {"list", "[a, b]"},
		{"expr", "${{ coalesce(parameters.a, 'x') }}"}, {"empty", ""}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("entries = %q\nwant      %q", got, want)
	}
	env := fm.Entries[0]
	if lines[0][env.KeyPos.Col:env.KeyPos.Col+3] != "env" || lines[0][env.ValuePos.Col:env.End.Col] != "prod" {
		t.Errorf("env positions wrong: %+v", env)
	}
}

func TestParseFlowMappingMultiLineAndUnterminated(t *testing.T) {
	lines := SplitLines(`- template: t.yml
  parameters: {
    env: prod,   # comment, with comma
    count: 3
  }
- script: echo`)
	fm := ParseFlowMapping(lines, 1, len("  parameters: "), 2)
	if !fm.Closed || fm.Close != (Pos{4, 2}) || len(fm.Entries) != 2 {
		t.Fatalf("fm = %+v", fm)
	}
	if fm.Entries[1].KeyPos != (Pos{3, 4}) || fm.Entries[1].Value != "3" {
		t.Errorf("count entry = %+v", fm.Entries[1])
	}

	open := SplitLines(`- template: t.yml
  parameters: { env: prod,
- script: echo`)
	fm = ParseFlowMapping(open, 1, len("  parameters: "), 2)
	if fm.Closed || len(fm.Entries) != 1 || fm.Entries[0].Key != "env" {
		t.Fatalf("unterminated fm = %+v", fm)
	}
}

func TestParseCallSiteFlow(t *testing.T) {
	lines := SplitLines(`steps:
  - template: t.yml
    parameters: { env: prod, count: 3 }   # trailing comment
  - script: echo`)
	cs := ParseCallSite(lines, 1)
	if cs.Flow == nil || !cs.Flow.Closed || !reflect.DeepEqual(cs.Order, []string{"env", "count"}) {
		t.Fatalf("cs = %+v", cs)
	}
	env, count := cs.Passed["env"], cs.Passed["count"]
	if env.Value != "prod" || lines[2][env.ValueCol:env.ValueEnd] != "prod" || !env.Flow {
		t.Errorf("env = %+v", env)
	}
	// deleting `env` removes "env: prod, "; deleting the last entry removes ", count: 3"
	if lines[2][env.DelStart.Col:env.DelEnd.Col] != "env: prod, " {
		t.Errorf("env deletion = %q", lines[2][env.DelStart.Col:env.DelEnd.Col])
	}
	if lines[2][count.DelStart.Col:count.DelEnd.Col] != ", count: 3" {
		t.Errorf("count deletion = %q", lines[2][count.DelStart.Col:count.DelEnd.Col])
	}
	if cs.LastLine != 2 {
		t.Errorf("LastLine = %d", cs.LastLine)
	}
}

func TestParseCallSiteForwarded(t *testing.T) {
	for _, src := range []string{
		"- template: t.yml\n  parameters: ${{ parameters }}",
		"extends:\n  parameters: ${{ parameters.inner }}\n  template: t.yml",
	} {
		lines := SplitLines(src)
		tl := 0
		for i, l := range lines {
			if _, ok := TemplateRefOnLine(l, i); ok {
				tl = i
			}
		}
		cs := ParseCallSite(lines, tl)
		if !cs.Forwarded || !cs.Unknowable() {
			t.Errorf("%q: not forwarded: %+v", src, cs)
		}
	}
}

func TestParseCallSiteBlockValueEndExcludesComment(t *testing.T) {
	lines := SplitLines("- template: t.yml\n  parameters:\n    env: prod   # the env")
	pp := ParseCallSite(lines, 0).Passed["env"]
	if lines[2][pp.ValueCol:pp.ValueEnd] != "prod" || pp.ValueLine != 2 {
		t.Errorf("pp = %+v", pp)
	}
}

func dumpSymbols(syms []*Symbol, depth int) string {
	out := ""
	for _, s := range syms {
		for i := 0; i < depth; i++ {
			out += "  "
		}
		out += s.Kind + " " + s.Name + "\n" + dumpSymbols(s.Children, depth+1)
	}
	return out
}

func TestPassedParamAt(t *testing.T) {
	lines := SplitLines(`steps:
  - template: t.yml
    parameters: { env: prod, count: 3 }
  - template: u.yml
    parameters: {
      region: west,
      size: 2
    }
  - template: v.yml
    parameters:
      block: yes`)
	cases := []struct {
		line, col int
		tl        int
		name      string
	}{
		{2, len("    parameters: { e"), 1, "env"},
		{2, len("    parameters: { env: pr"), 1, "env"},
		{2, len("    parameters: { env: prod, cou"), 1, "count"},
		{5, len("      reg"), 3, "region"},
		{6, len("      size: 2"), 3, "size"},
		{10, len("      bl"), 8, "block"},
	}
	for _, c := range cases {
		tl, pp, ok := PassedParamAt(lines, c.line, c.col)
		if !ok || tl != c.tl || pp.Name != c.name {
			t.Errorf("(%d,%d): got tl=%d %q ok=%v, want tl=%d %q", c.line, c.col, tl, pp.Name, ok, c.tl, c.name)
		}
	}
	if _, _, ok := PassedParamAt(lines, 2, len("    param")); ok {
		t.Error("cursor on the parameters key should not match an entry")
	}
	if _, _, ok := PassedParamAt(lines, 0, 2); ok {
		t.Error("steps: matched")
	}
}

func TestLooksLikeAzurePipelines(t *testing.T) {
	yes := []string{
		"trigger: none\nsteps:\n  - script: echo",
		"variables:\n  a: 1",
		"${{ if eq(parameters.x, 'y') }}:\n  steps: []",
		"parameters:\n  - name: a\njobs: []",
	}
	no := []string{
		"apiVersion: v1\nkind: ConfigMap\ndata: {}",
		"on: push\njobs:\n  build:\n    runs-on: ubuntu-latest",
		"services:\n  web:\n    image: nginx",
		"name: env\nchannels: [conda-forge]\ndependencies: []",
		"",
	}
	for _, s := range yes {
		if !LooksLikeAzurePipelines(SplitLines(s)) {
			t.Errorf("should be Azure Pipelines: %q", s)
		}
	}
	for _, s := range no {
		if LooksLikeAzurePipelines(SplitLines(s)) {
			t.Errorf("should not be Azure Pipelines: %q", s)
		}
	}
}

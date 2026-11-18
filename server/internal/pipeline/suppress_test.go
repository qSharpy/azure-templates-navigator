package pipeline

import (
	"reflect"
	"testing"
)

func TestFindSuppressions(t *testing.T) {
	ls := lines(`
# atn-ignore-file type-mismatch
parameters:
# kept for old callers
# atn-ignore unused-param
- name: legacy
  default: x
- name: other # atn-ignore: unused-param, undeclared-param
  default: y
# atn-ignore unknown-param

- name: gap
steps:
- script: |
    # atn-ignore unused-param
    echo hi
- script: echo # atn-ignore
- script: echo # not atn-ignore unused-param
`)
	s := FindSuppressions(ls)
	want := map[int]map[string]bool{
		4: {"unused-param": true},
		6: {"unused-param": true, "undeclared-param": true},
	}
	if !reflect.DeepEqual(s.Lines, want) {
		t.Errorf("lines: %v", s.Lines)
	}
	if !reflect.DeepEqual(s.File, map[string]bool{"type-mismatch": true}) {
		t.Errorf("file: %v", s.File)
	}
	if !s.Ignores(4, "unused-param") || s.Ignores(4, "unknown-param") || !s.Ignores(99, "type-mismatch") {
		t.Error("Ignores")
	}
}

package pipeline

import (
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestUnknownFunctions(t *testing.T) {
	lines := SplitLines(`steps:
- script: echo ${{ add(1, 2) }} ${{ format('{0}', parameters.x) }}
- ${{ if eq(parameters.a, 'lenght(x)') }}:
  - script: |
      echo ${{ lenght(parameters.list) }}
- ${{ elseif ne(1, 2) }}:
  - script: echo ${{ Length(parameters.list) }} ${{ coalesce(variables.a, 'b') }}
- ${{ each x in parameters.items }}:
  - script: echo ${{ x.run() }}
# ${{ commented(1) }}
- script: echo ${{ parameters.x }} # ${{ trailing(1) }}
- ${{ if(eq(1, 1)) }}:
  - script: echo
- ${{ if succeeded() }}:
  - script: echo $[ counter('a', 1) ]
`)
	var got []string
	for _, c := range UnknownFunctions(lines) {
		got = append(got, c.Name+"@"+strconv.Itoa(c.Line)+":"+lines[c.Line][c.Col:c.End])
	}
	want := []string{"add@1:add", "lenght@4:lenght", "succeeded@13:succeeded"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func cycleSummary(lines []string) []string {
	var out []string
	for _, e := range DependencyCycles(lines) {
		s := e.Owner.Name + "->" + e.On
		if e.Implicit {
			s += " (implicit)"
		}
		out = append(out, s+" ["+strings.Join(e.Cycle, " → ")+"] "+lines[e.Line][e.Col:e.End])
	}
	return out
}

func TestDependencyCyclesStages(t *testing.T) {
	lines := SplitLines(`stages:
- stage: A
  jobs: []
- stage: B
  dependsOn: [A, C]
  jobs: []
- stage: C
  dependsOn:
  - B
  jobs: []
`)
	want := []string{"B->C [B → C → B] C", "C->B [C → B → C] B"}
	if got := cycleSummary(lines); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestDependencyCyclesSelfAndJobsPerStage(t *testing.T) {
	lines := SplitLines(`stages:
- stage: One
  jobs:
  - job: b
    dependsOn: c
  - job: c
- stage: Two
  dependsOn: One
  jobs:
  - job: c
    dependsOn: b
  - job: b
  - job: me
    dependsOn: me
`)
	// b→c in One and c→b in Two are different jobs: no cycle.
	want := []string{"me->me [me → me] me"}
	if got := cycleSummary(lines); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestDependencyCyclesImplicitPreviousStage(t *testing.T) {
	lines := SplitLines(`stages:
- stage: A
  dependsOn: B
  jobs: []
- stage: B
  jobs: []
`)
	want := []string{"A->B [A → B → A] B", "B->A (implicit) [B → A → B] B"}
	if got := cycleSummary(lines); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestDependencyCyclesLeavesUncertainItemsAlone(t *testing.T) {
	lines := SplitLines(`stages:
- stage: A
  dependsOn: B
  jobs: []
- template: stages/more.yml
- stage: B
  jobs: []
- ${{ if eq(parameters.x, true) }}:
  - stage: C
    dependsOn: D
    jobs: []
- ${{ else }}:
  - stage: D
    dependsOn: C
    jobs: []
- stage: E
  dependsOn: []
  jobs: []
- stage: F
  dependsOn: E
  jobs:
  - job: x
    steps:
    - script: |
        - job: y
          dependsOn: y
`)
	if got := cycleSummary(lines); len(got) != 0 {
		t.Fatalf("expected no cycles, got %v", got)
	}
}

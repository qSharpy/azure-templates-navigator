package pipeline

import (
	"fmt"
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

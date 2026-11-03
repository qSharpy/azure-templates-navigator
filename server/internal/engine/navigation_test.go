package engine

import (
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// samplesEngine indexes the repository's samples/ folder, whose extends/
// tree models platform teams publishing templates and app teams extending them.
func samplesEngine(t *testing.T) (string, *Engine) {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(file), "..", "..", "..", "samples")
	root, _ = filepath.Abs(root)
	s := DefaultSettings()
	s.Repositories.AutoFetch = false
	s.Repositories.PreferLocalClones = false
	s.Repositories.CacheDir = filepath.Join(t.TempDir(), "cache")
	e := New([]string{root}, s)
	e.Rescan()
	return root, e
}

func rels(root string, us []Usage) []string {
	var out []string
	for _, u := range us {
		r, _ := filepath.Rel(root, u.Path)
		out = append(out, filepath.ToSlash(r)+":"+strconv.Itoa(u.Line+1))
	}
	return out
}

func TestTemplateUsagesAcrossTeams(t *testing.T) {
	root, e := samplesEngine(t)
	base := filepath.Join(root, "extends/platform-templates/pipelines/secure-base.yml")
	res := e.References(base, 0, 0, false)
	got := rels(root, res.Usages)
	// dotnet-service.yml (golden path), the smoke test, and the three teams
	// that extend it directly
	for _, want := range []string{
		"extends/platform-templates/pipelines/dotnet-service.yml",
		"extends/platform-templates/tests/secure-base-smoke.yml",
		"extends/teams/search/azure-pipelines.yml",
		"extends/teams/mobile/release.yml",
	} {
		found := false
		for _, g := range got {
			if strings.HasPrefix(g, want+":") {
				found = true
			}
		}
		if !found {
			t.Errorf("missing usage in %s; got %v", want, got)
		}
	}
	// dotnet-service.yml has a top-level extends: but is a template (the
	// teams' pipelines reference it), so it is not counted as a pipeline
	lenses := e.CodeLenses(base)
	want := strconv.Itoa(len(res.Usages)) + " usages · " + strconv.Itoa(len(res.Usages)-1) + " pipelines"
	if len(lenses) != 1 || lenses[0].Title != want {
		t.Errorf("lens %+v, want %q", lenses, want)
	}
}

func TestParameterUsages(t *testing.T) {
	root, e := samplesEngine(t)
	deploy := filepath.Join(root, "extends/platform-templates/stages/deploy-webapp.yml")
	f := e.file(deploy)
	var declLine int
	for _, p := range f.params {
		if p.Name == "environment" {
			declLine = p.Line
		}
	}
	res := e.References(deploy, declLine, 12, true)
	if !strings.Contains(res.Title, "'environment'") {
		t.Errorf("title %q", res.Title)
	}
	var decl, body, sites int
	for _, u := range res.Usages {
		switch {
		case u.Declaration:
			decl++
		case u.Path == deploy:
			body++
		default:
			sites++
		}
	}
	// declared once; used 3 times in the body (stage name, display name,
	// environment); passed by dotnet-service, search and mobile
	if decl != 1 || body != 3 || sites != 3 {
		t.Errorf("decl=%d body=%d sites=%d: %v", decl, body, sites, rels(root, res.Usages))
	}

	// the same search starting from a call site
	mobile := filepath.Join(root, "extends/teams/mobile/release.yml")
	mf := e.file(mobile)
	for i, l := range mf.lines {
		if strings.Contains(l, "environment: ${{ parameters.environment }}") {
			r2 := e.References(mobile, i, strings.Index(l, "environment")+1, true)
			if len(r2.Usages) != len(res.Usages) {
				t.Errorf("from call site: %d usages, want %d", len(r2.Usages), len(res.Usages))
			}
		}
	}
}

func TestCallHierarchy(t *testing.T) {
	root, e := samplesEngine(t)
	golden := filepath.Join(root, "extends/platform-templates/pipelines/dotnet-service.yml")
	var out []string
	for _, c := range e.OutgoingCalls(golden) {
		out = append(out, c.Item.Name)
	}
	if strings.Join(out, ",") != "secure-base.yml,build-dotnet.yml,deploy-webapp.yml" {
		t.Errorf("outgoing: %v", out)
	}
	// payments and data extend it; data/nightly.yml misspells the alias
	// ('@platfrom') so it does not resolve and is not a caller
	var in []string
	for _, c := range e.IncomingCalls(golden) {
		r, _ := filepath.Rel(root, c.Item.Path)
		in = append(in, filepath.ToSlash(r)+"("+c.Item.Kind+")")
	}
	if got := strings.Join(in, ","); got != "extends/teams/data/azure-pipelines.yml(pipeline),extends/teams/payments/azure-pipelines.yml(pipeline)" {
		t.Errorf("incoming: %v", got)
	}
	// prepare on a template: line returns the referenced template
	payments := filepath.Join(root, "extends/teams/payments/azure-pipelines.yml")
	pf := e.file(payments)
	for i, l := range pf.lines {
		if strings.Contains(l, "template: pipelines/dotnet-service.yml@platform") {
			if it, _ := e.PrepareHierarchy(payments, i); it.Path != golden {
				t.Errorf("prepare: %+v", it)
			}
		}
	}
}

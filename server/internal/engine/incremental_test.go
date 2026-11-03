package engine

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// indexState is everything an incremental re-link must agree on with a
// full rebuild.
type indexState struct {
	Diags   map[string][]Diagnostic
	Forward map[string][]string
	Reverse map[string][]string
	Aliases map[string]string
}

func stateOf(e *Engine) indexState {
	diags := e.DiagnoseAll()
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ensureLinked()
	st := indexState{Diags: map[string][]Diagnostic{}, Forward: map[string][]string{}, Reverse: map[string][]string{}, Aliases: map[string]string{}}
	for p, ds := range diags {
		if len(ds) > 0 {
			st.Diags[p] = ds
		}
	}
	for p, eds := range e.forward {
		var ts []string
		for _, ed := range eds {
			ts = append(ts, ed.ref.Ref+"→"+ed.target+"|"+string(ed.res.Kind))
		}
		if len(ts) > 0 {
			st.Forward[p] = ts
		}
	}
	for t, srcs := range e.reverse {
		var ss []string
		for s := range srcs {
			ss = append(ss, s)
		}
		sort.Strings(ss)
		if len(ss) > 0 {
			st.Reverse[t] = ss
		}
	}
	for a, d := range e.aliases {
		st.Aliases[a] = d.Name + "@" + d.Ref + "#" + e.aliasFrom[a]
	}
	return st
}

// fresh builds a new engine over the same disk state and open documents.
func fresh(t *testing.T, root string, open map[string]string) *Engine {
	t.Helper()
	s := DefaultSettings()
	s.Repositories.AutoFetch = false
	s.Repositories.CacheDir = filepath.Join(root, ".cache-unused")
	e := New([]string{root}, s)
	e.Rescan()
	for p, txt := range open {
		e.OpenDocument(p, txt)
	}
	return e
}

func TestIncrementalRelinkMatchesFullRebuild(t *testing.T) {
	root, e := newWorkspace(t)
	e.SetSettings(func() Settings {
		s := e.Settings()
		s.Repositories.CacheDir = filepath.Join(root, ".cache-unused")
		return s
	}())
	open := map[string]string{}
	pipe := filepath.Join(root, "azure-pipelines.yml")
	build := filepath.Join(root, "templates/build.yml")
	newT := filepath.Join(root, "templates/nope.yml")
	consumer := filepath.Join(root, "consumer.yml")

	steps := []struct {
		name string
		do   func()
	}{
		{"open pipeline unchanged", func() { open[pipe] = mustRead(t, pipe); e.OpenDocument(pipe, open[pipe]) }},
		{"type in a comment", func() { open[pipe] = "# hello\n" + open[pipe]; e.OpenDocument(pipe, open[pipe]) }},
		{"break a reference", func() {
			open[pipe] = strings.Replace(open[pipe], "templates/build.yml", "templates/biuld.yml", 1)
			e.OpenDocument(pipe, open[pipe])
		}},
		{"template gains a required parameter", func() {
			open[build] = strings.Replace(buildTemplate[1:], "parameters:\n", "parameters:\n  - name: extra\n    type: string\n", 1)
			e.OpenDocument(build, open[build])
		}},
		{"missing template is created on disk", func() {
			write(t, root, "templates/nope.yml", "steps:\n  - script: echo\n")
			e.FileChanged(newT)
		}},
		{"new consumer declares a repository", func() {
			write(t, root, "consumer.yml", "trigger: none\nresources:\n  repositories:\n    - repository: tools\n      type: github\n      name: org/tools\nsteps:\n  - template: x.yml@tools\n")
			e.FileChanged(consumer)
		}},
		{"alias moves to another line", func() {
			write(t, root, "consumer.yml", "trigger: none\n\n\nresources:\n  repositories:\n    - repository: tools\n      type: github\n      name: org/tools\nsteps:\n  - template: x.yml@tools\n")
			e.FileChanged(consumer)
		}},
		{"alias changes ref", func() {
			write(t, root, "consumer.yml", "trigger: none\nresources:\n  repositories:\n    - repository: tools\n      type: github\n      name: org/tools\n      ref: refs/heads/dev\nsteps:\n  - template: x.yml@tools\n")
			e.FileChanged(consumer)
		}},
		{"template deleted", func() {
			_ = os.Remove(newT)
			e.FileChanged(newT)
		}},
		{"close the edited template", func() { delete(open, build); e.CloseDocument(build) }},
	}
	for _, st := range steps {
		st.do()
		got, want := stateOf(e), stateOf(fresh(t, root, open))
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("after %q the incremental index differs from a full rebuild:\n got  %+v\n want %+v", st.name, got, want)
		}
	}
}

func TestChangedDiagnosticsOnlyAffectedFiles(t *testing.T) {
	root, e := newWorkspace(t)
	write(t, root, "unrelated.yml", "steps:\n  - script: echo\n")
	e.Rescan()
	if d := e.ChangedDiagnostics(); !d.Full {
		t.Fatal("first delta should be full")
	}
	if d := e.ChangedDiagnostics(); len(d.Files) != 0 || d.Full {
		t.Fatalf("nothing changed, got %+v", d)
	}
	v0 := e.ChangedDiagnostics().Version

	build := filepath.Join(root, "templates/build.yml")
	e.OpenDocument(build, strings.Replace(buildTemplate[1:], "parameters:\n", "parameters:\n  - name: extra\n    type: string\n", 1))
	d := e.ChangedDiagnostics()
	var got []string
	for p := range d.Files {
		got = append(got, e.relPath(p))
	}
	sort.Strings(got)
	want := []string{"azure-pipelines.yml", "templates/build.yml", "templates/wrapper.yml"}
	if d.Full || !reflect.DeepEqual(got, want) {
		t.Errorf("changed = %v (full=%v), want %v", got, d.Full, want)
	}
	if d.Version == v0 {
		t.Error("parameter change did not bump the index version")
	}
	v1 := d.Version

	// editing a script body changes nothing other files or views depend on
	e.OpenDocument(filepath.Join(root, "unrelated.yml"), "steps:\n  - script: echo changed\n")
	d = e.ChangedDiagnostics()
	if len(d.Files) != 1 || d.Full {
		t.Errorf("unexpected delta %+v", d)
	}
	if d.Version != v1 {
		t.Errorf("version moved on an edit that changed nothing: %d -> %d", v1, d.Version)
	}
}

func TestNonPipelineYAMLIsNotIndexed(t *testing.T) {
	root, e := newWorkspace(t)
	write(t, root, "k8s/deploy.yml", "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: x\nspec:\n  template:\n    spec: {}\n")
	write(t, root, ".github/workflows/ci.yml", "on: push\njobs:\n  build:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo ${{ inputs.x }}\n")
	write(t, root, "docker-compose.yml", "services:\n  web:\n    image: nginx\n")
	e.Rescan()
	for _, p := range e.WorkspaceFiles() {
		rel := e.relPath(p)
		if strings.HasPrefix(rel, "k8s/") || strings.HasPrefix(rel, ".github/") || rel == "docker-compose.yml" {
			t.Errorf("%s was indexed", rel)
		}
	}
	if ds := e.Diagnose(filepath.Join(root, "k8s/deploy.yml")); len(ds) != 0 {
		t.Errorf("k8s manifest diagnostics: %+v", ds)
	}
}

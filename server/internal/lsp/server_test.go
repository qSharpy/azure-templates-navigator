package lsp

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type client struct {
	t     *testing.T
	c     *conn
	id    int
	notes chan *message
	resps map[int]chan *message
	in    chan *message
}

func startServer(t *testing.T) *client {
	sr, cw := io.Pipe() // client -> server
	cr, sw := io.Pipe() // server -> client
	go func() { _ = Serve(sr, sw) }()
	cl := &client{t: t, c: &conn{r: bufio.NewReader(cr), w: cw}, notes: make(chan *message, 1000), in: make(chan *message, 1000)}
	go func() {
		for {
			m, err := cl.c.read()
			if err != nil {
				return
			}
			if m.Method != "" {
				cl.notes <- m
			} else {
				cl.in <- m
			}
		}
	}()
	return cl
}

func (cl *client) call(method string, params interface{}, out interface{}) {
	cl.t.Helper()
	cl.id++
	if err := cl.c.request(cl.id, method, params); err != nil {
		cl.t.Fatal(err)
	}
	select {
	case m := <-cl.in:
		if m.Error != nil {
			cl.t.Fatalf("%s: %s", method, m.Error.Message)
		}
		if out != nil {
			if err := json.Unmarshal(m.Result, out); err != nil {
				cl.t.Fatalf("%s: %v (%s)", method, err, m.Result)
			}
		}
	case <-time.After(10 * time.Second):
		cl.t.Fatalf("%s: timeout", method)
	}
}

func (cl *client) waitNote(method string, pred func(*message) bool) *message {
	cl.t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case m := <-cl.notes:
			if m.Method == method && (pred == nil || pred(m)) {
				return m
			}
		case <-deadline:
			cl.t.Fatalf("timeout waiting for %s", method)
		}
	}
}

func TestLSPRoundTrip(t *testing.T) {
	root := t.TempDir()
	_ = os.Mkdir(filepath.Join(root, ".git"), 0o755)
	_ = os.MkdirAll(filepath.Join(root, "t"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "t", "a.yml"), []byte("parameters:\n  - name: émoji\n  - name: b\n    default: 1\nsteps:\n  - script: echo ${{ parameters.émoji }} ${{ parameters.b }}\n"), 0o644)
	pipe := "trigger: none\nsteps:\n  - template: t/a.yml\n    parameters:\n      b: 2\n"
	pipePath := filepath.Join(root, "p.yml")
	_ = os.WriteFile(pipePath, []byte(pipe), 0o644)

	cl := startServer(t)
	var initRes map[string]interface{}
	cl.call("initialize", map[string]interface{}{
		"rootUri":               pathToURI(root),
		"capabilities":          map[string]interface{}{},
		"initializationOptions": map[string]interface{}{"settings": map[string]interface{}{"repositories": map[string]interface{}{"autoFetch": false}}},
	}, &initRes)
	if initRes["capabilities"] == nil {
		t.Fatal("no capabilities")
	}
	_ = cl.c.notify("initialized", map[string]interface{}{})
	uri := pathToURI(pipePath)
	_ = cl.c.notify("textDocument/didOpen", map[string]interface{}{"textDocument": map[string]interface{}{"uri": uri, "languageId": "yaml", "version": 1, "text": pipe}})

	m := cl.waitNote("textDocument/publishDiagnostics", func(m *message) bool { return strings.Contains(string(m.Params), "p.yml") })
	if !strings.Contains(string(m.Params), "missing-required-param") {
		t.Errorf("diagnostics: %s", m.Params)
	}

	var hover struct {
		Contents struct{ Value string }
	}
	cl.call("textDocument/hover", map[string]interface{}{"textDocument": map[string]string{"uri": uri}, "position": map[string]int{"line": 2, "character": 8}}, &hover)
	if !strings.Contains(hover.Contents.Value, "émoji") {
		t.Errorf("hover: %+v", hover)
	}

	var actions []struct {
		Title string
		Edit  struct {
			Changes map[string][]struct {
				Range   lspRange
				NewText string
			}
		}
	}
	cl.call("textDocument/codeAction", map[string]interface{}{"textDocument": map[string]string{"uri": uri},
		"range": lspRange{Start: position{2, 0}, End: position{2, 0}}, "context": map[string]interface{}{"diagnostics": []interface{}{}}}, &actions)
	if len(actions) != 3 || !strings.Contains(actions[0].Title, "Add missing parameter 'émoji'") ||
		actions[1].Title != "Ignore 'missing-required-param' on this line" || actions[2].Title != "Ignore 'missing-required-param' in this file" {
		t.Fatalf("actions: %+v", actions)
	}
	edit := actions[0].Edit.Changes[uri][0]
	if edit.Range.Start.Line != 4 || edit.NewText != "\n      émoji: ''" {
		t.Errorf("edit: %+v", edit)
	}

	var comp struct{ Items []struct{ Label string } }
	cl.call("textDocument/completion", map[string]interface{}{"textDocument": map[string]string{"uri": uri}, "position": map[string]int{"line": 4, "character": 6}}, &comp)
	if len(comp.Items) != 2 {
		t.Errorf("completion: %+v", comp)
	}

	var tree struct {
		IsCalling []struct{ Label string }
	}
	cl.call("atn/dependencyTree", map[string]string{"uri": uri}, &tree)
	if len(tree.IsCalling) != 1 || tree.IsCalling[0].Label != "a.yml" {
		t.Errorf("tree: %+v", tree)
	}
	var graph struct{ Nodes, Edges []interface{} }
	cl.call("atn/graph", map[string]interface{}{}, &graph)
	if len(graph.Nodes) != 2 || len(graph.Edges) != 1 {
		t.Errorf("graph: %+v", graph)
	}
	var filtered struct {
		Nodes      []struct{ ID string }
		TotalNodes int
		Facets     struct{ Kinds map[string]int }
	}
	cl.call("atn/graph", map[string]interface{}{"kinds": []string{"pipeline"}}, &filtered)
	if len(filtered.Nodes) != 1 || filtered.TotalNodes != 2 || filtered.Facets.Kinds["pipeline"] != 1 {
		t.Errorf("filtered graph: %+v", filtered)
	}
	var stats []struct {
		ID   string
		Rows []struct{ Label string }
	}
	cl.call("atn/stats", nil, &stats)
	if len(stats) == 0 || stats[0].ID != "overview" || len(stats[0].Rows) == 0 {
		t.Errorf("stats: %+v", stats)
	}
	var wd []struct{ RelativePath string }
	cl.call("atn/workspaceDiagnostics", nil, &wd)
	if len(wd) != 1 || wd[0].RelativePath != "p.yml" {
		t.Errorf("workspace diagnostics: %+v", wd)
	}

	// ---- navigation features (Structure view, Find Usages, Code Vision, Call Hierarchy)
	tpl := pathToURI(filepath.Join(root, "t", "a.yml"))
	var syms []struct {
		Name     string
		Kind     int
		Children []struct{ Name string }
	}
	cl.call("textDocument/documentSymbol", map[string]interface{}{"textDocument": map[string]string{"uri": tpl}}, &syms)
	if len(syms) != 2 || syms[0].Name != "parameters" || len(syms[0].Children) != 2 || syms[0].Children[0].Name != "émoji" {
		t.Errorf("symbols: %+v", syms)
	}
	var refs []struct {
		URI   string
		Range lspRange
	}
	// on `${{ parameters.émoji }}`: the declaration and the body use (UTF-16 columns)
	cl.call("textDocument/references", map[string]interface{}{"textDocument": map[string]string{"uri": tpl},
		"position": map[string]int{"line": 5, "character": 30}, "context": map[string]bool{"includeDeclaration": true}}, &refs)
	if len(refs) != 2 || refs[1].Range.Start.Character != 32 || refs[1].Range.End.Character != 37 {
		t.Errorf("param references: %+v", refs)
	}
	cl.call("textDocument/references", map[string]interface{}{"textDocument": map[string]string{"uri": tpl},
		"position": map[string]int{"line": 4, "character": 0}, "context": map[string]bool{"includeDeclaration": false}}, &refs)
	if len(refs) != 1 || refs[0].URI != uri || refs[0].Range.Start.Line != 2 {
		t.Errorf("template references: %+v", refs)
	}
	var lenses []struct {
		Command struct {
			Title, Command string
		}
	}
	cl.call("textDocument/codeLens", map[string]interface{}{"textDocument": map[string]string{"uri": tpl}}, &lenses)
	if len(lenses) != 1 || lenses[0].Command.Title != "1 usage · 1 pipeline" || lenses[0].Command.Command != "showUsages" {
		t.Errorf("lenses: %+v", lenses)
	}
	var items []map[string]interface{}
	cl.call("textDocument/prepareCallHierarchy", map[string]interface{}{"textDocument": map[string]string{"uri": uri},
		"position": map[string]int{"line": 2, "character": 8}}, &items)
	if len(items) != 1 || items[0]["name"] != "a.yml" {
		t.Fatalf("prepare: %+v", items)
	}
	var incoming []struct {
		From struct{ Name string }
	}
	cl.call("callHierarchy/incomingCalls", map[string]interface{}{"item": items[0]}, &incoming)
	if len(incoming) != 1 || incoming[0].From.Name != "p.yml" {
		t.Errorf("incoming: %+v", incoming)
	}
	cl.call("shutdown", nil, nil)
}

func TestUTF16(t *testing.T) {
	line := "a😀b"
	if byteToUTF16(line, len("a😀")) != 3 || utf16ToByte(line, 3) != len("a😀") {
		t.Error("utf16 conversion")
	}
}

// Template path completion sends the required-parameters block as a snippet
// only to clients that declare snippetSupport (indented relative to the line,
// for the client to adjust); plain text is absolute and inserted as-is.
func TestTemplateCompletionSnippetSupport(t *testing.T) {
	for _, snippets := range []bool{true, false} {
		root := t.TempDir()
		_ = os.Mkdir(filepath.Join(root, ".git"), 0o755)
		_ = os.MkdirAll(filepath.Join(root, "t"), 0o755)
		_ = os.WriteFile(filepath.Join(root, "t", "a.yml"), []byte("parameters:\n  - name: env\n    values: [dev, prod]\nsteps: []\n"), 0o644)
		pipe := "steps:\n  - template: \n"
		pipePath := filepath.Join(root, "p.yml")
		_ = os.WriteFile(pipePath, []byte(pipe), 0o644)

		cl := startServer(t)
		var initRes map[string]interface{}
		caps := map[string]interface{}{"textDocument": map[string]interface{}{"completion": map[string]interface{}{
			"completionItem": map[string]interface{}{"snippetSupport": snippets}}}}
		cl.call("initialize", map[string]interface{}{
			"rootUri":               pathToURI(root),
			"capabilities":          caps,
			"initializationOptions": map[string]interface{}{"settings": map[string]interface{}{"repositories": map[string]interface{}{"autoFetch": false}}},
		}, &initRes)
		_ = cl.c.notify("initialized", map[string]interface{}{})
		// the initial scan runs in the background; t/a.yml is only offered once it is indexed
		cl.waitNote("window/logMessage", func(m *message) bool { return strings.Contains(string(m.Params), "indexed") })
		uri := pathToURI(pipePath)
		_ = cl.c.notify("textDocument/didOpen", map[string]interface{}{"textDocument": map[string]interface{}{"uri": uri, "languageId": "yaml", "version": 1, "text": pipe}})

		var comp struct {
			Items []struct {
				Label            string
				InsertTextFormat int
				InsertTextMode   int
				TextEdit         struct{ NewText string }
			}
		}
		cl.call("textDocument/completion", map[string]interface{}{"textDocument": map[string]string{"uri": uri}, "position": map[string]int{"line": 1, "character": 14}}, &comp)
		if len(comp.Items) != 1 {
			t.Fatalf("snippets=%v: items %+v", snippets, comp.Items)
		}
		it := comp.Items[0]
		// plain: absolute indentation, inserted as-is; snippet: relative to the
		// line, which the client indents (adjustIndentation)
		want, format, mode := "t/a.yml\n    parameters:\n      env: ''", 0, 1
		if snippets {
			want, format, mode = "t/a.yml\n  parameters:\n    env: ${1|dev,prod|}$0", 2, 2
		}
		if it.TextEdit.NewText != want || it.InsertTextFormat != format || it.InsertTextMode != mode {
			t.Errorf("snippets=%v: %+v", snippets, it)
		}
	}
}

func TestPipelineVerdictLensAndRequest(t *testing.T) {
	root := t.TempDir()
	_ = os.Mkdir(filepath.Join(root, ".git"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "t.yml"), []byte("parameters:\n  - name: émoji\nsteps:\n  - script: echo ${{ parameters.émoji }}\n"), 0o644)
	pipe := "trigger: none\nsteps:\n  - template: t.yml\n"
	pipePath := filepath.Join(root, "p.yml")
	_ = os.WriteFile(pipePath, []byte(pipe), 0o644)

	cl := startServer(t)
	cl.call("initialize", map[string]interface{}{
		"rootUri": pathToURI(root),
		"capabilities": map[string]interface{}{"workspace": map[string]interface{}{
			"codeLens": map[string]interface{}{"refreshSupport": true},
		}},
		"initializationOptions": map[string]interface{}{
			"settings": map[string]interface{}{"repositories": map[string]interface{}{"autoFetch": false}},
			"client":   map[string]interface{}{"commandPrefix": "x."},
		},
	}, nil)
	_ = cl.c.notify("initialized", map[string]interface{}{})
	cl.waitNote("workspace/codeLens/refresh", nil)

	uri := pathToURI(pipePath)
	var lenses []struct {
		Command struct {
			Title     string
			Command   string
			Arguments []map[string]string
		}
	}
	cl.call("textDocument/codeLens", map[string]interface{}{"textDocument": map[string]string{"uri": uri}}, &lenses)
	if len(lenses) != 1 || lenses[0].Command.Command != "x.showPipelineProblems" || lenses[0].Command.Arguments[0]["uri"] != uri ||
		lenses[0].Command.Title != "✗ Will fail when queued: 1 error" {
		t.Fatalf("lenses %+v", lenses)
	}

	var rep struct {
		Title    string
		Verdict  string
		FilePath string
		Errors   int
		Problems []struct {
			URI      string
			FilePath string
			Range    lspRange
			Code     string
		}
	}
	cl.call("atn/pipelineCheck", map[string]string{"uri": uri}, &rep)
	if rep.Errors != 1 || rep.Verdict != "fail" || rep.FilePath != pipePath || len(rep.Problems) != 1 ||
		rep.Problems[0].Code != "missing-required-param" || rep.Problems[0].URI != uri || rep.Problems[0].FilePath != pipePath ||
		rep.Problems[0].Range.Start.Line != 2 {
		t.Fatalf("report %+v", rep)
	}
	var all []struct{ URI string }
	cl.call("atn/pipelineCheck", map[string]string{}, &all)
	if len(all) != 1 || all[0].URI != uri {
		t.Fatalf("all %+v", all)
	}
	var none interface{}
	cl.call("atn/pipelineCheck", map[string]string{"uri": pathToURI(filepath.Join(root, "t.yml"))}, &none)
	if none != nil {
		t.Fatalf("template got a report: %v", none)
	}
}

// A document opened after its diagnostics were published gets them again, though
// they haven't changed: LSP4IJ drops diagnostics for files that weren't open when
// they arrived, so without this a file opened later showed no problems until edited.
func TestDiagnosticsResentOnOpen(t *testing.T) {
	root := t.TempDir()
	_ = os.Mkdir(filepath.Join(root, ".git"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "a.yml"), []byte("parameters:\n  - name: x\n    type: string\nsteps:\n  - script: echo ${{ parameters.x }}\n"), 0o644)
	pipe := "trigger: none\nsteps:\n  - template: a.yml\n"
	pipePath := filepath.Join(root, "p.yml")
	_ = os.WriteFile(pipePath, []byte(pipe), 0o644)

	cl := startServer(t)
	cl.call("initialize", map[string]interface{}{
		"rootUri":               pathToURI(root),
		"capabilities":          map[string]interface{}{},
		"initializationOptions": map[string]interface{}{"settings": map[string]interface{}{"repositories": map[string]interface{}{"autoFetch": false}}},
	}, nil)
	_ = cl.c.notify("initialized", map[string]interface{}{})
	forPipe := func(m *message) bool { return strings.Contains(string(m.Params), "p.yml") }
	// The workspace is diagnosed before anything is opened.
	cl.waitNote("textDocument/publishDiagnostics", forPipe)

	uri := pathToURI(pipePath)
	for i := 1; i <= 2; i++ { // opened, closed and opened again, never edited
		_ = cl.c.notify("textDocument/didOpen", map[string]interface{}{"textDocument": map[string]interface{}{"uri": uri, "languageId": "yaml", "version": i, "text": pipe}})
		m := cl.waitNote("textDocument/publishDiagnostics", forPipe)
		if !strings.Contains(string(m.Params), "missing-required-param") {
			t.Fatalf("open %d: %s", i, m.Params)
		}
		if i == 1 {
			// Nothing changed, so the clients' views must not reload.
			deadline := time.After(500 * time.Millisecond)
		drain:
			for {
				select {
				case n := <-cl.notes:
					if n.Method == "atn/didChange" || n.Method == "workspace/codeLens/refresh" {
						t.Fatalf("opening an unchanged file sent %s", n.Method)
					}
				case <-deadline:
					break drain
				}
			}
		}
		_ = cl.c.notify("textDocument/didClose", map[string]interface{}{"textDocument": map[string]interface{}{"uri": uri}})
	}
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// atn/changes, the "new" marks in atn/pipelineCheck, the lens note and
// choosing the base.
func TestChangesRequests(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("ATN_BASELINE_DIR", t.TempDir())
	root := t.TempDir()
	_ = os.WriteFile(filepath.Join(root, "t.yml"), []byte("parameters:\n  - name: a\n    type: string\nsteps:\n  - script: echo ${{ parameters.a }}\n"), 0o644)
	pipe := "trigger: none\nsteps:\n  - template: t.yml\n    parameters:\n      a: x\n"
	pipePath := filepath.Join(root, "p.yml")
	_ = os.WriteFile(pipePath, []byte(pipe), 0o644)
	gitRun(t, root, "init", "-q", "-b", "main")
	gitRun(t, root, "-c", "user.email=t@example.com", "-c", "user.name=t", "-c", "commit.gpgsign=false", "add", "-A")
	gitRun(t, root, "-c", "user.email=t@example.com", "-c", "user.name=t", "-c", "commit.gpgsign=false", "commit", "-q", "-m", "start")
	gitRun(t, root, "switch", "-q", "-c", "mywork")

	cl := startServer(t)
	cl.call("initialize", map[string]interface{}{
		"rootUri": pathToURI(root),
		"capabilities": map[string]interface{}{"workspace": map[string]interface{}{
			"codeLens": map[string]interface{}{"refreshSupport": true},
		}},
		"initializationOptions": map[string]interface{}{
			"settings": map[string]interface{}{"repositories": map[string]interface{}{"autoFetch": false}},
			"client":   map[string]interface{}{"commandPrefix": "x."},
		},
	}, nil)
	_ = cl.c.notify("initialized", map[string]interface{}{})
	cl.waitNote("workspace/codeLens/refresh", nil)

	uri := pathToURI(pipePath)
	_ = cl.c.notify("textDocument/didOpen", map[string]interface{}{"textDocument": map[string]interface{}{
		"uri": uri, "languageId": "yaml", "version": 1, "text": pipe + "      b: y\n"}})
	cl.waitNote("atn/didChange", nil)

	var ch struct {
		Available bool
		Label     string
		Summary   string
		NewErrors int
		Pipelines []struct {
			URI    string
			Status string
			Title  string
			New    []struct{ Code string }
		}
		Files []struct{ RelativePath, Status string }
	}
	cl.call("atn/changes", map[string]string{}, &ch)
	if !ch.Available || ch.Label == "" || ch.NewErrors != 1 || len(ch.Pipelines) != 1 || ch.Pipelines[0].URI != uri ||
		len(ch.Pipelines[0].New) != 1 || ch.Pipelines[0].New[0].Code != "unknown-param" || len(ch.Files) != 1 || ch.Files[0].Status != "modified" {
		t.Fatalf("changes %+v", ch)
	}

	var all []struct {
		Change   *struct{ New int }
		Problems []struct{ New bool }
	}
	cl.call("atn/pipelineCheck", map[string]string{}, &all)
	if len(all) != 1 || all[0].Change == nil || all[0].Change.New != 1 || len(all[0].Problems) != 1 || !all[0].Problems[0].New {
		t.Fatalf("pipelineCheck %+v", all)
	}

	var lenses []struct{ Command struct{ Title string } }
	cl.call("textDocument/codeLens", map[string]interface{}{"textDocument": map[string]string{"uri": uri}}, &lenses)
	if len(lenses) != 1 || !strings.HasSuffix(lenses[0].Command.Title, " · +1 new since main") {
		t.Fatalf("lens %+v", lenses)
	}

	var bases []struct {
		Ref    string
		Picked bool
	}
	cl.call("atn/changeBases", map[string]string{}, &bases)
	if len(bases) < 3 || bases[0].Ref != "" || !bases[0].Picked || bases[1].Ref != "HEAD" {
		t.Fatalf("bases %+v", bases)
	}
	cl.call("atn/setChangesBase", map[string]string{"ref": "HEAD"}, &ch)
	if len(ch.Pipelines) != 1 || !strings.HasPrefix(ch.Label, "uncommitted") {
		t.Fatalf("after choosing HEAD %+v", ch)
	}
	cl.id++
	_ = cl.c.request(cl.id, "atn/setChangesBase", map[string]string{"ref": "no-such-ref"})
	if m := <-cl.in; m.Error == nil {
		t.Fatal("an unknown ref must be refused")
	}
}

// changes.dimExistingProblems: a problem that was there at the base is sent
// as information, the one the work added keeps its severity. The
// Diagnostics view's My changes mode lists only the new one.
func TestDimExistingProblems(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("ATN_BASELINE_DIR", t.TempDir())
	root := t.TempDir()
	_ = os.WriteFile(filepath.Join(root, "t.yml"), []byte("parameters:\n  - name: a\n    type: string\nsteps:\n  - script: echo ${{ parameters.a }}\n"), 0o644)
	pipe := "trigger: none\nsteps:\n  - template: t.yml\n    parameters:\n      a: x\n      b: y\n"
	pipePath := filepath.Join(root, "p.yml")
	_ = os.WriteFile(pipePath, []byte(pipe), 0o644)
	gitRun(t, root, "init", "-q", "-b", "main")
	gitRun(t, root, "-c", "user.email=t@example.com", "-c", "user.name=t", "-c", "commit.gpgsign=false", "add", "-A")
	gitRun(t, root, "-c", "user.email=t@example.com", "-c", "user.name=t", "-c", "commit.gpgsign=false", "commit", "-q", "-m", "start")
	gitRun(t, root, "switch", "-q", "-c", "mywork")

	cl := startServer(t)
	cl.call("initialize", map[string]interface{}{
		"rootUri": pathToURI(root),
		"initializationOptions": map[string]interface{}{"settings": map[string]interface{}{
			"repositories": map[string]interface{}{"autoFetch": false},
			"changes":      map[string]interface{}{"enabled": true, "dimExistingProblems": true},
		}},
	}, nil)
	_ = cl.c.notify("initialized", map[string]interface{}{})
	_ = cl.c.notify("textDocument/didOpen", map[string]interface{}{"textDocument": map[string]interface{}{
		"uri": pathToURI(pipePath), "languageId": "yaml", "version": 1, "text": pipe + "      c: z\n"}})

	m := cl.waitNote("textDocument/publishDiagnostics", func(m *message) bool {
		return strings.Contains(string(m.Params), "p.yml") && strings.Contains(string(m.Params), "already there at main")
	})
	var p struct {
		Diagnostics []struct {
			Severity int
			Message  string
		}
	}
	_ = json.Unmarshal(m.Params, &p)
	got := map[string]int{}
	for _, d := range p.Diagnostics {
		switch {
		case strings.Contains(d.Message, "'b'"):
			got["b"] = d.Severity
			if !strings.HasSuffix(d.Message, "(already there at main)") {
				t.Errorf("message %q", d.Message)
			}
		case strings.Contains(d.Message, "'c'"):
			got["c"] = d.Severity
		}
	}
	if got["b"] != 3 || got["c"] != 1 {
		t.Fatalf("severities %v in %s", got, m.Params)
	}

	// the Diagnostics view's My changes mode: only c
	var only struct {
		Available bool
		Label     string
		Existing  int
		NewErrors int
		Files     []struct {
			RelativePath string
			Diagnostics  []struct{ Message string }
		}
	}
	cl.call("atn/workspaceDiagnostics", map[string]bool{"onlyNew": true}, &only)
	if !only.Available || only.Label == "" || only.Existing != 1 || only.NewErrors != 1 || len(only.Files) != 1 ||
		len(only.Files[0].Diagnostics) != 1 || !strings.Contains(only.Files[0].Diagnostics[0].Message, "'c'") {
		t.Fatalf("onlyNew %+v", only)
	}
	var all []struct{ Diagnostics []struct{ Message string } }
	cl.call("atn/workspaceDiagnostics", map[string]bool{}, &all)
	if len(all) != 1 || len(all[0].Diagnostics) != 2 {
		t.Fatalf("all %+v", all)
	}
}

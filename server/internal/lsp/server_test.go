package lsp

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
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
	if len(actions) != 1 || !strings.Contains(actions[0].Title, "Add missing parameter 'émoji'") {
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

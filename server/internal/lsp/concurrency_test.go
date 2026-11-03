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

// A request cancelled while still queued is answered with RequestCancelled
// instead of being run.
func TestCancelQueuedRequest(t *testing.T) {
	cr, sw := io.Pipe()
	s := &Server{c: &conn{w: sw}, pending: map[string]bool{}, queue: make(chan *message, 1)}
	id := json.RawMessage(`7`)
	s.pending[string(id)] = false
	s.cancel(json.RawMessage(`{"id":7}`))
	s.queue <- &message{ID: &id, Method: "textDocument/hover", Params: json.RawMessage(`{}`)}
	close(s.queue)
	go s.worker()

	m, err := (&conn{r: bufio.NewReader(cr)}).read()
	if err != nil {
		t.Fatal(err)
	}
	if m.Error == nil || m.Error.Code != codeRequestCancelled || string(*m.ID) != "7" {
		t.Fatalf("reply = %+v", m)
	}
	// cancelling an unknown or finished request is a no-op
	s.cancel(json.RawMessage(`{"id":8}`))
	if _, ok := s.pending["8"]; ok {
		t.Error("cancel created a pending entry")
	}
}

// atn/didChange makes both clients reload their trees and the graph, so it
// must only be sent when the index or diagnostics actually changed.
func TestDidChangeOnlyWhenSomethingChanged(t *testing.T) {
	root := t.TempDir()
	_ = os.Mkdir(filepath.Join(root, ".git"), 0o755)
	_ = os.WriteFile(filepath.Join(root, "t.yml"), []byte("parameters:\n  - name: a\nsteps:\n  - script: echo ${{ parameters.a }}\n"), 0o644)
	pipe := "trigger: none\nsteps:\n  - template: t.yml\n    parameters:\n      a: 1\n"
	pipePath := filepath.Join(root, "p.yml")
	_ = os.WriteFile(pipePath, []byte(pipe), 0o644)

	cl := startServer(t)
	cl.call("initialize", map[string]interface{}{
		"rootUri":               pathToURI(root),
		"capabilities":          map[string]interface{}{},
		"initializationOptions": map[string]interface{}{"settings": map[string]interface{}{"repositories": map[string]interface{}{"autoFetch": false}}},
	}, nil)
	_ = cl.c.notify("initialized", map[string]interface{}{})
	cl.waitNote("atn/didChange", nil)

	uri := pathToURI(pipePath)
	change := func(version int, text string) {
		_ = cl.c.notify("textDocument/didChange", map[string]interface{}{
			"textDocument":   map[string]interface{}{"uri": uri, "version": version},
			"contentChanges": []interface{}{map[string]interface{}{"text": text}},
		})
	}
	quiet := func(what string) {
		t.Helper()
		deadline := time.After(700 * time.Millisecond)
		for {
			select {
			case m := <-cl.notes:
				if m.Method == "atn/didChange" || m.Method == "textDocument/publishDiagnostics" {
					t.Fatalf("%s: unexpected %s %s", what, m.Method, m.Params)
				}
			case <-deadline:
				return
			}
		}
	}

	_ = cl.c.notify("textDocument/didOpen", map[string]interface{}{"textDocument": map[string]interface{}{"uri": uri, "languageId": "yaml", "version": 1, "text": pipe}})
	quiet("opening a file with its disk content")
	change(2, "# a comment\n"+pipe)
	cl.waitNote("atn/didChange", nil) // refs moved one line down: views must refresh
	change(3, "# another comment\n"+pipe)
	quiet("editing a comment")

	change(4, strings.Replace("# another comment\n"+pipe, "      a: 1\n", "", 1))
	m := cl.waitNote("textDocument/publishDiagnostics", nil)
	if !strings.Contains(string(m.Params), "missing-required-param") {
		t.Errorf("diagnostics: %s", m.Params)
	}
	cl.waitNote("atn/didChange", nil)
}

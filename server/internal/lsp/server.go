package lsp

import (
	"bufio"
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/qSharpy/azure-templates-navigator/server/internal/engine"
)

// Version is set at build time.
var Version = "dev"

// Server is the language server.
type Server struct {
	c        *conn
	eng      *engine.Engine
	cf       engine.ClientFeatures
	mu       sync.Mutex
	timer    *time.Timer
	pubHash  map[string]string // path -> hash of last published diagnostics
	reqID    int
	shutdown bool
	watchReg bool

	// pubMu serialises publish runs so an older run can never overwrite the
	// diagnostics of a newer one.
	pubMu      sync.Mutex
	pubVersion int // index version last announced with atn/didChange

	// Requests run on workers so the reader keeps taking document changes
	// and $/cancelRequest while a slow request is in flight.
	queue     chan *message
	pendingMu sync.Mutex
	pending   map[string]bool // request id -> cancelled
}

// workers is the number of goroutines serving requests. The engine
// serialises its own work; more than one worker lets cheap requests (hover)
// finish while an expensive one (graph) waits for its turn.
const workers = 4

// codeRequestCancelled is LSP's RequestCancelled error code.
const codeRequestCancelled = -32800

// Serve runs the server until the client exits.
func Serve(in io.Reader, out io.Writer) error {
	s := &Server{
		c:       &conn{r: bufio.NewReaderSize(in, 1<<16), w: out},
		pubHash: map[string]string{},
		queue:   make(chan *message, 256),
		pending: map[string]bool{},
	}
	for i := 0; i < workers; i++ {
		go s.worker()
	}
	defer close(s.queue)
	for {
		m, err := s.c.read()
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		s.dispatch(m)
	}
}

// dispatch handles notifications (document sync, cancellation) and
// lifecycle requests in order on the reader goroutine, and queues every
// other request for a worker.
func (s *Server) dispatch(m *message) {
	if m.ID == nil || m.Method == "" || s.eng == nil {
		if m.Method == "$/cancelRequest" {
			s.cancel(m.Params)
			return
		}
		s.handle(m)
		return
	}
	switch m.Method {
	case "initialize", "shutdown", "exit":
		s.handle(m)
		return
	}
	s.pendingMu.Lock()
	s.pending[string(*m.ID)] = false
	s.pendingMu.Unlock()
	s.queue <- m
}

func (s *Server) worker() {
	for m := range s.queue {
		key := string(*m.ID)
		s.pendingMu.Lock()
		cancelled := s.pending[key]
		s.pendingMu.Unlock()
		if cancelled {
			_ = s.c.reply(m.ID, nil, &rpcError{Code: codeRequestCancelled, Message: "request cancelled"})
		} else {
			s.handle(m)
		}
		s.pendingMu.Lock()
		delete(s.pending, key)
		s.pendingMu.Unlock()
	}
}

// cancel marks a queued request as cancelled. A request that already
// started runs to completion (engine calls are short and not interruptible).
func (s *Server) cancel(raw json.RawMessage) {
	var p struct {
		ID json.RawMessage `json:"id"`
	}
	if json.Unmarshal(raw, &p) != nil || len(p.ID) == 0 {
		return
	}
	s.pendingMu.Lock()
	if _, ok := s.pending[string(p.ID)]; ok {
		s.pending[string(p.ID)] = true
	}
	s.pendingMu.Unlock()
}

func (s *Server) logf(format string, args ...interface{}) {
	_ = s.c.notify("window/logMessage", map[string]interface{}{"type": 4, "message": fmt.Sprintf(format, args...)})
}

func (s *Server) showMessage(typ int, msg string) {
	_ = s.c.notify("window/showMessage", map[string]interface{}{"type": typ, "message": msg})
}

func (s *Server) handle(m *message) {
	if m.Method == "" {
		return // response to one of our requests
	}
	isRequest := m.ID != nil
	defer func() {
		if r := recover(); r != nil {
			s.logf("panic handling %s: %v", m.Method, r)
			if isRequest {
				_ = s.c.reply(m.ID, nil, &rpcError{Code: -32603, Message: fmt.Sprint(r)})
			}
		}
	}()

	if s.eng == nil && m.Method != "initialize" && m.Method != "exit" {
		if isRequest {
			_ = s.c.reply(m.ID, nil, &rpcError{Code: -32002, Message: "server not initialized"})
		}
		return
	}

	var result interface{}
	var rerr *rpcError
	switch m.Method {
	case "initialize":
		result, rerr = s.initialize(m.Params)
	case "initialized":
		s.initialized()
	case "shutdown":
		s.shutdown = true
	case "exit":
		if s.shutdown {
			os.Exit(0)
		}
		os.Exit(1)
	case "$/setTrace", "textDocument/didSave", "workspace/didChangeWorkspaceFolders":
		if m.Method == "workspace/didChangeWorkspaceFolders" {
			s.workspaceFoldersChanged(m.Params)
		}
	case "textDocument/didOpen":
		var p struct {
			TextDocument struct {
				URI  string `json:"uri"`
				Text string `json:"text"`
			} `json:"textDocument"`
		}
		if json.Unmarshal(m.Params, &p) == nil {
			if path := uriToPath(p.TextDocument.URI); path != "" {
				s.eng.OpenDocument(path, p.TextDocument.Text)
				s.schedule(50 * time.Millisecond)
			}
		}
	case "textDocument/didChange":
		var p struct {
			TextDocument   textDocumentID `json:"textDocument"`
			ContentChanges []struct {
				Text string `json:"text"`
			} `json:"contentChanges"`
		}
		if json.Unmarshal(m.Params, &p) == nil && len(p.ContentChanges) > 0 {
			if path := uriToPath(p.TextDocument.URI); path != "" {
				s.eng.OpenDocument(path, p.ContentChanges[len(p.ContentChanges)-1].Text)
				s.schedule(300 * time.Millisecond)
			}
		}
	case "textDocument/didClose":
		var p struct {
			TextDocument textDocumentID `json:"textDocument"`
		}
		if json.Unmarshal(m.Params, &p) == nil {
			if path := uriToPath(p.TextDocument.URI); path != "" {
				s.eng.CloseDocument(path)
				s.schedule(100 * time.Millisecond)
			}
		}
	case "workspace/didChangeWatchedFiles":
		var p struct {
			Changes []struct {
				URI string `json:"uri"`
			} `json:"changes"`
		}
		if json.Unmarshal(m.Params, &p) == nil {
			for _, ch := range p.Changes {
				if path := uriToPath(ch.URI); path != "" {
					s.eng.FileChanged(path)
				}
			}
			s.schedule(200 * time.Millisecond)
		}
	case "workspace/didChangeConfiguration":
		var p struct {
			Settings json.RawMessage `json:"settings"`
		}
		if json.Unmarshal(m.Params, &p) == nil && len(p.Settings) > 0 && string(p.Settings) != "null" {
			s.eng.SetSettings(engine.ParseSettings(p.Settings))
			s.schedule(50 * time.Millisecond)
		}
	case "textDocument/hover":
		result = s.hover(m.Params)
	case "textDocument/definition":
		result = s.definition(m.Params)
	case "textDocument/completion":
		result = s.completion(m.Params)
	case "textDocument/codeAction":
		result = s.codeAction(m.Params)
	case "textDocument/documentSymbol":
		result = s.documentSymbols(m.Params)
	case "textDocument/references":
		result = s.references(m.Params)
	case "textDocument/codeLens":
		result = s.codeLens(m.Params)
	case "textDocument/prepareCallHierarchy":
		result = s.prepareCallHierarchy(m.Params)
	case "callHierarchy/incomingCalls":
		result = s.hierarchyCalls(m.Params, true)
	case "callHierarchy/outgoingCalls":
		result = s.hierarchyCalls(m.Params, false)
	case "workspace/executeCommand":
		result, rerr = s.executeCommand(m.Params)

	// ---- custom requests used by the IDE views ----
	case "atn/dependencyTree":
		var p struct {
			URI string `json:"uri"`
		}
		_ = json.Unmarshal(m.Params, &p)
		if t, ok := s.eng.Tree(uriToPath(p.URI)); ok {
			result = t
		}
	case "atn/graph":
		var p struct {
			URI      string `json:"uri"`
			Depth    int    `json:"depth"`
			RootPath string `json:"rootPath"`
		}
		_ = json.Unmarshal(m.Params, &p)
		req := engine.GraphRequest{Depth: p.Depth, RootPath: p.RootPath}
		if p.URI != "" {
			req.File = uriToPath(p.URI)
		}
		if req.RootPath == "" && req.File == "" {
			req.RootPath = s.eng.Settings().Graph.RootPath
		}
		result = s.eng.BuildGraph(req)
	case "atn/workspaceDiagnostics":
		result = s.workspaceDiagnostics()
	case "atn/templates":
		result = s.eng.ListTemplates()
	case "atn/parameters":
		var p struct {
			URI  string `json:"uri"`
			Line *int   `json:"line"`
		}
		_ = json.Unmarshal(m.Params, &p)
		line := -1
		if p.Line != nil {
			line = *p.Line
		}
		file, list := s.eng.TemplateParameters(uriToPath(p.URI), line)
		result = map[string]interface{}{"filePath": file, "parameters": list}
	case "atn/repositories":
		result = s.eng.Repositories()
	case "atn/references":
		result = s.usageList(m.Params)
	case "atn/info":
		result = map[string]interface{}{
			"version":  Version,
			"cacheDir": s.eng.Settings().Repositories.CacheDir,
			"roots":    s.eng.Roots(),
			"files":    len(s.eng.WorkspaceFiles()),
		}
	default:
		if isRequest {
			rerr = &rpcError{Code: -32601, Message: "method not found: " + m.Method}
		}
	}
	if isRequest {
		if rerr != nil {
			_ = s.c.reply(m.ID, nil, rerr)
		} else {
			_ = s.c.reply(m.ID, result, nil)
		}
	}
}

// ---------------------------------------------------------------------------
// lifecycle
// ---------------------------------------------------------------------------

type initParams struct {
	RootURI          string `json:"rootUri"`
	RootPath         string `json:"rootPath"`
	WorkspaceFolders []struct {
		URI string `json:"uri"`
	} `json:"workspaceFolders"`
	InitializationOptions struct {
		Settings json.RawMessage       `json:"settings"`
		Client   engine.ClientFeatures `json:"client"`
	} `json:"initializationOptions"`
	Capabilities struct {
		Workspace struct {
			DidChangeWatchedFiles struct {
				DynamicRegistration bool `json:"dynamicRegistration"`
			} `json:"didChangeWatchedFiles"`
		} `json:"workspace"`
	} `json:"capabilities"`
}

func (s *Server) initialize(raw json.RawMessage) (interface{}, *rpcError) {
	var p initParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, &rpcError{Code: -32602, Message: err.Error()}
	}
	var roots []string
	for _, f := range p.WorkspaceFolders {
		if path := uriToPath(f.URI); path != "" {
			roots = append(roots, path)
		}
	}
	if len(roots) == 0 && p.RootURI != "" {
		if path := uriToPath(p.RootURI); path != "" {
			roots = append(roots, path)
		}
	}
	if len(roots) == 0 && p.RootPath != "" {
		roots = append(roots, p.RootPath)
	}
	s.cf = p.InitializationOptions.Client
	s.watchReg = p.Capabilities.Workspace.DidChangeWatchedFiles.DynamicRegistration
	s.eng = engine.New(roots, engine.ParseSettings(p.InitializationOptions.Settings))
	s.eng.Log = func(msg string) { s.logf("%s", msg) }
	s.eng.Repos.OnChange(func() {
		s.eng.Invalidate()
		s.schedule(10 * time.Millisecond)
	})

	return map[string]interface{}{
		"capabilities": map[string]interface{}{
			"positionEncoding":   "utf-16",
			"textDocumentSync":   map[string]interface{}{"openClose": true, "change": 1},
			"hoverProvider":      true,
			"definitionProvider": true,
			"completionProvider": map[string]interface{}{
				"triggerCharacters": []string{" ", ":", "/"},
			},
			"codeActionProvider":     map[string]interface{}{"codeActionKinds": []string{"quickfix"}},
			"documentSymbolProvider": map[string]interface{}{"label": "Azure Pipelines"},
			"referencesProvider":     true,
			"callHierarchyProvider":  true,
			"codeLensProvider":       map[string]interface{}{"resolveProvider": false},
			"executeCommandProvider": map[string]interface{}{
				"commands": []string{"atn.fetchRepository", "atn.fetchAllRepositories", "atn.refresh"},
			},
			"workspace": map[string]interface{}{
				"workspaceFolders": map[string]interface{}{"supported": true, "changeNotifications": true},
			},
		},
		"serverInfo": map[string]interface{}{"name": "atn", "version": Version},
	}, nil
}

func (s *Server) initialized() {
	if s.watchReg {
		s.reqID++
		_ = s.c.request(s.reqID, "client/registerCapability", map[string]interface{}{
			"registrations": []interface{}{map[string]interface{}{
				"id": "atn-watch", "method": "workspace/didChangeWatchedFiles",
				"registerOptions": map[string]interface{}{"watchers": []interface{}{
					map[string]interface{}{"globPattern": "**/*.{yml,yaml}"},
					map[string]interface{}{"globPattern": "**/" + engine.ProjectConfigFile},
				}},
			}},
		})
	}
	go func() {
		start := time.Now()
		s.eng.Rescan()
		n := len(s.eng.WorkspaceFiles())
		s.publish()
		s.logf("Azure Templates Navigator %s: indexed %d YAML file(s) in %s", Version, n, time.Since(start).Round(time.Millisecond))
	}()
}

func (s *Server) workspaceFoldersChanged(raw json.RawMessage) {
	var p struct {
		Event struct {
			Added   []struct{ URI string } `json:"added"`
			Removed []struct{ URI string } `json:"removed"`
		} `json:"event"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return
	}
	roots := map[string]bool{}
	for _, r := range s.eng.Roots() {
		roots[r] = true
	}
	for _, a := range p.Event.Added {
		roots[uriToPath(a.URI)] = true
	}
	for _, r := range p.Event.Removed {
		delete(roots, uriToPath(r.URI))
	}
	var list []string
	for r := range roots {
		list = append(list, r)
	}
	sort.Strings(list)
	s.eng.SetRoots(list)
	go func() {
		s.eng.Rescan()
		s.publish()
	}()
}

// ---------------------------------------------------------------------------
// diagnostics publishing
// ---------------------------------------------------------------------------

func (s *Server) schedule(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.timer != nil {
		s.timer.Stop()
	}
	s.timer = time.AfterFunc(d, s.publish)
}

type lspDiagnostic struct {
	Range    lspRange `json:"range"`
	Severity int      `json:"severity"`
	Code     string   `json:"code"`
	Source   string   `json:"source"`
	Message  string   `json:"message"`
}

func (s *Server) toLSPDiag(path string, d engine.Diagnostic) lspDiagnostic {
	return lspDiagnostic{
		Range:    s.rangeOf(path, d.Line, d.StartCol, d.EndLine, d.EndCol),
		Severity: d.Severity, Code: d.Code, Source: engine.Source, Message: d.Message,
	}
}

func (s *Server) rangeOf(path string, l1, c1, l2, c2 int) lspRange {
	t1 := s.eng.LineText(path, l1)
	t2 := t1
	if l2 != l1 {
		t2 = s.eng.LineText(path, l2)
	}
	return lspRange{
		Start: position{Line: l1, Character: byteToUTF16(t1, c1)},
		End:   position{Line: l2, Character: byteToUTF16(t2, c2)},
	}
}

// publish sends the diagnostics that changed since the last run, and
// atn/didChange when the index or any diagnostics changed (clients reload
// their trees and the graph on it, so it must not fire on every keystroke).
func (s *Server) publish() {
	s.pubMu.Lock()
	defer s.pubMu.Unlock()
	delta := s.eng.ChangedDiagnostics()
	all := delta.Files
	if delta.Full {
		// files that disappeared: clear
		s.mu.Lock()
		for p := range s.pubHash {
			if _, ok := all[p]; !ok {
				all[p] = nil
			}
		}
		s.mu.Unlock()
	}

	paths := make([]string, 0, len(all))
	for p := range all {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	changed := false
	for _, p := range paths {
		ds := all[p]
		lds := make([]lspDiagnostic, 0, len(ds))
		for _, d := range ds {
			lds = append(lds, s.toLSPDiag(p, d))
		}
		b, _ := json.Marshal(lds)
		h := fmt.Sprintf("%x", sha1.Sum(b))
		s.mu.Lock()
		prev, seen := s.pubHash[p]
		if (seen && prev == h) || (!seen && len(lds) == 0) {
			s.mu.Unlock()
			continue
		}
		if len(lds) == 0 {
			delete(s.pubHash, p) // nothing shown any more
		} else {
			s.pubHash[p] = h
		}
		s.mu.Unlock()
		changed = true
		_ = s.c.notify("textDocument/publishDiagnostics", map[string]interface{}{"uri": pathToURI(p), "diagnostics": lds})
	}
	if changed || delta.Version != s.pubVersion {
		s.pubVersion = delta.Version
		_ = s.c.notify("atn/didChange", map[string]interface{}{"reason": "index"})
	}
}

func (s *Server) workspaceDiagnostics() interface{} {
	type item struct {
		Line     int    `json:"line"`
		Col      int    `json:"col"`
		Severity int    `json:"severity"`
		Code     string `json:"code"`
		Message  string `json:"message"`
	}
	type file struct {
		FilePath     string `json:"filePath"`
		URI          string `json:"uri"`
		RelativePath string `json:"relativePath"`
		Errors       int    `json:"errors"`
		Warnings     int    `json:"warnings"`
		Diagnostics  []item `json:"diagnostics"`
	}
	out := []file{}
	for _, fd := range s.eng.WorkspaceDiagnostics() {
		f := file{FilePath: fd.FilePath, URI: pathToURI(fd.FilePath), RelativePath: fd.RelativePath, Errors: fd.Errors, Warnings: fd.Warnings}
		for _, d := range fd.Diagnostics {
			f.Diagnostics = append(f.Diagnostics, item{Line: d.Line, Col: byteToUTF16(s.eng.LineText(fd.FilePath, d.Line), d.StartCol),
				Severity: d.Severity, Code: d.Code, Message: d.Message})
		}
		out = append(out, f)
	}
	return out
}

// ---------------------------------------------------------------------------
// language features
// ---------------------------------------------------------------------------

func (s *Server) pos(raw json.RawMessage) (path string, line, col int, ok bool) {
	var p textDocumentPositionParams
	if json.Unmarshal(raw, &p) != nil {
		return "", 0, 0, false
	}
	path = uriToPath(p.TextDocument.URI)
	if path == "" {
		return "", 0, 0, false
	}
	text := s.eng.LineText(path, p.Position.Line)
	return path, p.Position.Line, utf16ToByte(text, p.Position.Character), true
}

func (s *Server) hover(raw json.RawMessage) interface{} {
	path, line, col, ok := s.pos(raw)
	if !ok {
		return nil
	}
	h, ok := s.eng.HoverAt(path, line, col, s.cf)
	if !ok {
		return nil
	}
	return map[string]interface{}{
		"contents": map[string]string{"kind": "markdown", "value": s.md(h.Markdown)},
		"range":    s.rangeOf(path, h.Line, h.StartCol, h.Line, h.EndCol),
	}
}

func (s *Server) definition(raw json.RawMessage) interface{} {
	path, line, col, ok := s.pos(raw)
	if !ok {
		return nil
	}
	loc, ok := s.eng.Definition(path, line, col)
	if !ok {
		return nil
	}
	r := s.rangeOf(loc.Path, loc.Line, loc.Col, loc.Line, loc.Col)
	return map[string]interface{}{"uri": pathToURI(loc.Path), "range": r}
}

func (s *Server) completion(raw json.RawMessage) interface{} {
	path, line, col, ok := s.pos(raw)
	if !ok {
		return nil
	}
	items := s.eng.Complete(path, line, col)
	text := s.eng.LineText(path, line)
	kinds := map[string]int{"property": 10, "value": 12, "file": 17}
	out := make([]map[string]interface{}, 0, len(items))
	for _, it := range items {
		m := map[string]interface{}{
			"label":    it.Label,
			"kind":     kinds[it.Kind],
			"detail":   it.Detail,
			"sortText": it.SortText,
			"textEdit": map[string]interface{}{
				"range": lspRange{
					Start: position{Line: line, Character: byteToUTF16(text, it.ReplaceStart)},
					End:   position{Line: line, Character: byteToUTF16(text, maxInt(it.ReplaceEnd, col))},
				},
				"newText": it.InsertText,
			},
		}
		if it.FilterText != "" {
			m["filterText"] = it.FilterText
		}
		if it.Documentation != "" {
			m["documentation"] = map[string]string{"kind": "markdown", "value": s.md(it.Documentation)}
		}
		out = append(out, m)
	}
	return map[string]interface{}{"isIncomplete": false, "items": out}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func (s *Server) codeAction(raw json.RawMessage) interface{} {
	var p struct {
		TextDocument textDocumentID `json:"textDocument"`
		Range        lspRange       `json:"range"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return nil
	}
	path := uriToPath(p.TextDocument.URI)
	uri := p.TextDocument.URI
	actions := s.eng.CodeActions(path, p.Range.Start.Line, p.Range.End.Line)
	out := []interface{}{}
	for _, a := range actions {
		m := map[string]interface{}{"title": a.Title, "kind": "quickfix", "isPreferred": a.Preferred}
		if a.Diagnostic != nil {
			m["diagnostics"] = []lspDiagnostic{s.toLSPDiag(path, *a.Diagnostic)}
		}
		if len(a.Edits) > 0 {
			var edits []interface{}
			for _, e := range a.Edits {
				edits = append(edits, map[string]interface{}{
					"range":   s.rangeOf(path, e.StartLine, e.StartCol, e.EndLine, e.EndCol),
					"newText": e.NewText,
				})
			}
			m["edit"] = map[string]interface{}{"changes": map[string]interface{}{uri: edits}}
		}
		if a.Command != "" {
			m["command"] = map[string]interface{}{"title": a.Title, "command": a.Command, "arguments": a.CommandArgs}
		}
		out = append(out, m)
	}
	return out
}

func (s *Server) executeCommand(raw json.RawMessage) (interface{}, *rpcError) {
	var p struct {
		Command   string            `json:"command"`
		Arguments []json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, &rpcError{Code: -32602, Message: err.Error()}
	}
	switch p.Command {
	case "atn.fetchRepository":
		var req engine.RepoRequest
		if len(p.Arguments) > 0 {
			_ = json.Unmarshal(p.Arguments[0], &req)
		}
		decl, remote, ok := s.eng.RepoDeclFor(req)
		if !ok {
			return nil, &rpcError{Code: -32602, Message: "unknown repository alias: " + req.Alias}
		}
		loc, err := s.eng.Repos.Fetch(decl, remote)
		if err != nil {
			s.showMessage(1, "Azure Templates Navigator: "+err.Error())
			return nil, nil
		}
		s.schedule(10 * time.Millisecond)
		return loc, nil
	case "atn.fetchAllRepositories":
		n := 0
		for _, rd := range s.eng.RepoDecls() {
			if _, err := s.eng.Repos.Fetch(rd.Decl, rd.Remote); err == nil {
				n++
			}
		}
		s.schedule(10 * time.Millisecond)
		return map[string]int{"started": n}, nil
	case "atn.refresh":
		go func() {
			s.eng.Rescan()
			s.publish()
		}()
		return true, nil
	}
	return nil, &rpcError{Code: -32601, Message: "unknown command: " + p.Command}
}

package lsp

import (
	"encoding/json"

	"github.com/qSharpy/azure-templates-navigator/server/internal/engine"
	"github.com/qSharpy/azure-templates-navigator/server/internal/pipeline"
)

// md adapts hover/completion markdown to what the client renders well.
func (s *Server) md(text string) string {
	if s.cf.PlainMarkdown {
		return engine.Plain(text)
	}
	return text
}

// LSP SymbolKind values.
var symbolKinds = map[string]int{
	"section": 3, "extends": 11, "template": 2, "stage": 5, "job": 6, "deployment": 6, "step": 12,
	"parameter": 7, "variable": 13, "repository": 4, "resource": 19, "condition": 25,
}

func (s *Server) docPath(raw json.RawMessage) string {
	var p struct {
		TextDocument textDocumentID `json:"textDocument"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return ""
	}
	return uriToPath(p.TextDocument.URI)
}

func (s *Server) documentSymbols(raw json.RawMessage) interface{} {
	path := s.docPath(raw)
	if path == "" {
		return nil
	}
	var conv func(syms []*pipeline.Symbol) []interface{}
	conv = func(syms []*pipeline.Symbol) []interface{} {
		out := make([]interface{}, 0, len(syms))
		for _, sy := range syms {
			endText := s.eng.LineText(path, sy.EndLine)
			m := map[string]interface{}{
				"name":           nonEmpty(sy.Name, sy.Kind),
				"kind":           symbolKinds[sy.Kind],
				"range":          s.rangeOf(path, sy.Line, 0, sy.EndLine, len(endText)),
				"selectionRange": s.rangeOf(path, sy.Line, sy.SelStart, sy.Line, sy.SelEnd),
				"children":       conv(sy.Children),
			}
			if sy.Detail != "" {
				m["detail"] = sy.Detail
			}
			out = append(out, m)
		}
		return out
	}
	return conv(s.eng.Symbols(path))
}

func nonEmpty(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func (s *Server) spanLocation(sp engine.Span) map[string]interface{} {
	return map[string]interface{}{"uri": pathToURI(sp.Path), "range": s.rangeOf(sp.Path, sp.Line, sp.StartCol, sp.Line, sp.EndCol)}
}

func (s *Server) references(raw json.RawMessage) interface{} {
	path, line, col, ok := s.pos(raw)
	if !ok {
		return nil
	}
	var p struct {
		Context struct {
			IncludeDeclaration bool `json:"includeDeclaration"`
		} `json:"context"`
	}
	_ = json.Unmarshal(raw, &p)
	out := []interface{}{}
	for _, u := range s.eng.References(path, line, col, p.Context.IncludeDeclaration).Usages {
		out = append(out, s.spanLocation(u.Span))
	}
	return out
}

// usageList answers atn/references: the same usages as textDocument/references,
// with labels, for clients that render their own usages popup.
func (s *Server) usageList(raw json.RawMessage) interface{} {
	path, line, col, ok := s.pos(raw)
	if !ok {
		return nil
	}
	res := s.eng.References(path, line, col, false)
	type item struct {
		engine.Usage
		URI    string `json:"uri"`
		Col    int    `json:"col"` // UTF-16
		EndCol int    `json:"endCol"`
	}
	items := make([]item, 0, len(res.Usages))
	for _, u := range res.Usages {
		t := s.eng.LineText(u.Path, u.Line)
		items = append(items, item{Usage: u, URI: pathToURI(u.Path), Col: byteToUTF16(t, u.StartCol), EndCol: byteToUTF16(t, u.EndCol)})
	}
	return map[string]interface{}{"title": res.Title, "usages": items}
}

func (s *Server) codeLens(raw json.RawMessage) interface{} {
	path := s.docPath(raw)
	if path == "" {
		return nil
	}
	out := []interface{}{}
	for _, l := range s.eng.CodeLenses(path) {
		r := s.rangeOf(path, l.Line, l.Col, l.Line, l.Col)
		out = append(out, map[string]interface{}{
			"range": r,
			"command": map[string]interface{}{
				"title":   l.Title,
				"command": s.cf.CommandPrefix + "showUsages",
				"arguments": []interface{}{map[string]interface{}{
					"uri": pathToURI(path), "line": l.Line, "character": 0,
				}},
			},
		})
	}
	return out
}

func (s *Server) hierarchyItem(it engine.HierarchyItem) map[string]interface{} {
	kind := 1 // File
	if it.Kind == "pipeline" {
		kind = 24 // Event: a pipeline is an entry point
	}
	last := s.eng.LineText(it.Path, it.LastLine)
	return map[string]interface{}{
		"name":           it.Name,
		"kind":           kind,
		"detail":         it.Detail,
		"uri":            pathToURI(it.Path),
		"range":          s.rangeOf(it.Path, 0, 0, it.LastLine, len(last)),
		"selectionRange": s.rangeOf(it.Path, 0, 0, 0, 0),
	}
}

func (s *Server) prepareCallHierarchy(raw json.RawMessage) interface{} {
	path, line, _, ok := s.pos(raw)
	if !ok {
		return nil
	}
	it, ok := s.eng.PrepareHierarchy(path, line)
	if !ok {
		return nil
	}
	return []interface{}{s.hierarchyItem(it)}
}

func (s *Server) hierarchyCalls(raw json.RawMessage, incoming bool) interface{} {
	var p struct {
		Item struct {
			URI string `json:"uri"`
		} `json:"item"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return nil
	}
	path := uriToPath(p.Item.URI)
	var calls []engine.HierarchyCall
	if incoming {
		calls = s.eng.IncomingCalls(path)
	} else {
		calls = s.eng.OutgoingCalls(path)
	}
	out := []interface{}{}
	for _, c := range calls {
		ranges := make([]interface{}, 0, len(c.Ranges))
		for _, r := range c.Ranges {
			ranges = append(ranges, s.rangeOf(r.Path, r.Line, r.StartCol, r.Line, r.EndCol))
		}
		key := "to"
		if incoming {
			key = "from"
		}
		out = append(out, map[string]interface{}{key: s.hierarchyItem(c.Item), "fromRanges": ranges})
	}
	return out
}

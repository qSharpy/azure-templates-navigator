package engine

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/qSharpy/azure-templates-navigator/server/internal/pipeline"
)

// TextEdit replaces [StartLine:StartCol, EndLine:EndCol) (byte columns).
type TextEdit struct {
	StartLine, StartCol, EndLine, EndCol int
	NewText                              string
}

// CodeAction is an IDE-independent quick fix.
type CodeAction struct {
	Title       string
	Diagnostic  *Diagnostic
	Edits       []TextEdit // edits to the same document
	Command     string     // server command id (when no edits)
	CommandArgs []interface{}
	Preferred   bool
}

// CodeActions returns quick fixes for diagnostics overlapping [startLine, endLine].
func (e *Engine) CodeActions(path string, startLine, endLine int) []CodeAction {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ensureLinked()
	path = filepath.Clean(path)
	f := e.file(path)
	if f == nil {
		return nil
	}
	diags := e.diagnose(path)
	var out []CodeAction
	missingByCall := map[int][]Diagnostic{}
	declaredOnce := map[string]bool{}
	for i := range diags {
		d := diags[i]
		if d.Line < startLine || d.Line > endLine {
			continue
		}
		dp := &diags[i]
		switch d.Code {
		case CodeMissingRequired:
			missingByCall[d.templateLine] = append(missingByCall[d.templateLine], d)
			out = append(out, CodeAction{Title: fmt.Sprintf("Add missing parameter '%s' (%s)", d.param, typeLabel(d.paramType)),
				Diagnostic: dp, Edits: addParamsEdit(f.lines, d.templateLine, []Diagnostic{d}), Preferred: true})
		case CodeUnknownParam:
			for _, s := range d.suggestions {
				cs := pipeline.ParseCallSite(f.lines, d.templateLine)
				pp := cs.Passed[d.param]
				out = append(out, CodeAction{Title: fmt.Sprintf("Rename to '%s'", s), Diagnostic: dp, Preferred: true,
					Edits: []TextEdit{{StartLine: pp.Line, StartCol: pp.NameCol, EndLine: pp.Line, EndCol: pp.NameCol + len(pp.Name), NewText: s}}})
			}
			cs := pipeline.ParseCallSite(f.lines, d.templateLine)
			pp := cs.Passed[d.param]
			del := TextEdit{StartLine: pp.DelStart.Line, StartCol: pp.DelStart.Col, EndLine: pp.DelEnd.Line, EndCol: pp.DelEnd.Col}
			if !pp.Flow {
				end := pp.Line
				if pp.IsBlock {
					end = pipeline.BlockEnd(f.lines, pp.Line)
				}
				del = deleteLines(f.lines, pp.Line, end)
			}
			out = append(out, CodeAction{Title: fmt.Sprintf("Remove unknown parameter '%s'", d.param), Diagnostic: dp,
				Edits: []TextEdit{del}, Preferred: len(d.suggestions) == 0})
		case CodeTypeMismatch:
			lit := pipeline.CanonicalLiteral(d.paramType)
			out = append(out, CodeAction{Title: fmt.Sprintf("Replace value with %s literal (%s)", d.paramType, lit), Diagnostic: dp, Preferred: true,
				Edits: []TextEdit{{StartLine: d.Line, StartCol: d.StartCol, EndLine: d.Line, EndCol: d.EndCol, NewText: lit}}})
		case CodeValueNotAllowed:
			for i, v := range d.values {
				if i >= 15 {
					break
				}
				out = append(out, CodeAction{Title: fmt.Sprintf("Change value to '%s'", v), Diagnostic: dp, Preferred: i == 0,
					Edits: []TextEdit{{StartLine: d.Line, StartCol: d.StartCol, EndLine: d.Line, EndCol: d.EndCol, NewText: yamlScalar(v)}}})
			}
		case CodeUnusedParam:
			end := pipeline.BlockEnd(f.lines, d.Line)
			out = append(out, CodeAction{Title: fmt.Sprintf("Remove unused parameter declaration '%s'", d.param), Diagnostic: dp,
				Edits: []TextEdit{deleteLines(f.lines, d.Line, end)}})
		case CodeUndeclaredParam:
			for _, sg := range d.suggestions {
				out = append(out, CodeAction{Title: fmt.Sprintf("Change to '%s'", sg), Diagnostic: dp, Preferred: true,
					Edits: []TextEdit{{StartLine: d.Line, StartCol: d.StartCol, EndLine: d.Line, EndCol: d.EndCol, NewText: sg}}})
			}
			if !declaredOnce[d.param] {
				declaredOnce[d.param] = true
				out = append(out, CodeAction{Title: fmt.Sprintf("Declare parameter '%s'", d.param), Diagnostic: dp,
					Preferred: len(d.suggestions) == 0, Edits: []TextEdit{declareParamEdit(f, d.param)}})
			}
		case CodeRepoUnavailable, CodeRepoFetching:
			title := fmt.Sprintf("Fetch repository '@%s'", d.alias)
			if strings.HasPrefix(d.Message, "Could not fetch") {
				title = fmt.Sprintf("Retry fetching repository '@%s'", d.alias)
			}
			out = append(out, CodeAction{Title: title, Diagnostic: dp, Command: "atn.fetchRepository",
				CommandArgs: []interface{}{RepoRequest{Alias: d.alias, File: path}}, Preferred: true})
		}
	}
	for tl, ds := range missingByCall {
		if len(ds) > 1 {
			out = append(out, CodeAction{Title: fmt.Sprintf("Add all %d missing required parameters", len(ds)),
				Diagnostic: &ds[0], Edits: addParamsEdit(f.lines, tl, ds)})
		}
	}
	return out
}

func yamlScalar(v string) string {
	if v == "" || strings.ContainsAny(v, ":#{}[],&*!|>'\"%@`") || strings.TrimSpace(v) != v {
		return "'" + strings.ReplaceAll(v, "'", "''") + "'"
	}
	return v
}

// addParamsEdit inserts `name: <literal>` entries into a call site's
// parameters block, creating the block when absent.
func addParamsEdit(lines []string, templateLine int, missing []Diagnostic) []TextEdit {
	tr, _ := pipeline.TemplateRefOnLine(lines[templateLine], templateLine)
	cs := pipeline.ParseCallSite(lines, templateLine)
	if cs.Flow != nil {
		return addFlowParamsEdit(cs, missing)
	}
	var entries []string
	if cs.ParamsLine < 0 {
		keyIndent := strings.Repeat(" ", tr.KeyCol)
		entryIndent := keyIndent + "  "
		text := "\n" + keyIndent + "parameters:"
		for _, d := range missing {
			text += "\n" + entryIndent + d.param + ": " + pipeline.CanonicalLiteral(d.paramType)
		}
		eol := len(lines[templateLine])
		return []TextEdit{{StartLine: templateLine, StartCol: eol, EndLine: templateLine, EndCol: eol, NewText: text}}
	}
	entryIndent := strings.Repeat(" ", pipeline.KeyColumn(lines[cs.ParamsLine])+2)
	if len(cs.Order) > 0 {
		first := cs.Passed[cs.Order[0]]
		entryIndent = strings.Repeat(" ", first.NameCol)
	}
	for _, d := range missing {
		entries = append(entries, entryIndent+d.param+": "+pipeline.CanonicalLiteral(d.paramType))
	}
	last := cs.LastLine
	eol := len(lines[last])
	return []TextEdit{{StartLine: last, StartCol: eol, EndLine: last, EndCol: eol, NewText: "\n" + strings.Join(entries, "\n")}}
}

// addFlowParamsEdit adds entries to `parameters: { ... }`: after the last
// entry, or inside empty braces. An unterminated mapping gets no edit.
func addFlowParamsEdit(cs pipeline.CallSite, missing []Diagnostic) []TextEdit {
	fm := cs.Flow
	if !fm.Closed {
		return nil
	}
	var entries []string
	for _, d := range missing {
		entries = append(entries, d.param+": "+pipeline.CanonicalLiteral(d.paramType))
	}
	text := strings.Join(entries, ", ")
	if n := len(fm.Entries); n > 0 {
		end := fm.Entries[n-1].End
		return []TextEdit{{StartLine: end.Line, StartCol: end.Col, EndLine: end.Line, EndCol: end.Col, NewText: ", " + text}}
	}
	return []TextEdit{{StartLine: fm.Open.Line, StartCol: fm.Open.Col + 1, EndLine: fm.Close.Line, EndCol: fm.Close.Col, NewText: " " + text + " "}}
}

// declareParamEdit appends a declaration for name to the file's parameters
// block, creating the block at the top of the file when there is none. The
// new parameter defaults to ” so callers keep today's behaviour (an
// undeclared parameter evaluates to an empty string).
func declareParamEdit(f *fileInfo, name string) TextEdit {
	start, end := pipeline.ParametersBlock(f.lines)
	if start < 0 {
		text := "parameters:\n  - name: " + name + "\n    type: string\n    default: ''\n\n"
		return TextEdit{StartLine: 0, StartCol: 0, EndLine: 0, EndCol: 0, NewText: text}
	}
	var text string
	if len(f.params) > 0 && f.params[0].Type == "" {
		// legacy map form: `name: default`
		text = "\n" + strings.Repeat(" ", f.params[0].NameCol) + name + ": ''"
	} else {
		ind := 2
		if len(f.params) > 0 {
			ind = pipeline.Indent(f.lines[f.params[0].Line])
		}
		pad, prop := strings.Repeat(" ", ind), strings.Repeat(" ", ind+2)
		text = "\n" + pad + "- name: " + name + "\n" + prop + "type: string\n" + prop + "default: ''"
	}
	eol := len(f.lines[end])
	return TextEdit{StartLine: end, StartCol: eol, EndLine: end, EndCol: eol, NewText: text}
}

// deleteLines removes whole lines [from, to] including the line break.
func deleteLines(lines []string, from, to int) TextEdit {
	if to+1 < len(lines) {
		return TextEdit{StartLine: from, StartCol: 0, EndLine: to + 1, EndCol: 0}
	}
	if from > 0 {
		return TextEdit{StartLine: from - 1, StartCol: len(lines[from-1]), EndLine: to, EndCol: len(lines[to])}
	}
	return TextEdit{StartLine: from, StartCol: 0, EndLine: to, EndCol: len(lines[to])}
}

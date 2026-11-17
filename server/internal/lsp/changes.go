package lsp

import (
	"encoding/json"

	"github.com/qSharpy/azure-templates-navigator/server/internal/engine"
)

// lspProblem is an engine.Problem with an LSP range (UTF-16) and the file's
// URI, as the views receive it.
type lspProblem struct {
	URI          string   `json:"uri"`
	FilePath     string   `json:"filePath"`
	RelativePath string   `json:"relativePath"`
	Range        lspRange `json:"range"`
	Severity     int      `json:"severity"`
	Code         string   `json:"code"`
	Message      string   `json:"message"`
	// New: your work added it (atn/pipelineCheck, when changes are on).
	New bool `json:"new,omitempty"`
}

func (s *Server) problems(ps []engine.Problem) []lspProblem {
	out := make([]lspProblem, 0, len(ps))
	for _, p := range ps {
		out = append(out, lspProblem{URI: pathToURI(p.Path), FilePath: p.Path, RelativePath: p.RelativePath,
			Range: s.rangeOf(p.Path, p.Line, p.StartCol, p.EndLine, p.EndCol), Severity: p.Severity, Code: p.Code, Message: p.Message, New: p.New})
	}
	return out
}

// changeSummary is what atn/pipelineCheck adds to a pipeline your work
// touched.
type changeSummary struct {
	Status string `json:"status"`
	Title  string `json:"title"`
	New    int    `json:"new"`
	Fixed  int    `json:"fixed"`
}

func summaryOf(c *engine.PipelineChange) *changeSummary {
	if c == nil {
		return nil
	}
	return &changeSummary{Status: c.Status, Title: c.Title(), New: len(c.New), Fixed: len(c.Fixed)}
}

// changes answers atn/changes: what the workspace changed compared with the
// base commit of each repository (engine/changes.go).
func (s *Server) changes() interface{} {
	r := s.eng.Changes()
	type file struct {
		URI          string       `json:"uri"`
		FilePath     string       `json:"filePath"`
		RelativePath string       `json:"relativePath"`
		Status       string       `json:"status"`
		Pipelines    int          `json:"pipelines"`
		New          []lspProblem `json:"new"`
		Fixed        []lspProblem `json:"fixed"`
	}
	type pipe struct {
		URI          string                 `json:"uri"`
		FilePath     string                 `json:"filePath"`
		RelativePath string                 `json:"relativePath"`
		Status       string                 `json:"status"`
		Title        string                 `json:"title"`
		Now          engine.PipelineVerdict `json:"now"`
		Before       engine.PipelineVerdict `json:"before"`
		New          []lspProblem           `json:"new"`
		Fixed        []lspProblem           `json:"fixed"`
		Existing     int                    `json:"existing"`
		Changed      []string               `json:"changed"`
	}
	out := map[string]interface{}{
		"available": r.Available,
		"reason":    r.Reason,
		"summary":   r.Summary(),
	}
	if !r.Available {
		return out
	}
	files := []file{}
	for _, f := range r.Files {
		files = append(files, file{URI: pathToURI(f.Path), FilePath: f.Path, RelativePath: f.RelativePath, Status: f.Status,
			Pipelines: f.Pipelines, New: s.problems(f.New), Fixed: s.fixedProblems(f.Fixed)})
	}
	pipes := []pipe{}
	for _, c := range r.Pipelines {
		pipes = append(pipes, pipe{URI: pathToURI(c.Path), FilePath: c.Path, RelativePath: c.RelativePath, Status: c.Status,
			Title: c.Title(), Now: c.Now, Before: c.Before, New: s.problems(c.New), Fixed: s.fixedProblems(c.Fixed),
			Existing: c.Existing, Changed: c.Changed})
	}
	out["bases"] = r.Bases
	out["label"] = r.Label
	out["files"] = files
	out["pipelines"] = pipes
	out["newErrors"] = r.NewErrors
	out["newWarnings"] = r.NewWarnings
	out["fixedProblems"] = r.FixedProblems
	out["nowFailing"] = r.NowFailing
	out["noLonger"] = r.NoLonger
	out["allPipelines"] = r.AllPipelines
	out["allFailing"] = r.AllFailing
	return out
}

// fixedProblems converts problems that only exist at the base: their lines
// are the base's, so the range is the line without columns (the workspace
// file's text may differ there).
func (s *Server) fixedProblems(ps []engine.Problem) []lspProblem {
	out := make([]lspProblem, 0, len(ps))
	for _, p := range ps {
		out = append(out, lspProblem{URI: pathToURI(p.Path), FilePath: p.Path, RelativePath: p.RelativePath,
			Range:    lspRange{Start: position{Line: p.Line}, End: position{Line: p.Line}},
			Severity: p.Severity, Code: p.Code, Message: p.Message})
	}
	return out
}

// setChangesBase answers atn/setChangesBase {ref}: "" = automatic, "HEAD" =
// uncommitted changes only, else a branch or commit. It returns the new
// comparison, and the views reload through atn/didChange.
func (s *Server) setChangesBase(raw json.RawMessage) (interface{}, *rpcError) {
	var p struct {
		Ref string `json:"ref"`
	}
	_ = json.Unmarshal(raw, &p)
	if err := s.eng.SetChangesBase(p.Ref); err != nil {
		return nil, &rpcError{Code: -32602, Message: err.Error()}
	}
	result := s.changes()
	s.pubMu.Lock()
	s.redim()
	s.pubMu.Unlock()
	_ = s.c.notify("atn/didChange", map[string]interface{}{"reason": "changes"})
	if s.lensRefresh {
		s.clientRequest("workspace/codeLens/refresh", nil)
	}
	return result, nil
}

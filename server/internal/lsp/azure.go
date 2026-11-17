package lsp

import (
	"context"
	"encoding/json"
	"time"

	"github.com/qSharpy/azure-templates-navigator/server/internal/engine"
)

// Check in Azure DevOps (engine/azure.go). The client asks for a plan
// (atn/azurePlan: reads only), shows it for consent, and starts the check
// with the plan's id (atn/azureValidate), which the server re-plans and
// compares, so nothing is sent that the user didn't see. The check runs in
// the background: atn/azureProgress while it runs, atn/didChange {reason:
// "azure"} when it is done, and atn/azureResults for the outcome.

type azureParams struct {
	URIs   []string `json:"uris"`
	PlanID string   `json:"planId"`
	URI    string   `json:"uri"`
}

func (s *Server) azureOptions(p azureParams) engine.AzureOptions {
	var opt engine.AzureOptions
	for _, u := range p.URIs {
		opt.Only = append(opt.Only, uriToPath(u))
	}
	return opt
}

// azurePlan answers atn/azurePlan {uris?}: the plan, or {error}.
func (s *Server) azurePlan(raw json.RawMessage) interface{} {
	var p azureParams
	_ = json.Unmarshal(raw, &p)
	if !s.eng.Settings().Azure.Enabled {
		return map[string]interface{}{"error": "Checking in Azure DevOps is turned off (azure.enabled)", "disabled": true}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	plan, err := s.eng.AzurePlan(ctx, s.azureOptions(p))
	if err != nil {
		return map[string]interface{}{"error": err.Error()}
	}
	type pipe struct {
		engine.AzurePlanPipeline
		URI string `json:"uri"`
	}
	pipes := []pipe{}
	for _, x := range plan.Pipelines {
		pipes = append(pipes, pipe{x, pathToURI(x.Path)})
	}
	return map[string]interface{}{
		"id": plan.ID, "organization": plan.Organization, "project": plan.Project, "user": plan.User, "base": plan.Base,
		"pipelines": pipes, "snapshots": plan.Snapshots, "summary": plan.Summary, "checkable": plan.Checkable(),
	}
}

// azureValidate answers atn/azureValidate {planId, uris?}: {started: true}
// or {error}.
func (s *Server) azureValidate(raw json.RawMessage) interface{} {
	var p azureParams
	_ = json.Unmarshal(raw, &p)
	if !s.eng.Settings().Azure.Enabled {
		return map[string]interface{}{"error": "Checking in Azure DevOps is turned off (azure.enabled)"}
	}
	if p.PlanID == "" {
		return map[string]interface{}{"error": "no plan: ask for one with atn/azurePlan and confirm it first"}
	}
	progress := func(done, total int, msg string) {
		_ = s.c.notify("atn/azureProgress", map[string]interface{}{"done": done, "total": total, "message": msg})
	}
	finished := func(r *engine.AzureReport) {
		s.logf("[azure] %s", r.Summary())
		_ = s.c.notify("atn/azureProgress", map[string]interface{}{"done": 1, "total": 1, "message": r.Summary(), "finished": true, "state": r.State})
		_ = s.c.notify("atn/didChange", map[string]interface{}{"reason": "azure"})
		if s.lensRefresh {
			s.clientRequest("workspace/codeLens/refresh", nil)
		}
	}
	if err := s.eng.AzureStart(p.PlanID, s.azureOptions(p), progress, finished); err != nil {
		return map[string]interface{}{"error": err.Error()}
	}
	_ = s.c.notify("atn/didChange", map[string]interface{}{"reason": "azure"})
	return map[string]interface{}{"started": true}
}

// azProblem is an Azure problem as the views receive it.
type azProblem struct {
	URI            string   `json:"uri"`
	FilePath       string   `json:"filePath"`
	RelativePath   string   `json:"relativePath"`
	Range          lspRange `json:"range"`
	Message        string   `json:"message"`
	AzurePath      string   `json:"azurePath,omitempty"`
	AlsoFoundByAtn bool     `json:"alsoFoundByAtn"`
}

func (s *Server) azProblems(ps []engine.AzureProblem) []azProblem {
	out := make([]azProblem, 0, len(ps))
	for _, p := range ps {
		out = append(out, azProblem{URI: pathToURI(p.Path), FilePath: p.Path, RelativePath: p.RelativePath,
			Range: s.rangeOf(p.Path, p.Line, p.Col, p.Line, p.Col), Message: p.Message, AzurePath: p.AzurePath, AlsoFoundByAtn: p.AlsoFoundByAtn})
	}
	return out
}

// azurePipeline is a pipeline's result as the views receive it, also inside
// atn/changes and atn/pipelineCheck.
type azurePipeline struct {
	URI          string                       `json:"uri"`
	FilePath     string                       `json:"filePath"`
	RelativePath string                       `json:"relativePath"`
	Title        string                       `json:"title"`
	Skip         string                       `json:"skip,omitempty"`
	Definition   *engine.AzureDefinition      `json:"definition,omitempty"`
	Verdict      string                       `json:"verdict"` // compiles | rejected | unknown | ""
	Before       string                       `json:"before"`
	Error        string                       `json:"error,omitempty"`
	Problems     []azProblem                  `json:"problems"`
	New          []azProblem                  `json:"new"`
	Fixed        []azProblem                  `json:"fixed"`
	Existing     int                          `json:"existing"`
	Agreement    string                       `json:"agreement"`
	Atn          engine.PipelineVerdict       `json:"atn"`
	Expansion    *engine.AzureExpansionChange `json:"expansion,omitempty"`
	Parameters   map[string]interface{}       `json:"parameters,omitempty"`
	Stale        bool                         `json:"stale"`
}

func (s *Server) azurePipeline(r engine.AzurePipelineResult, stale bool) azurePipeline {
	return azurePipeline{URI: pathToURI(r.Path), FilePath: r.Path, RelativePath: r.RelativePath, Title: r.Title(), Skip: r.Skip, Definition: r.Definition,
		Verdict: r.Now.Verdict, Before: r.Before.Verdict, Error: r.Now.Error, Problems: s.azProblems(r.Now.Problems),
		New: s.azProblems(r.New), Fixed: s.azProblems(r.Fixed), Existing: r.Existing, Agreement: r.Agreement, Atn: r.Atn,
		Expansion: r.Expansion, Parameters: r.Parameters, Stale: stale}
}

// azureFor is a pipeline's entry in the last report (for atn/changes and
// atn/pipelineCheck), nil when it has none.
func (s *Server) azureFor(last *engine.AzureReport, path string) *azurePipeline {
	if last == nil || last.State != "done" {
		return nil
	}
	r, ok := last.Result(path)
	if !ok {
		return nil
	}
	p := s.azurePipeline(r, last.Stale)
	return &p
}

// azureResults answers atn/azureResults: the last report, or null.
func (s *Server) azureResults() interface{} {
	r := s.eng.AzureLast()
	if r == nil {
		return nil
	}
	pipes := []azurePipeline{}
	for _, p := range r.Pipelines {
		pipes = append(pipes, s.azurePipeline(p, r.Stale))
	}
	return map[string]interface{}{
		"state": r.State, "error": r.Error, "summary": r.Summary(), "organization": r.Organization, "project": r.Project,
		"base": r.Base, "startedAt": r.StartedAt, "finishedAt": r.FinishedAt, "stale": r.Stale, "totals": r.Totals,
		"pipelines": pipes, "warnings": r.Warnings,
	}
}

// azureExpansion answers atn/azureExpansion {uri}: the expanded YAML at the
// base and with your changes, for the client's diff editor.
func (s *Server) azureExpansion(raw json.RawMessage) interface{} {
	var p azureParams
	_ = json.Unmarshal(raw, &p)
	path := uriToPath(p.URI)
	base, change, ok := s.eng.AzureExpansion(path)
	if !ok {
		return nil
	}
	title := path
	if r := s.eng.AzureLast(); r != nil {
		if res, ok := r.Result(path); ok {
			title = res.RelativePath
		}
	}
	return map[string]interface{}{"base": base, "change": change, "title": title}
}

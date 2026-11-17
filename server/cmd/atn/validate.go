package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/qSharpy/azure-templates-navigator/server/internal/azure"
	"github.com/qSharpy/azure-templates-navigator/server/internal/engine"
)

// runValidate is `atn validate`: Azure DevOps compiles every pipeline your
// changes reach, at the base and with your changes (engine/azure.go). Exit
// code 1 when your changes make Azure reject a pipeline, 2 when the check
// couldn't be made (or a pipeline couldn't be checked).
func runValidate(args []string) int {
	fs := flag.NewFlagSet("validate", flag.ExitOnError)
	var c common
	c.register(fs)
	since := fs.String("since", "auto", "compare with this branch, commit, HEAD or auto")
	yes := fs.Bool("yes", false, "don't ask before sending your changes to Azure DevOps")
	format := fs.String("format", "text", "text | json")
	diff := fs.Bool("diff", false, "print how each expanded pipeline changed")
	keep := fs.Bool("keep-snapshots", false, "keep the refs/atn/preview/… refs after the check")
	noFetch := fs.Bool("no-fetch", false, "don't fetch missing repositories")
	_ = fs.Parse(args)

	e, err := c.engine(false)
	if err != nil {
		fmt.Fprintln(os.Stderr, "atn:", err)
		return 2
	}
	if *keep {
		s := e.Settings()
		s.Azure.KeepSnapshots = true
		e.SetSettings(s)
	}
	if !*noFetch {
		fetchAll(e, true)
	}
	var only []string
	for _, a := range fs.Args() {
		if p, err := filepath.Abs(a); err == nil {
			only = append(only, p)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	opt := engine.AzureOptions{Since: *since, Explicit: true, Only: only}
	if *since == "auto" {
		opt.Since = ""
	}
	plan, err := e.AzurePlan(ctx, opt)
	if err != nil {
		fmt.Fprintf(os.Stderr, "atn: can't check in Azure DevOps: %v\n", err)
		return 2
	}
	root, _ := filepath.Abs(c.root)
	printPlan(plan, root)
	if plan.Checkable() == 0 {
		fmt.Fprintln(os.Stderr, "Nothing to check.")
		return 0
	}
	if !*yes {
		if st, err := os.Stdin.Stat(); err != nil || st.Mode()&os.ModeCharDevice == 0 {
			fmt.Fprintln(os.Stderr, "atn: pass --yes to send your changes to Azure DevOps")
			return 2
		}
		fmt.Fprint(os.Stderr, "Go ahead? [y/N] ")
		ans, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(ans)); a != "y" && a != "yes" {
			return 2
		}
	}
	rep := e.AzureRun(ctx, plan, func(done, total int, msg string) {
		if *format == "text" {
			fmt.Fprintf(os.Stderr, "  [%d/%d] %s\n", done, total, msg)
		}
	})
	for _, w := range rep.Warnings {
		fmt.Fprintln(os.Stderr, "atn: warning:", w)
	}
	if rep.State != "done" {
		fmt.Fprintln(os.Stderr, "atn:", rep.Summary())
		return 2
	}

	if *format == "json" {
		printValidateJSON(e, rep, root, *diff)
	} else {
		printValidateText(e, rep, root, *diff)
	}
	switch {
	case rep.Totals.WithNewProblems > 0:
		return 1
	case rep.Totals.Unknown > 0:
		return 2
	}
	return 0
}

func printPlan(p *engine.AzurePlan, root string) {
	fmt.Fprintf(os.Stderr, "Azure DevOps %s/%s, as %s, comparing with %s\n", p.Organization, p.Project, p.User, p.Base)
	for _, s := range p.Snapshots {
		fmt.Fprintf(os.Stderr, "  push to %s in %s:\n", s.Ref, s.Repo)
		for _, f := range s.Files {
			fmt.Fprintf(os.Stderr, "    %-8s %s\n", f.Status, f.RelativePath)
		}
		if s.UnpushedBase {
			fmt.Fprintf(os.Stderr, "    (the base %.10s doesn't seem to be on the server: its history is uploaded too)\n", s.Base)
		}
	}
	for _, x := range p.Pipelines {
		if x.Skip != "" {
			fmt.Fprintf(os.Stderr, "  skip %s: %s\n", x.RelativePath, x.Skip)
		} else {
			fmt.Fprintf(os.Stderr, "  check %s with pipeline #%d %s\n", x.RelativePath, x.Definition.ID, x.Definition.Name)
		}
	}
	fmt.Fprintln(os.Stderr, p.Summary)
}

func printValidateText(e *engine.Engine, rep *engine.AzureReport, root string, diff bool) {
	width := 0
	for _, r := range rep.Pipelines {
		if n := len(r.RelativePath); n > width {
			width = n
		}
	}
	fmt.Fprintln(os.Stderr)
	for _, r := range rep.Pipelines {
		fmt.Fprintf(os.Stderr, "  %-*s  %s\n", width, r.RelativePath, r.Title())
	}
	for _, r := range rep.Pipelines {
		for _, p := range r.New {
			fmt.Printf("%s:%d:%d: error: %s [azure]\n", p.RelativePath, p.Line+1, p.Col+1, p.Message)
		}
		if r.Agreement == "atnOnly" {
			fmt.Fprintf(os.Stderr, "note: %s: Azure compiles it, but atn reports %d error(s); they may still fail the run when it is queued\n", r.RelativePath, r.Atn.Errors)
		}
	}
	if diff {
		for _, r := range rep.Pipelines {
			if r.Expansion == nil || !r.Expansion.Changed {
				continue
			}
			a, b, _ := e.AzureExpansion(r.Path)
			rel := r.RelativePath
			fmt.Print(azure.Unified(azure.Diff(azure.SplitLines(a), azure.SplitLines(b)), rel+" (expanded, base)", rel+" (expanded, your changes)", 3))
		}
	}
	fmt.Fprintf(os.Stderr, "\n%s\n", rep.Summary())
}

func printValidateJSON(e *engine.Engine, rep *engine.AzureReport, root string, diff bool) {
	type problem struct {
		File           string `json:"file"`
		Line           int    `json:"line"`
		Column         int    `json:"column"`
		Message        string `json:"message"`
		AzurePath      string `json:"azurePath,omitempty"`
		AlsoFoundByAtn bool   `json:"alsoFoundByAtn"`
	}
	type pipe struct {
		File       string                       `json:"file"`
		Skip       string                       `json:"skip,omitempty"`
		Definition *engine.AzureDefinition      `json:"definition,omitempty"`
		Verdict    string                       `json:"verdict"`
		Before     string                       `json:"before"`
		Error      string                       `json:"error,omitempty"`
		Agreement  string                       `json:"agreement"`
		Atn        engine.PipelineVerdict       `json:"atn"`
		New        []problem                    `json:"new"`
		Fixed      []problem                    `json:"fixed"`
		Existing   int                          `json:"existing"`
		Expansion  *engine.AzureExpansionChange `json:"expansion,omitempty"`
		Diff       string                       `json:"diff,omitempty"`
	}
	conv := func(ps []engine.AzureProblem) []problem {
		out := []problem{}
		for _, p := range ps {
			out = append(out, problem{p.RelativePath, p.Line + 1, p.Col + 1, p.Message, p.AzurePath, p.AlsoFoundByAtn})
		}
		return out
	}
	out := struct {
		Organization string             `json:"organization"`
		Project      string             `json:"project"`
		Base         string             `json:"base"`
		Summary      string             `json:"summary"`
		Totals       engine.AzureTotals `json:"totals"`
		Pipelines    []pipe             `json:"pipelines"`
		Warnings     []string           `json:"warnings"`
	}{rep.Organization, rep.Project, rep.Base, rep.Summary(), rep.Totals, []pipe{}, rep.Warnings}
	for _, r := range rep.Pipelines {
		p := pipe{File: r.RelativePath, Skip: r.Skip, Definition: r.Definition, Verdict: r.Now.Verdict, Before: r.Before.Verdict,
			Error: r.Now.Error, Agreement: r.Agreement, Atn: r.Atn, New: conv(r.New), Fixed: conv(r.Fixed), Existing: r.Existing, Expansion: r.Expansion}
		if diff && r.Expansion != nil && r.Expansion.Changed {
			a, b, _ := e.AzureExpansion(r.Path)
			p.Diff = azure.Unified(azure.Diff(azure.SplitLines(a), azure.SplitLines(b)), "base", "change", 3)
		}
		out.Pipelines = append(out.Pipelines, p)
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(b))
}

// Command atn is the Azure Templates Navigator engine: a language server for
// IDE plugins and a CLI for terminals / CI pipelines.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/qSharpy/azure-templates-navigator/server/internal/engine"
	"github.com/qSharpy/azure-templates-navigator/server/internal/lsp"
	"github.com/qSharpy/azure-templates-navigator/server/internal/repos"
)

var version = "dev"

const usage = `atn — Azure Templates Navigator %s

Usage:
  atn lsp                      Run the language server on stdio (used by IDE plugins)
  atn check [flags] [files…]   Find what would make a pipeline fail before you run it; exit code 1 on errors.
                               A pipeline given as a file is checked with every template it uses.
  atn fetch [flags]            Download every repository referenced by resources.repositories
  atn repos [flags]            List referenced repositories and where they resolve
  atn validate [flags] [pipelines…]
                               Ask Azure DevOps to compile the pipelines your changes reach, at the
                               base and with your changes (no run is created). Pushes your changed
                               YAML to refs/atn/preview/… first. Exit code 1 when your changes make
                               Azure reject a pipeline, 2 when the check can't be made.
  atn version                  Print the version

Common flags:
  --root DIR        Workspace root (default: current directory); repeat it for several
  --config FILE     Settings JSON (same shape as the IDE settings)
  --cache-dir DIR   Repository cache (default: ~/.azure-templates-navigator/repos)

check flags:
  --no-fetch        Don't download missing repositories
  --format FMT      text (default) | json | azure (Azure Pipelines logging commands)
  --strict          Treat warnings as errors
  --since REF       Only report problems that are new since REF, and fail only on those:
                    a branch (where HEAD splits from it), a commit, HEAD (uncommitted
                    changes only) or auto (where this branch started). Needs the history
                    back to REF: in Azure Pipelines, checkout with fetchDepth: 0.

validate flags:
  --since REF       The base to compare with (default auto), as for check
  --yes             Don't ask before pushing and previewing (required without a terminal)
  --format FMT      text (default) | json
  --diff            Print how each expanded pipeline changed (unified diff)
  --keep-snapshots  Keep the refs/atn/preview/… refs after the check
  --no-fetch        Don't download missing repositories
  Signs in with ATN_AZURE_TOKEN or SYSTEM_ACCESSTOKEN when set, else the Azure CLI
  (az login --allow-no-subscriptions).
`

func main() {
	lsp.Version = version
	args := os.Args[1:]
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, usage, version)
		os.Exit(2)
	}
	cmd := args[0]
	if cmd == "--stdio" || cmd == "lsp" || cmd == "serve" {
		if err := lsp.Serve(os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "atn:", err)
			os.Exit(1)
		}
		return
	}
	switch cmd {
	case "version", "--version", "-v":
		fmt.Println(version)
	case "check":
		os.Exit(runCheck(args[1:]))
	case "fetch":
		os.Exit(runFetch(args[1:]))
	case "repos":
		os.Exit(runRepos(args[1:]))
	case "validate":
		os.Exit(runValidate(args[1:]))
	case "help", "-h", "--help":
		fmt.Printf(usage, version)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n"+usage, cmd, version)
		os.Exit(2)
	}
}

type common struct {
	root, config, cacheDir string
	roots                  rootList
}

// rootList is --root, which may be given more than once (a workspace of
// several repositories, e.g. templates and the pipelines that use them).
type rootList []string

func (r *rootList) String() string { return strings.Join(*r, ",") }
func (r *rootList) Set(v string) error {
	*r = append(*r, v)
	return nil
}

func (c *common) register(fs *flag.FlagSet) {
	fs.Var(&c.roots, "root", "workspace root (repeatable; default: current directory)")
	fs.StringVar(&c.config, "config", "", "settings JSON file")
	fs.StringVar(&c.cacheDir, "cache-dir", "", "repository cache directory")
}

func (c *common) engine(autoFetch bool) (*engine.Engine, error) {
	var raw json.RawMessage
	if c.config != "" {
		b, err := os.ReadFile(c.config)
		if err != nil {
			return nil, err
		}
		raw = b
	}
	s := engine.ParseSettings(raw)
	if c.cacheDir != "" {
		s.Repositories.CacheDir = c.cacheDir
	}
	s.Repositories.AutoFetch = autoFetch
	if len(c.roots) == 0 {
		c.roots = rootList{"."}
	}
	c.root = c.roots[0]
	var roots []string
	for _, r := range c.roots {
		abs, err := filepath.Abs(r)
		if err != nil {
			return nil, err
		}
		roots = append(roots, abs)
	}
	e := engine.New(roots, s)
	e.Rescan()
	return e, nil
}

func fetchAll(e *engine.Engine, quiet bool) int {
	failures := 0
	for _, rd := range e.RepoDecls() {
		loc := e.Repos.Locate(rd.Decl, []string{filepath.Dir(engine.FindRepoRoot(filepath.Dir(rd.File)))}, rd.Remote)
		if loc.Source == repos.SourceSibling || loc.Source == repos.SourceLocal {
			if !quiet {
				fmt.Fprintf(os.Stderr, "• %-40s %s (%s)\n", rd.Decl.Name, loc.Dir, loc.Source)
			}
			continue
		}
		if !quiet {
			fmt.Fprintf(os.Stderr, "↓ %-40s fetching %s…\n", rd.Decl.Name, refOr(rd.Decl.Ref))
		}
		l, err := e.Repos.FetchSync(rd.Decl, rd.Remote)
		if err != nil {
			failures++
			fmt.Fprintf(os.Stderr, "✗ %-40s %v\n", rd.Decl.Name, err)
			continue
		}
		if !quiet {
			fmt.Fprintf(os.Stderr, "✓ %-40s %s @ %.10s\n", rd.Decl.Name, l.Dir, l.Commit)
		}
	}
	e.Invalidate()
	return failures
}

func refOr(r string) string {
	if r == "" {
		return "default branch"
	}
	return r
}

func runFetch(args []string) int {
	fs := flag.NewFlagSet("fetch", flag.ExitOnError)
	var c common
	c.register(fs)
	_ = fs.Parse(args)
	e, err := c.engine(false)
	if err != nil {
		fmt.Fprintln(os.Stderr, "atn:", err)
		return 2
	}
	if fetchAll(e, false) > 0 {
		return 1
	}
	return 0
}

func runRepos(args []string) int {
	fs := flag.NewFlagSet("repos", flag.ExitOnError)
	var c common
	c.register(fs)
	_ = fs.Parse(args)
	e, err := c.engine(false)
	if err != nil {
		fmt.Fprintln(os.Stderr, "atn:", err)
		return 2
	}
	for _, r := range e.Repositories() {
		where := r.Location.Dir
		if r.Location.Error != "" {
			where = r.Location.Error
		}
		fmt.Printf("%-12s %-45s %-10s %s\n", "@"+r.Alias, r.Name, r.Location.State, where)
	}
	return 0
}

func runCheck(args []string) int {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	var c common
	c.register(fs)
	noFetch := fs.Bool("no-fetch", false, "don't fetch missing repositories")
	format := fs.String("format", "text", "text | json | azure")
	strict := fs.Bool("strict", false, "treat warnings as errors")
	since := fs.String("since", "", "only report problems that are new since this branch, commit, HEAD or auto")
	_ = fs.Parse(args)

	e, err := c.engine(false)
	if err != nil {
		fmt.Fprintln(os.Stderr, "atn:", err)
		return 2
	}
	if !*noFetch {
		fetchAll(e, *format != "text")
	}

	if *since != "" {
		return runCheckSince(e, *since, *format, *strict, c.root, fs.Args())
	}

	all := e.DiagnoseAll()
	only := map[string]bool{}
	reports := e.PipelineChecks()
	for _, a := range fs.Args() {
		p, err := filepath.Abs(a)
		if err != nil {
			continue
		}
		only[p] = true
		// a pipeline brings its templates, wherever they live
		if rep, ok := e.PipelineCheck(p); ok {
			for _, f := range rep.Files {
				only[f] = true
				if _, ok := all[f]; !ok {
					all[f] = e.Diagnose(f)
				}
			}
		}
	}
	root, _ := filepath.Abs(c.root)
	var paths []string
	for p := range all {
		if len(only) == 0 || only[p] {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)

	errorsN, warningsN := 0, 0
	type jd struct {
		File     string `json:"file"`
		Line     int    `json:"line"`
		Column   int    `json:"column"`
		Severity string `json:"severity"`
		Code     string `json:"code"`
		Message  string `json:"message"`
	}
	var jout []jd
	sevName := map[int]string{1: "error", 2: "warning", 3: "info", 4: "hint"}
	for _, p := range paths {
		rel, err := filepath.Rel(root, p)
		if err != nil {
			rel = p
		}
		rel = filepath.ToSlash(rel)
		for _, d := range all[p] {
			sev := d.Severity
			if sev > engine.SevWarning {
				continue
			}
			if sev == engine.SevWarning && *strict {
				sev = engine.SevError
			}
			if sev == engine.SevError {
				errorsN++
			} else {
				warningsN++
			}
			switch *format {
			case "json":
				jout = append(jout, jd{rel, d.Line + 1, d.StartCol + 1, sevName[sev], d.Code, d.Message})
			case "azure":
				fmt.Printf("##vso[task.logissue type=%s;sourcepath=%s;linenumber=%d;columnnumber=%d;code=%s]%s\n",
					sevName[sev], rel, d.Line+1, d.StartCol+1, d.Code, strings.ReplaceAll(d.Message, "\n", " "))
			default:
				fmt.Printf("%s:%d:%d: %s: %s [%s]\n", rel, d.Line+1, d.StartCol+1, sevName[sev], d.Message, d.Code)
			}
		}
	}
	switch *format {
	case "json":
		if jout == nil {
			jout = []jd{}
		}
		b, _ := json.MarshalIndent(jout, "", "  ")
		fmt.Println(string(b))
	case "text":
		fmt.Fprintf(os.Stderr, "\n%d file(s) checked: %d error(s), %d warning(s)\n", len(paths), errorsN, warningsN)
		printVerdicts(reports, only, root)
	}
	if errorsN > 0 {
		return 1
	}
	return 0
}

// printVerdicts lists every root pipeline (or the ones asked for) with a
// one-line answer to "will it run?".
func printVerdicts(reports []engine.PipelineReport, only map[string]bool, root string) {
	var shown []engine.PipelineReport
	width := 0
	for _, r := range reports {
		if len(only) > 0 && !only[r.Path] {
			continue
		}
		shown = append(shown, r)
		if n := len(relTo(root, r.Path)); n > width {
			width = n
		}
	}
	if len(shown) == 0 {
		return
	}
	fmt.Fprintf(os.Stderr, "\nPipelines:\n")
	for _, r := range shown {
		fmt.Fprintf(os.Stderr, "  %-*s  %s\n", width, relTo(root, r.Path), r.Title())
	}
}

func relTo(root, p string) string {
	rel, err := filepath.Rel(root, p)
	if err != nil || strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(p)
	}
	return filepath.ToSlash(rel)
}

// runCheckSince is `check --since`: the problems your work added since a
// commit, for a gate that teams adopt before the existing problems are
// fixed. Exit code 1 on new errors (or new warnings with --strict), 2 when
// the comparison can't be made.
func runCheckSince(e *engine.Engine, since, format string, strict bool, rootFlag string, files []string) int {
	r, err := e.ChangesSince(since)
	if err != nil {
		fmt.Fprintf(os.Stderr, "atn: can't compare with %s: %v\n", since, err)
		return 2
	}
	if format != "text" {
		fmt.Fprintf(os.Stderr, "atn: comparing with %s: %s\n", r.Label, r.Summary())
	}
	root, _ := filepath.Abs(rootFlag)
	only := map[string]bool{}
	for _, a := range files {
		if p, err := filepath.Abs(a); err == nil {
			only[p] = true
		}
	}
	// every new problem once: a template's problem reaches several pipelines
	type key struct {
		path      string
		line, col int
		code, msg string
	}
	seen := map[key]bool{}
	var news []engine.Problem
	add := func(scope string, ps []engine.Problem) {
		if len(only) > 0 && !only[scope] {
			return
		}
		for _, p := range ps {
			k := key{p.Path, p.Line, p.StartCol, p.Code, p.Message}
			if !seen[k] {
				seen[k] = true
				news = append(news, p)
			}
		}
	}
	for _, c := range r.Pipelines {
		add(c.Path, c.New)
	}
	for _, f := range r.Files {
		add(f.Path, f.New)
	}
	sort.SliceStable(news, func(i, j int) bool {
		if news[i].Path != news[j].Path {
			return news[i].Path < news[j].Path
		}
		return news[i].Line < news[j].Line
	})

	sevName := map[int]string{1: "error", 2: "warning"}
	errorsN, warningsN := 0, 0
	type jd struct {
		File     string `json:"file"`
		Line     int    `json:"line"`
		Column   int    `json:"column"`
		Severity string `json:"severity"`
		Code     string `json:"code"`
		Message  string `json:"message"`
	}
	jout := []jd{}
	for _, p := range news {
		sev := p.Severity
		if sev == engine.SevWarning && strict {
			sev = engine.SevError
		}
		if sev == engine.SevError {
			errorsN++
		} else {
			warningsN++
		}
		rel := relTo(root, p.Path)
		switch format {
		case "json":
			jout = append(jout, jd{rel, p.Line + 1, p.StartCol + 1, sevName[sev], p.Code, p.Message})
		case "azure":
			fmt.Printf("##vso[task.logissue type=%s;sourcepath=%s;linenumber=%d;columnnumber=%d;code=%s]%s\n",
				sevName[sev], rel, p.Line+1, p.StartCol+1, p.Code, strings.ReplaceAll(p.Message, "\n", " "))
		default:
			fmt.Printf("%s:%d:%d: %s: %s [%s]\n", rel, p.Line+1, p.StartCol+1, sevName[sev], p.Message, p.Code)
		}
	}
	switch format {
	case "json":
		b, _ := json.MarshalIndent(jout, "", "  ")
		fmt.Println(string(b))
	case "text":
		fmt.Fprintf(os.Stderr, "\nSince %s: %s\n", r.Label, r.Summary())
		if r.AllPipelines > 0 {
			fmt.Fprintf(os.Stderr, "All pipelines: %d of %d fail when queued\n", r.AllFailing, r.AllPipelines)
		}
		width := 0
		for _, c := range r.Pipelines {
			if n := len(relTo(root, c.Path)); n > width {
				width = n
			}
		}
		if len(r.Pipelines) > 0 {
			fmt.Fprintf(os.Stderr, "\nPipelines your changes reach:\n")
		}
		for _, c := range r.Pipelines {
			mark := map[string]string{"fail": "✗", "warn": "⚠", "ok": "✓", "": "–"}[c.Now.Verdict]
			fmt.Fprintf(os.Stderr, "  %s %-*s  %s\n", mark, width, relTo(root, c.Path), c.Title())
		}
	}
	if errorsN > 0 {
		return 1
	}
	return 0
}

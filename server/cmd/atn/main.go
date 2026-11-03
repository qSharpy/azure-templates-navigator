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
  atn check [flags] [files…]   Validate template call sites; exit code 1 on errors
  atn fetch [flags]            Download every repository referenced by resources.repositories
  atn repos [flags]            List referenced repositories and where they resolve
  atn version                  Print the version

Common flags:
  --root DIR        Workspace root (default: current directory)
  --config FILE     Settings JSON (same shape as the IDE settings)
  --cache-dir DIR   Repository cache (default: ~/.azure-templates-navigator/repos)

check flags:
  --no-fetch        Don't download missing repositories
  --format FMT      text (default) | json | azure (Azure Pipelines logging commands)
  --strict          Treat warnings as errors
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
	case "help", "-h", "--help":
		fmt.Printf(usage, version)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n"+usage, cmd, version)
		os.Exit(2)
	}
}

type common struct {
	root, config, cacheDir string
}

func (c *common) register(fs *flag.FlagSet) {
	fs.StringVar(&c.root, "root", ".", "workspace root")
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
	root, err := filepath.Abs(c.root)
	if err != nil {
		return nil, err
	}
	e := engine.New([]string{root}, s)
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
	_ = fs.Parse(args)

	e, err := c.engine(false)
	if err != nil {
		fmt.Fprintln(os.Stderr, "atn:", err)
		return 2
	}
	if !*noFetch {
		fetchAll(e, *format != "text")
	}

	all := e.DiagnoseAll()
	only := map[string]bool{}
	for _, a := range fs.Args() {
		if p, err := filepath.Abs(a); err == nil {
			only[p] = true
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
	}
	if errorsN > 0 {
		return 1
	}
	return 0
}

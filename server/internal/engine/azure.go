package engine

// Check in Azure DevOps (docs/design/phase2-design.md, Phase 2a): every root
// pipeline that "My changes" lists is compiled by Azure DevOps itself, through
// the pipelines preview API, once at the base and once with your changes.
// Your changed YAML reaches Azure's git server first, as one snapshot commit
// per repository pushed to a custom ref (package azure). The result is
// Azure's verdict, the problems your change added in Azure's eyes, whether
// atn agrees, and how the expanded pipeline changed.
//
// Nothing is sent before a plan was shown and confirmed: Plan only reads (who
// you are, the repositories, the definitions), and Run takes the plan.
//
// Locks: the inputs are gathered with the usual chMu → mu, then released.
// Network calls and git pushes run without engine locks. The last report and
// the running job are under azMu, a leaf.

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/qSharpy/azure-templates-navigator/server/internal/azure"
	"github.com/qSharpy/azure-templates-navigator/server/internal/pipeline"
	"github.com/qSharpy/azure-templates-navigator/server/internal/repos"
)

// AzureDefinition is the pipeline definition a root pipeline is previewed
// with.
type AzureDefinition struct {
	ID     int    `json:"id"`
	Name   string `json:"name"`
	URL    string `json:"url"`
	Others []int  `json:"others"` // other definitions of the same file
}

// AzurePlanPipeline is a pipeline the plan checks, or skips (Skip says why).
type AzurePlanPipeline struct {
	Path         string           `json:"path"`
	RelativePath string           `json:"relativePath"`
	Definition   *AzureDefinition `json:"definition,omitempty"`
	Skip         string           `json:"skip,omitempty"`
	// YAMLOverride: the changed file is sent in the preview request
	// (yamlOverride) instead of being pushed.
	YAMLOverride bool `json:"yamlOverride,omitempty"`
}

// AzurePlanFile is a file a snapshot uploads.
type AzurePlanFile struct {
	RelativePath string `json:"relativePath"` // to the repository top
	Status       string `json:"status"`       // added | modified | deleted
}

// AzurePlanSnapshot is what is pushed to one repository.
type AzurePlanSnapshot struct {
	Repo         string          `json:"repo"`
	RemoteURL    string          `json:"remoteUrl"`
	Ref          string          `json:"ref"`
	Base         string          `json:"base"` // commit
	Files        []AzurePlanFile `json:"files"`
	UnpushedBase bool            `json:"unpushedBase"` // the base may not be on the server: its history is uploaded too
	// GitHub: the repository is on GitHub and is pushed to with the user's
	// own git credentials.
	GitHub bool `json:"github,omitempty"`
}

// AzurePlan is what a check will do, shown for consent before anything is
// sent.
type AzurePlan struct {
	ID           string              `json:"id"`
	Organization string              `json:"organization"`
	Project      string              `json:"project"`
	User         string              `json:"user"`
	Base         string              `json:"base"` // "origin/main @ 3f2a1c4"
	Pipelines    []AzurePlanPipeline `json:"pipelines"`
	Snapshots    []AzurePlanSnapshot `json:"snapshots"`
	Summary      string              `json:"summary"`

	// what Run needs, gathered when the plan was made
	org      string
	project  string
	client   *azure.Client
	repos    map[string]*azRepo // by top
	pipes    []*azPipe
	ours     map[string]PipelineReport
	gen      int
	baseKey  string
	keepRefs bool
	gitPath  string
}

// Checkable is the number of pipelines Azure will be asked about.
func (p *AzurePlan) Checkable() int {
	n := 0
	for _, x := range p.Pipelines {
		if x.Skip == "" {
			n++
		}
	}
	return n
}

type azRepo struct {
	top       string
	name      string // Azure repository name, or the GitHub "owner/repo"
	project   string
	github    string // "owner/repo" when its origin is on GitHub
	remoteURL string // the push target: Azure's URL, or the GitHub origin (without credentials)
	origin    string // the local remote's URL, when it is the same repository
	base      string
	branch    string
	ref       string
	changes   []azure.FileChange
	files     []AzurePlanFile
	unpushed  bool
	snapshot  string // commit, once pushed
	refused   string // why the push was refused: its pipelines can't be checked
	// override: nothing is pushed. Only checked root pipelines changed, so
	// their text goes in the request (yamlOverride) and self is pinned to
	// the base, on selfRef (the remote branch that holds it).
	override bool
	selfRef  string
}

type azAlias struct {
	alias string
	dir   string  // local copy ("" when none)
	repo  *azRepo // the workspace repository it is, when it has changes
}

type azPipe struct {
	path, rel string
	topRel    string  // relative to its repository top, forward slashes
	repo      *azRepo // its own repository
	project   string  // the Azure DevOps project of its definition
	text      string  // its changed text, sent as yamlOverride (repo.override)
	def       AzureDefinition
	aliases   map[string]*azAlias
	params    map[string]interface{}
}

// AzureProblem is a problem Azure reports, on a local file when it can be
// placed.
type AzureProblem struct {
	Path         string `json:"path"`
	RelativePath string `json:"relativePath"`
	Line         int    `json:"line"` // 0-based
	Col          int    `json:"col"`
	Message      string `json:"message"`
	AzurePath    string `json:"azurePath"` // "/templates/stages.yml@templates"
	// AlsoFoundByAtn: atn reports an error in the same file within a line.
	AlsoFoundByAtn bool `json:"alsoFoundByAtn"`
	key            string
}

// AzureSide is Azure's answer for one side of the comparison.
type AzureSide struct {
	Verdict  string         `json:"verdict"` // compiles | rejected | unknown
	Error    string         `json:"error,omitempty"`
	Problems []AzureProblem `json:"problems"`
	yaml     string
}

// AzurePipelineResult is a pipeline's outcome.
type AzurePipelineResult struct {
	Path         string           `json:"path"`
	RelativePath string           `json:"relativePath"`
	Definition   *AzureDefinition `json:"definition,omitempty"`
	Skip         string           `json:"skip,omitempty"`
	Now          AzureSide        `json:"now"`
	Before       AzureSide        `json:"before"`
	// Problems your change added in Azure's eyes, those it removed, and the
	// number in both.
	New      []AzureProblem `json:"new"`
	Fixed    []AzureProblem `json:"fixed"`
	Existing int            `json:"existing"`
	// Our verdict now.
	Atn PipelineVerdict `json:"atn"`
	// Agreement: agree | azureOnly (Azure rejects it, atn finds no error) |
	// atnOnly (atn finds errors, Azure compiles it) | "" (no verdict).
	Agreement string `json:"agreement"`
	// Expansion: how the expanded YAML changed (when both sides compile).
	Expansion  *AzureExpansionChange  `json:"expansion,omitempty"`
	Parameters map[string]interface{} `json:"parameters,omitempty"`
}

// AzureExpansionChange counts the lines of the expanded pipeline that the
// change added and removed.
type AzureExpansionChange struct {
	Changed bool `json:"changed"`
	Added   int  `json:"added"`
	Removed int  `json:"removed"`
}

// Title is a one-line summary of a pipeline's result.
func (r AzurePipelineResult) Title() string {
	if r.Skip != "" {
		return "not checked: " + r.Skip
	}
	var s string
	switch r.Now.Verdict {
	case "compiles":
		s = "✓ compiles"
	case "rejected":
		s = fmt.Sprintf("✗ rejected (%d problem%s", len(r.Now.Problems), plural(len(r.Now.Problems)))
		if len(r.New) > 0 {
			s += fmt.Sprintf(", %d new", len(r.New))
		}
		s += ")"
	default:
		return "? couldn't check: " + r.Now.Error
	}
	switch r.Agreement {
	case "agree":
		s += " · atn agrees"
	case "azureOnly":
		s += " · atn found no error"
	case "atnOnly":
		s += " · atn reports errors"
	}
	if r.Expansion != nil && r.Expansion.Changed {
		s += fmt.Sprintf(" · expansion +%d −%d", r.Expansion.Added, r.Expansion.Removed)
	} else if r.Expansion != nil {
		s += " · expansion unchanged"
	}
	return s
}

// AzureTotals are the report's counts.
type AzureTotals struct {
	Pipelines         int `json:"pipelines"` // checked
	Compiles          int `json:"compiles"`
	Rejected          int `json:"rejected"`
	NewlyRejected     int `json:"newlyRejected"` // rejected now, compiled at the base
	WithNewProblems   int `json:"withNewProblems"`
	Unknown           int `json:"unknown"`
	Disagree          int `json:"disagree"`
	ChangedExpansions int `json:"changedExpansions"`
	Skipped           int `json:"skipped"`
}

// AzureReport is the outcome of a check.
type AzureReport struct {
	State        string                `json:"state"` // running | done | cancelled | failed
	Error        string                `json:"error,omitempty"`
	PlanID       string                `json:"planId"`
	Organization string                `json:"organization"`
	Project      string                `json:"project"`
	Base         string                `json:"base"`
	StartedAt    time.Time             `json:"startedAt"`
	FinishedAt   time.Time             `json:"finishedAt"`
	Stale        bool                  `json:"stale"` // the workspace changed since
	Totals       AzureTotals           `json:"totals"`
	Pipelines    []AzurePipelineResult `json:"pipelines"`
	Warnings     []string              `json:"warnings"` // e.g. a snapshot ref that couldn't be deleted

	gen     int
	baseKey string
}

// Summary is a one-line summary.
func (r *AzureReport) Summary() string {
	switch r.State {
	case "running":
		return "Checking in Azure DevOps…"
	case "failed":
		return "Azure DevOps check failed: " + r.Error
	case "cancelled":
		return "Azure DevOps check cancelled"
	}
	t := r.Totals
	if t.Pipelines == 0 {
		return "Azure DevOps: nothing to check"
	}
	parts := []string{fmt.Sprintf("%d of %d compile", t.Compiles, t.Pipelines)}
	if t.WithNewProblems > 0 {
		parts = append(parts, fmt.Sprintf("%d rejected because of your changes", t.WithNewProblems))
	} else if t.Rejected > 0 {
		parts = append(parts, fmt.Sprintf("%d rejected (already at the base)", t.Rejected))
	}
	if t.Unknown > 0 {
		parts = append(parts, fmt.Sprintf("%d couldn't be checked", t.Unknown))
	}
	if t.Disagree > 0 {
		parts = append(parts, fmt.Sprintf("%d disagree%s with atn", t.Disagree, map[bool]string{true: "s", false: ""}[t.Disagree == 1]))
	}
	if t.ChangedExpansions > 0 {
		parts = append(parts, fmt.Sprintf("%d expansion%s changed", t.ChangedExpansions, plural(t.ChangedExpansions)))
	}
	return "Azure DevOps: " + strings.Join(parts, " · ")
}

// Result returns a pipeline's result, if the report has one.
func (r *AzureReport) Result(path string) (AzurePipelineResult, bool) {
	if r == nil {
		return AzurePipelineResult{}, false
	}
	for _, p := range r.Pipelines {
		if p.Path == path {
			return p, true
		}
	}
	return AzurePipelineResult{}, false
}

// azState is the engine's Azure state (under azMu).
type azState struct {
	mu      sync.Mutex
	last    *AzureReport
	yaml    map[string][2]string // path -> base, change finalYaml of the last report
	cancel  context.CancelFunc
	running bool
	az      *azure.AzCLI
	tokens  azure.TokenSource // replaces env + az (SetAzureTokens)
}

// SetAzureTokens replaces where tokens come from (tests, or a client that
// provides one).
func (e *Engine) SetAzureTokens(t azure.TokenSource) {
	e.az.mu.Lock()
	e.az.tokens = t
	e.az.mu.Unlock()
}

func (e *Engine) azureTokens(s Settings) azure.TokenSource {
	e.az.mu.Lock()
	defer e.az.mu.Unlock()
	if e.az.tokens != nil {
		return e.az.tokens
	}
	if e.az.az == nil || e.az.az.Path != s.Azure.AzPath {
		e.az.az = &azure.AzCLI{Path: s.Azure.AzPath}
	}
	return azure.Chain{azure.EnvToken{}, e.az.az}
}

// AzureOptions says what to check.
type AzureOptions struct {
	// Since is the base ("" = the one My changes uses). With Explicit, the
	// comparison fails instead of guessing (the CLI).
	Since    string
	Explicit bool
	// Only narrows the check to these root pipelines (absolute paths).
	Only []string
}

// ---------------------------------------------------------------------------
// plan
// ---------------------------------------------------------------------------

// AzurePlan works out what a check would do: which pipelines, with which
// definitions, and what is pushed where. It only reads from Azure DevOps.
func (e *Engine) AzurePlan(ctx context.Context, opt AzureOptions) (*AzurePlan, error) {
	s := e.Settings()
	var r ChangesReport
	var err error
	if opt.Explicit || opt.Since != "" {
		r, err = e.ChangesSince(opt.Since)
	} else {
		r = e.Changes()
		if !r.Available {
			err = errors.New(r.Reason)
		}
	}
	if err != nil {
		return nil, err
	}
	only := map[string]bool{}
	for _, p := range opt.Only {
		only[filepath.Clean(p)] = true
	}

	plan := &AzurePlan{Base: r.Label, Pipelines: []AzurePlanPipeline{}, Snapshots: []AzurePlanSnapshot{},
		repos: map[string]*azRepo{}, ours: map[string]PipelineReport{}, keepRefs: s.Azure.KeepSnapshots, gitPath: s.Repositories.GitPath}
	e.mu.Lock()
	plan.gen = e.diagGen
	e.mu.Unlock()
	plan.baseKey = r.Label

	// repositories and the YAML they changed
	for _, b := range r.Bases {
		rp := &azRepo{top: b.Top, base: b.SHA, branch: b.Branch}
		for _, f := range r.Files {
			if !under(f.Path, b.Top) {
				continue
			}
			rel, _ := filepath.Rel(b.Top, f.Path)
			rel = filepath.ToSlash(rel)
			fc := azure.FileChange{Path: rel, Deleted: f.Status == "deleted"}
			if !fc.Deleted {
				t, ok := e.fileText(f.Path)
				if !ok {
					continue
				}
				fc.Text = t
			}
			rp.changes = append(rp.changes, fc)
			rp.files = append(rp.files, AzurePlanFile{RelativePath: rel, Status: f.Status})
		}
		plan.repos[b.Top] = rp
	}

	// pipelines: the ones your changes reach
	for _, c := range r.Pipelines {
		if c.Status == "removed" || (len(only) > 0 && !only[c.Path]) {
			continue
		}
		pp := &azPipe{path: c.Path, rel: c.RelativePath, aliases: map[string]*azAlias{}}
		for top, rp := range plan.repos {
			if under(c.Path, top) && (pp.repo == nil || len(top) > len(pp.repo.top)) {
				pp.repo = rp
			}
		}
		aliases, params, ok := e.azureRootInfo(c.Path)
		if !ok {
			continue
		}
		pp.params = runtimeParams(params)
		for _, a := range aliases {
			al := &azAlias{alias: a.alias, dir: a.dir}
			if a.dir != "" {
				for top, rp := range plan.repos {
					if (a.dir == top || under(a.dir, top)) && len(rp.changes) > 0 && sameRepo(a.decl, top) {
						al.repo = rp
					}
				}
			}
			pp.aliases[a.alias] = al
		}
		if rep, ok := e.PipelineCheck(c.Path); ok {
			plan.ours[c.Path] = rep
		}
		plan.pipes = append(plan.pipes, pp)
	}
	sort.Slice(plan.pipes, func(i, j int) bool { return plan.pipes[i].rel < plan.pipes[j].rel })

	// Azure: organization, project, repositories, definitions
	skip := map[*azPipe]string{}
	var ghPipes []*azPipe
	for _, pp := range plan.pipes {
		if pp.repo == nil {
			skip[pp] = "not in a git repository"
			continue
		}
		if pp.repo.base == "" {
			skip[pp] = "the repository has no commits yet"
			continue
		}
		rm, ok := repos.ParseRemote(repos.OriginURL(pp.repo.top))
		if gh := githubRepo(rm, ok); gh != "" {
			// Azure DevOps reads it through a GitHub service connection
			pp.repo.github, pp.repo.name = gh, gh
			ghPipes = append(ghPipes, pp)
			continue
		}
		if !ok || rm.Host != "dev.azure.com" {
			skip[pp] = "its repository is neither on Azure Repos nor on GitHub"
			continue
		}
		if plan.org == "" {
			plan.org = rm.Org
		}
		if !strings.EqualFold(plan.org, rm.Org) {
			skip[pp] = "in another organization (" + rm.Org + ")"
			continue
		}
		pp.repo.name, pp.repo.project = unescape(rm.Repo), unescape(rm.Project)
	}
	// GitHub: the remote doesn't say which organization runs its pipelines.
	// The settings do, else the Azure Repos pipelines or repositories of
	// this check (a GitHub pipeline with its templates on Azure Repos).
	if len(ghPipes) > 0 {
		org := s.Repositories.AzureDevOpsOrganization
		if org == "" {
			org = plan.org
		}
		if org == "" {
			org = azureReposOrg(plan.repos)
		}
		switch {
		case org == "":
			return nil, fmt.Errorf("%s is on GitHub, and atn can't tell which Azure DevOps organization runs its pipelines. "+
				"Set 'repositories.azureDevOpsOrganization' (and 'repositories.azureDevOpsProject' to look in one project only)", ghPipes[0].repo.github)
		case plan.org != "" && !strings.EqualFold(org, plan.org):
			for _, pp := range ghPipes {
				skip[pp] = "in another organization (" + org + ")"
			}
		default:
			plan.org = org
		}
	}
	// changed repositories reached only through an alias
	for _, rp := range plan.repos {
		if rp.name != "" || len(rp.changes) == 0 {
			continue
		}
		rm, ok := repos.ParseRemote(repos.OriginURL(rp.top))
		if gh := githubRepo(rm, ok); gh != "" {
			rp.github, rp.name = gh, gh // a GitHub templates repository (`type: github` alias)
		} else if ok && rm.Host == "dev.azure.com" && (plan.org == "" || strings.EqualFold(plan.org, rm.Org)) {
			rp.name, rp.project = unescape(rm.Repo), unescape(rm.Project)
		}
	}
	if plan.org == "" {
		if len(plan.pipes) == 0 {
			return nil, errors.New("your changes reach no pipeline")
		}
		return nil, errors.New("none of the pipelines your changes reach is in an Azure Repos or GitHub repository")
	}
	plan.Organization = plan.org
	plan.client = azure.NewClient(plan.org, e.azureTokens(s))
	plan.client.Log = func(m string) { e.Log("[azure] " + m) }
	me, err := plan.client.Me(ctx)
	if err != nil {
		return nil, explainAzureError(err, plan.client, ctx)
	}
	plan.User = me.Name

	defsByRepo := map[*azRepo][]azure.Definition{}
	if err := e.githubDefinitions(ctx, plan, s, me.ID, ghPipes, skip, defsByRepo); err != nil {
		return nil, err
	}
	for _, rp := range plan.repos {
		if rp.name == "" || rp.github != "" {
			continue
		}
		info, err := plan.client.Repository(ctx, rp.project, rp.name)
		if err != nil {
			if azure.IsAuth(err) {
				return nil, explainAzureError(err, plan.client, ctx)
			}
			e.Log(fmt.Sprintf("[azure] repository %s/%s: %v", rp.project, rp.name, err))
			rp.name = "" // can't be pushed to, nor have definitions
			continue
		}
		rp.remoteURL = info.RemoteURL
		if rm, ok := repos.ParseRemote(repos.OriginURL(rp.top)); ok && strings.EqualFold(rm.Repo, rp.name) {
			rp.origin = "origin"
		}
		rp.ref = azure.RefName(me.ID, rp.branch)
		defs, err := plan.client.Definitions(ctx, rp.project, info.ID)
		if err != nil {
			return nil, explainAzureError(err, plan.client, ctx)
		}
		defsByRepo[rp] = defs
	}
	for _, pp := range plan.pipes {
		if skip[pp] != "" {
			continue
		}
		if pp.repo.name == "" {
			skip[pp] = "Azure DevOps doesn't know its repository"
			continue
		}
		rel, _ := filepath.Rel(pp.repo.top, pp.path)
		pp.topRel = filepath.ToSlash(rel)
		m := azure.MatchDefinitions(defsByRepo[pp.repo], pp.topRel)
		if len(m) == 0 {
			skip[pp] = "not registered in Azure DevOps"
			if pp.repo.github != "" {
				skip[pp] = fmt.Sprintf("no YAML pipeline in %s reads it from GitHub repository %s", plan.org, pp.repo.github)
			}
			continue
		}
		pp.project = m[0].Project
		if pp.project == "" {
			pp.project = pp.repo.project
		}
		if plan.project == "" {
			plan.project = pp.project
		}
		pp.def = AzureDefinition{ID: m[0].ID, Name: m[0].Name, URL: m[0].WebURL, Others: []int{}}
		for _, o := range m[1:] {
			pp.def.Others = append(pp.def.Others, o.ID)
		}
	}
	plan.Project = plan.project

	// snapshots: repositories with changes that a checked pipeline uses
	used := map[*azRepo]bool{}
	for _, pp := range plan.pipes {
		if skip[pp] != "" {
			continue
		}
		if len(pp.repo.changes) > 0 {
			used[pp.repo] = true
		}
		for _, a := range pp.aliases {
			if a.repo != nil && a.repo.remoteURL != "" {
				used[a.repo] = true
			} else if a.repo != nil {
				a.repo = nil // can't push it: Azure reads the declared ref
			}
		}
	}
	plan.overrides(ctx, used, skip)
	for rp := range used {
		g := azure.Git{Path: plan.gitPath, Dir: rp.top}
		rp.unpushed = !g.OnRemote(ctx, rp.base)
		plan.Snapshots = append(plan.Snapshots, AzurePlanSnapshot{Repo: rp.name, RemoteURL: rp.remoteURL, Ref: rp.ref, Base: rp.base, Files: rp.files,
			UnpushedBase: rp.unpushed, GitHub: rp.github != ""})
	}
	sort.Slice(plan.Snapshots, func(i, j int) bool { return plan.Snapshots[i].Repo < plan.Snapshots[j].Repo })
	for _, pp := range plan.pipes {
		p := AzurePlanPipeline{Path: pp.path, RelativePath: pp.rel, Skip: skip[pp]}
		if p.Skip == "" {
			d := pp.def
			p.Definition = &d
			p.YAMLOverride = pp.text != ""
		}
		plan.Pipelines = append(plan.Pipelines, p)
	}
	plan.Summary = plan.summary()
	plan.ID = plan.hash()
	return plan, nil
}

func (p *AzurePlan) summary() string {
	n := p.Checkable()
	var parts []string
	files := 0
	for _, s := range p.Snapshots {
		files += len(s.Files)
	}
	if len(p.Snapshots) > 0 {
		repos := []string{}
		for _, s := range p.Snapshots {
			repos = append(repos, s.Repo)
		}
		parts = append(parts, fmt.Sprintf("push %d changed YAML file%s to %s in %s", files, plural(files), p.Snapshots[0].Ref, strings.Join(repos, ", ")))
		for _, s := range p.Snapshots {
			if s.GitHub {
				parts[len(parts)-1] += " (GitHub, with your own git credentials)"
				break
			}
		}
	}
	if o := p.overridden(); o > 0 {
		parts = append(parts, fmt.Sprintf("send %d changed pipeline file%s in the preview request (nothing is pushed for %s)", o, plural(o), map[bool]string{true: "it", false: "them"}[o == 1]))
	}
	parts = append(parts, fmt.Sprintf("ask %s/%s to compile %d pipeline%s at %s and with your changes (%d previews, no run is created)", p.Organization, p.Project, n, plural(n), p.Base, 2*n))
	if len(p.Snapshots) > 0 && !p.keepRefs {
		parts = append(parts, "then delete the snapshot ref")
	}
	s := strings.Join(parts, ", ")
	return strings.ToUpper(s[:1]) + s[1:] + "."
}

// hash identifies the plan's content: what is pushed (file texts included)
// and what is asked.
func (p *AzurePlan) hash() string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%s\n", p.Organization, p.Project, p.Base)
	for _, pp := range p.Pipelines {
		b, _ := json.Marshal(pp)
		h.Write(b)
	}
	for _, s := range p.Snapshots {
		fmt.Fprintf(h, "%s|%s|%s\n", s.RemoteURL, s.Ref, s.Base)
	}
	var tops []string
	for top := range p.repos {
		tops = append(tops, top)
	}
	sort.Strings(tops)
	for _, top := range tops {
		for _, c := range p.repos[top].changes {
			fmt.Fprintf(h, "%s|%v|%x\n", c.Path, c.Deleted, sha256.Sum256([]byte(c.Text)))
		}
	}
	return fmt.Sprintf("p-%x", h.Sum(nil))[:18]
}

func under(p, top string) bool {
	return strings.HasPrefix(p, top+string(filepath.Separator))
}

func unescape(s string) string {
	if u, err := url.PathUnescape(s); err == nil {
		return u
	}
	return s
}

// sameRepo: the alias declares the repository the local folder is a clone
// of (its origin), so a snapshot of the folder is that repository's change.
func sameRepo(d pipeline.RepoDecl, top string) bool {
	rm, ok := repos.ParseRemote(repos.OriginURL(top))
	name := strings.Trim(strings.TrimSuffix(d.Name, ".git"), "/")
	if strings.EqualFold(d.Type, "github") {
		gh := githubRepo(rm, ok)
		return gh != "" && strings.EqualFold(name, gh)
	}
	if d.Type != "" && d.Type != "git" {
		return false
	}
	if !ok || rm.Host != "dev.azure.com" {
		return false
	}
	repo := name
	if i := strings.LastIndex(name, "/"); i >= 0 {
		repo = name[i+1:]
		if !strings.EqualFold(name[:i], unescape(rm.Project)) {
			return false
		}
	}
	return strings.EqualFold(repo, unescape(rm.Repo))
}

type rootAliasInfo struct {
	alias string
	decl  pipeline.RepoDecl
	dir   string
}

// azureRootInfo returns a root pipeline's own repository declarations (Azure
// resolves @alias against the root file only), each with where it resolves
// locally, and its parameters.
func (e *Engine) azureRootInfo(path string) ([]rootAliasInfo, []pipeline.Param, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	f := e.file(path)
	if f == nil {
		return nil, nil, false
	}
	var out []rootAliasInfo
	repoRoot := e.repoRoot(filepath.Dir(f.path))
	parents := []string{filepath.Dir(e.realRepo(repoRoot))}
	for _, root := range e.roots {
		parents = append(parents, filepath.Dir(e.realRepo(e.repoRoot(root))))
	}
	for alias, decl := range f.repos {
		loc := e.Repos.Locate(decl, parents, e.remoteFor(f.path))
		a := rootAliasInfo{alias: alias, decl: decl}
		if loc.State == repos.StateReady {
			a.dir = filepath.Clean(loc.Dir)
		}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].alias < out[j].alias })
	return out, append([]pipeline.Param(nil), f.params...), true
}

// runtimeParams fills in the root's runtime parameters that have no default:
// both sides would otherwise fail the same way and there'd be nothing to see.
func runtimeParams(ps []pipeline.Param) map[string]interface{} {
	out := map[string]interface{}{}
	for _, p := range ps {
		if p.HasDefault {
			continue
		}
		if len(p.Values) > 0 {
			out[p.Name] = pipeline.Unquote(p.Values[0])
			continue
		}
		switch strings.ToLower(p.Type) {
		case "boolean":
			out[p.Name] = false
		case "number":
			out[p.Name] = 0
		case "object":
			out[p.Name] = map[string]interface{}{}
		case "step", "steplist", "job", "joblist", "deployment", "deploymentlist", "stage", "stagelist":
			out[p.Name] = []interface{}{}
		default:
			out[p.Name] = "atn"
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func explainAzureError(err error, c *azure.Client, ctx context.Context) error {
	var he *azure.HTTPError
	if errors.As(err, &he) && (he.Status == 401 || he.Status == 403) {
		who := ""
		if tok, e := c.Token(ctx); e == nil {
			if u := azure.TokenUser(tok); u != "" {
				who = " (signed in as " + u + ")"
			}
		}
		if he.Status == 403 {
			return fmt.Errorf("Azure DevOps organization %s refused%s: %s. Previews need the 'View builds' and 'Queue builds' permissions", c.Org, who, he.Message)
		}
		return fmt.Errorf("Azure DevOps organization %s rejected the token%s: %s. Run `%s`", c.Org, who, he.Message, azure.LoginHint(c.Tenant(ctx)))
	}
	return err
}

// ---------------------------------------------------------------------------
// run
// ---------------------------------------------------------------------------

// refused is why a pipeline can't be checked: the push of a repository it
// needs (its own, or an alias's) was refused. "" when it can be.
func (pp *azPipe) refused() string {
	if pp.repo != nil && pp.repo.refused != "" {
		return pp.repo.refused
	}
	names := make([]string, 0, len(pp.aliases))
	for name := range pp.aliases {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if a := pp.aliases[name]; a.repo != nil && a.repo.refused != "" {
			return a.repo.refused
		}
	}
	return ""
}

// firstRefused is why a repository's push was refused (the first, in a
// stable order), or "" when none was.
func firstRefused(plan *AzurePlan) string {
	var tops []string
	for top, rp := range plan.repos {
		if rp.refused != "" {
			tops = append(tops, top)
		}
	}
	if len(tops) == 0 {
		return ""
	}
	sort.Strings(tops)
	return plan.repos[tops[0]].refused
}

// AzureProgress reports a running check's progress.
type AzureProgress func(done, total int, message string)

// AzureRun carries out a plan: push the snapshots, preview every pipeline at
// the base and with the changes, delete the snapshot refs, and compare.
func (e *Engine) AzureRun(ctx context.Context, plan *AzurePlan, progress AzureProgress) *AzureReport {
	if progress == nil {
		progress = func(int, int, string) {}
	}
	rep := &AzureReport{State: "running", PlanID: plan.ID, Organization: plan.Organization, Project: plan.Project, Base: plan.Base,
		StartedAt: time.Now(), Pipelines: []AzurePipelineResult{}, Warnings: []string{}, gen: plan.gen, baseKey: plan.baseKey}
	fail := func(err error) *AzureReport {
		rep.State, rep.Error, rep.FinishedAt = "failed", err.Error(), time.Now()
		if errors.Is(err, context.Canceled) {
			rep.State, rep.Error = "cancelled", ""
		}
		return rep
	}
	total := 2*plan.Checkable() + len(plan.Snapshots)
	done := 0

	// snapshots
	var pushed []*azRepo
	defer func() {
		if plan.keepRefs {
			return
		}
		for _, rp := range pushed {
			cctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			tok := ""
			if rp.github == "" { // GitHub gets the user's own git credentials
				tok, _ = plan.client.Token(cctx)
			}
			g := azure.Git{Path: plan.gitPath, Dir: rp.top}
			if err := g.Push(cctx, rp.pushTarget(), tok, rp.origin, ":"+rp.ref); err != nil {
				rep.Warnings = append(rep.Warnings, fmt.Sprintf("couldn't delete %s in %s: %v", rp.ref, rp.name, err))
			}
			cancel()
		}
	}()
	for _, s := range plan.Snapshots {
		var rp *azRepo
		for _, r := range plan.repos {
			if r.remoteURL == s.RemoteURL && r.ref == s.Ref {
				rp = r
			}
		}
		if rp == nil {
			continue
		}
		progress(done, total, "Pushing your changes to "+rp.name+"…")
		g := azure.Git{Path: plan.gitPath, Dir: rp.top}
		sha, err := g.Snapshot(ctx, rp.base, rp.changes)
		if err != nil {
			return fail(fmt.Errorf("couldn't build the snapshot of %s: %v", rp.name, err))
		}
		tok := ""
		if rp.github == "" { // GitHub gets the user's own git credentials
			if tok, err = plan.client.Token(ctx); err != nil {
				return fail(err)
			}
		}
		if err := g.Push(ctx, rp.pushTarget(), tok, rp.origin, sha+":"+rp.ref); err != nil {
			if ctx.Err() != nil {
				return fail(ctx.Err())
			}
			// a policy or a permission in this repository: check the
			// pipelines that don't need it, and say why the others weren't
			rp.refused = fmt.Sprintf("couldn't push your changes to %s in %s: %v. You need permission to create refs in that repository", rp.ref, rp.name, err)
			if rp.github != "" {
				rp.refused = fmt.Sprintf("couldn't push your changes to %s in %s on GitHub: %v. atn pushes there with your own git credentials, so you need write access to the repository", rp.ref, rp.name, err)
			}
			rep.Warnings = append(rep.Warnings, rp.refused)
			done++
			continue
		}
		rp.snapshot = sha
		pushed = append(pushed, rp)
		done++
	}
	if why := firstRefused(plan); why != "" && len(pushed) == 0 {
		// nothing reached the server: the whole check is that one reason
		return fail(errors.New(why))
	}

	// previews, 4 at a time
	type job struct {
		pp     *azPipe
		before bool
	}
	results := map[*azPipe]*[2]AzureSide{}
	var jobs []job
	for i, pp := range plan.pipes {
		if plan.Pipelines[i].Skip != "" {
			continue
		}
		if why := pp.refused(); why != "" {
			side := AzureSide{Verdict: "unknown", Error: why, Problems: []AzureProblem{}}
			results[pp] = &[2]AzureSide{side, side}
			done += 2
			continue
		}
		results[pp] = &[2]AzureSide{}
		jobs = append(jobs, job{pp, true}, job{pp, false})
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	sem := make(chan struct{}, 4)
	for _, j := range jobs {
		j := j
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			side := e.azurePreview(ctx, plan, j.pp, j.before)
			mu.Lock()
			if j.before {
				results[j.pp][0] = side
			} else {
				results[j.pp][1] = side
			}
			done++
			progress(done, total, "Compiling "+j.pp.rel+"…")
			mu.Unlock()
		}()
	}
	wg.Wait()
	if ctx.Err() != nil {
		return fail(ctx.Err())
	}

	yaml := map[string][2]string{}
	for i, pp := range plan.pipes {
		res := AzurePipelineResult{Path: pp.path, RelativePath: pp.rel, Skip: plan.Pipelines[i].Skip, New: []AzureProblem{}, Fixed: []AzureProblem{},
			Parameters: pp.params, Agreement: ""}
		if ours, ok := plan.ours[pp.path]; ok {
			res.Atn = verdictOf(&ours)
		}
		if res.Skip != "" {
			rep.Totals.Skipped++
			rep.Pipelines = append(rep.Pipelines, res)
			continue
		}
		d := pp.def
		res.Definition = &d
		sides := results[pp]
		res.Before, res.Now = sides[0], sides[1]
		compareAzure(&res, plan.ours[pp.path])
		if res.Before.Verdict == "compiles" && res.Now.Verdict == "compiles" {
			yaml[pp.path] = [2]string{res.Before.yaml, res.Now.yaml}
		}
		t := &rep.Totals
		t.Pipelines++
		switch res.Now.Verdict {
		case "compiles":
			t.Compiles++
		case "rejected":
			t.Rejected++
			if res.Before.Verdict == "compiles" {
				t.NewlyRejected++
			}
		default:
			t.Unknown++
		}
		if len(res.New) > 0 {
			t.WithNewProblems++
		}
		if res.Agreement == "azureOnly" || res.Agreement == "atnOnly" {
			t.Disagree++
		}
		if res.Expansion != nil && res.Expansion.Changed {
			t.ChangedExpansions++
		}
		rep.Pipelines = append(rep.Pipelines, res)
	}
	sort.SliceStable(rep.Pipelines, func(i, j int) bool {
		rank := func(r AzurePipelineResult) int {
			switch {
			case r.Skip != "":
				return 5
			case len(r.New) > 0:
				return 0
			case r.Agreement == "azureOnly" || r.Agreement == "atnOnly":
				return 1
			case r.Now.Verdict == "unknown":
				return 2
			case r.Expansion != nil && r.Expansion.Changed:
				return 3
			}
			return 4
		}
		return rank(rep.Pipelines[i]) < rank(rep.Pipelines[j])
	})
	rep.State, rep.FinishedAt = "done", time.Now()
	e.az.mu.Lock()
	e.az.yaml = yaml
	e.az.mu.Unlock()
	return rep
}

// azurePreview compiles one side of one pipeline.
func (e *Engine) azurePreview(ctx context.Context, plan *AzurePlan, pp *azPipe, before bool) AzureSide {
	over := map[string]azure.RepoOverride{}
	version := func(rp *azRepo) string {
		if before {
			return rp.base
		}
		return rp.snapshot
	}
	req := azure.PreviewRequest{Repositories: over, TemplateParameters: pp.params}
	if pp.repo.snapshot != "" {
		over["self"] = azure.RepoOverride{RefName: pp.repo.ref, Version: version(pp.repo)}
	} else if pp.repo.override {
		over["self"] = azure.RepoOverride{RefName: pp.repo.selfRef, Version: pp.repo.base}
		if !before {
			req.YAMLOverride = pp.text
		}
	}
	for name, a := range pp.aliases {
		if a.repo != nil && a.repo.snapshot != "" {
			over[name] = azure.RepoOverride{RefName: a.repo.ref, Version: version(a.repo)}
		}
	}
	res, err := plan.client.Preview(ctx, pp.project, pp.def.ID, req)
	if err != nil {
		return AzureSide{Verdict: "unknown", Error: explainAzureError(err, plan.client, ctx).Error(), Problems: []AzureProblem{}}
	}
	if !res.Rejected {
		return AzureSide{Verdict: "compiles", Problems: []AzureProblem{}, yaml: res.FinalYAML}
	}
	side := AzureSide{Verdict: "rejected", Problems: []AzureProblem{}}
	for _, p := range res.Problems {
		side.Problems = append(side.Problems, e.placeAzureProblem(pp, p))
	}
	return side
}

// placeAzureProblem puts an Azure problem on the local file it is about:
// /path is in the pipeline's own repository, /path@alias in the alias's
// local copy. Problems without a place go on the root pipeline.
func (e *Engine) placeAzureProblem(pp *azPipe, p azure.Problem) AzureProblem {
	out := AzureProblem{Path: pp.path, Message: p.Message, AzurePath: p.AzurePath, key: p.Key()}
	dir := ""
	switch {
	case p.Path == "":
	case p.Alias == "":
		dir = pp.repo.top
	default:
		if a := pp.aliases[p.Alias]; a != nil {
			dir = a.dir
		}
	}
	if dir != "" {
		out.Path = filepath.Join(dir, filepath.FromSlash(p.Path))
		if p.Line > 0 {
			out.Line, out.Col = p.Line-1, p.Col-1
		}
		if out.Col < 0 {
			out.Col = 0
		}
	} else if p.AzurePath != "" {
		out.Message = p.AzurePath + ": " + p.Message
		if p.Line > 0 {
			out.Message = fmt.Sprintf("%s (line %d): %s", p.AzurePath, p.Line, p.Message)
		}
	}
	out.RelativePath = e.relPathLocked(out.Path)
	return out
}

// compareAzure fills in what the change did in Azure's eyes, the agreement
// with atn and the expansion diff.
func compareAzure(r *AzurePipelineResult, ours PipelineReport) {
	count := map[string]int{}
	for _, p := range r.Before.Problems {
		count[p.key]++
	}
	for _, p := range r.Now.Problems {
		if count[p.key] > 0 {
			count[p.key]--
			r.Existing++
			continue
		}
		r.New = append(r.New, p)
	}
	left := map[string]int{}
	for _, p := range r.Now.Problems {
		left[p.key]++
	}
	for _, p := range r.Before.Problems {
		if left[p.key] > 0 {
			left[p.key]--
			continue
		}
		r.Fixed = append(r.Fixed, p)
	}
	// atn's errors near Azure's problems
	mark := func(ps []AzureProblem) {
		for i := range ps {
			for _, q := range ours.Problems {
				if q.Severity == SevError && q.Path == ps[i].Path && q.Line >= ps[i].Line-1 && q.Line <= ps[i].Line+1 {
					ps[i].AlsoFoundByAtn = true
				}
			}
		}
	}
	mark(r.Now.Problems)
	mark(r.New)
	switch {
	case r.Now.Verdict == "compiles" && ours.Errors == 0, r.Now.Verdict == "rejected" && ours.Errors > 0:
		r.Agreement = "agree"
	case r.Now.Verdict == "rejected":
		r.Agreement = "azureOnly"
	case r.Now.Verdict == "compiles":
		r.Agreement = "atnOnly"
	}
	if r.Before.Verdict == "compiles" && r.Now.Verdict == "compiles" {
		add, rem := azure.Count(azure.Diff(azure.SplitLines(r.Before.yaml), azure.SplitLines(r.Now.yaml)))
		r.Expansion = &AzureExpansionChange{Changed: add+rem > 0, Added: add, Removed: rem}
	}
}

// ---------------------------------------------------------------------------
// the last report and the background job (LSP)
// ---------------------------------------------------------------------------

// AzureLast returns the last report (nil when none), marked stale when the
// workspace changed since it was planned.
func (e *Engine) AzureLast() *AzureReport {
	e.az.mu.Lock()
	last := e.az.last
	e.az.mu.Unlock()
	if last == nil {
		return nil
	}
	cp := *last
	e.mu.Lock()
	e.ensureLinked()
	gen := e.diagGen
	e.mu.Unlock()
	cp.Stale = cp.State == "done" && cp.gen != gen
	return &cp
}

// AzureExpansion returns the base and changed expanded YAML of a pipeline
// from the last report.
func (e *Engine) AzureExpansion(path string) (base, change string, ok bool) {
	e.az.mu.Lock()
	defer e.az.mu.Unlock()
	y, ok := e.az.yaml[filepath.Clean(path)]
	return y[0], y[1], ok
}

// AzureStart plans again, checks that the plan is the one the user agreed
// to (planID), and runs it in the background. finished is called when it is
// done (also when it fails or is cancelled).
func (e *Engine) AzureStart(planID string, opt AzureOptions, progress AzureProgress, finished func(*AzureReport)) error {
	e.az.mu.Lock()
	if e.az.running {
		e.az.mu.Unlock()
		return errors.New("a check is already running")
	}
	e.az.running = true
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	e.az.cancel = cancel
	e.az.mu.Unlock()
	stop := func() {
		cancel()
		e.az.mu.Lock()
		e.az.running, e.az.cancel = false, nil
		e.az.mu.Unlock()
	}
	plan, err := e.AzurePlan(ctx, opt)
	if err != nil {
		stop()
		return err
	}
	if plan.ID != planID {
		stop()
		return errors.New("your changes changed since you confirmed: check again")
	}
	e.az.mu.Lock()
	e.az.last = &AzureReport{State: "running", PlanID: plan.ID, Organization: plan.Organization, Project: plan.Project, Base: plan.Base,
		StartedAt: time.Now(), Pipelines: []AzurePipelineResult{}, Warnings: []string{}, gen: plan.gen}
	e.az.mu.Unlock()
	go func() {
		rep := e.AzureRun(ctx, plan, progress)
		e.az.mu.Lock()
		e.az.last = rep
		e.az.mu.Unlock()
		stop()
		if finished != nil {
			finished(rep)
		}
	}()
	return nil
}

// AzureCancel cancels a running check.
func (e *Engine) AzureCancel() bool {
	e.az.mu.Lock()
	defer e.az.mu.Unlock()
	if e.az.cancel == nil {
		return false
	}
	e.az.cancel()
	return true
}

// azureNote is what the verdict lens adds for a pipeline in the last
// report, e.g. " · Azure ✓". Caller holds e.mu.
func (e *Engine) azureNote(path string) string {
	e.az.mu.Lock()
	last := e.az.last
	e.az.mu.Unlock()
	if last == nil || last.State != "done" || last.gen != e.diagGen {
		return ""
	}
	r, ok := last.Result(path)
	if !ok || r.Skip != "" {
		return ""
	}
	switch r.Now.Verdict {
	case "compiles":
		return " · Azure ✓"
	case "rejected":
		return " · Azure ✗"
	}
	return ""
}

// ---------------------------------------------------------------------------
// GitHub
// ---------------------------------------------------------------------------

// githubRepo is "owner/repo" for a github.com remote, else "". Azure DevOps
// pipelines read such repositories through a GitHub service connection.
func githubRepo(rm repos.Remote, ok bool) string {
	if !ok || !strings.EqualFold(rm.Host, "github.com") || rm.Org == "" || rm.Repo == "" {
		return ""
	}
	return rm.Org + "/" + strings.TrimSuffix(rm.Repo, ".git")
}

// azureReposOrg is the organization of the workspace's Azure Repos
// repositories in this check (changed ones first), or "".
func azureReposOrg(rs map[string]*azRepo) string {
	tops := make([]string, 0, len(rs))
	for top := range rs {
		tops = append(tops, top)
	}
	sort.Slice(tops, func(i, j int) bool {
		ci, cj := len(rs[tops[i]].changes) > 0, len(rs[tops[j]].changes) > 0
		if ci != cj {
			return ci
		}
		return tops[i] < tops[j]
	})
	for _, top := range tops {
		if rm, ok := repos.ParseRemote(repos.OriginURL(top)); ok && rm.Host == "dev.azure.com" {
			return rm.Org
		}
	}
	return ""
}

// maxProjectsSearched bounds the search for a GitHub repository's pipelines
// when no project is set.
const maxProjectsSearched = 100

// githubDefinitions finds the definitions of the GitHub repositories the
// checked pipelines live in: in the project set in the settings, else in
// every project of the organization. It also sets each repository's push
// target: its origin, with the user's own git credentials.
func (e *Engine) githubDefinitions(ctx context.Context, plan *AzurePlan, s Settings, userID string, ghPipes []*azPipe, skip map[*azPipe]string, defsByRepo map[*azRepo][]azure.Definition) error {
	need := map[*azRepo]bool{}
	for _, pp := range ghPipes {
		if skip[pp] == "" {
			need[pp.repo] = true
		}
	}
	for _, rp := range plan.repos {
		if rp.github != "" {
			rp.remoteURL = redactURL(repos.OriginURL(rp.top))
			rp.origin = "origin"
			rp.ref = azure.RefName(userID, rp.branch)
		}
	}
	if len(need) == 0 {
		return nil
	}
	projects := []string{s.Repositories.AzureDevOpsProject}
	if projects[0] == "" {
		all, err := plan.client.Projects(ctx)
		if err != nil {
			return explainAzureError(err, plan.client, ctx)
		}
		if len(all) > maxProjectsSearched {
			for _, pp := range ghPipes {
				if skip[pp] == "" {
					skip[pp] = fmt.Sprintf("%s has %d projects: set 'repositories.azureDevOpsProject' to the one whose pipelines read %s", plan.org, len(all), pp.repo.github)
				}
			}
			return nil
		}
		projects = all
	}
	tops := make([]string, 0, len(need))
	for rp := range need {
		tops = append(tops, rp.top)
	}
	sort.Strings(tops)
	for _, top := range tops {
		rp := plan.repos[top]
		for _, project := range projects {
			defs, err := plan.client.DefinitionsFor(ctx, project, azure.RepoTypeGitHub, rp.github)
			if err != nil {
				if azure.IsAuth(err) && len(projects) == 1 {
					return explainAzureError(err, plan.client, ctx)
				}
				e.Log(fmt.Sprintf("[azure] definitions of %s in %s: %v", rp.github, project, err))
				continue
			}
			defsByRepo[rp] = append(defsByRepo[rp], defs...)
		}
		// lowest id first across projects too
		d := defsByRepo[rp]
		sort.Slice(d, func(i, j int) bool { return d[i].ID < d[j].ID })
	}
	return nil
}

// overrides takes out of `used` the GitHub repositories that need no push:
// every change in them is a checked root pipeline (sent as yamlOverride),
// none of them is used through an alias (aliases only take a ref), and their
// base is on a remote branch that self can be pinned to. Azure Repos
// repositories keep their snapshots.
func (p *AzurePlan) overrides(ctx context.Context, used map[*azRepo]bool, skip map[*azPipe]string) {
	roots := map[*azRepo]map[string]*azPipe{}
	viaAlias := map[*azRepo]bool{}
	for _, pp := range p.pipes {
		if skip[pp] != "" {
			continue
		}
		if roots[pp.repo] == nil {
			roots[pp.repo] = map[string]*azPipe{}
		}
		roots[pp.repo][pp.topRel] = pp
		for _, a := range pp.aliases {
			if a.repo != nil {
				viaAlias[a.repo] = true
			}
		}
	}
	for rp := range used {
		if rp.github == "" || viaAlias[rp] || len(rp.changes) == 0 {
			continue
		}
		onlyRoots := true
		for _, c := range rp.changes {
			if c.Deleted || roots[rp][c.Path] == nil {
				onlyRoots = false
			}
		}
		if !onlyRoots {
			continue
		}
		g := azure.Git{Path: p.gitPath, Dir: rp.top}
		ref := g.RemoteBranch(ctx, rp.base, "origin")
		if ref == "" {
			continue // the base isn't on GitHub: push it
		}
		rp.override, rp.selfRef = true, ref
		for _, c := range rp.changes {
			roots[rp][c.Path].text = c.Text
		}
		delete(used, rp)
	}
}

// pushTarget is where a snapshot is pushed: Azure's URL (with the Azure
// DevOps token), or for GitHub the origin remote itself, so the user's own
// git setup (pushurl, ssh, credential helper) applies.
func (rp *azRepo) pushTarget() string {
	if rp.github != "" {
		return "origin"
	}
	return rp.remoteURL
}

// overridden is the number of checked pipelines whose changed file goes in
// the request.
func (p *AzurePlan) overridden() int {
	n := 0
	for _, x := range p.Pipelines {
		if x.YAMLOverride {
			n++
		}
	}
	return n
}

// redactURL drops credentials from a URL (an origin may carry a token).
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	u.User = nil
	return u.String()
}

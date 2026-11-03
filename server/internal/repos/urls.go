package repos

import (
	"bufio"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/qSharpy/azure-templates-navigator/server/internal/pipeline"
)

// Remote describes where a repository is hosted, parsed from a git URL.
type Remote struct {
	Host    string // github.com, dev.azure.com, ...
	SSH     bool
	Org     string // GitHub owner / Azure DevOps organization
	Project string // Azure DevOps project
	Repo    string
	Raw     string
}

var (
	adoHTTPSRe = regexp.MustCompile(`^https?://(?:[^@/]+@)?dev\.azure\.com/([^/]+)/([^/]+)/_git/([^/?#]+)`)
	vstsHTTPRe = regexp.MustCompile(`^https?://(?:[^@/]+@)?([^./]+)\.visualstudio\.com/(?:DefaultCollection/)?([^/]+)/_git/([^/?#]+)`)
	adoSSHRe   = regexp.MustCompile(`^(?:ssh://)?[^@]+@(?:ssh\.dev\.azure\.com|vs-ssh\.visualstudio\.com):(?:22/)?v3/([^/]+)/([^/]+)/([^/?#]+)`)
	scpLikeRe  = regexp.MustCompile(`^(?:ssh://)?[^@]+@([^:/]+)[:/](.+)$`)
	httpsRe    = regexp.MustCompile(`^https?://(?:[^@/]+@)?([^/]+)/(.+)$`)
)

// ParseRemote parses common git remote URL shapes.
func ParseRemote(raw string) (Remote, bool) {
	raw = strings.TrimSpace(raw)
	if m := adoHTTPSRe.FindStringSubmatch(raw); m != nil {
		return Remote{Host: "dev.azure.com", Org: m[1], Project: m[2], Repo: strings.TrimSuffix(m[3], ".git"), Raw: raw}, true
	}
	if m := vstsHTTPRe.FindStringSubmatch(raw); m != nil {
		return Remote{Host: "dev.azure.com", Org: m[1], Project: m[2], Repo: strings.TrimSuffix(m[3], ".git"), Raw: raw}, true
	}
	if m := adoSSHRe.FindStringSubmatch(raw); m != nil {
		return Remote{Host: "dev.azure.com", SSH: true, Org: m[1], Project: m[2], Repo: strings.TrimSuffix(m[3], ".git"), Raw: raw}, true
	}
	if m := scpLikeRe.FindStringSubmatch(raw); m != nil && !strings.HasPrefix(raw, "http") {
		parts := strings.Split(strings.TrimSuffix(m[2], ".git"), "/")
		r := Remote{Host: m[1], SSH: true, Raw: raw}
		if len(parts) >= 2 {
			r.Org, r.Repo = parts[len(parts)-2], parts[len(parts)-1]
		}
		return r, true
	}
	if m := httpsRe.FindStringSubmatch(raw); m != nil {
		parts := strings.Split(strings.TrimSuffix(m[2], ".git"), "/")
		r := Remote{Host: m[1], Raw: raw}
		if len(parts) >= 2 {
			r.Org, r.Repo = parts[len(parts)-2], parts[len(parts)-1]
		}
		return r, true
	}
	return Remote{}, false
}

// OriginURL reads the `origin` remote URL from a repository's .git/config
// without spawning git. Returns "" when unavailable.
func OriginURL(repoRoot string) string {
	gitPath := filepath.Join(repoRoot, ".git")
	st, err := os.Stat(gitPath)
	if err != nil {
		return ""
	}
	cfgPath := filepath.Join(gitPath, "config")
	if !st.IsDir() {
		// worktree / submodule: ".git" is a file containing "gitdir: <path>"
		b, err := os.ReadFile(gitPath)
		if err != nil {
			return ""
		}
		gd := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(b)), "gitdir:"))
		if !filepath.IsAbs(gd) {
			gd = filepath.Join(repoRoot, gd)
		}
		cfgPath = filepath.Join(gd, "config")
		if _, err := os.Stat(cfgPath); err != nil {
			// worktrees keep config in the common dir
			cfgPath = filepath.Join(gd, "..", "..", "config")
		}
	}
	f, err := os.Open(cfgPath)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	inOrigin := false
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(l, "[") {
			inOrigin = l == `[remote "origin"]`
			continue
		}
		if inOrigin && strings.HasPrefix(l, "url") {
			if i := strings.Index(l, "="); i >= 0 {
				return strings.TrimSpace(l[i+1:])
			}
		}
	}
	return ""
}

// CloneURL works out the clone URL for a repository declaration.
//
// Lookup order:
//  1. explicit override (by alias, full name, or repo name) from settings
//  2. derived from the declaration type + the workspace's own remote
//     (so SSH users get SSH URLs and Azure DevOps org/project are inferred)
//  3. configured Azure DevOps organization / GitHub Enterprise host
func CloneURL(decl pipeline.RepoDecl, cfg Config, workspaceRemote string) (string, error) {
	for _, k := range []string{decl.Alias, decl.Name, decl.RepoName()} {
		if u, ok := cfg.URLs[k]; ok && u != "" {
			return u, nil
		}
	}
	name := strings.Trim(strings.TrimSuffix(decl.Name, ".git"), "/")
	if name == "" {
		return "", fmt.Errorf("repository '%s' has no 'name:'", decl.Alias)
	}
	ws, hasWS := ParseRemote(workspaceRemote)

	switch decl.Type {
	case "github", "githubenterprise":
		host := "github.com"
		if decl.Type == "githubenterprise" {
			host = cfg.GitHubEnterpriseHost
			if host == "" && hasWS && ws.Host != "github.com" && ws.Host != "dev.azure.com" {
				host = ws.Host
			}
			if host == "" {
				return "", fmt.Errorf("set 'repositories.githubEnterpriseHost' (or a URL override) to fetch GitHub Enterprise repository '%s'", name)
			}
		}
		if hasWS && ws.SSH && ws.Host == host {
			return fmt.Sprintf("git@%s:%s.git", host, name), nil
		}
		return fmt.Sprintf("https://%s/%s.git", host, name), nil

	case "bitbucket":
		if hasWS && ws.SSH && ws.Host == "bitbucket.org" {
			return fmt.Sprintf("git@bitbucket.org:%s.git", name), nil
		}
		return fmt.Sprintf("https://bitbucket.org/%s.git", name), nil

	default: // "git" = Azure Repos
		project, repo := "", name
		if i := strings.Index(name, "/"); i >= 0 {
			project, repo = name[:i], name[i+1:]
		}
		org := cfg.AzureDevOpsOrganization
		ssh := false
		if hasWS && ws.Host == "dev.azure.com" {
			if org == "" {
				org = ws.Org
			}
			if project == "" {
				project = ws.Project
			}
			ssh = ws.SSH
		}
		if project == "" {
			project = cfg.AzureDevOpsProject
		}
		if org == "" {
			return "", fmt.Errorf("can't tell which Azure DevOps organization hosts '%s'; set 'repositories.azureDevOpsOrganization' or add a URL override", name)
		}
		if project == "" {
			return "", fmt.Errorf("repository '%s' has no project; set 'repositories.azureDevOpsProject' or use 'name: project/%s'", name, name)
		}
		if ssh {
			return fmt.Sprintf("git@ssh.dev.azure.com:v3/%s/%s/%s", org, project, repo), nil
		}
		return fmt.Sprintf("https://dev.azure.com/%s/%s/_git/%s", url.PathEscape(org), url.PathEscape(project), url.PathEscape(repo)), nil
	}
}

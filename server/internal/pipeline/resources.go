package pipeline

import (
	"regexp"
	"strings"
)

// RepoDecl is an entry of `resources.repositories`.
type RepoDecl struct {
	Alias    string `json:"alias"`
	Type     string `json:"type"`     // git (Azure Repos) | github | githubenterprise | bitbucket
	Name     string `json:"name"`     // e.g. "org/repo" or "project/repo"
	Ref      string `json:"ref"`      // e.g. "refs/heads/main" (empty = default branch)
	Endpoint string `json:"endpoint"` // service connection name (informational)
	Line     int    `json:"line"`
}

// RepoName returns the last path segment of Name (the folder name a local
// clone would normally have).
func (r RepoDecl) RepoName() string {
	n := strings.TrimSuffix(strings.TrimSpace(r.Name), ".git")
	if i := strings.LastIndex(n, "/"); i >= 0 {
		return n[i+1:]
	}
	return n
}

var (
	resourcesRe    = regexp.MustCompile(`^resources\s*:\s*$`)
	repositoriesRe = regexp.MustCompile(`^\s+repositories\s*:\s*$`)
	repoItemRe     = regexp.MustCompile(`^(\s*)-\s+repository\s*:\s*(.+)$`)
	kvRe           = regexp.MustCompile(`^\s*(?:-\s+)?([A-Za-z_]\w*)\s*:\s*(.*)$`)
)

// ParseRepositories parses `resources.repositories` into alias -> declaration.
func ParseRepositories(lines []string) map[string]RepoDecl {
	out := map[string]RepoDecl{}
	inResources, inRepos := false, false
	reposIndent, itemIndent := -1, -1
	var cur *RepoDecl
	flush := func() {
		if cur != nil && cur.Alias != "" {
			if cur.Type == "" {
				cur.Type = "git"
			}
			out[cur.Alias] = *cur
		}
		cur = nil
	}
	for i, raw := range lines {
		l := StripComment(raw)
		if !inResources {
			if resourcesRe.MatchString(l) {
				inResources = true
			}
			continue
		}
		if IsBlank(l) {
			continue
		}
		ind := Indent(l)
		if ind == 0 {
			break // left resources:
		}
		if !inRepos {
			if repositoriesRe.MatchString(l) {
				inRepos = true
				reposIndent = ind
			}
			continue
		}
		// a sibling of repositories: (e.g. pipelines:, containers:)
		if ind <= reposIndent && !strings.HasPrefix(strings.TrimSpace(l), "-") {
			flush()
			inRepos = false
			if repositoriesRe.MatchString(l) {
				inRepos = true
				reposIndent = ind
			}
			continue
		}
		if m := repoItemRe.FindStringSubmatch(l); m != nil {
			if itemIndent < 0 {
				itemIndent = len(m[1])
			}
			if len(m[1]) == itemIndent {
				flush()
				cur = &RepoDecl{Alias: Unquote(m[2]), Line: i}
				continue
			}
		}
		if cur == nil {
			continue
		}
		if m := kvRe.FindStringSubmatch(l); m != nil && ind > itemIndent {
			v := Unquote(m[2])
			switch m[1] {
			case "type":
				cur.Type = strings.ToLower(v)
			case "name":
				cur.Name = v
			case "ref":
				cur.Ref = v
			case "endpoint":
				cur.Endpoint = v
			}
		}
	}
	flush()
	return out
}

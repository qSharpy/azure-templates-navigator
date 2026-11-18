package azure

import (
	"context"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// Repository is an Azure Repos git repository.
type Repository struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	RemoteURL     string `json:"remoteUrl"`
	WebURL        string `json:"webUrl"`
	DefaultBranch string `json:"defaultBranch"`
	Project       struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"project"`
}

// Repository looks a repository up by name (or id) in a project.
func (c *Client) Repository(ctx context.Context, project, name string) (Repository, error) {
	var r Repository
	err := c.do(ctx, "GET", url.PathEscape(project)+"/_apis/git/repositories/"+url.PathEscape(name), nil, nil, &r, nil)
	return r, err
}

// Definition is a YAML pipeline definition.
type Definition struct {
	ID           int    `json:"id"`
	Name         string `json:"name"`
	Folder       string `json:"folder"`
	YAMLFilename string `json:"yamlFilename"` // as Azure has it (may start with "/")
	RepoID       string `json:"repositoryId"`
	RepoType     string `json:"repositoryType"`
	WebURL       string `json:"url"`
	Project      string `json:"project"` // the project it was listed in
}

type rawDefinition struct {
	ID      int    `json:"id"`
	Name    string `json:"name"`
	Path    string `json:"path"`
	Process struct {
		Type         int    `json:"type"`
		YAMLFilename string `json:"yamlFilename"`
	} `json:"process"`
	Repository struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	} `json:"repository"`
	Links struct {
		Web struct {
			Href string `json:"href"`
		} `json:"web"`
	} `json:"_links"`
}

// Repository types of a definition (repository.type).
const (
	RepoTypeAzure  = "TfsGit"
	RepoTypeGitHub = "GitHub"
)

// Definitions lists the YAML pipeline definitions of an Azure Repos
// repository, following continuation tokens.
func (c *Client) Definitions(ctx context.Context, project, repoID string) ([]Definition, error) {
	return c.DefinitionsFor(ctx, project, RepoTypeAzure, repoID)
}

// DefinitionsFor lists the YAML pipeline definitions of a repository of the
// given type: an Azure Repos id (RepoTypeAzure) or a GitHub "owner/repo"
// (RepoTypeGitHub, matched without regard to case). The server filters by
// repository, and the result is filtered again here, so a server that ignores
// the filter can't hand back another repository's pipelines.
func (c *Client) DefinitionsFor(ctx context.Context, project, repoType, repoID string) ([]Definition, error) {
	var out []Definition
	token := ""
	for page := 0; page < 50; page++ {
		q := url.Values{}
		q.Set("includeAllProperties", "true")
		q.Set("repositoryId", repoID)
		q.Set("repositoryType", repoType)
		q.Set("$top", "500")
		if token != "" {
			q.Set("continuationToken", token)
		}
		var resp struct {
			Value []rawDefinition `json:"value"`
		}
		var hdr http.Header
		if err := c.do(ctx, "GET", url.PathEscape(project)+"/_apis/build/definitions", q, nil, &resp, &hdr); err != nil {
			return nil, err
		}
		for _, d := range resp.Value {
			if d.Process.Type != 2 || d.Process.YAMLFilename == "" {
				continue // a classic (designer) pipeline
			}
			if !strings.EqualFold(d.Repository.Type, repoType) || !strings.EqualFold(d.Repository.ID, repoID) {
				continue
			}
			out = append(out, Definition{ID: d.ID, Name: d.Name, Folder: d.Path, YAMLFilename: d.Process.YAMLFilename,
				RepoID: d.Repository.ID, RepoType: d.Repository.Type, WebURL: d.Links.Web.Href, Project: project})
		}
		token = hdr.Get("X-Ms-Continuationtoken")
		if token == "" {
			break
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// NormalizePath is a repository path as definitions are matched: no leading
// slash, forward slashes, lower case.
func NormalizePath(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	return strings.ToLower(strings.TrimLeft(p, "/"))
}

// MatchDefinitions returns the definitions whose YAML file is relPath (relative
// to the repository top), lowest id first.
func MatchDefinitions(defs []Definition, relPath string) []Definition {
	want := NormalizePath(relPath)
	var out []Definition
	for _, d := range defs {
		if NormalizePath(d.YAMLFilename) == want {
			out = append(out, d)
		}
	}
	return out
}

// Projects lists the names of the organization's projects (at most a few
// thousand), following continuation tokens.
func (c *Client) Projects(ctx context.Context) ([]string, error) {
	var out []string
	token := ""
	for page := 0; page < 20; page++ {
		q := url.Values{}
		q.Set("$top", "500")
		if token != "" {
			q.Set("continuationToken", token)
		}
		var resp struct {
			Value []struct {
				Name string `json:"name"`
			} `json:"value"`
		}
		var hdr http.Header
		if err := c.do(ctx, "GET", "_apis/projects", q, nil, &resp, &hdr); err != nil {
			return nil, err
		}
		for _, p := range resp.Value {
			out = append(out, p.Name)
		}
		token = hdr.Get("X-Ms-Continuationtoken")
		if token == "" {
			break
		}
	}
	sort.Strings(out)
	return out, nil
}

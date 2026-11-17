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

// Definitions lists the YAML pipeline definitions of a repository, following
// continuation tokens.
func (c *Client) Definitions(ctx context.Context, project, repoID string) ([]Definition, error) {
	var out []Definition
	token := ""
	for page := 0; page < 50; page++ {
		q := url.Values{}
		q.Set("includeAllProperties", "true")
		q.Set("repositoryId", repoID)
		q.Set("repositoryType", "TfsGit")
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
			out = append(out, Definition{ID: d.ID, Name: d.Name, Folder: d.Path, YAMLFilename: d.Process.YAMLFilename,
				RepoID: d.Repository.ID, RepoType: d.Repository.Type, WebURL: d.Links.Web.Href})
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

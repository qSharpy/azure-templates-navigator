// Package azuretest is a fake of the Azure DevOps REST API for tests: the
// calls the azure package makes, answered from memory, with responses
// recorded from a real organization on 2026-11-17 (testdata/; the
// organization is renamed fabrikam).
package azuretest

import (
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
)

//go:embed testdata
var testdata embed.FS

// Fixture returns a recorded file, e.g. "preview/T2_tpl_error_inside_template.json".
func Fixture(name string) []byte {
	b, err := testdata.ReadFile("testdata/" + name)
	if err != nil {
		panic(err)
	}
	return b
}

// Recorded preview answers by case name (the spike's names), with the HTTP
// status Azure gave.
func PreviewFixture(name string) (int, []byte) {
	b := Fixture("preview/" + name + ".json")
	var d struct {
		FinalYAML *string `json:"finalYaml"`
		TypeKey   string  `json:"typeKey"`
	}
	_ = json.Unmarshal(b, &d)
	switch {
	case d.FinalYAML != nil:
		return 200, b
	case d.TypeKey == "PipelineValidationException":
		return 400, b
	}
	return 500, b
}

// Repo is a repository the fake knows.
type Repo struct {
	ID, Name, Project, RemoteURL string
}

// Def is a pipeline definition the fake knows.
type Def struct {
	ID           int
	Name         string
	Project      string
	YAMLFilename string
	RepoID       string
}

// Call is a request the fake received.
type Call struct {
	Method, Path, Query string
	Header              http.Header
	Body                map[string]interface{}
}

// PreviewFunc answers a preview: status and body.
type PreviewFunc func(project string, definition int, body map[string]interface{}) (int, []byte)

// Server is the fake.
type Server struct {
	*httptest.Server
	Org      string // the only organization it serves ("fabrikam")
	Tenant   string // x-vss-resourcetenant of the anonymous connectionData ("" = header absent)
	Token    string // the bearer token it accepts ("" = any)
	UserID   string
	UserName string
	Repos    []Repo
	Defs     []Def
	PageSize int // definitions per page (0 = all at once)
	Preview  PreviewFunc
	// Throttle answers this many requests with 429 (Retry-After: 0) first.
	Throttle int

	mu    sync.Mutex
	calls []Call
}

// New starts a fake for organization fabrikam with the recorded definitions
// and repositories. Close it when done.
func New() *Server {
	s := &Server{Org: "fabrikam", Tenant: "00000000-0000-0000-0000-000000000000", UserID: "3f2a9c1e-7b4d-4e8a-9c6f-1d2e3f4a5b6c", UserName: "Test User"}
	var defs struct {
		Value []struct {
			ID         int                           `json:"id"`
			Name       string                        `json:"name"`
			Process    struct{ YAMLFilename string } `json:"process"`
			Repository struct{ ID string }           `json:"repository"`
		} `json:"value"`
	}
	_ = json.Unmarshal(Fixture("definitions.json"), &defs)
	for _, d := range defs.Value {
		s.Defs = append(s.Defs, Def{ID: d.ID, Name: d.Name, Project: "personal", YAMLFilename: d.Process.YAMLFilename, RepoID: d.Repository.ID})
	}
	var repos struct {
		Value []struct {
			ID, Name, RemoteURL string
			Project             struct{ Name string }
		} `json:"value"`
	}
	_ = json.Unmarshal(Fixture("repositories.json"), &repos)
	for _, r := range repos.Value {
		s.Repos = append(s.Repos, Repo{ID: r.ID, Name: r.Name, Project: r.Project.Name, RemoteURL: r.RemoteURL})
	}
	s.Preview = func(string, int, map[string]interface{}) (int, []byte) { return PreviewFixture("A_empty") }
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

// Calls returns the requests received so far.
func (s *Server) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Call(nil), s.calls...)
}

// Previews returns the bodies of the previews received so far.
func (s *Server) Previews() []Call {
	var out []Call
	for _, c := range s.Calls() {
		if strings.HasSuffix(c.Path, "/preview") {
			out = append(out, c)
		}
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if b, ok := v.([]byte); ok {
		_, _ = w.Write(b)
		return
	}
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	call := Call{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Header: r.Header.Clone()}
	if b, _ := io.ReadAll(r.Body); len(b) > 0 {
		_ = json.Unmarshal(b, &call.Body)
	}
	s.mu.Lock()
	s.calls = append(s.calls, call)
	throttle := s.Throttle > 0
	if throttle {
		s.Throttle--
	}
	s.mu.Unlock()

	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 2 || !strings.EqualFold(parts[0], s.Org) {
		writeJSON(w, 404, map[string]string{"message": "TF400898: organization not found", "typeKey": "NotFound"})
		return
	}
	parts = parts[1:]
	auth := r.Header.Get("Authorization")
	if parts[0] == "_apis" && parts[1] == "connectionData" && auth == "" {
		if s.Tenant != "" {
			w.Header().Set("X-VSS-ResourceTenant", s.Tenant)
		}
		w.Header().Set("Location", "https://spsprodweu5.vssps.visualstudio.com/_signin")
		w.WriteHeader(302)
		return
	}
	if !strings.HasPrefix(auth, "Bearer ") || (s.Token != "" && auth != "Bearer "+s.Token) {
		writeJSON(w, 401, Fixture("unauthorized.json"))
		return
	}
	if r.Header.Get("X-VSS-ForceMsaPassThrough") != "true" {
		// what an MSA-backed organization does without the header
		w.Header().Set("Location", "https://spsprodweu5.vssps.visualstudio.com/_signin")
		w.WriteHeader(302)
		return
	}
	if throttle {
		w.Header().Set("Retry-After", "0")
		writeJSON(w, 429, map[string]string{"message": "TF400733: throttled"})
		return
	}
	w.Header().Set("X-RateLimit-Cost", "0.07")
	switch {
	case len(parts) == 2 && parts[0] == "_apis" && parts[1] == "connectionData":
		writeJSON(w, 200, map[string]interface{}{"authenticatedUser": map[string]interface{}{"id": s.UserID, "providerDisplayName": s.UserName}})
	case len(parts) == 4 && parts[1] == "_apis" && parts[2] == "git" && parts[3] == "repositories":
		var out []map[string]interface{}
		for _, rp := range s.Repos {
			if strings.EqualFold(rp.Project, parts[0]) {
				out = append(out, s.repoJSON(rp))
			}
		}
		writeJSON(w, 200, map[string]interface{}{"count": len(out), "value": out})
	case len(parts) == 5 && parts[1] == "_apis" && parts[2] == "git" && parts[3] == "repositories":
		for _, rp := range s.Repos {
			if strings.EqualFold(rp.Project, parts[0]) && (strings.EqualFold(rp.Name, parts[4]) || rp.ID == parts[4]) {
				writeJSON(w, 200, s.repoJSON(rp))
				return
			}
		}
		writeJSON(w, 404, map[string]string{"message": fmt.Sprintf("TF401019: The Git repository with name or identifier %s does not exist or you do not have permissions for the operation you are attempting.", parts[4]), "typeKey": "GitRepositoryNotFoundException"})
	case len(parts) == 4 && parts[1] == "_apis" && parts[2] == "build" && parts[3] == "definitions":
		s.definitions(w, r, parts[0])
	case len(parts) == 5 && parts[1] == "_apis" && parts[2] == "pipelines" && parts[4] == "preview":
		id, _ := strconv.Atoi(parts[3])
		found := false
		for _, d := range s.Defs {
			found = found || (d.ID == id && strings.EqualFold(d.Project, parts[0]))
		}
		if !found {
			writeJSON(w, 404, map[string]string{"message": fmt.Sprintf("Pipeline %d not found.", id), "typeKey": "PipelineNotFoundException"})
			return
		}
		st, b := s.Preview(parts[0], id, call.Body)
		writeJSON(w, st, b)
	default:
		writeJSON(w, 404, map[string]string{"message": "not faked: " + r.URL.Path})
	}
}

func (s *Server) repoJSON(rp Repo) map[string]interface{} {
	return map[string]interface{}{"id": rp.ID, "name": rp.Name, "remoteUrl": rp.RemoteURL, "defaultBranch": "refs/heads/main",
		"project": map[string]interface{}{"name": rp.Project}}
}

func (s *Server) definitions(w http.ResponseWriter, r *http.Request, project string) {
	q := r.URL.Query()
	var all []map[string]interface{}
	for _, d := range s.Defs {
		if !strings.EqualFold(d.Project, project) || (q.Get("repositoryId") != "" && q.Get("repositoryId") != d.RepoID) {
			continue
		}
		all = append(all, map[string]interface{}{
			"id": d.ID, "name": d.Name, "path": "\\",
			"process":    map[string]interface{}{"type": 2, "yamlFilename": d.YAMLFilename},
			"repository": map[string]interface{}{"id": d.RepoID, "type": "TfsGit"},
			"_links":     map[string]interface{}{"web": map[string]interface{}{"href": fmt.Sprintf("%s/%s/%s/_build/definition?definitionId=%d", s.URL, s.Org, project, d.ID)}},
		})
	}
	start, _ := strconv.Atoi(q.Get("continuationToken"))
	end := len(all)
	if s.PageSize > 0 && start+s.PageSize < end {
		end = start + s.PageSize
		w.Header().Set("X-MS-ContinuationToken", strconv.Itoa(end))
	}
	if start > len(all) {
		start = len(all)
	}
	page := all[start:end]
	writeJSON(w, 200, map[string]interface{}{"count": len(page), "value": page})
}

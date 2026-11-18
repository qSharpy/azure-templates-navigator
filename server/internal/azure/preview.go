package azure

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// RepoOverride sets the version of one repository resource for a preview:
// "self" (the pipeline's own repository) or an alias.
type RepoOverride struct {
	RefName string `json:"refName,omitempty"`
	Version string `json:"version,omitempty"`
}

// PreviewRequest is the body of a preview (RunPipelineParameters).
type PreviewRequest struct {
	Repositories       map[string]RepoOverride
	TemplateParameters map[string]interface{}
	YAMLOverride       string
}

func (r PreviewRequest) body() map[string]interface{} {
	b := map[string]interface{}{"previewRun": true}
	if len(r.Repositories) > 0 {
		b["resources"] = map[string]interface{}{"repositories": r.Repositories}
	}
	if len(r.TemplateParameters) > 0 {
		b["templateParameters"] = r.TemplateParameters
	}
	if r.YAMLOverride != "" {
		b["yamlOverride"] = r.YAMLOverride
	}
	return b
}

// PreviewResult is Azure's verdict on a pipeline: it compiles (FinalYAML) or
// it is rejected (Problems).
type PreviewResult struct {
	FinalYAML string
	Rejected  bool
	Problems  []Problem
}

// Preview asks Azure to compile a pipeline without running it. A pipeline
// Azure rejects is a result, not an error; an error means there is no verdict
// (sign-in, permissions, a ref or commit the server can't see, network).
func (c *Client) Preview(ctx context.Context, project string, definition int, req PreviewRequest) (PreviewResult, error) {
	var out struct {
		FinalYAML string `json:"finalYaml"`
	}
	path := url.PathEscape(project) + "/_apis/pipelines/" + strconv.Itoa(definition) + "/preview"
	err := c.do(ctx, "POST", path, nil, req.body(), &out, nil)
	if err == nil {
		return PreviewResult{FinalYAML: out.FinalYAML}, nil
	}
	var he *HTTPError
	if errors.As(err, &he) && he.Status == 400 && he.TypeKey == "PipelineValidationException" {
		if strings.Contains(he.Message, "Could not get the latest source version") || strings.Contains(he.Message, "Unable to resolve the reference") {
			return PreviewResult{}, fmt.Errorf("Azure can't read a repository at the version asked for: %s", he.Message)
		}
		return PreviewResult{Rejected: true, Problems: ParseProblems(he.Message)}, nil
	}
	return PreviewResult{}, err
}

// ---------------------------------------------------------------------------
// problems
// ---------------------------------------------------------------------------

// Problem is one line of a PipelineValidationException.
type Problem struct {
	AzurePath string `json:"azurePath"` // "/templates/stages.yml@templates" ("" without a location)
	Path      string `json:"path"`      // repository-relative: "templates/stages.yml"
	Alias     string `json:"alias"`     // "" for the pipeline's own repository
	Line      int    `json:"line"`      // 1-based; 0 when unknown
	Col       int    `json:"col"`
	Message   string `json:"message"`
}

// Key identifies a problem without its position, for comparing the base and
// the change.
func (p Problem) Key() string {
	return p.AzurePath + "\x00" + p.Message
}

var (
	locatedRe = regexp.MustCompile(`^(/[^\s()]+?)(?:@([A-Za-z0-9_.\-]+))? \(Line: (\d+), Col: (\d+)\):\s*(.*)$`)
	fileRe    = regexp.MustCompile(`^(/[^\s():]+?)(?:@([A-Za-z0-9_.\-]+))?:\s+(.*)$`)
	noise     = []string{"Object reference not set to an instance of an object."}
)

// ParseProblems splits an error message into problems, one per line, with
// the file and position Azure gives. Lines that carry no information of their
// own are folded into the problem before them.
func ParseProblems(message string) []Problem {
	var out []Problem
	for _, l := range strings.Split(strings.ReplaceAll(message, "\r\n", "\n"), "\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		isNoise := false
		for _, n := range noise {
			isNoise = isNoise || l == n
		}
		if isNoise && len(out) > 0 {
			continue
		}
		var p Problem
		if m := locatedRe.FindStringSubmatch(l); m != nil {
			p.Line, _ = strconv.Atoi(m[3])
			p.Col, _ = strconv.Atoi(m[4])
			p.AzurePath, p.Alias, p.Message = m[1], m[2], strings.TrimSpace(m[5])
		} else if m := fileRe.FindStringSubmatch(l); m != nil {
			p.AzurePath, p.Alias, p.Message = m[1], m[2], strings.TrimSpace(m[3])
		} else {
			p.Message = l
		}
		if p.AzurePath != "" {
			p.Path = strings.TrimLeft(p.AzurePath, "/")
			if p.Alias != "" {
				p.AzurePath += "@" + p.Alias
			}
		}
		if p.Alias == "self" {
			p.Alias = ""
		}
		out = append(out, p)
	}
	return out
}

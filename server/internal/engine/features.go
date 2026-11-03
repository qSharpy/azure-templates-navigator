package engine

import (
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/qSharpy/azure-templates-navigator/server/internal/pipeline"
	"github.com/qSharpy/azure-templates-navigator/server/internal/repos"
)

// ClientFeatures describes what the connected IDE can render.
type ClientFeatures struct {
	// CommandLinks: hover markdown may contain `command:` links (VS Code).
	CommandLinks bool `json:"commandLinks"`
	// CommandPrefix is prepended to client-side command ids in links and
	// code lenses.
	CommandPrefix string `json:"commandPrefix"`
	// PlainMarkdown: no emoji and no inline HTML (IntelliJ renders hovers in
	// its own documentation popup, where neither looks native).
	PlainMarkdown bool `json:"plainMarkdown"`
}

var (
	emojiRe     = regexp.MustCompile(`(?:📄|🔧|🔗|⚠️|⏳|📦)\s?`)
	colorSpanRe = regexp.MustCompile(`<span style="color:[^"]*">(.*?)</span>`)
)

// Plain converts hover markdown for clients with PlainMarkdown set.
func Plain(md string) string {
	md = emojiRe.ReplaceAllString(md, "")
	return colorSpanRe.ReplaceAllString(md, "$1")
}

// Hover is a hover result. Columns are byte offsets.
type Hover struct {
	Markdown string
	Line     int
	StartCol int
	EndCol   int
}

// Location is a target position.
type Location struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Col  int    `json:"col"`
}

// FileURI converts a path to a file:// URI.
func FileURI(p string) string {
	p = filepath.ToSlash(p)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p // windows drive letter
	}
	u := url.URL{Scheme: "file", Path: p}
	return u.String()
}

func commandLink(cf ClientFeatures, label, cmd string, arg interface{}) string {
	b, _ := json.Marshal([]interface{}{arg})
	return fmt.Sprintf("[%s](command:%s%s?%s)", label, cf.CommandPrefix, cmd, url.QueryEscape(string(b)))
}

// HoverAt computes hover content at (line, byte col).
func (e *Engine) HoverAt(path string, line, col int, cf ClientFeatures) (Hover, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ensureLinked()
	f := e.file(path)
	if f == nil || line < 0 || line >= len(f.lines) {
		return Hover{}, false
	}
	text := f.lines[line]

	if v, ok := pipeline.VariableRefAt(text, col); ok {
		return Hover{Markdown: variableHover(f, v.Name), Line: line, StartCol: v.Start, EndCol: v.End}, true
	}
	if pr, ok := pipeline.ParamRefAt(text, col); ok {
		if p, ok := pipeline.ParamByName(f.params, pr.Name); ok {
			md := paramDoc(p, e.settings.RequiredParameterColor) + fmt.Sprintf("\n\n_Declared on line %d of this template_", p.Line+1)
			return Hover{Markdown: md, Line: line, StartCol: pr.Start, EndCol: pr.End}, true
		}
		md := fmt.Sprintf("**⚠️ Undeclared parameter** `%s`\n\nNot declared in this file's `parameters:` block, so it evaluates to an empty string.", pr.Name)
		return Hover{Markdown: md, Line: line, StartCol: pr.Start, EndCol: pr.End}, true
	}
	if r, ok := f.refAt(line); ok {
		res := e.resolve(r, f)
		return Hover{Markdown: e.templateHover(f, r, res, cf), Line: line, StartCol: r.KeyCol, EndCol: r.ValueEnd}, true
	}
	// Hover over a parameter key passed at a call site
	if tr, pp, ok := e.passedParamKeyAt(f, line, col); ok {
		if res := e.resolve(tr, f); res.Exists {
			if tf := e.file(res.Path); tf != nil {
				if p, ok := pipeline.ParamByName(tf.params, pp.Name); ok {
					md := paramDoc(p, e.settings.RequiredParameterColor) + fmt.Sprintf("\n\n_From template_ `%s`", tr.Ref)
					return Hover{Markdown: md, Line: line, StartCol: pp.NameCol, EndCol: pp.NameCol + len(pp.Name)}, true
				}
			}
		}
	}
	return Hover{}, false
}

func paramDoc(p pipeline.Param, color string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**🔧 Parameter** `%s`: `%s`", p.Name, typeLabel(p.Type))
	if p.Required {
		fmt.Fprintf(&b, " — <span style=\"color:%s;\">**required**</span>", color)
	} else if p.Default != "" {
		fmt.Fprintf(&b, " — default: `%s`", p.Default)
	}
	if p.DisplayName != "" {
		fmt.Fprintf(&b, "\n\n%s", p.DisplayName)
	}
	if len(p.Values) > 0 {
		fmt.Fprintf(&b, "\n\nAllowed values: %s", codeList(p.Values))
	}
	return b.String()
}

func codeList(vs []string) string {
	q := make([]string, len(vs))
	for i, v := range vs {
		q[i] = "`" + v + "`"
	}
	return strings.Join(q, ", ")
}

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	}
	return fmt.Sprintf("%d days ago", int(d.Hours()/24))
}

func (e *Engine) repoLine(res Resolution) string {
	if res.Repo == nil {
		return ""
	}
	parts := []string{fmt.Sprintf("`%s`", res.Repo.Name), refLabel(res.Repo.Ref)}
	if l := res.RepoLoc; l != nil {
		switch l.Source {
		case repos.SourceSibling:
			parts = append(parts, "local clone (working copy)")
		case repos.SourceLocal:
			parts = append(parts, "configured local path")
		case repos.SourceCache:
			if !l.FetchedAt.IsZero() {
				parts = append(parts, "cached, fetched "+ago(l.FetchedAt))
			} else if l.State == repos.StateReady {
				parts = append(parts, "cached")
			}
		}
	}
	return fmt.Sprintf("**🔗 Repository** `@%s`: %s", res.Alias, strings.Join(parts, " · "))
}

func (e *Engine) templateHover(f *fileInfo, r pipeline.TemplateRef, res Resolution, cf ClientFeatures) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**📄 Template:** `%s`\n\n", r.Ref)
	switch res.Kind {
	case KindDynamic:
		b.WriteString("_This reference contains an expression and is resolved at pipeline run time._")
		return b.String()
	case KindUnknownAlias:
		fmt.Fprintf(&b, "**⚠️ Repository alias not found:** `@%s`\n\n", res.Alias)
		fmt.Fprintf(&b, "_Add a `resources.repositories` entry with `repository: %s` to the pipeline._", res.Alias)
		return b.String()
	case KindRepoUnavailable:
		b.WriteString(e.repoLine(res) + "\n\n")
		loc := res.RepoLoc
		arg := RepoRequest{Alias: res.Alias, File: f.path}
		switch loc.State {
		case repos.StateFetching:
			b.WriteString("⏳ **Fetching repository…** The template will be available in a moment.")
		case repos.StateUnconfigured:
			fmt.Fprintf(&b, "**⚠️ Can't fetch automatically:** %s\n\n", loc.Error)
			b.WriteString("Map the repository to a clone URL in `repositories.urls` (IDE settings) or in a committed `" + ProjectConfigFile + "`.")
		default:
			if loc.State == repos.StateFailed {
				fmt.Fprintf(&b, "**⚠️ Fetch failed:** %s\n\n", loc.Error)
			} else {
				b.WriteString("**Repository not downloaded yet.**\n\n")
			}
			if cf.CommandLinks {
				b.WriteString(commandLink(cf, "$(cloud-download) Fetch repository", "fetchRepository", arg) + " · ")
			}
			b.WriteString("or use the quick fix on this line.")
		}
		return b.String()
	}

	if res.Kind == KindExternal {
		b.WriteString(e.repoLine(res) + "\n\n")
	}
	if !res.Exists {
		fmt.Fprintf(&b, "**⚠️ Template not found:**\n\n`%s`", res.Path)
		return b.String()
	}
	open := fmt.Sprintf("[Open](%s)", FileURI(res.Path))
	if cf.CommandLinks {
		open = commandLink(cf, "$(go-to-file) Open", "openTemplate", map[string]interface{}{"filePath": res.Path}) +
			" · " + commandLink(cf, "$(split-horizontal) Open to side", "openTemplate", map[string]interface{}{"filePath": res.Path, "beside": true})
	}
	b.WriteString(open + "\n\n")

	tf := e.file(res.Path)
	if tf == nil || len(tf.params) == 0 {
		b.WriteString("_No parameters defined_")
		return b.String()
	}
	req := 0
	for _, p := range tf.params {
		if p.Required {
			req++
		}
	}
	fmt.Fprintf(&b, "**Parameters** (%d", len(tf.params))
	if req > 0 {
		fmt.Fprintf(&b, ", %d required", req)
	}
	b.WriteString(")")
	if cf.CommandLinks {
		b.WriteString(" · " + commandLink(cf, "$(search) Search", "searchParameters", map[string]interface{}{"filePath": res.Path}))
	}
	b.WriteString(":\n\n")
	color := e.settings.RequiredParameterColor
	for _, p := range tf.params {
		name := fmt.Sprintf("**%s**", p.Name)
		if p.Required {
			name = fmt.Sprintf("<span style=\"color:%s;\">**%s**</span>", color, p.Name)
		}
		fmt.Fprintf(&b, "- %s: `%s`", name, typeLabel(p.Type))
		if p.Required {
			b.WriteString(" _(required)_")
		} else if p.Default != "" {
			fmt.Fprintf(&b, " — default: `%s`", p.Default)
		}
		if len(p.Values) > 0 {
			fmt.Fprintf(&b, " — one of %s", codeList(p.Values))
		}
		b.WriteString("\n")
	}
	return b.String()
}

func variableHover(f *fileInfo, name string) string {
	vs := pipeline.ParseVariables(f.lines)
	var b strings.Builder
	if v, ok := vs.Vars[name]; ok {
		fmt.Fprintf(&b, "**📦 Variable:** `%s`\n\n", name)
		if v.Value != "" {
			fmt.Fprintf(&b, "**Value:** `%s`\n\n", v.Value)
		} else {
			b.WriteString("**Value:** _(empty string)_\n\n")
		}
		fmt.Fprintf(&b, "**Source:** `variables:` block (line %d)", v.Line+1)
		return b.String()
	}
	if pipeline.IsSystemVariable(name) {
		fmt.Fprintf(&b, "**📦 System variable:** `%s`\n\n", name)
		b.WriteString("Azure DevOps predefined variable — available at runtime.\n\n")
		b.WriteString("[View predefined variables ↗](https://learn.microsoft.com/en-us/azure/devops/pipelines/build/variables)")
		return b.String()
	}
	fmt.Fprintf(&b, "**📦 Variable:** `%s`\n\n_Not defined in this file's `variables:` blocks._", name)
	if len(vs.Groups) > 0 {
		names := make([]string, len(vs.Groups))
		for i, g := range vs.Groups {
			names[i] = g.Name
		}
		fmt.Fprintf(&b, "\n\nMay come from variable group(s): %s", codeList(names))
	}
	return b.String()
}

// Definition returns the go-to-definition target at (line, byte col).
func (e *Engine) Definition(path string, line, col int) (Location, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ensureLinked()
	f := e.file(path)
	if f == nil || line < 0 || line >= len(f.lines) {
		return Location{}, false
	}
	text := f.lines[line]
	if r, ok := f.refAt(line); ok {
		res := e.resolve(r, f)
		if res.Exists {
			return Location{Path: res.Path}, true
		}
		return Location{}, false
	}
	if pr, ok := pipeline.ParamRefAt(text, col); ok {
		if p, ok := pipeline.ParamByName(f.params, pr.Name); ok {
			return Location{Path: f.path, Line: p.Line, Col: p.NameCol}, true
		}
	}
	if v, ok := pipeline.VariableRefAt(text, col); ok {
		if vv, ok := pipeline.ParseVariables(f.lines).Vars[v.Name]; ok {
			return Location{Path: f.path, Line: vv.Line, Col: pipeline.Indent(f.lines[vv.Line])}, true
		}
	}
	if tr, pp, ok := e.passedParamKeyAt(f, line, col); ok {
		if res := e.resolve(tr, f); res.Exists {
			if tf := e.file(res.Path); tf != nil {
				if p, ok := pipeline.ParamByName(tf.params, pp.Name); ok {
					return Location{Path: res.Path, Line: p.Line, Col: p.NameCol}, true
				}
			}
		}
	}
	return Location{}, false
}

// passedParamKeyAt returns the call-site parameter whose name is under the
// cursor, with the template call it belongs to.
func (e *Engine) passedParamKeyAt(f *fileInfo, line, col int) (pipeline.TemplateRef, pipeline.PassedParam, bool) {
	tl, pp, ok := pipeline.PassedParamAt(f.lines, line, col)
	if !ok || pp.Line != line || col < pp.NameCol || col > pp.NameCol+len(pp.Name) {
		return pipeline.TemplateRef{}, pipeline.PassedParam{}, false
	}
	tr, ok := f.refAt(tl)
	return tr, pp, ok
}

// CompletionItem is an IDE-independent completion proposal.
type CompletionItem struct {
	Label         string `json:"label"`
	Detail        string `json:"detail"`
	Documentation string `json:"documentation"`
	InsertText    string `json:"insertText"`
	SortText      string `json:"sortText"`
	Kind          string `json:"kind"` // property | value | file
	FilterText    string `json:"filterText,omitempty"`
	// Replace range (byte cols on the cursor line); -1 = word at cursor
	ReplaceStart int `json:"-"`
	ReplaceEnd   int `json:"-"`
}

// Complete returns completion items at (line, byte col).
func (e *Engine) Complete(path string, line, col int) []CompletionItem {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ensureLinked()
	f := e.file(path)
	if f == nil || line < 0 || line >= len(f.lines) {
		return nil
	}
	text := f.lines[line]
	if col > len(text) {
		col = len(text)
	}

	// 1. template path completion: "- template: <cursor>"
	if m := templateKeyPrefixRe.FindStringIndex(text); m != nil && col >= m[1] {
		return e.completeTemplatePaths(f, text, m[1], col)
	}

	ctx, ok := pipeline.FindCallSite(f.lines, line)
	if !ok || !ctx.InParameters {
		return nil
	}
	tr, ok := f.refAt(ctx.TemplateLine)
	if !ok {
		return nil
	}
	res := e.resolve(tr, f)
	if !res.Exists {
		return nil
	}
	tf := e.file(res.Path)
	if tf == nil {
		return nil
	}
	cs := pipeline.ParseCallSite(f.lines, ctx.TemplateLine)

	// 2. value completion: "  param: <cursor>"
	before := text[:col]
	trimmed := strings.TrimLeft(before, " \t")
	if i := strings.Index(trimmed, ":"); i > 0 {
		name := strings.TrimSpace(trimmed[:i])
		p, ok := pipeline.ParamByName(tf.params, name)
		if !ok {
			return nil
		}
		valueStart := len(before) - len(trimmed) + i + 1
		for valueStart < len(text) && text[valueStart] == ' ' {
			valueStart++
		}
		var vals []string
		if len(p.Values) > 0 {
			vals = p.Values
		} else if strings.EqualFold(p.Type, "boolean") {
			vals = []string{"true", "false"}
		}
		var items []CompletionItem
		for i, v := range vals {
			items = append(items, CompletionItem{Label: v, InsertText: v, Kind: "value", SortText: fmt.Sprintf("%03d", i),
				Detail: fmt.Sprintf("%s value", p.Name), ReplaceStart: valueStart, ReplaceEnd: len(strings.TrimRight(text, " "))})
		}
		return items
	}

	// 3. parameter name completion
	wordStart := len(before) - len(trimmed)
	var items []CompletionItem
	for _, p := range tf.params {
		item := CompletionItem{
			Label:         p.Name,
			Kind:          "property",
			Detail:        typeLabel(p.Type),
			Documentation: paramDoc(p, e.settings.RequiredParameterColor) + fmt.Sprintf("\n\n_From template_ `%s`", tr.Ref),
			InsertText:    p.Name + ": ",
			SortText:      "1_" + p.Name,
			ReplaceStart:  wordStart,
			ReplaceEnd:    col,
		}
		if p.Required {
			item.Detail += " (required)"
			item.SortText = "0_" + p.Name
		}
		if _, passed := cs.Passed[p.Name]; passed {
			item.Detail += " (already set)"
			item.SortText = "2_" + p.Name
		}
		items = append(items, item)
	}
	return items
}

var templateKeyPrefixRe = regexp.MustCompile(`^\s*(?:-\s+)?template\s*:\s*`)

func (e *Engine) completeTemplatePaths(f *fileInfo, text string, start, col int) []CompletionItem {
	_ = col
	dir := filepath.Dir(f.path)
	repoRoot := e.repoRoot(dir)
	var items []CompletionItem
	for _, p := range e.workspaceFiles() {
		if p == f.path || !strings.HasPrefix(p, repoRoot+string(filepath.Separator)) {
			continue
		}
		pf := e.file(p)
		if pf == nil || pf.isPipeline {
			continue
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			continue
		}
		rel = filepath.ToSlash(rel)
		abs, _ := filepath.Rel(repoRoot, p)
		abs = "/" + filepath.ToSlash(abs)
		label := rel
		if strings.HasPrefix(rel, "../") {
			label = abs
		}
		detail := fmt.Sprintf("%d parameter(s)", len(pf.params))
		items = append(items, CompletionItem{Label: label, InsertText: label, Kind: "file", Detail: detail,
			SortText: fmt.Sprintf("%03d_%s", strings.Count(label, "/"), label), FilterText: label,
			ReplaceStart: start, ReplaceEnd: len(strings.TrimRight(pipeline.StripComment(text), " "))})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].SortText < items[j].SortText })
	if len(items) > 200 {
		items = items[:200]
	}
	return items
}

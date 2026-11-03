package pipeline

import (
	"regexp"
	"strings"
)

var (
	paramDotRe     = regexp.MustCompile(`\bparameters\.([\p{L}_][\p{L}\p{N}_]*)`)
	paramIndexRe   = regexp.MustCompile(`\bparameters\[\s*['"]([\p{L}_][\p{L}\p{N}_-]*)['"]\s*\]`)
	exprRe         = regexp.MustCompile(`\$\{\{.*?\}\}|\$\[.*?\]`)
	paramsWordRe   = regexp.MustCompile(`\bparameters\b`)
	conditionKeyRe = regexp.MustCompile(`^\s*(?:-\s+)?condition\s*:`)
)

// ParamUsage summarises how a template body references its parameters.
type ParamUsage struct {
	Names map[string]bool
	// UsesAll is true when the whole `parameters` object is referenced (e.g.
	// convertToJson(parameters) or ${{ each p in parameters }}), in which case
	// every parameter must be considered used.
	UsesAll bool
}

// CollectParamUsage scans a document (excluding the declaration block) for
// parameter references.
func CollectParamUsage(lines []string) ParamUsage {
	u := ParamUsage{Names: map[string]bool{}}
	declStart, declEnd := ParametersBlock(lines)
	for i, l := range lines {
		if i >= declStart && i <= declEnd && declStart >= 0 {
			continue
		}
		if !strings.Contains(l, "parameters") {
			continue
		}
		for _, m := range paramDotRe.FindAllStringSubmatch(l, -1) {
			u.Names[m[1]] = true
		}
		for _, m := range paramIndexRe.FindAllStringSubmatch(l, -1) {
			u.Names[m[1]] = true
		}
		// Bare `parameters` inside an expression (or a condition) means the
		// entire object is used.
		var segments []string
		for _, e := range exprRe.FindAllString(l, -1) {
			segments = append(segments, e)
		}
		if conditionKeyRe.MatchString(l) {
			segments = append(segments, l)
		}
		for _, seg := range segments {
			for _, loc := range paramsWordRe.FindAllStringIndex(seg, -1) {
				rest := strings.TrimLeft(seg[loc[1]:], " \t")
				if !strings.HasPrefix(rest, ".") && !strings.HasPrefix(rest, "[") {
					u.UsesAll = true
				}
			}
		}
	}
	return u
}

// ParametersBlock returns the first and last line of the top-level
// `parameters:` declaration block, or (-1, -1) when there is none.
func ParametersBlock(lines []string) (start, end int) {
	start = FindTopLevelParameters(lines)
	if start < 0 {
		return -1, -1
	}
	end = start
	for i := start + 1; i < len(lines); i++ {
		l := lines[i]
		if IsBlank(l) || IsComment(l) {
			continue
		}
		if Indent(l) == 0 && !strings.HasPrefix(l, "-") {
			break
		}
		end = i
	}
	return start, end
}

// ParamRefLoc is a `parameters.x` / `parameters['x']` reference inside a
// template expression. NameStart/NameEnd are byte columns of the name.
type ParamRefLoc struct {
	Name               string
	Line               int
	NameStart, NameEnd int
}

var templateExprRe = regexp.MustCompile(`\$\{\{.*?\}\}`)

// FindParamRefs returns every parameter reference made inside `${{ }}`
// expressions outside the declaration block. References that are members of
// something else (`step.parameters.x`, `stage.parameters['x']`) are not
// references to this template's parameters and are skipped.
func FindParamRefs(lines []string) []ParamRefLoc {
	declStart, declEnd := ParametersBlock(lines)
	var out []ParamRefLoc
	for i, l := range lines {
		if declStart >= 0 && i >= declStart && i <= declEnd {
			continue
		}
		if !strings.Contains(l, "parameters") || IsComment(l) {
			continue
		}
		clean := StripComment(l)
		for _, span := range templateExprRe.FindAllStringIndex(clean, -1) {
			expr := clean[span[0]:span[1]]
			for _, re := range []*regexp.Regexp{paramDotRe, paramIndexRe} {
				for _, m := range re.FindAllStringSubmatchIndex(expr, -1) {
					if m[0] > 0 && expr[m[0]-1] == '.' {
						continue
					}
					out = append(out, ParamRefLoc{Name: expr[m[2]:m[3]], Line: i,
						NameStart: span[0] + m[2], NameEnd: span[0] + m[3]})
				}
			}
		}
	}
	return out
}

// ParamRef is a `parameters.x` usage under the cursor.
type ParamRef struct {
	Name       string
	Start, End int
}

// ParamRefAt returns the parameter reference under a byte column.
func ParamRefAt(line string, col int) (ParamRef, bool) {
	for _, re := range []*regexp.Regexp{paramDotRe, paramIndexRe} {
		for _, m := range re.FindAllStringSubmatchIndex(line, -1) {
			if col >= m[0] && col <= m[1] {
				return ParamRef{Name: line[m[2]:m[3]], Start: m[0], End: m[1]}, true
			}
		}
	}
	return ParamRef{}, false
}

// ---------------------------------------------------------------------------
// Type inference for call-site values
// ---------------------------------------------------------------------------

var (
	boolRe   = regexp.MustCompile(`(?i)^(true|false|yes|no|on|off|y|n)$`)
	numberRe = regexp.MustCompile(`^-?\d+(\.\d+)?([eE][-+]?\d+)?$`)
)

// InferValueType classifies an inline YAML scalar for basic type checking:
// "boolean" | "number" | "object" | "string".
func InferValueType(value string) string {
	v := strings.TrimSpace(value)
	switch {
	case v == "":
		return "string"
	case strings.HasPrefix(v, "[") || strings.HasPrefix(v, "{"):
		return "object"
	case strings.HasPrefix(v, "'") || strings.HasPrefix(v, "\""):
		return "string"
	case boolRe.MatchString(v):
		return "boolean"
	case numberRe.MatchString(v):
		return "number"
	}
	return "string"
}

// compatibleTypes maps a declared parameter type to the inferred value kinds
// that are accepted for it.
var compatibleTypes = map[string][]string{
	"string":            {"string", "number", "boolean"}, // YAML scalars are coerced to string
	"number":            {"number", "string"},
	"boolean":           {"boolean"},
	"object":            {"object", "string", "number", "boolean"},
	"step":              {"object"},
	"steplist":          {"object"},
	"job":               {"object"},
	"joblist":           {"object"},
	"deployment":        {"object"},
	"deploymentlist":    {"object"},
	"stage":             {"object"},
	"stagelist":         {"object"},
	"filepath":          {"string"},
	"environment":       {"string", "object"},
	"pool":              {"string", "object"},
	"securefile":        {"string"},
	"serviceconnection": {"string"},
	"container":         {"object", "string"},
	"containerlist":     {"object"},
}

// TypeCompatible reports whether an inline value fits a declared type.
// Unknown or untyped declarations are always compatible.
func TypeCompatible(declaredType, value string) (inferred string, ok bool) {
	inferred = InferValueType(value)
	allowed, known := compatibleTypes[strings.ToLower(declaredType)]
	if !known {
		return inferred, true
	}
	if strings.EqualFold(declaredType, "number") && inferred == "string" {
		// quoted numbers are fine; a bare word is not
		uq := Unquote(value)
		return inferred, numberRe.MatchString(uq)
	}
	for _, a := range allowed {
		if a == inferred {
			return inferred, true
		}
	}
	return inferred, false
}

// CanonicalLiteral returns a placeholder literal for a parameter type.
func CanonicalLiteral(paramType string) string {
	switch strings.ToLower(paramType) {
	case "boolean":
		return "true"
	case "number":
		return "0"
	case "object":
		return "{}"
	case "step", "steplist", "job", "joblist", "deployment", "deploymentlist", "stage", "stagelist", "containerlist":
		return "[]"
	}
	return "''"
}

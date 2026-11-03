// Package pipeline contains pure, dependency-free parsers for Azure Pipelines
// YAML. Everything here works on raw text / lines so that line numbers are
// preserved exactly and template expressions (${{ }}) never trip up a YAML
// library. Nothing in this package touches the filesystem.
package pipeline

import (
	"regexp"
	"strings"
)

// SplitLines normalises CRLF and splits text into lines.
func SplitLines(text string) []string {
	return strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
}

// Indent returns the number of leading whitespace characters of a line.
func Indent(line string) int {
	return len(line) - len(strings.TrimLeft(line, " \t"))
}

// IsBlank reports whether a line is empty or whitespace only.
func IsBlank(line string) bool {
	return strings.TrimSpace(line) == ""
}

// IsComment reports whether a line is a full-line YAML comment.
func IsComment(line string) bool {
	return strings.HasPrefix(strings.TrimSpace(line), "#")
}

// StripComment removes a trailing YAML comment from a line. A '#' only starts
// a comment at the beginning of the line or when preceded by whitespace, and
// never inside a quoted scalar.
func StripComment(line string) string {
	inSingle, inDouble := false, false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case c == '\'' && !inDouble:
			inSingle = !inSingle
		case c == '"' && !inSingle:
			inDouble = !inDouble
		case c == '#' && !inSingle && !inDouble:
			if i == 0 || line[i-1] == ' ' || line[i-1] == '\t' {
				return strings.TrimRight(line[:i], " \t")
			}
		}
	}
	return strings.TrimRight(line, " \t")
}

// Unquote strips one level of matching single or double quotes.
func Unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 {
		if (s[0] == '\'' && s[len(s)-1] == '\'') || (s[0] == '"' && s[len(s)-1] == '"') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// IsExpression reports whether a value contains a runtime or compile-time
// expression that cannot be evaluated at edit time.
func IsExpression(s string) bool {
	return strings.Contains(s, "${{") || strings.Contains(s, "$(") || strings.Contains(s, "$[")
}

var controlLineRe = regexp.MustCompile(`^\s*-?\s*\$\{\{\s*(if|elseif|else|each)\b.*\}\}\s*:`)

// IsControlLine reports whether a line is a template-expression control key
// such as `${{ if eq(...) }}:` or `${{ each x in y }}:`.
func IsControlLine(line string) bool {
	return controlLineRe.MatchString(line)
}

var topLevelKeyRe = regexp.MustCompile(`^([A-Za-z_][\w-]*)\s*:`)

// TopLevelKeys returns the set of top-level mapping keys in a document.
func TopLevelKeys(lines []string) map[string]int {
	keys := map[string]int{}
	for i, l := range lines {
		if m := topLevelKeyRe.FindStringSubmatch(l); m != nil {
			if _, ok := keys[m[1]]; !ok {
				keys[m[1]] = i
			}
		}
	}
	return keys
}

// IsPipelineRoot is a heuristic for "this file is a pipeline entry point
// rather than a reusable template". Templates cannot declare triggers,
// resources, pools at the root, or extend other templates.
func IsPipelineRoot(lines []string) bool {
	keys := TopLevelKeys(lines)
	for _, k := range []string{"trigger", "pr", "schedules", "resources", "extends", "pool", "name", "appendCommitMessageToRunName", "lockBehavior"} {
		if _, ok := keys[k]; ok {
			return true
		}
	}
	return false
}

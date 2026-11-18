package pipeline

import (
	"regexp"
	"strings"
)

// Suppressions are the problems a file acknowledges with comments:
//
//	# atn-ignore unused-param            the next line (or this line, as a trailing comment)
//	# atn-ignore-file unused-param, type-mismatch   the whole file
//
// Codes are required, so a comment never hides more than it names. A
// comment on the line above may share a block of comment lines with other
// comments; the block must touch the line it applies to.
type Suppressions struct {
	Lines map[int]map[string]bool // line → codes ignored on it
	File  map[string]bool         // codes ignored in the whole file
}

// Ignores reports whether a problem with code on line is suppressed.
func (s Suppressions) Ignores(line int, code string) bool {
	return s.File[code] || s.Lines[line][code]
}

// Empty reports whether nothing is suppressed.
func (s Suppressions) Empty() bool { return len(s.Lines) == 0 && len(s.File) == 0 }

var ignoreRe = regexp.MustCompile(`^#\s*atn-ignore(-file)?(?:\s*:)?\s+([a-z][a-z0-9-]*(?:\s*,\s*[a-z][a-z0-9-]*)*)\s*$`)

// IgnoreComment is the comment that suppresses code on the line after it.
func IgnoreComment(code string) string { return "# atn-ignore " + code }

// FindSuppressions reads the atn-ignore comments of a document. Comments
// inside block scalars (`script: |` bodies) are text and are not read.
func FindSuppressions(lines []string) Suppressions {
	s := Suppressions{}
	var mask []bool
	for i, l := range lines {
		if !strings.Contains(l, "atn-ignore") {
			continue
		}
		if mask == nil {
			mask = BlockScalarMask(lines)
		}
		if mask[i] {
			continue
		}
		file, codes, ok := ParseIgnoreComment(l)
		if !ok {
			continue
		}
		if file {
			if s.File == nil {
				s.File = map[string]bool{}
			}
			for _, code := range codes {
				s.File[code] = true
			}
			continue
		}
		target := i // trailing comment
		if IsComment(l) {
			target = -1
			for j := i + 1; j < len(lines); j++ {
				if IsBlank(lines[j]) {
					break
				}
				if !IsComment(lines[j]) {
					target = j
					break
				}
			}
		}
		if target < 0 {
			continue
		}
		if s.Lines == nil {
			s.Lines = map[int]map[string]bool{}
		}
		if s.Lines[target] == nil {
			s.Lines[target] = map[string]bool{}
		}
		for _, code := range codes {
			s.Lines[target][code] = true
		}
	}
	return s
}

// ParseIgnoreComment reads an atn-ignore comment on a line: whether it is
// the -file form, and the codes it names.
func ParseIgnoreComment(line string) (file bool, codes []string, ok bool) {
	m := ignoreRe.FindStringSubmatch(commentText(line))
	if m == nil {
		return false, nil, false
	}
	for _, code := range strings.Split(m[2], ",") {
		codes = append(codes, strings.TrimSpace(code))
	}
	return m[1] != "", codes, true
}

// commentText returns the comment on a line, from its `#`, or "".
func commentText(line string) string {
	code := StripComment(line)
	rest := strings.TrimLeft(line[len(code):], " \t")
	if !strings.HasPrefix(rest, "#") {
		return ""
	}
	return strings.TrimRight(rest, " \t\r")
}

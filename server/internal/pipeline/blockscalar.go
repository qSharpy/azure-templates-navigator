package pipeline

import "strings"

// BlockScalarMask reports, for every line, whether it is content of a YAML
// block scalar (`key: |`, `key: >-`, `- |`, ...). Such lines are text, not
// YAML structure: `template: x.yml` inside a `script: |` is shell, not a
// template call. They may still contain `${{ }}` expressions, so scanners
// that look for parameter references must keep reading them.
func BlockScalarMask(lines []string) []bool {
	mask := make([]bool, len(lines))
	for i := 0; i < len(lines); i++ {
		parent, ok := blockScalarHeader(lines[i])
		if !ok {
			continue
		}
		j := i + 1
		for ; j < len(lines); j++ {
			if IsBlank(lines[j]) {
				continue
			}
			if Indent(lines[j]) <= parent {
				break
			}
		}
		// trailing blank lines belong to whatever follows
		last := j - 1
		for last > i && IsBlank(lines[last]) {
			last--
		}
		for k := i + 1; k <= last; k++ {
			mask[k] = true
		}
		i = last
	}
	return mask
}

// blockScalarHeader reports whether a line ends with a block scalar
// indicator, and returns the column its content must be indented beyond.
func blockScalarHeader(line string) (parent int, ok bool) {
	s := strings.TrimRight(StripComment(line), " \t")
	sp := strings.LastIndexAny(s, " \t")
	if sp < 0 {
		return 0, false
	}
	if !isBlockIndicator(s[sp+1:]) {
		return 0, false
	}
	before := strings.TrimRight(s[:sp], " \t")
	switch {
	case strings.HasSuffix(before, ":"):
		// `key: |` (or `- key: |`): content is indented beyond the key
		return KeyColumn(s), true
	case strings.TrimSpace(before) == "-" || strings.HasSuffix(before, " -"):
		// `- |`: content is indented beyond the dash
		return len(before) - 1, true
	}
	return 0, false
}

// isBlockIndicator matches `|`, `>` with optional chomping (`-`/`+`) and
// indentation (`1`-`9`) indicators in either order.
func isBlockIndicator(tok string) bool {
	if tok == "" || (tok[0] != '|' && tok[0] != '>') || len(tok) > 3 {
		return false
	}
	var chomp, digit bool
	for _, c := range tok[1:] {
		switch {
		case (c == '-' || c == '+') && !chomp:
			chomp = true
		case c >= '1' && c <= '9' && !digit:
			digit = true
		default:
			return false
		}
	}
	return true
}

// StructuralLines returns a copy of lines with block scalar content blanked
// out. Line numbers and the columns of the remaining lines are unchanged.
func StructuralLines(lines []string) []string {
	mask := BlockScalarMask(lines)
	out := make([]string, len(lines))
	for i, l := range lines {
		if !mask[i] {
			out[i] = l
		}
	}
	return out
}

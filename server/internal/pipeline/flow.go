package pipeline

import "strings"

// Pos is a (line, byte column) position.
type Pos struct {
	Line, Col int
}

// Before reports whether p comes strictly before q.
func (p Pos) Before(q Pos) bool {
	return p.Line < q.Line || (p.Line == q.Line && p.Col < q.Col)
}

// FlowEntry is one `key: value` entry of a YAML flow mapping.
type FlowEntry struct {
	Key      string
	KeyPos   Pos
	Value    string // trimmed raw value ("" when absent)
	ValuePos Pos    // start of the value (== End when absent)
	End      Pos    // end of the value (exclusive)
}

// FlowMapping is a parsed `{ a: 1, b: 2 }` mapping, possibly spanning lines.
type FlowMapping struct {
	Open    Pos // position of `{`
	Close   Pos // position of the matching `}`
	Closed  bool
	Entries []FlowEntry
}

// ParseFlowMapping parses the flow mapping whose `{` is at (line, col) and
// whose key sits at keyCol. Nested `{}` / `[]` (including `${{ }}`
// expressions) and quoted scalars are kept inside the value they belong to.
// An unterminated mapping (while the user is typing) returns the entries read
// so far with Closed == false; reading stops at the first later line that is
// not indented beyond keyCol (other than a closing `}` at keyCol).
func ParseFlowMapping(lines []string, line, col, keyCol int) FlowMapping {
	fm := FlowMapping{Open: Pos{line, col}}
	if line < 0 || line >= len(lines) || col >= len(lines[line]) || lines[line][col] != '{' {
		return fm
	}
	var (
		depth            int
		inSingle         bool
		inDouble         bool
		entryStart       = Pos{-1, -1}
		lastContentEnd   Pos // end of the last non-space char seen in the entry
		key              string
		haveKey          bool
		entryHasContent  bool
		valueHasContent  bool
		valueContentFrom Pos
	)
	text := func(from, to Pos) string {
		if from.Line == to.Line {
			return lines[from.Line][from.Col:to.Col]
		}
		var b strings.Builder
		b.WriteString(lines[from.Line][from.Col:])
		for l := from.Line + 1; l < to.Line; l++ {
			b.WriteString("\n" + lines[l])
		}
		b.WriteString("\n" + lines[to.Line][:to.Col])
		return b.String()
	}
	finish := func() {
		if !entryHasContent {
			return
		}
		e := FlowEntry{End: lastContentEnd}
		if haveKey {
			e.Key = Unquote(strings.TrimSpace(key))
			e.KeyPos = entryStart
			if valueHasContent {
				e.ValuePos = valueContentFrom
				e.Value = strings.TrimSpace(text(valueContentFrom, lastContentEnd))
			} else {
				e.ValuePos = lastContentEnd
			}
		} else {
			e.Key = Unquote(strings.TrimSpace(text(entryStart, lastContentEnd)))
			e.KeyPos = entryStart
			e.ValuePos = lastContentEnd
		}
		if e.Key != "" {
			fm.Entries = append(fm.Entries, e)
		}
	}
	reset := func() {
		entryStart = Pos{-1, -1}
		key, haveKey, entryHasContent, valueHasContent = "", false, false, false
	}

	for l := line; l < len(lines); l++ {
		if l > line && !IsBlank(lines[l]) && !IsComment(lines[l]) {
			ind := Indent(lines[l])
			if ind < keyCol || (ind == keyCol && !strings.HasPrefix(strings.TrimSpace(lines[l]), "}")) {
				break
			}
		}
		s := StripComment(lines[l])
		start := 0
		if l == line {
			start = col
		}
		for c := start; c < len(s); c++ {
			ch := s[c]
			here := Pos{l, c}
			switch {
			case inDouble:
				if ch == '\\' {
					c++
				} else if ch == '"' {
					inDouble = false
				}
				lastContentEnd = Pos{l, c + 1}
				continue
			case inSingle:
				if ch == '\'' {
					inSingle = false
				}
				lastContentEnd = Pos{l, c + 1}
				continue
			}
			if depth == 1 {
				switch {
				case ch == ',':
					finish()
					reset()
					continue
				case ch == '}':
					finish()
					fm.Close, fm.Closed = here, true
					return fm
				case ch == ':' && !haveKey && entryHasContent &&
					(c+1 == len(s) || s[c+1] == ' ' || s[c+1] == '\t' || s[c+1] == ',' || s[c+1] == '}'):
					key = text(entryStart, here)
					haveKey = true
					lastContentEnd = Pos{l, c + 1}
					continue
				}
			}
			switch ch {
			case '{', '[':
				depth++
				if depth == 1 {
					continue // the mapping's own `{`
				}
			case '}', ']':
				depth--
			case '"':
				inDouble = true
			case '\'':
				inSingle = true
			}
			if ch == ' ' || ch == '\t' {
				continue
			}
			if !entryHasContent {
				entryStart, entryHasContent = here, true
			}
			if haveKey && !valueHasContent {
				valueContentFrom, valueHasContent = here, true
			}
			lastContentEnd = Pos{l, c + 1}
		}
	}
	finish()
	return fm
}

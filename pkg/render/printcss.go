package render

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// withoutSourcePages removes only source @page at-rules, including named and
// pseudo pages inside conditional rules. A later plain @page cannot overrule
// more specific source pages, even with !important. Strings, comments and
// function arguments are kept verbatim; this is not a general CSS rewriter.
func withoutSourcePages(css string) string {
	var out strings.Builder
	start := 0
	for i := 0; i < len(css); {
		if css[i] == '@' {
			name, end := cssAtName(css, i+1)
			if strings.EqualFold(name, "page") {
				out.WriteString(css[start:i])
				i = cssPageEnd(css, end)
				start = i
				continue
			}
		}
		if end := cssNameOrURLEnd(css, i); end > i {
			i = end
			continue
		}
		if end := cssProtectedEnd(css, i); end > i {
			i = end
			continue
		}
		if css[i] == '(' {
			i = cssDelimitedEnd(css, i, '(', ')')
			continue
		}
		i++
	}
	if start == 0 {
		return css
	}
	out.WriteString(css[start:])
	return out.String()
}

func cssSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' }
func cssHex(c byte) bool   { return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F' }

func cssEscape(css string, i int) (rune, int) {
	i++
	if i == len(css) {
		return utf8.RuneError, i
	}
	start := i
	for i < len(css) && i-start < 6 && cssHex(css[i]) {
		i++
	}
	if i > start {
		value, _ := strconv.ParseUint(css[start:i], 16, 32)
		if i < len(css) && cssSpace(css[i]) {
			if css[i] == '\r' && i+1 < len(css) && css[i+1] == '\n' {
				i++
			}
			i++
		}
		return rune(value), i
	}
	r, size := utf8.DecodeRuneInString(css[i:])
	if r == '\r' && i+size < len(css) && css[i+size] == '\n' {
		size++
	}
	return r, i + size
}

func cssNameOrURLEnd(css string, i int) int {
	c := css[i]
	if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '\\' || c >= 128) {
		return i
	}
	name, end := cssAtName(css, i)
	if !strings.EqualFold(name, "url") || end == len(css) || css[end] != '(' {
		return end
	}
	open := end
	for end++; end < len(css) && cssSpace(css[end]); end++ {
	}
	if end < len(css) && (css[end] == '\'' || css[end] == '"') {
		return cssDelimitedEnd(css, open, '(', ')')
	}
	// In unquoted url(), a quote is an error, not the start of a string.
	// CSS recovers by consuming the bad URL through its closing parenthesis.
	for end < len(css) {
		if css[end] == '\\' {
			_, end = cssEscape(css, end)
			continue
		}
		if css[end] == ')' {
			return end + 1
		}
		end++
	}
	return len(css)
}

func cssAtName(css string, i int) (string, int) {
	var name strings.Builder
	count := 0
	for i < len(css) {
		var r rune
		if css[i] == '\\' {
			r, i = cssEscape(css, i)
		} else {
			c := css[i]
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c >= 128) {
				break
			}
			var size int
			r, size = utf8.DecodeRuneInString(css[i:])
			i += size
		}
		// A name longer than page cannot match; retain at most five runes.
		if count < 5 {
			name.WriteRune(r)
		}
		count++
	}
	return name.String(), i
}

func cssProtectedEnd(css string, i int) int {
	if css[i] == '\\' {
		_, end := cssEscape(css, i)
		return end
	}
	if strings.HasPrefix(css[i:], "/*") {
		if end := strings.Index(css[i+2:], "*/"); end >= 0 {
			return i + 2 + end + 2
		}
		return len(css)
	}
	if quote := css[i]; quote == '\'' || quote == '"' {
		for j := i + 1; j < len(css); {
			if css[j] == '\\' {
				_, j = cssEscape(css, j)
				continue
			}
			if css[j] == quote {
				return j + 1
			}
			if css[j] == '\n' || css[j] == '\r' || css[j] == '\f' {
				return j
			}
			j++
		}
		return len(css)
	}
	return i
}

func cssDelimitedEnd(css string, i int, open, close byte) int {
	depth := 1
	for i++; i < len(css); {
		if end := cssNameOrURLEnd(css, i); end > i {
			i = end
			continue
		}
		if end := cssProtectedEnd(css, i); end > i {
			i = end
			continue
		}
		if open == '{' && css[i] == '(' {
			i = cssDelimitedEnd(css, i, '(', ')')
			continue
		}
		if css[i] == open {
			depth++
		}
		if css[i] == close {
			depth--
			if depth == 0 {
				return i + 1
			}
		}
		i++
	}
	return len(css)
}

func cssPageEnd(css string, i int) int {
	for i < len(css) {
		if end := cssNameOrURLEnd(css, i); end > i {
			i = end
			continue
		}
		if end := cssProtectedEnd(css, i); end > i {
			i = end
			continue
		}
		switch css[i] {
		case '(':
			i = cssDelimitedEnd(css, i, '(', ')')
			continue
		case '{':
			return cssDelimitedEnd(css, i, '{', '}')
		case ';':
			return i + 1
		case '}':
			return i
		}
		i++
	}
	return len(css)
}

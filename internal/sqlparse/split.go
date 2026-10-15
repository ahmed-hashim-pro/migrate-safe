// Package sqlparse splits a migration file into statements and tokenizes
// them. It is not a SQL parser: it knows enough about quoting, comments and
// dollar-quoted bodies to find statement boundaries, and leaves meaning to
// the linter.
package sqlparse

import (
	"fmt"
	"strings"
)

// Statement is one statement from a migration file.
type Statement struct {
	SQL  string // the statement text, without the trailing semicolon
	Line int    // 1-based line where the statement starts
	// Directives are the `-- migrate-safe: key value` comments written
	// between the previous statement and this one.
	Directives map[string]string
}

// Directive returns a directive's value and whether it was set.
func (s Statement) Directive(key string) (string, bool) {
	v, ok := s.Directives[key]
	return v, ok
}

const directivePrefix = "migrate-safe:"

// Split breaks src into statements at top-level semicolons. Semicolons
// inside quotes, quoted identifiers, comments and dollar-quoted strings do
// not end a statement.
func Split(src string) ([]Statement, error) {
	var out []Statement
	var b strings.Builder
	directives := map[string]string{}
	line, start := 1, 0 // start: line of the first non-space byte of the current statement
	flush := func() {
		text := strings.TrimSpace(b.String())
		b.Reset()
		if text != "" {
			out = append(out, Statement{SQL: text, Line: start, Directives: directives})
			directives = map[string]string{}
		}
		start = 0
	}
	mark := func() {
		if start == 0 {
			start = line
		}
	}
	n := len(src)
	for i := 0; i < n; {
		c := src[i]
		switch {
		case c == '\n':
			line++
			b.WriteByte(c)
			i++
		case c == '-' && i+1 < n && src[i+1] == '-':
			j := strings.IndexByte(src[i:], '\n')
			if j < 0 {
				j = n - i
			}
			comment := strings.TrimSpace(src[i+2 : i+j])
			if strings.HasPrefix(comment, directivePrefix) {
				if start != 0 {
					return nil, fmt.Errorf("line %d: a migrate-safe directive must come before a statement, not inside one", line)
				}
				k, v, _ := strings.Cut(strings.TrimSpace(strings.TrimPrefix(comment, directivePrefix)), " ")
				if k == "" {
					return nil, fmt.Errorf("line %d: empty migrate-safe directive", line)
				}
				directives[k] = strings.TrimSpace(v)
			}
			i += j // keep the newline for the line count
		case c == '/' && i+1 < n && src[i+1] == '*':
			depth, j := 1, i+2
			for j < n && depth > 0 {
				switch {
				case src[j] == '\n':
					line++
					j++
				case src[j] == '/' && j+1 < n && src[j+1] == '*':
					depth++
					j += 2
				case src[j] == '*' && j+1 < n && src[j+1] == '/':
					depth--
					j += 2
				default:
					j++
				}
			}
			if depth > 0 {
				return nil, fmt.Errorf("unterminated /* comment")
			}
			b.WriteByte(' ')
			i = j
		case c == '\'' || c == '"':
			mark()
			// E'...' strings allow backslash escapes.
			backslash := c == '\'' && i > 0 && (src[i-1] == 'E' || src[i-1] == 'e') && (i < 2 || !isIdent(src[i-2]))
			j := i + 1
			for {
				if j >= n {
					return nil, fmt.Errorf("line %d: unterminated %c", line, c)
				}
				if backslash && src[j] == '\\' {
					j += 2
					continue
				}
				if src[j] == '\n' {
					line++
				}
				if src[j] == c {
					if j+1 < n && src[j+1] == c { // doubled quote
						j += 2
						continue
					}
					break
				}
				j++
			}
			b.WriteString(src[i : j+1])
			i = j + 1
		case c == '$' && (i == 0 || !isIdent(src[i-1])):
			if tag, ok := dollarTag(src[i:]); ok {
				mark()
				end := strings.Index(src[i+len(tag):], tag)
				if end < 0 {
					return nil, fmt.Errorf("line %d: unterminated %s string", line, tag)
				}
				body := src[i : i+len(tag)+end+len(tag)]
				line += strings.Count(body, "\n")
				b.WriteString(body)
				i += len(body)
				continue
			}
			mark()
			b.WriteByte(c)
			i++
		case c == ';':
			flush()
			i++
		default:
			if c != ' ' && c != '\t' && c != '\r' {
				mark()
			}
			b.WriteByte(c)
			i++
		}
	}
	flush()
	if len(directives) > 0 {
		return nil, fmt.Errorf("migrate-safe directive at the end of the file applies to no statement")
	}
	return out, nil
}

// dollarTag returns the opening tag ($$ or $name$) at the start of s.
func dollarTag(s string) (string, bool) {
	for j := 1; j < len(s); j++ {
		if s[j] == '$' {
			return s[:j+1], true
		}
		if !isIdent(s[j]) || (j == 1 && s[j] >= '0' && s[j] <= '9') {
			return "", false // $1 is a parameter, not a tag
		}
	}
	return "", false
}

func isIdent(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c >= 0x80
}

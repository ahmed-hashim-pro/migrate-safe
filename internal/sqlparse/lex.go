package sqlparse

import "strings"

// Kind classifies a token.
type Kind int

const (
	Word   Kind = iota // keyword or identifier, lowercased unless quoted
	Ident              // "quoted identifier", case kept
	String             // '...', E'...', $$...$$
	Number
	Punct
)

// Token is one lexical token. Text is lowercased for unquoted words.
type Token struct {
	Kind Kind
	Text string
}

// Is reports whether the token is the unquoted word w.
func (t Token) Is(w string) bool { return t.Kind == Word && t.Text == w }

// Name reports whether the token can be used as a name (an identifier).
func (t Token) Name() bool { return t.Kind == Word || t.Kind == Ident }

// Tokenize splits a single statement into tokens. Comments are dropped.
func Tokenize(s string) []Token {
	var out []Token
	n := len(s)
	for i := 0; i < n; {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '-' && i+1 < n && s[i+1] == '-':
			for i < n && s[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < n && s[i+1] == '*':
			depth := 1
			i += 2
			for i < n && depth > 0 {
				if s[i] == '/' && i+1 < n && s[i+1] == '*' {
					depth++
					i += 2
				} else if s[i] == '*' && i+1 < n && s[i+1] == '/' {
					depth--
					i += 2
				} else {
					i++
				}
			}
		case c == '\'':
			j := i + 1
			for j < n {
				if s[j] == '\'' {
					if j+1 < n && s[j+1] == '\'' {
						j += 2
						continue
					}
					break
				}
				j++
			}
			out = append(out, Token{String, unquote(s[i+1:min(j, n)], '\'')})
			i = j + 1
		case c == '"':
			j := i + 1
			for j < n {
				if s[j] == '"' {
					if j+1 < n && s[j+1] == '"' {
						j += 2
						continue
					}
					break
				}
				j++
			}
			out = append(out, Token{Ident, unquote(s[i+1:min(j, n)], '"')})
			i = j + 1
		case c == '$' && i+1 < n && s[i+1] >= '0' && s[i+1] <= '9':
			j := i + 1
			for j < n && s[j] >= '0' && s[j] <= '9' {
				j++
			}
			out = append(out, Token{Punct, s[i:j]})
			i = j
		case c == '$':
			if tag, ok := dollarTag(s[i:]); ok {
				end := strings.Index(s[i+len(tag):], tag)
				if end < 0 {
					end = n - i - len(tag)
				}
				out = append(out, Token{String, s[i+len(tag) : i+len(tag)+end]})
				i += len(tag) + end + len(tag)
				continue
			}
			out = append(out, Token{Punct, "$"})
			i++
		case (c == 'e' || c == 'E') && i+1 < n && s[i+1] == '\'':
			j := i + 2
			for j < n && s[j] != '\'' {
				if s[j] == '\\' {
					j++
				}
				j++
			}
			out = append(out, Token{String, s[i+2 : min(j, n)]})
			i = j + 1
		case c >= '0' && c <= '9' || c == '.' && i+1 < n && s[i+1] >= '0' && s[i+1] <= '9':
			j := i
			for j < n && (s[j] >= '0' && s[j] <= '9' || s[j] == '.' || s[j] == 'e' || s[j] == 'E') {
				j++
			}
			out = append(out, Token{Number, s[i:j]})
			i = j
		case isIdent(c):
			j := i
			for j < n && (isIdent(s[j]) || s[j] == '$') {
				j++
			}
			out = append(out, Token{Word, strings.ToLower(s[i:j])})
			i = j
		case c == ':' && i+1 < n && s[i+1] == ':':
			out = append(out, Token{Punct, "::"})
			i += 2
		default:
			out = append(out, Token{Punct, string(c)})
			i++
		}
	}
	return out
}

func unquote(s string, q byte) string {
	return strings.ReplaceAll(s, string([]byte{q, q}), string(q))
}

// Cursor walks a token slice.
type Cursor struct {
	T []Token
	I int
}

// NewCursor tokenizes s.
func NewCursor(s string) *Cursor { return &Cursor{T: Tokenize(s)} }

// Done reports whether every token has been consumed.
func (c *Cursor) Done() bool { return c.I >= len(c.T) }

// Peek returns the token k positions ahead, or a zero Token.
func (c *Cursor) Peek(k int) Token {
	if c.I+k < len(c.T) {
		return c.T[c.I+k]
	}
	return Token{Kind: Punct}
}

// Next consumes and returns one token.
func (c *Cursor) Next() Token {
	t := c.Peek(0)
	if c.I < len(c.T) {
		c.I++
	}
	return t
}

// Accept consumes the given sequence of words if it comes next.
func (c *Cursor) Accept(words ...string) bool {
	for k, w := range words {
		if !c.Peek(k).Is(w) {
			return false
		}
	}
	c.I += len(words)
	return true
}

// AcceptPunct consumes p if it comes next.
func (c *Cursor) AcceptPunct(p string) bool {
	if t := c.Peek(0); t.Kind == Punct && t.Text == p {
		c.I++
		return true
	}
	return false
}

// Name consumes a possibly schema-qualified name and returns its last part.
func (c *Cursor) Name() (string, bool) {
	t := c.Peek(0)
	if !t.Name() {
		return "", false
	}
	c.I++
	name := t.Text
	for c.Peek(0).Kind == Punct && c.Peek(0).Text == "." && c.Peek(1).Name() {
		name = c.Peek(1).Text
		c.I += 2
	}
	return name, true
}

// Group consumes a parenthesised group, starting at "(", and returns the
// tokens inside it.
func (c *Cursor) Group() ([]Token, bool) {
	if !c.AcceptPunct("(") {
		return nil, false
	}
	start, depth := c.I, 1
	for !c.Done() {
		t := c.Next()
		if t.Kind == Punct && t.Text == "(" {
			depth++
		} else if t.Kind == Punct && t.Text == ")" {
			depth--
			if depth == 0 {
				return c.T[start : c.I-1], true
			}
		}
	}
	return nil, false
}

// SplitTop splits tokens at top-level commas.
func SplitTop(ts []Token) [][]Token {
	var out [][]Token
	depth, start := 0, 0
	for i, t := range ts {
		if t.Kind != Punct {
			continue
		}
		switch t.Text {
		case "(", "[":
			depth++
		case ")", "]":
			depth--
		case ",":
			if depth == 0 {
				out = append(out, ts[start:i])
				start = i + 1
			}
		}
	}
	return append(out, ts[start:])
}

// Join renders tokens back to text, roughly as written.
func Join(ts []Token) string {
	var b strings.Builder
	for i, t := range ts {
		if i > 0 && !(t.Kind == Punct && strings.Contains(",)].:", t.Text)) && !(ts[i-1].Kind == Punct && strings.Contains("([.:", ts[i-1].Text)) {
			b.WriteByte(' ')
		}
		switch t.Kind {
		case String:
			b.WriteString("'" + strings.ReplaceAll(t.Text, "'", "''") + "'")
		case Ident:
			b.WriteString(`"` + strings.ReplaceAll(t.Text, `"`, `""`) + `"`)
		default:
			b.WriteString(t.Text)
		}
	}
	return b.String()
}

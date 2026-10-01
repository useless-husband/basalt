// Package sql contains basalt's SQL lexer, abstract syntax tree and
// recursive-descent parser. The dialect is PostgreSQL's.
package sql

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/useless-husband/basalt/internal/pgerr"
)

// TokKind classifies tokens.
type TokKind int

const (
	TEOF TokKind = iota
	TIdent
	TQuotedIdent
	TNumber
	TString
	TParam
	TOp // operators and punctuation
)

// Token is a lexical token. Pos is the byte offset in the input.
type Token struct {
	Kind TokKind
	Val  string // identifiers are lower-cased unless quoted
	Pos  int
}

func (t Token) String() string {
	switch t.Kind {
	case TEOF:
		return "end of input"
	case TString:
		return "'" + t.Val + "'"
	}
	return t.Val
}

func syntaxErr(pos int, format string, args ...any) error {
	e := pgerr.New(pgerr.SyntaxError, format, args...)
	e.Position = pos + 1
	return e
}

// Lex splits a query into tokens.
func Lex(src string) ([]Token, error) {
	var toks []Token
	i := 0
	n := len(src)
	for i < n {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f':
			i++
		case c == '-' && i+1 < n && src[i+1] == '-':
			for i < n && src[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < n && src[i+1] == '*':
			depth := 0
			j := i
			for j < n {
				if j+1 < n && src[j] == '/' && src[j+1] == '*' {
					depth++
					j += 2
				} else if j+1 < n && src[j] == '*' && src[j+1] == '/' {
					depth--
					j += 2
					if depth == 0 {
						break
					}
				} else {
					j++
				}
			}
			if depth != 0 {
				return nil, syntaxErr(i, "unterminated /* comment at or near \"%s\"", src[i:min(n, i+10)])
			}
			i = j
		case (c == 'e' || c == 'E') && i+1 < n && src[i+1] == '\'':
			s, end, err := lexEscapeString(src, i+1)
			if err != nil {
				return nil, err
			}
			toks = append(toks, Token{TString, s, i})
			i = end
		case (c == 'x' || c == 'X' || c == 'b' || c == 'B') && i+1 < n && src[i+1] == '\'':
			// Bit-string literals are not supported; hex literal X'..' becomes bytea text.
			s, end, err := lexString(src, i+1)
			if err != nil {
				return nil, err
			}
			if c == 'x' || c == 'X' {
				toks = append(toks, Token{TString, `\x` + s, i})
			} else {
				return nil, syntaxErr(i, "bit string literals are not supported")
			}
			i = end
		case c == '\'':
			s, end, err := lexString(src, i)
			if err != nil {
				return nil, err
			}
			toks = append(toks, Token{TString, s, i})
			i = end
		case c == '"':
			j := i + 1
			var b strings.Builder
			for {
				if j >= n {
					return nil, syntaxErr(i, "unterminated quoted identifier at or near \"%s\"", src[i:])
				}
				if src[j] == '"' {
					if j+1 < n && src[j+1] == '"' {
						b.WriteByte('"')
						j += 2
						continue
					}
					break
				}
				b.WriteByte(src[j])
				j++
			}
			if b.Len() == 0 {
				return nil, syntaxErr(i, "zero-length delimited identifier at or near \"\"\"\"")
			}
			toks = append(toks, Token{TQuotedIdent, b.String(), i})
			i = j + 1
		case c == '$' && i+1 < n && src[i+1] >= '0' && src[i+1] <= '9':
			j := i + 1
			for j < n && src[j] >= '0' && src[j] <= '9' {
				j++
			}
			toks = append(toks, Token{TParam, src[i+1 : j], i})
			i = j
		case c == '$':
			// Dollar-quoted string: $tag$ ... $tag$
			j := i + 1
			for j < n && src[j] != '$' && isIdentChar(src[j]) {
				j++
			}
			if j >= n || src[j] != '$' {
				return nil, syntaxErr(i, "syntax error at or near \"$\"")
			}
			tag := src[i : j+1]
			end := strings.Index(src[j+1:], tag)
			if end < 0 {
				return nil, syntaxErr(i, "unterminated dollar-quoted string at or near \"%s\"", tag)
			}
			toks = append(toks, Token{TString, src[j+1 : j+1+end], i})
			i = j + 1 + end + len(tag)
		case c >= '0' && c <= '9' || (c == '.' && i+1 < n && src[i+1] >= '0' && src[i+1] <= '9'):
			j := i
			for j < n && src[j] >= '0' && src[j] <= '9' {
				j++
			}
			if j < n && src[j] == '.' && !(j+1 < n && src[j+1] == '.') {
				j++
				for j < n && src[j] >= '0' && src[j] <= '9' {
					j++
				}
			}
			if j < n && (src[j] == 'e' || src[j] == 'E') {
				k := j + 1
				if k < n && (src[k] == '+' || src[k] == '-') {
					k++
				}
				if k < n && src[k] >= '0' && src[k] <= '9' {
					for k < n && src[k] >= '0' && src[k] <= '9' {
						k++
					}
					j = k
				}
			}
			toks = append(toks, Token{TNumber, src[i:j], i})
			i = j
		case isIdentStart(c) || c >= 0x80:
			j := i
			for j < n {
				if src[j] >= 0x80 {
					r, size := utf8.DecodeRuneInString(src[j:])
					if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
						break
					}
					j += size
					continue
				}
				if !isIdentChar(src[j]) {
					break
				}
				j++
			}
			if j == i {
				return nil, syntaxErr(i, "syntax error at or near \"%s\"", src[i:i+1])
			}
			toks = append(toks, Token{TIdent, strings.ToLower(src[i:j]), i})
			i = j
		default:
			op, ok := lexOperator(src[i:])
			if !ok {
				return nil, syntaxErr(i, "syntax error at or near \"%c\"", c)
			}
			toks = append(toks, Token{TOp, op, i})
			i += len(op)
		}
	}
	toks = append(toks, Token{TEOF, "", n})
	return toks, nil
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentChar(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9') || c == '$'
}

func lexString(src string, i int) (string, int, error) {
	var b strings.Builder
	j := i + 1
	for {
		if j >= len(src) {
			return "", 0, syntaxErr(i, "unterminated quoted string at or near \"%s\"", src[i:])
		}
		if src[j] == '\'' {
			if j+1 < len(src) && src[j+1] == '\'' {
				b.WriteByte('\'')
				j += 2
				continue
			}
			j++
			// Adjacent string literals separated by a newline concatenate.
			k := j
			sawNewline := false
			for k < len(src) && (src[k] == ' ' || src[k] == '\t' || src[k] == '\n' || src[k] == '\r') {
				if src[k] == '\n' {
					sawNewline = true
				}
				k++
			}
			if sawNewline && k < len(src) && src[k] == '\'' {
				j = k + 1
				continue
			}
			return b.String(), j, nil
		}
		b.WriteByte(src[j])
		j++
	}
}

func lexEscapeString(src string, i int) (string, int, error) {
	var b strings.Builder
	j := i + 1
	for {
		if j >= len(src) {
			return "", 0, syntaxErr(i, "unterminated quoted string")
		}
		c := src[j]
		if c == '\'' {
			if j+1 < len(src) && src[j+1] == '\'' {
				b.WriteByte('\'')
				j += 2
				continue
			}
			return b.String(), j + 1, nil
		}
		if c == '\\' && j+1 < len(src) {
			j++
			switch src[j] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case 'b':
				b.WriteByte('\b')
			case 'f':
				b.WriteByte('\f')
			case 'x':
				v := 0
				k := j + 1
				for ; k < len(src) && k < j+3 && isHex(src[k]); k++ {
					v = v*16 + hexVal(src[k])
				}
				b.WriteByte(byte(v))
				j = k - 1
			default:
				if src[j] >= '0' && src[j] <= '7' {
					v := 0
					k := j
					for ; k < len(src) && k < j+3 && src[k] >= '0' && src[k] <= '7'; k++ {
						v = v*8 + int(src[k]-'0')
					}
					b.WriteByte(byte(v))
					j = k - 1
				} else {
					b.WriteByte(src[j])
				}
			}
			j++
			continue
		}
		b.WriteByte(c)
		j++
	}
}

func isHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func hexVal(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	}
	return int(c-'A') + 10
}

// Multi-character operators, longest first.
var operators = []string{
	"!~~*", "!~~", "~~*", "!~*", "::", "<=", ">=", "<>", "!=", "||", "~~", "!~", "~*", "->>", "->", "<<", ">>", "@>", "<@", "&&",
	"+", "-", "*", "/", "%", "^", "<", ">", "=", "~", "(", ")", "[", "]", ",", ";", ".", ":", "&", "|", "#", "@", "!",
}

func lexOperator(s string) (string, bool) {
	for _, op := range operators {
		if strings.HasPrefix(s, op) {
			return op, true
		}
	}
	return "", false
}

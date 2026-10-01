package expr

import (
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/useless-husband/basalt/internal/pgerr"
)

// Like matches s against a LIKE pattern (% and _ wildcards, esc escapes).
// It works on runes and uses a two-pointer algorithm with backtracking to
// the last %, which is linear for patterns without pathological nesting.
func Like(s, pattern string, esc byte, icase bool) (bool, error) {
	if icase {
		s, pattern = strings.ToLower(s), strings.ToLower(pattern)
	}
	type tok struct {
		r    rune
		kind byte // 'c' literal, '_' any one, '%' any run
	}
	var toks []tok
	for i := 0; i < len(pattern); {
		c := pattern[i]
		if esc != 0 && c == esc {
			if i+1 >= len(pattern) {
				return false, pgerr.New("22025", "LIKE pattern must not end with escape character")
			}
			r, size := utf8.DecodeRuneInString(pattern[i+1:])
			toks = append(toks, tok{r, 'c'})
			i += 1 + size
			continue
		}
		switch c {
		case '%':
			toks = append(toks, tok{0, '%'})
			i++
		case '_':
			toks = append(toks, tok{0, '_'})
			i++
		default:
			r, size := utf8.DecodeRuneInString(pattern[i:])
			toks = append(toks, tok{r, 'c'})
			i += size
		}
	}
	runes := []rune(s)
	si, pi := 0, 0
	starP, starS := -1, 0
	for si < len(runes) {
		if pi < len(toks) && (toks[pi].kind == '_' || (toks[pi].kind == 'c' && toks[pi].r == runes[si])) {
			si++
			pi++
		} else if pi < len(toks) && toks[pi].kind == '%' {
			starP, starS = pi, si
			pi++
		} else if starP >= 0 {
			pi = starP + 1
			starS++
			si = starS
		} else {
			return false, nil
		}
	}
	for pi < len(toks) && toks[pi].kind == '%' {
		pi++
	}
	return pi == len(toks), nil
}

var (
	reCacheMu sync.Mutex
	reCache   = map[string]*regexp.Regexp{}
)

// compileRegex compiles a POSIX-style regular expression (Go RE2 syntax,
// which covers the common subset) with a small cache.
func compileRegex(pat string, icase bool) (*regexp.Regexp, error) {
	key := pat
	if icase {
		key = "(?i)" + pat
	}
	reCacheMu.Lock()
	re, ok := reCache[key]
	reCacheMu.Unlock()
	if ok {
		return re, nil
	}
	re, err := regexp.Compile(key)
	if err != nil {
		return nil, pgerr.New(pgerr.InvalidRegularExpression, "invalid regular expression: %v", err)
	}
	reCacheMu.Lock()
	if len(reCache) > 1000 {
		reCache = map[string]*regexp.Regexp{}
	}
	reCache[key] = re
	reCacheMu.Unlock()
	return re, nil
}

// LikePrefix returns the literal prefix of a LIKE pattern (for index range
// scans) and whether the whole pattern is that literal.
func LikePrefix(pattern string) (string, bool) {
	var b strings.Builder
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		switch c {
		case '%', '_':
			return b.String(), false
		case '\\':
			if i+1 < len(pattern) {
				i++
				b.WriteByte(pattern[i])
				continue
			}
			return b.String(), false
		}
		b.WriteByte(c)
	}
	return b.String(), true
}

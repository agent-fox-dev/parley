package identity

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// The limits on a token's paths list. A glob is counted in characters, as
// the API documents it, not in bytes.
const (
	maxPathGlobs  = 50
	maxGlobLength = 255
)

// ValidatePaths checks the paths member of a token request, given as
// encoding/json decoded it into an any (a []string is taken too, for
// callers that build the request in code). nil, an absent or null member or
// a nil []string, means the token has no path limit and is returned as nil.
// Otherwise the value must be an array of 1 to 50 strings, each 1 to 255
// characters long and not starting with /; the globs are returned in the
// order given, with duplicates kept. The error's text names the entry it
// refuses, for the 400 answer.
func ValidatePaths(v any) ([]string, error) {
	var entries []any
	switch v := v.(type) {
	case nil:
		return nil, nil
	case []any:
		entries = v
	case []string:
		if v == nil {
			return nil, nil
		}
		entries = make([]any, len(v))
		for i, s := range v {
			entries[i] = s
		}
	default:
		return nil, errors.New("paths must be an array of globs")
	}
	if len(entries) == 0 {
		return nil, errors.New("paths must hold at least one glob; omit it for no path limit")
	}
	if len(entries) > maxPathGlobs {
		return nil, fmt.Errorf("paths holds %d globs, at most %d are allowed", len(entries), maxPathGlobs)
	}
	globs := make([]string, len(entries))
	for i, e := range entries {
		g, ok := e.(string)
		switch {
		case !ok:
			return nil, fmt.Errorf("paths[%d] is not a string", i)
		case g == "":
			return nil, fmt.Errorf("paths[%d] is empty", i)
		case utf8.RuneCountInString(g) > maxGlobLength:
			return nil, fmt.Errorf("paths[%d] is longer than %d characters", i, maxGlobLength)
		case strings.HasPrefix(g, "/"):
			return nil, fmt.Errorf("paths[%d] %q starts with /; globs are relative to the repository root", i, g)
		}
		globs[i] = g
	}
	return globs, nil
}

// MatchPath reports whether the whole of path matches glob, compared
// case-sensitively. In a glob, * matches any run of characters without /,
// ** any run including /, both also the empty run, and wherever they stand,
// not only as a whole segment; every other character, ?, [ and \ included,
// matches only itself. A run of three or more asterisks reads as ** followed
// by the rest, which matches exactly what ** alone does.
//
// This is the one matcher of path limits: minting compares globs with
// Covers, and the parts that enforce a token's paths on writes call
// MatchAny with the token's list.
func MatchPath(glob, path string) bool {
	return matchGlob(compileGlob(glob), path, func(s string, i int) bool { return s[i] != '/' })
}

// MatchAny reports whether a token whose paths list is globs may write path:
// a nil list is no path limit and allows every path; otherwise one glob of
// the list must match it. A non-nil empty list allows nothing.
func MatchAny(globs []string, path string) bool {
	if globs == nil {
		return true
	}
	for _, g := range globs {
		if MatchPath(g, path) {
			return true
		}
	}
	return false
}

// Covers reports whether the glob c covers the glob g: every path g matches,
// c matches too, so that a token holding c may give g to a token it mints.
// It holds when c equals g, or when c, read as a pattern, matches the text
// of g taken literally, where a * of c takes only a run of g that has no /
// and no * belonging to a run of two or more asterisks (that is, it never
// takes the sequence ** or part of it), and a ** of c takes any run.
//
// Why the * rule makes the answer sound: a * of g matches runs without /,
// which a * of c matches too, while a ** of g matches runs with /, which a *
// of c does not. Testing each asterisk of g that a * of c would take against
// its neighbours, rather than the run for the sequence **, answers the same:
// a * of c is never next to another wildcard of c (compileGlob reads a run
// of asterisks as one **) and a literal of c never matches an asterisk, so a
// neighbouring asterisk outside the run could not be matched anyway.
func Covers(c, g string) bool {
	return c == g || matchGlob(compileGlob(c), g, func(s string, i int) bool {
		switch {
		case s[i] == '/':
			return false
		case s[i] != '*':
			return true
		}
		return (i == 0 || s[i-1] != '*') && (i+1 == len(s) || s[i+1] != '*')
	})
}

// CoveredByAny reports whether one of the globs covers g. A nil list is no
// path limit and covers every glob.
func CoveredByAny(globs []string, g string) bool {
	if globs == nil {
		return true
	}
	for _, c := range globs {
		if Covers(c, g) {
			return true
		}
	}
	return false
}

// FirstUncovered returns the first glob of requested, in its order, that no
// glob of held covers, and true; or "" and false when held covers them all.
// A nil held is no path limit and covers everything. Whether a request
// without paths may be made by a token with paths is the caller's rule, not
// this function's.
func FirstUncovered(held, requested []string) (string, bool) {
	for _, g := range requested {
		if !CoveredByAny(held, g) {
			return g, true
		}
	}
	return "", false
}

// globToken is one element of a compiled glob: a literal byte, or one of
// the wildcards.
type globToken struct {
	wild byte // 0 for a literal, '*' for *, '/' for ** (it may take /)
	lit  byte
}

// compileGlob splits a glob into literals and wildcards. A lone * is *, and
// a run of two or more asterisks is one **: ** followed by more asterisks
// matches what ** alone does, in a path and when covering alike.
func compileGlob(glob string) []globToken {
	toks := make([]globToken, 0, len(glob))
	for i := 0; i < len(glob); {
		if glob[i] != '*' {
			toks = append(toks, globToken{lit: glob[i]})
			i++
			continue
		}
		n := 1
		for i+n < len(glob) && glob[i+n] == '*' {
			n++
		}
		if n == 1 {
			toks = append(toks, globToken{wild: '*'})
		} else {
			toks = append(toks, globToken{wild: '/'})
		}
		i += n
	}
	return toks
}

// matchGlob reports whether the tokens match the whole of s, where a * may
// take s[i] only when starOK(s, i). It works over the bytes of s: the only
// special bytes are ASCII, which never occur inside a multi-byte UTF-8
// character, so a literal byte matches exactly when the characters do.
// at[j] is whether the tokens read so far match s[:j]; each token is one pass
// over s, so the cost is the product of the lengths and never exponential.
func matchGlob(toks []globToken, s string, starOK func(s string, i int) bool) bool {
	at := make([]bool, len(s)+1)
	next := make([]bool, len(s)+1)
	at[0] = true
	for _, t := range toks {
		switch t.wild {
		case 0:
			next[0] = false
			for j := 1; j <= len(s); j++ {
				next[j] = at[j-1] && s[j-1] == t.lit
			}
		case '/':
			reached := false
			for j := range at {
				reached = reached || at[j]
				next[j] = reached
			}
		default:
			next[0] = at[0]
			for j := 1; j <= len(s); j++ {
				next[j] = at[j] || next[j-1] && starOK(s, j-1)
			}
		}
		at, next = next, at
	}
	return at[len(s)]
}

package workspace

import (
	"errors"
	"strings"
	"unicode/utf8"
)

// This is the one matcher of branch globs of the hub: every feature that
// reads protected_branches or another list of branch globs calls it, so that
// a glob means the same everywhere.

// globToken is one element of a branch glob: a byte that matches itself, or
// a star.
type globToken struct {
	literal byte
	star    bool // * : any run of characters except /
	any     bool // ** : any run of characters, / included
}

// tokens splits glob into its elements; ** is read before *, so *** is **
// followed by *.
func tokens(glob string) []globToken {
	var out []globToken
	for i := 0; i < len(glob); i++ {
		switch {
		case glob[i] == '*' && i+1 < len(glob) && glob[i+1] == '*':
			out = append(out, globToken{any: true})
			i++
		case glob[i] == '*':
			out = append(out, globToken{star: true})
		default:
			out = append(out, globToken{literal: glob[i]})
		}
	}
	return out
}

// MatchBranchGlob reports whether glob matches the whole of branch,
// case-sensitive: * matches any run of characters except /, ** any run of
// characters including /, and every other character, ? included, only
// itself. It compares bytes, which is exact for UTF-8 names because / never
// occurs inside a multi-byte character, and it takes time proportional to
// the product of the two lengths whatever the glob, so a glob of many stars
// cannot make it slow.
func MatchBranchGlob(glob, branch string) bool {
	// at[j] is whether the elements read so far match branch[:j].
	at := make([]bool, len(branch)+1)
	next := make([]bool, len(branch)+1)
	at[0] = true
	for _, tok := range tokens(glob) {
		for j := range next {
			switch {
			case tok.any:
				next[j] = at[j] || (j > 0 && next[j-1])
			case tok.star:
				next[j] = at[j] || (j > 0 && next[j-1] && branch[j-1] != '/')
			default:
				next[j] = j > 0 && at[j-1] && branch[j-1] == tok.literal
			}
		}
		at, next = next, at
	}
	return at[len(branch)]
}

// MatchAnyBranchGlob reports whether at least one glob of globs matches
// branch; an empty list matches nothing.
func MatchAnyBranchGlob(globs []string, branch string) bool {
	for _, g := range globs {
		if MatchBranchGlob(g, branch) {
			return true
		}
	}
	return false
}

// ValidateBranchGlob returns nil when glob is a branch glob the hub accepts,
// and otherwise an error saying which rule glob breaks: 1 to 255 characters
// that make a valid branch name (ValidateBranchName) once * is allowed too.
func ValidateBranchGlob(glob string) error {
	if n := utf8.RuneCountInString(glob); n < 1 || n > 255 {
		return errors.New("must be 1 to 255 characters long")
	}
	// A star stands for some ordinary characters of a branch name, so the
	// glob passes when the name with each star replaced by one does.
	return ValidateBranchName(strings.ReplaceAll(glob, "*", "x"))
}

package policy

// This is the matcher of the paths and exclude globs of a policy, and of
// nothing else: the paths of a token are matched by the identity package,
// where ? is an ordinary character, and the targets of a rule by the
// branch-glob matcher of the workspace package.

// pathToken is one element of a path glob.
type pathToken struct {
	literal rune
	star    bool // * : any run of characters except /
	any     bool // ** : any run of characters, / included
	one     bool // ? : exactly one character except /
}

// pathTokens splits glob into its elements; ** is read before *, so a longer
// run of stars is ** followed by the rest.
func pathTokens(glob string) []pathToken {
	runes := []rune(glob)
	out := make([]pathToken, 0, len(runes))
	for i := 0; i < len(runes); i++ {
		switch {
		case runes[i] == '*' && i+1 < len(runes) && runes[i+1] == '*':
			out = append(out, pathToken{any: true})
			i++
		case runes[i] == '*':
			out = append(out, pathToken{star: true})
		case runes[i] == '?':
			out = append(out, pathToken{one: true})
		default:
			out = append(out, pathToken{literal: runes[i]})
		}
	}
	return out
}

// MatchPath reports whether glob matches the whole of path, a
// repository-relative path, case-sensitive: * matches any run of characters
// except /, possibly empty, ** any run of characters including /, possibly
// empty, ? exactly one character other than /, and every other character,
// [ and \ included, only itself. It takes time proportional to the product
// of the two lengths whatever the glob, so a glob of many stars cannot make
// it slow.
func MatchPath(glob, path string) bool {
	// ? stands for one character, not one byte, so the path is read in runes.
	name := []rune(path)
	// at[j] is whether the elements read so far match name[:j].
	at := make([]bool, len(name)+1)
	next := make([]bool, len(name)+1)
	at[0] = true
	for _, tok := range pathTokens(glob) {
		for j := range next {
			switch {
			case tok.any:
				next[j] = at[j] || (j > 0 && next[j-1])
			case tok.star:
				next[j] = at[j] || (j > 0 && next[j-1] && name[j-1] != '/')
			case tok.one:
				next[j] = j > 0 && at[j-1] && name[j-1] != '/'
			default:
				next[j] = j > 0 && at[j-1] && name[j-1] == tok.literal
			}
		}
		at, next = next, at
	}
	return at[len(name)]
}

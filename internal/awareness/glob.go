package awareness

import (
	"strings"

	"github.com/agent-fox-dev/parley/internal/identity"
)

// canOverlap reports whether the globs a and b can match the same files:
// exactly when one of them, used as a pattern of the path-glob matcher of
// the token path limits, matches the literal prefix of the other, its text
// before its first asterisk. The answer is the same in both orders.
func canOverlap(a, b string) bool {
	return identity.MatchPath(a, literalPrefix(b)) || identity.MatchPath(b, literalPrefix(a))
}

// literalPrefix is the text of the glob g before its first asterisk, all
// of it when it has none.
func literalPrefix(g string) string {
	if i := strings.IndexByte(g, '*'); i >= 0 {
		return g[:i]
	}
	return g
}

// matchesAny reports whether path matches one of globs.
func matchesAny(globs []string, path string) bool {
	for _, g := range globs {
		if identity.MatchPath(g, path) {
			return true
		}
	}
	return false
}

// overlapsAny reports whether one of the globs a can match the same files
// as one of the globs b.
func overlapsAny(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if canOverlap(x, y) {
				return true
			}
		}
	}
	return false
}

package scm

import "github.com/agent-fox-dev/parley/internal/identity"

// matchesAny reports whether path matches one of globs, with the hub's one
// path matcher: * within a segment, ** across segments, the whole path,
// case-sensitively. No globs match every path.
func matchesAny(globs []string, path string) bool {
	if len(globs) == 0 {
		return true
	}
	for _, g := range globs {
		if identity.MatchPath(g, path) {
			return true
		}
	}
	return false
}

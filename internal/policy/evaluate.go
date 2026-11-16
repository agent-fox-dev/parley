package policy

import "github.com/agent-fox-dev/parley/internal/workspace"

// Select returns the name of the rule of doc that governs file on a change
// into the target branch: the first rule in document order whose targets
// match target, whose paths match file and whose exclude does not, or
// "default" when no rule qualifies.
func Select(doc *Document, target, file string) string {
	for _, r := range doc.Rules {
		if targetMatches(r.Targets, target) && matchesAny(r.Paths, file) && !matchesAny(r.Exclude, file) {
			return r.Name
		}
	}
	return defaultRule
}

// targetMatches reports whether a rule with the targets globs applies to a
// change into branch: absent targets apply to every branch. A targets entry
// is a branch glob, matched by the one branch-glob matcher of the hub so that
// it means what the same glob means in protected_branches.
func targetMatches(globs []string, branch string) bool {
	return globs == nil || workspace.MatchAnyBranchGlob(globs, branch)
}

// matchesAny reports whether at least one path glob of globs matches file.
func matchesAny(globs []string, file string) bool {
	for _, g := range globs {
		if MatchPath(g, file) {
			return true
		}
	}
	return false
}

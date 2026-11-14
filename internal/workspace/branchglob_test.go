package workspace_test

import (
	"testing"

	"github.com/agent-fox-dev/parley/internal/workspace"
)

func TestBranchGlob_MatchesTheWholeBranchName(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		glob, branch string
		want         bool
	}{
		{"main", "main", true},
		{"main", "main2", false},
		{"main", "Main", false},
		{"release/*", "release/1.x", true},
		{"release/*", "release/1/x", false},
		{"release/*", "release", false},
		{"release/**", "release/1/x", true},
		{"**", "a/b/c", true},
		{"*", "main", true},
		{"*", "a/b", false},
		{"feature/*/wip", "feature/x/wip", true},
		{"a?b", "a?b", true},
		{"a?b", "axb", false},
	} {
		if got := workspace.MatchBranchGlob(c.glob, c.branch); got != c.want {
			t.Errorf("MatchBranchGlob(%q, %q) = %v, want %v", c.glob, c.branch, got, c.want)
		}
	}
}

func TestBranchGlob_MatchesAListWhenOneGlobMatches(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		globs  []string
		branch string
		want   bool
	}{
		{[]string{}, "main", false},
		{[]string{"dev", "release/*"}, "release/2", true},
		{[]string{"dev"}, "main", false},
	} {
		if got := workspace.MatchAnyBranchGlob(c.globs, c.branch); got != c.want {
			t.Errorf("MatchAnyBranchGlob(%q, %q) = %v, want %v", c.globs, c.branch, got, c.want)
		}
	}
}

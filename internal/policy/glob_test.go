package policy_test

import (
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/policy"
)

func TestMatchPath_MatchesTheWholePathWithStarsAndQuestionMarks(t *testing.T) {
	// a*a*…*a of 255 characters against 4 096 characters that end in b:
	// a matcher that backtracks would not return.
	manyStars := strings.Repeat("a*", 127) + "a"
	longPath := strings.Repeat("a", 4095) + "b"
	for _, tc := range []struct {
		glob, path string
		want       bool
	}{
		{"docs/**", "docs/a/b.md", true},
		{"docs/**", "docs/", true},
		{"docs/**", "docs", false},
		{"docs/**", "xdocs/a", false},
		{"*.md", "README.md", true},
		{"*.md", "docs/a.md", false},
		{"**/*.md", "docs/a.md", true},
		{"**/*.md", "a/b/c.md", true},
		{"**/*.md", "README.md", false},
		{"src/*.go", "src/.go", true},
		{"src/*.go", "src/a/b.go", false},
		{"a?c", "abc", true},
		{"a?c", "ac", false},
		{"a?c", "a/c", false},
		{"a?c", "abbc", false},
		{"a?c", "aéc", true},
		{"billing/**", "billing/x", true},
		{"Billing/**", "billing/x", false},
		{"a[b]", "a[b]", true},
		{"a[b]", "ab", false},
		{`a\*`, `a\x`, true},
		{"a***b", "a/x/b", true},
		{"**", "any/path", true},
		{"exact/file.txt", "exact/file.txt", true},
		{"exact/file.txt", "exact/file.txt2", false},
		{manyStars, longPath, false},
		{manyStars, longPath[:4095], true},
	} {
		if got := policy.MatchPath(tc.glob, tc.path); got != tc.want {
			t.Errorf("MatchPath(%.40q, %.40q) = %v, want %v", tc.glob, tc.path, got, tc.want)
		}
	}
}

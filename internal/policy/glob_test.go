package policy_test

import (
	"math/rand/v2"
	"regexp"
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

// globRegexp is the regular expression that means what glob means.
func globRegexp(glob string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString(`(?s)^`)
	runes := []rune(glob)
	for i := 0; i < len(runes); i++ {
		switch {
		case runes[i] == '*' && i+1 < len(runes) && runes[i+1] == '*':
			b.WriteString(`.*`)
			i++
		case runes[i] == '*':
			b.WriteString(`[^/]*`)
		case runes[i] == '?':
			b.WriteString(`[^/]`)
		default:
			b.WriteString(regexp.QuoteMeta(string(runes[i])))
		}
	}
	b.WriteString(`$`)
	return regexp.MustCompile(b.String())
}

func TestMatchPath_AgreesWithTheRegularExpressionOfTheGlob(t *testing.T) {
	r := rand.New(rand.NewPCG(192, 1))
	globParts := []string{"a", "b", "é", "/", "*", "**", "?"}
	pathParts := []string{"a", "a", "b", "é", "/"}
	draw := func(parts []string, max int) string {
		var b strings.Builder
		for range r.IntN(max + 1) {
			b.WriteString(parts[r.IntN(len(parts))])
		}
		return b.String()
	}
	for range 20000 {
		// Paths of up to 200 characters cross the 64-position words the
		// matcher works on.
		glob, path := draw(globParts, 12), draw(pathParts, 200)
		if got, want := policy.MatchPath(glob, path), globRegexp(glob).MatchString(path); got != want {
			t.Fatalf("MatchPath(%q, %q) = %v, want %v", glob, path, got, want)
		}
	}
}

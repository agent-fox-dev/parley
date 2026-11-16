package policy

import "testing"

func TestTargetMatches_TakesTheBranchGlobMatcher(t *testing.T) {
	for _, tc := range []struct {
		globs  []string
		branch string
		want   bool
	}{
		{nil, "anything/at/all", true},
		{[]string{"main"}, "main", true},
		{[]string{"main"}, "main2", false},
		{[]string{"release/*"}, "release/1.2", true},
		{[]string{"release/*"}, "release/1/2", false},
		{[]string{"release/**"}, "release/1/2", true},
		{[]string{"*"}, "main", true},
		{[]string{"*"}, "feature/x", false},
		{[]string{"**"}, "feature/x", true},
		{[]string{"rel*"}, "release", true},
		{[]string{"main", "release/*"}, "release/2", true},
	} {
		if got := targetMatches(tc.globs, tc.branch); got != tc.want {
			t.Errorf("targetMatches(%q, %q) = %v, want %v", tc.globs, tc.branch, got, tc.want)
		}
	}
}

func TestSelect_TakesTheFirstQualifyingRuleOrTheDefault(t *testing.T) {
	doc := &Document{Default: NeverSet.Default, Rules: []Rule{
		{Name: "docs", Paths: []string{"docs/**", "**/*.md"}, Exclude: []string{"docs/internal/**"}},
		{Name: "billing", Paths: []string{"billing/**"}, Exclude: []string{}, Targets: []string{"main", "release/*"}},
		{Name: "all-billing", Paths: []string{"billing/**"}, Exclude: []string{}},
		{Name: "secrets", Paths: []string{"deploy/secrets/**"}, Exclude: []string{}},
	}}
	for _, tc := range []struct {
		doc          *Document
		file, target string
		want         string
	}{
		{doc, "docs/a.txt", "main", "docs"},
		{doc, "billing/README.md", "main", "docs"},
		{doc, "docs/internal/x.txt", "main", "default"},
		{doc, "billing/a.go", "main", "billing"},
		{doc, "billing/a.go", "release/2", "billing"},
		{doc, "billing/a.go", "dev", "all-billing"},
		{doc, "src/a.go", "main", "default"},
		{doc, "deploy/secrets/k", "dev", "secrets"},
		{&NeverSet, "docs/a.txt", "main", "default"},
		{&NeverSet, "billing/a.go", "dev", "default"},
	} {
		if got := Select(tc.doc, tc.target, tc.file); got != tc.want {
			t.Errorf("Select(%d rules, %q, %q) = %q, want %q", len(tc.doc.Rules), tc.target, tc.file, got, tc.want)
		}
	}
}

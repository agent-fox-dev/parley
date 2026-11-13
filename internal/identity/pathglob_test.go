package identity_test

import (
	"encoding/json"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/identity"
)

// globsJSON is a JSON array of n copies of glob.
func globsJSON(n int, glob string) string {
	return "[" + strings.TrimSuffix(strings.Repeat(`"`+glob+`",`, n), ",") + "]"
}

func TestValidatePaths_AcceptsUpToFiftyGlobsAndNamesTheEntryItRefuses(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a", 255)
	for _, c := range []struct {
		body    string // the paths member as sent
		want    []string
		problem string // a part of the error's text; "" when the value is accepted
	}{
		{`null`, nil, ""},
		{`["docs/**","src/*.go","docs/**"]`, []string{"docs/**", "src/*.go", "docs/**"}, ""},
		{globsJSON(50, "a"), slices.Repeat([]string{"a"}, 50), ""},
		{`["` + long + `"]`, []string{long}, ""},
		{`[]`, nil, "at least one glob"},
		{globsJSON(51, "a"), nil, "at most 50"},
		{`[""]`, nil, "paths[0]"},
		{`["a","` + long + `a"]`, nil, "paths[1]"},
		{`["/src/**"]`, nil, "paths[0]"},
		{`["docs/**", 7]`, nil, "paths[1]"},
		{`"docs/**"`, nil, "array"},
		{`{}`, nil, "array"},
	} {
		var v any
		if err := json.Unmarshal([]byte(c.body), &v); err != nil {
			t.Fatal(err)
		}
		got, err := identity.ValidatePaths(v)
		switch {
		case c.problem == "" && (err != nil || !slices.Equal(got, c.want) || (got == nil) != (c.want == nil)):
			t.Errorf("ValidatePaths(%.40s) = %q, %v; want %q, nil", c.body, got, err, c.want)
		case c.problem != "" && (err == nil || !strings.Contains(err.Error(), c.problem)):
			t.Errorf("ValidatePaths(%.40s) error = %v, want one naming %q", c.body, err, c.problem)
		}
	}
	if got, err := identity.ValidatePaths([]string(nil)); got != nil || err != nil {
		t.Errorf("ValidatePaths of a nil []string = %q, %v; want nil, nil", got, err)
	}
	if _, err := identity.ValidatePaths([]string{"src/**", "/etc"}); err == nil || !strings.Contains(err.Error(), "paths[1]") {
		t.Errorf("ValidatePaths of a []string with /etc: error = %v, want one naming paths[1]", err)
	}
}

func TestMatchPath_MatchesTheWholePathCaseSensitively(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		glob, path string
		want       bool
	}{
		{"docs/**", "docs/a.md", true},
		{"docs/**", "docs/api/b.md", true},
		{"docs/**", "docs", false},
		{"docs/**", "xdocs/a.md", false},
		{"src/*.go", "src/main.go", true},
		{"src/*.go", "src/x/y.go", false},
		{"src/*.go", "src/.go", true},
		{"src/*.go", "src/main.go.bak", false},
		{"**", "any/deep/file", true},
		{"*", "a/b", false},
		{"*", "a", true},
		{"**/*.md", "docs/a.md", true},
		{"**/*.md", "a.md", false},
		{"README.md", "README.md", true},
		{"README.md", "readme.md", false},
		{"a?c", "abc", false},
		{"a?c", "a?c", true},
		{"a[b]c", "a[b]c", true},
		{"a[b]c", "abc", false},
		{`a\*`, `a\b`, true},
		{"src/**/test/*", "src/a/b/test/x", true},
		{"src/a**", "src/a/b", true},
		{"a***b", "a/x/b", true},
		{"a*b*c", "aXbYbZc", true},
	} {
		if got := identity.MatchPath(c.glob, c.path); got != c.want {
			t.Errorf("MatchPath(%q, %q) = %v, want %v", c.glob, c.path, got, c.want)
		}
	}
	globs := []string{"docs/**", "src/*.go"}
	if !identity.MatchAny(globs, "src/main.go") || identity.MatchAny(globs, "lib/x") {
		t.Errorf("MatchAny(%q) allows src/main.go = %v and lib/x = %v, want true and false",
			globs, identity.MatchAny(globs, "src/main.go"), identity.MatchAny(globs, "lib/x"))
	}
	if !identity.MatchAny(nil, "lib/x") {
		t.Errorf("MatchAny(nil, lib/x) = false, want true: a token without paths is unrestricted")
	}
}

func TestCovers_AcceptsOnlyAGlobThatMatchesEverythingTheRequestedOneDoes(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		c, g string
		want bool
	}{
		{"src/**", "src/api/*.go", true},
		{"src/**", "src/**", true},
		{"src/*", "src/**", false},
		{"src/**", "docs/**", false},
		{"src/*", "src/*", true},
		{"src/*", "src/*.go", true},
		{"src/*", "src/a**", false},
		{"src/*", "src/a/b", false},
		{"**", "anything/**", true},
		{"**", "*", true},
		{"*", "**", false},
		{"src/*.go", "src/main.go", true},
		{"src/*.go", "src/**", false},
		{"docs/*", "docs/**", false},
		{"a/**/c", "a/b/**/c", true},
		{"*", "a*", true},
		{"*", "*b", true},
		{"*", "a**b", false},
		{"a*b", "a**b", false},
		{"*/*", "a**/b", false},
		{"src/*", "src/***", false},
		{"a***", "a**", true},
	} {
		if got := identity.Covers(c.c, c.g); got != c.want {
			t.Errorf("Covers(%q, %q) = %v, want %v", c.c, c.g, got, c.want)
		}
	}
	held := []string{"src/**", "docs/*"}
	if !identity.CoveredByAny(held, "docs/a.md") || identity.CoveredByAny(held, "lib/**") {
		t.Errorf("CoveredByAny(%q) docs/a.md = %v, lib/** = %v; want true, false",
			held, identity.CoveredByAny(held, "docs/a.md"), identity.CoveredByAny(held, "lib/**"))
	}
	for _, c := range []struct {
		requested []string
		first     string
	}{
		{[]string{"src/a/*.go", "docs/x.md"}, ""},
		{[]string{"src/x", "lib/**", "docs/**"}, "lib/**"},
	} {
		first, ok := identity.FirstUncovered(held, c.requested)
		if first != c.first || ok != (c.first != "") {
			t.Errorf("FirstUncovered(%q, %q) = %q, %v; want %q", held, c.requested, first, ok, c.first)
		}
	}
}

// TestCovers_NeverAcceptsAGlobThatMatchesAPathTheCoveringOneDoesNot draws
// globs over {a, b, /, ., *, **} of up to 6 tokens and, for each, paths it
// matches of up to 8 characters and covering candidates (the glob itself,
// the glob with a part replaced by * or **, a random glob), and holds every
// accepted cover to the paths of the requested glob.
func TestCovers_NeverAcceptsAGlobThatMatchesAPathTheCoveringOneDoesNot(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(6, 2))
	tokens := []string{"a", "b", "/", ".", "*", "**"}
	glob := func() string {
		var b strings.Builder
		for range 1 + r.IntN(6) {
			b.WriteString(tokens[r.IntN(len(tokens))])
		}
		return b.String()
	}
	run := func(chars string, max int) string {
		b := make([]byte, r.IntN(max+1))
		for i := range b {
			b[i] = chars[r.IntN(len(chars))]
		}
		return string(b)
	}
	// path expands a glob into a path it should match: a lone * into a run
	// without /, a longer run of asterisks into any run.
	path := func(g string) string {
		var b strings.Builder
		for i := 0; i < len(g); {
			n := 0
			for i+n < len(g) && g[i+n] == '*' {
				n++
			}
			switch {
			case n == 1:
				b.WriteString(run("ab.", 2))
			case n > 1:
				b.WriteString(run("ab/.", 3))
			default:
				b.WriteByte(g[i])
				n = 1
			}
			i += n
		}
		return b.String()
	}
	widen := func(g string) string {
		i := r.IntN(len(g) + 1)
		j := i + r.IntN(len(g)-i+1)
		return g[:i] + []string{"*", "**"}[r.IntN(2)] + g[j:]
	}
	triples, wider := 0, 0
	for draws := 0; triples < 20000; draws++ {
		if draws == 100000 {
			t.Fatalf("%d globs drawn gave only %d triples whose glob covers the requested one", draws, triples)
		}
		g := glob()
		var paths []string
		for range 6 {
			if p := path(g); len(p) <= 8 && identity.MatchPath(g, p) {
				paths = append(paths, p)
			}
		}
		for _, c := range []string{g, widen(g), widen(g), widen(widen(g)), glob()} {
			if !identity.Covers(c, g) {
				continue
			}
			for _, p := range paths {
				triples++
				if c != g {
					wider++
				}
				if !identity.MatchPath(c, p) {
					t.Fatalf("Covers(%q, %q) = true, but %q matches %q and %q does not", c, g, g, p, c)
				}
			}
		}
	}
	if wider < triples/4 {
		t.Fatalf("only %d of %d triples had a covering glob other than the requested one", wider, triples)
	}
}

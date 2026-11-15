package gitrepo_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
)

// searchRepo opens a repository whose main is gitrepotest.SearchStream and
// returns it with the id of main.
func searchRepo(t *testing.T) (*gitrepo.Repo, string) {
	t.Helper()
	dir := t.TempDir()
	gitrepotest.Git(t, dir, "init", "--quiet", "--bare", "--initial-branch=main", dir)
	if _, err := gitrepotest.Run(dir, gitrepotest.SearchStream(), "fast-import", "--quiet"); err != nil {
		t.Fatal(err)
	}
	repo, err := (gitrepo.Git{}).Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	return repo, strings.TrimSpace(gitrepotest.Git(t, dir, "rev-parse", "main"))
}

func TestGrep_FindsTheLinesThatMatchAPatternInPathOrder(t *testing.T) {
	repo, main := searchRepo(t)
	type m = gitrepo.GrepMatch
	for _, c := range []struct {
		name string
		q    gitrepo.GrepQuery
		want []m
	}{
		{"a literal string, with regard to case, in no binary file", gitrepo.GrepQuery{Pattern: "Greeting"},
			[]m{{"api/greeting.go", 3, "func Greeting() string {"}, {"docs/a.md", 1, "# Greeting"}}},
		{"without regard to case", gitrepo.GrepQuery{Pattern: "greeting", IgnoreCase: true},
			[]m{{"api/greeting.go", 3, "func Greeting() string {"}, {"api/greeting.go", 4, "\treturn \"greeting\""}, {"docs/a.md", 1, "# Greeting"}}},
		{"characters special in a regular expression match themselves", gitrepo.GrepQuery{Pattern: "a.b(c)*"},
			[]m{{"literal.txt", 1, "a.b(c)*"}}},
		{"an extended regular expression", gitrepo.GrepQuery{Pattern: `func +[A-Z]\w*\(`, Regexp: true},
			[]m{{"api/greeting.go", 3, "func Greeting() string {"}, {"api/greeting.go", 7, "func Farewell() string { return \"bye\" }"}}},
		{"GNU's escapes beside a bracket expression", gitrepo.GrepQuery{Pattern: `^[\w]indows\s\w+`, Regexp: true},
			[]m{{"crlf.txt", 1, "windows line"}}},
		{"a dot in a regular expression", gitrepo.GrepQuery{Pattern: "a.b", Regexp: true},
			[]m{{"literal.txt", 1, "a.b(c)*"}, {"literal.txt", 2, "aXb"}}},
		{"a pattern that looks like an option", gitrepo.GrepQuery{Pattern: "--all"}, []m{}},
		{"a pattern across lines", gitrepo.GrepQuery{Pattern: "a.b(c)*\naXb"}, []m{}},
		{"nothing", gitrepo.GrepQuery{Pattern: "zzz-not-there"}, []m{}},
		{"a line ended by CR LF", gitrepo.GrepQuery{Pattern: "windows"}, []m{{"crlf.txt", 1, "windows line"}}},
		{"a path with a space and a character beyond ASCII", gitrepo.GrepQuery{Pattern: "unicode"},
			[]m{{"dir with space/ü.txt", 1, "unicode path"}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := []m{}
			err := repo.Grep(context.Background(), main, c.q, func(x m) bool { got = append(got, x); return true })
			if err != nil || !reflect.DeepEqual(got, c.want) {
				t.Errorf("Grep(%+v) = %+v, %v, want %+v", c.q, got, err, c.want)
			}
		})
	}

	// A search stops reading once each says so.
	var lines []int
	err := repo.Grep(context.Background(), main, gitrepo.GrepQuery{Pattern: "hit"}, func(x m) bool {
		lines = append(lines, x.Line)
		return len(lines) < 3
	})
	if err != nil || !reflect.DeepEqual(lines, []int{1, 2, 3}) {
		t.Errorf("Grep(hit) stopped after three = %v, %v, want the lines [1 2 3]", lines, err)
	}
	if long := grepAll(t, repo, main, "needle"); len(long) != 1 || long[0].Text != gitrepotest.SearchLong {
		t.Errorf("Grep(needle) = %d matches, want long.txt's whole line", len(long))
	}
}

func TestGrep_KeepsTheFirstCharactersOfALongLine(t *testing.T) {
	repo, main := searchRepo(t)
	for _, c := range []struct {
		max  int
		want string
	}{
		{500, gitrepotest.SearchLong[:len("needle")+494*len("é")]},
		{1, "n"},
		{2000, gitrepotest.SearchLong},
		{5000, gitrepotest.SearchLong},
	} {
		var got []gitrepo.GrepMatch
		err := repo.Grep(context.Background(), main, gitrepo.GrepQuery{Pattern: "needle", MaxText: c.max}, func(x gitrepo.GrepMatch) bool {
			got = append(got, x)
			return true
		})
		if err != nil || len(got) != 1 || got[0].Text != c.want {
			t.Errorf("Grep(needle, MaxText %d) = %+v, %v, want the line's first %d characters", c.max, got, err, c.max)
		}
	}
	// A line cut short keeps a CR it holds; a whole line loses the CR of its CR LF.
	if got := grepWith(t, repo, main, gitrepo.GrepQuery{Pattern: "windows", MaxText: 12}); len(got) != 1 || got[0].Text != "windows line" {
		t.Errorf("Grep(windows, MaxText 12) = %+v, want the line without its CR", got)
	}
}

func TestGrep_ContinuesAfterAMatch(t *testing.T) {
	repo, main := searchRepo(t)
	type m = gitrepo.GrepMatch
	for _, c := range []struct {
		name string
		q    gitrepo.GrepQuery
		want []m
	}{
		{"after a line of a file", gitrepo.GrepQuery{Pattern: "hit 44", AfterPath: "hits.txt", AfterLine: 44},
			[]m{{"hits.txt", 440, "hit 440"}, {"hits.txt", 441, "hit 441"}, {"hits.txt", 442, "hit 442"}, {"hits.txt", 443, "hit 443"},
				{"hits.txt", 444, "hit 444"}, {"hits.txt", 445, "hit 445"}, {"hits.txt", 446, "hit 446"}, {"hits.txt", 447, "hit 447"},
				{"hits.txt", 448, "hit 448"}, {"hits.txt", 449, "hit 449"}}},
		{"into the files after it", gitrepo.GrepQuery{Pattern: "a", AfterPath: "docs/a.md", AfterLine: 1},
			[]m{{"docs/a.md", 3, "Say hello."}, {"literal.txt", 1, "a.b(c)*"}, {"literal.txt", 2, "aXb"}}},
		{"after a path no file has", gitrepo.GrepQuery{Pattern: "unicode", AfterPath: "dir with space/a", AfterLine: 9},
			[]m{{"dir with space/ü.txt", 1, "unicode path"}}},
		{"after a path beyond ASCII", gitrepo.GrepQuery{Pattern: "e", AfterPath: "dir with space/ü.txt", AfterLine: 1, MaxText: 6},
			[]m{{"docs/a.md", 1, "# Gree"}, {"docs/a.md", 3, "Say he"}, {"long.txt", 1, "needle"}}},
		{"after the last match", gitrepo.GrepQuery{Pattern: "hit", AfterPath: "long.txt", AfterLine: 1}, []m{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := grepWith(t, repo, main, c.q); !reflect.DeepEqual(got, c.want) {
				t.Errorf("Grep(%+v) = %+v, want %+v", c.q, got, c.want)
			}
		})
	}
}

func TestGrep_ContinuesAfterAnyPath(t *testing.T) {
	dir := t.TempDir()
	gitrepotest.Git(t, dir, "init", "--quiet", "--bare", "--initial-branch=main", dir)
	paths := []string{":x/1", "a b", "a*b", "a-b", "a.b/c", "a/b", "a/b/c", "a[b]", "a\\b", "a?", "ab", "é/ü", "\xff"}
	stream := "commit refs/heads/main\ncommitter T <t@example.invalid> 1700000000 +0000\ndata 1\nx\n"
	for _, p := range paths {
		if p == "a/b" {
			continue // a/b is a directory
		}
		stream += "M 100644 inline " + p + "\ndata 4\nhit\n\n"
	}
	if _, err := gitrepotest.Run(dir, stream+"\n", "fast-import", "--quiet"); err != nil {
		t.Fatal(err)
	}
	repo, err := (gitrepo.Git{}).Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	main := strings.TrimSpace(gitrepotest.Git(t, dir, "rev-parse", "main"))
	all := grepWith(t, repo, main, gitrepo.GrepQuery{Pattern: "hit"})
	for _, after := range append(paths, "", "0", "a", "a/", "a/a", "a/b/", "../x", "a/./b", "/a", "a//b", "\xff\xff", "a\x00b") {
		want := []gitrepo.GrepMatch{}
		for _, x := range all {
			if after == "" || x.Path > after {
				want = append(want, x)
			}
		}
		if got := grepWith(t, repo, main, gitrepo.GrepQuery{Pattern: "hit", AfterPath: after, AfterLine: 1}); !reflect.DeepEqual(got, want) {
			t.Errorf("Grep(hit) after %q = %v, want %v", after, got, want)
		}
	}
}

func TestGrep_RefusesAPatternOrCommitItCannotSearch(t *testing.T) {
	repo, main := searchRepo(t)
	for _, c := range []struct {
		name, commit string
		q            gitrepo.GrepQuery
		want         error
	}{
		{"an unbalanced parenthesis", main, gitrepo.GrepQuery{Pattern: "(unclosed", Regexp: true}, gitrepo.ErrInvalidPattern},
		{"a trailing backslash", main, gitrepo.GrepQuery{Pattern: `a\`, Regexp: true}, gitrepo.ErrInvalidPattern},
		{"an empty pattern", main, gitrepo.GrepQuery{}, gitrepo.ErrInvalidPattern},
		{"a NUL byte", main, gitrepo.GrepQuery{Pattern: "a\x00b"}, gitrepo.ErrInvalidPattern},
		{"a commit that is no full id", main[:7], gitrepo.GrepQuery{Pattern: "hit"}, gitrepo.ErrNotFound},
		{"a commit that is an option", "--all", gitrepo.GrepQuery{Pattern: "hit"}, gitrepo.ErrNotFound},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := repo.Grep(context.Background(), c.commit, c.q, func(gitrepo.GrepMatch) bool { return true })
			if !errors.Is(err, c.want) {
				t.Errorf("Grep(%s, %+v) = %v, want %v", c.commit, c.q, err, c.want)
			}
		})
	}
	if got := grepAll(t, repo, main, "(unclosed"); len(got) != 0 {
		t.Errorf("Grep of the fixed string (unclosed = %v, want no match and no error", got)
	}
}

// grepAll is every match of the fixed string pattern at commit.
func grepAll(t *testing.T, repo *gitrepo.Repo, commit, pattern string) []gitrepo.GrepMatch {
	t.Helper()
	return grepWith(t, repo, commit, gitrepo.GrepQuery{Pattern: pattern})
}

// grepWith is every match of q at commit.
func grepWith(t *testing.T, repo *gitrepo.Repo, commit string, q gitrepo.GrepQuery) []gitrepo.GrepMatch {
	t.Helper()
	got := []gitrepo.GrepMatch{}
	if err := repo.Grep(context.Background(), commit, q, func(x gitrepo.GrepMatch) bool {
		got = append(got, x)
		return true
	}); err != nil {
		t.Fatalf("Grep(%+v): %v", q, err)
	}
	return got
}

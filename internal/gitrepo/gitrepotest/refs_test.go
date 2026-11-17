package gitrepotest_test

import (
	"maps"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
)

// forEachRef is what git for-each-ref lists below prefix in dir, as Refs
// maps it.
func forEachRef(t *testing.T, dir, prefix string) map[string]string {
	t.Helper()
	args := []string{"for-each-ref", "--format=%(refname) %(objectname)"}
	if prefix != "" {
		args = append(args, prefix)
	}
	refs := map[string]string{}
	for line := range strings.Lines(gitrepotest.Git(t, dir, args...)) {
		name, id, _ := strings.Cut(strings.TrimSpace(line), " ")
		refs[strings.TrimPrefix(name, prefix)] = id
	}
	return refs
}

func TestRefs_ReadWhatForEachRefLists(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "r.git")
	gitrepotest.InitBare(t, dir, "main")
	one := gitrepotest.Commit(t, dir, "one", map[string]string{"a": "1\n"})
	two := gitrepotest.Commit(t, dir, "two", map[string]string{"a": "2\n"}, one)
	gitrepotest.Git(t, dir, "update-ref", "refs/heads/main", two)
	gitrepotest.Git(t, dir, "update-ref", "refs/heads/feature/x", one)
	gitrepotest.Git(t, dir, "update-ref", "refs/remotes/origin/main", one)
	gitrepotest.Git(t, dir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	gitrepotest.Git(t, dir, "symbolic-ref", "refs/remotes/origin/gone", "refs/remotes/origin/missing")
	gitrepotest.Git(t, dir, "tag", "-a", "-m", "v1", "v1", one)
	gitrepotest.Git(t, dir, "tag", "v2", two)

	for _, c := range []struct {
		name  string
		setup func(t *testing.T)
	}{
		{"loose refs", func(*testing.T) {}},
		{"packed refs", func(t *testing.T) { gitrepotest.Git(t, dir, "pack-refs", "--all") }},
		{"a loose ref over its packed value", func(t *testing.T) {
			gitrepotest.Git(t, dir, "update-ref", "refs/heads/feature/x", two)
		}},
		{"a packed ref deleted", func(t *testing.T) { gitrepotest.Git(t, dir, "update-ref", "-d", "refs/tags/v2") }},
	} {
		c.setup(t)
		for _, prefix := range []string{"", "refs/", "refs/heads/", "refs/remotes/origin/", "refs/tags/", "refs/none/"} {
			if got, want := gitrepotest.Refs(t, dir, prefix), forEachRef(t, dir, prefix); !maps.Equal(got, want) {
				t.Errorf("%s: Refs below %q = %v, want %v", c.name, prefix, got, want)
			}
		}
		for name, want := range forEachRef(t, dir, "") {
			if got := gitrepotest.Ref(dir, name); got != want {
				t.Errorf("%s: Ref %s = %q, want %q", c.name, name, got, want)
			}
		}
		if got := gitrepotest.Ref(dir, "refs/heads/none"); got != "" {
			t.Errorf("%s: Ref of a missing branch = %q, want \"\"", c.name, got)
		}
	}
}

func TestSetRef_LeavesTheRefsUpdateRefLeaves(t *testing.T) {
	t.Parallel()
	// Each case runs SetRef in one repository and git update-ref in a
	// clone of it made before, and compares what for-each-ref lists.
	for _, c := range []struct {
		name  string
		setup func(t *testing.T, dir string)
		ref   string
		to    func(one, two string) string
	}{
		{"a new branch", func(*testing.T, string) {}, "refs/heads/new", func(one, _ string) string { return one }},
		{"a new ref in a new directory", func(*testing.T, string) {}, "refs/parley/replaced/x/y", func(_, two string) string { return two }},
		{"a loose branch moved", func(*testing.T, string) {}, "refs/heads/main", func(one, _ string) string { return one }},
		{"a packed branch moved", func(t *testing.T, dir string) { gitrepotest.Git(t, dir, "pack-refs", "--all") },
			"refs/heads/feature/x", func(_, two string) string { return two }},
		{"through a symbolic ref", func(t *testing.T, dir string) {
			gitrepotest.Git(t, dir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
		}, "refs/remotes/origin/HEAD", func(_, two string) string { return two }},
		{"by a ref name", func(*testing.T, string) {}, "refs/heads/copy", func(string, string) string { return "refs/heads/main" }},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := filepath.Join(t.TempDir(), "got.git")
			gitrepotest.InitBare(t, got, "main")
			one := gitrepotest.Commit(t, got, "one", map[string]string{"a": "1\n"})
			two := gitrepotest.Commit(t, got, "two", map[string]string{"a": "2\n"}, one)
			gitrepotest.Git(t, got, "update-ref", "refs/heads/main", two)
			gitrepotest.Git(t, got, "update-ref", "refs/heads/feature/x", one)
			gitrepotest.Git(t, got, "update-ref", "refs/remotes/origin/main", one)
			c.setup(t, got)
			want := filepath.Join(t.TempDir(), "want.git")
			gitrepotest.Git(t, got, "clone", "--quiet", "--mirror", got, want)
			c.setup(t, want)

			gitrepotest.SetRef(t, got, c.ref, c.to(one, two))
			gitrepotest.Git(t, want, "update-ref", c.ref, c.to(one, two))
			if g, w := forEachRef(t, got, ""), forEachRef(t, want, ""); !maps.Equal(g, w) {
				t.Errorf("after SetRef %s the refs are %v, want %v", c.ref, g, w)
			}
		})
	}
}

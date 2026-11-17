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

package gitrepotest_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
)

// serverInfo is what the files of git's file-based HTTP transport in dir
// hold: info/refs, and the lines of objects/info/packs sorted, since git
// lists the packs in an order of its own.
func serverInfo(t *testing.T, dir string) (refs string, packs []string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "info", "refs"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := os.ReadFile(filepath.Join(dir, "objects", "info", "packs"))
	if err != nil {
		t.Fatal(err)
	}
	packs = strings.Split(string(p), "\n")
	slices.Sort(packs)
	return string(raw), packs
}

func TestPublish_WritesWhatUpdateServerInfoWrites(t *testing.T) {
	t.Parallel()
	o := gitrepotest.NewOrigin(t)
	dir := o.Dir()
	for _, c := range []struct {
		name  string
		setup func(t *testing.T)
	}{
		{"an empty repository", func(*testing.T) {}},
		{"loose objects and refs", func(*testing.T) {
			o.Commit("main", "one")
			o.Commit("feature/x", "two")
		}},
		{"packed objects and refs", func(t *testing.T) {
			gitrepotest.Git(t, dir, "repack", "-q", "-a", "-d")
			gitrepotest.Git(t, dir, "pack-refs", "--all")
		}},
		{"a loose commit on a pack, and a symbolic ref", func(t *testing.T) {
			o.Commit("main", "three")
			gitrepotest.Git(t, dir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/heads/main")
		}},
		{"two packs", func(t *testing.T) {
			o.Commit("main", "four")
			gitrepotest.Git(t, dir, "repack", "-q", "-d")
		}},
		{"an annotated tag", func(t *testing.T) {
			gitrepotest.Git(t, dir, "tag", "-a", "-m", "v1", "v1", "main")
		}},
	} {
		c.setup(t)
		o.Publish()
		gotRefs, gotPacks := serverInfo(t, dir)
		gitrepotest.Git(t, dir, "update-server-info", "--force")
		wantRefs, wantPacks := serverInfo(t, dir)
		if gotRefs != wantRefs {
			t.Errorf("%s: info/refs = %q, want %q", c.name, gotRefs, wantRefs)
		}
		if !slices.Equal(gotPacks, wantPacks) {
			t.Errorf("%s: objects/info/packs lines = %q, want %q", c.name, gotPacks, wantPacks)
		}
	}
}

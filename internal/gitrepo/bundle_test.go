package gitrepo_test

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"testing"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
)

// bundleOf writes a git bundle of the rev-list arguments revs of the
// repository src and returns its path.
func bundleOf(t *testing.T, src string, revs ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "x.bundle")
	gitrepotest.Git(t, src, append([]string{"bundle", "create", "--quiet", path}, revs...)...)
	return path
}

// openBare opens a new empty repository.
func openBare(t *testing.T) (*gitrepo.Repo, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "dst.git")
	gitrepotest.InitBare(t, dir, "main")
	repo, err := (gitrepo.Git{}).Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	return repo, dir
}

// A first bundle holds the whole history of main's last-but-one commit; a
// second, thin one holds only main's last commit and needs the first's.
func TestFetchBundle_SetsTheRefsAtTheBundlesTipsAndKeepsNothingElse(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	src, head, release, _ := forkParent(t, 3)
	prev := gitrepotest.Git(t, src, "rev-parse", "main~1")
	gitrepotest.Git(t, src, "update-ref", "refs/heads/prev", prev)
	repo, dst := openBare(t)

	first := map[string]string{"refs/parley/sandboxes/s/main": prev, "refs/parley/sandboxes/s/release": release}
	if err := repo.FetchBundle(ctx, bundleOf(t, src, "refs/heads/prev", "refs/heads/release"), first); err != nil {
		t.Fatalf("FetchBundle of the whole history: %v", err)
	}
	if refs := gitrepotest.Refs(t, dst, ""); !maps.Equal(refs, first) {
		t.Fatalf("refs after the first bundle = %v, want %v", refs, first)
	}
	second := map[string]string{"refs/parley/sandboxes/s/main": head}
	if err := repo.FetchBundle(ctx, bundleOf(t, src, "refs/heads/main", "--not", "refs/heads/prev"), second); err != nil {
		t.Fatalf("FetchBundle of a thin bundle: %v", err)
	}
	want := map[string]string{"refs/parley/sandboxes/s/main": head, "refs/parley/sandboxes/s/release": release}
	if refs := gitrepotest.Refs(t, dst, ""); !maps.Equal(refs, want) {
		t.Errorf("refs after the second bundle = %v, want %v", refs, want)
	}
}

func TestFetchBundle_FailsWithoutSettingARef(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	src, head, release, _ := forkParent(t, 2)
	prev := gitrepotest.Git(t, src, "rev-parse", "main~1")
	gitrepotest.Git(t, src, "update-ref", "refs/heads/prev", prev)
	notABundle := filepath.Join(t.TempDir(), "plain")
	if err := os.WriteFile(notABundle, []byte("not a bundle\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		path string
		refs map[string]string
	}{
		{"an id that is no tip of the bundle", bundleOf(t, src, "refs/heads/main"),
			map[string]string{"refs/parley/sandboxes/s/main": head, "refs/parley/sandboxes/s/prev": prev}},
		{"an id the bundle does not hold", bundleOf(t, src, "refs/heads/main"),
			map[string]string{"refs/parley/sandboxes/s/main": head, "refs/parley/sandboxes/s/release": release}},
		{"a bundle whose prerequisite the repository lacks", bundleOf(t, src, "refs/heads/main", "--not", "refs/heads/prev"),
			map[string]string{"refs/parley/sandboxes/s/main": head}},
		{"a file that is no bundle", notABundle, map[string]string{"refs/parley/sandboxes/s/main": head}},
		{"a path that begins with a dash", "-x.bundle", map[string]string{"refs/parley/sandboxes/s/main": head}},
		{"an invalid ref name", bundleOf(t, src, "refs/heads/main"), map[string]string{"refs/parley/sandboxes/s/a..b": head}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			repo, dst := openBare(t)
			if err := repo.FetchBundle(ctx, tc.path, tc.refs); err == nil {
				t.Error("FetchBundle = nil, want an error")
			}
			if refs := gitrepotest.Refs(t, dst, ""); len(refs) != 0 {
				t.Errorf("refs after a failed fetch = %v, want none", refs)
			}
		})
	}
}

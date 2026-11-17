package gitrepotest_test

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
)

// repoFiles maps every file and directory of the repository dir, but the
// sample hooks, to its mode and content.
func repoFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		if strings.HasSuffix(rel, ".sample") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		content := ""
		if !d.IsDir() {
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			content = string(raw)
		}
		files[rel] = info.Mode().String() + " " + content
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestInitBare_MakesWhatGitInitMakes(t *testing.T) {
	t.Parallel()
	for _, branch := range []string{"main", "", "trunk"} {
		t.Run("branch "+branch, func(t *testing.T) {
			t.Parallel()
			args := []string{"init", "--quiet", "--bare"}
			if branch != "" {
				args = append(args, "--initial-branch="+branch)
			}
			// The second repository of a branch copies the files the first
			// one read from git.
			for i := range 2 {
				got := filepath.Join(t.TempDir(), "got.git")
				gitrepotest.InitBare(t, got, branch)
				want := filepath.Join(t.TempDir(), "want.git")
				gitrepotest.Git(t, want, append(args, want)...)
				g, w := repoFiles(t, got), repoFiles(t, want)
				names := slices.Sorted(maps.Keys(w))
				for name := range g {
					if _, ok := w[name]; !ok {
						names = append(names, name)
					}
				}
				for _, name := range names {
					if g[name] != w[name] {
						t.Errorf("repository %d: %s = %q, want %q", i, name, g[name], w[name])
					}
				}
			}
		})
	}
}

func TestInitBare_ReinitialisesARepositoryThatExists(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "r.git")
	gitrepotest.InitBare(t, dir, "main")
	id := gitrepotest.Commit(t, dir, "one", map[string]string{"a": "1\n"})
	gitrepotest.Git(t, dir, "update-ref", "refs/heads/main", id)
	gitrepotest.InitBare(t, dir, "main")
	if got := gitrepotest.Ref(dir, "refs/heads/main"); got != id {
		t.Errorf("main after a second InitBare = %q, want %s", got, id)
	}
}

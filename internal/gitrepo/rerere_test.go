package gitrepo_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
)

// rrCacheFiles are the files under the rr-cache of the git directory dir,
// each by its path there, sorted.
func rrCacheFiles(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	root := filepath.Join(dir, "rr-cache")
	err := filepath.WalkDir(root, func(path string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(out)
	return out
}

func TestForgetRerere_RemovesTheConflictWithEveryVariantAndNothingElse(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	gitrepotest.InitBare(t, dir, "")
	forgotten, kept := strings.Repeat("a", 40), strings.Repeat("b", 40)
	// Git keeps the variants of one conflict, numbered, in the conflict's
	// own directory.
	for path, content := range map[string]string{
		forgotten + "/preimage": "<<<<<<<\n", forgotten + "/postimage": "a\n",
		forgotten + "/preimage.1": "<<<<<<<\n", forgotten + "/postimage.1": "a1\n",
		kept + "/preimage": "<<<<<<<\n", kept + "/postimage": "b\n",
	} {
		p := filepath.Join(dir, "rr-cache", path)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	repo, err := gitrepo.Git{}.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	keptFiles := []string{".", kept, kept + "/postimage", kept + "/preimage"}

	if err := repo.ForgetRerere(forgotten); err != nil {
		t.Fatalf("ForgetRerere(%s) = %v, want nil", forgotten, err)
	}
	if got := rrCacheFiles(t, dir); !slices.Equal(got, keptFiles) {
		t.Errorf("rr-cache after the forget holds %q, want %q", got, keptFiles)
	}
	if ok, err := repo.RerereResolved(forgotten); err != nil || ok {
		t.Errorf("RerereResolved(%s) after the forget = %v, %v; want false", forgotten, ok, err)
	}
	if ok, err := repo.RerereResolved(kept); err != nil || !ok {
		t.Errorf("RerereResolved(%s) after the forget of another = %v, %v; want true", kept, ok, err)
	}

	// A conflict that is gone already is forgotten.
	if err := repo.ForgetRerere(forgotten); err != nil {
		t.Errorf("ForgetRerere(%s) again = %v, want nil", forgotten, err)
	}

	// A value that is not a conflict id names no path of the repository.
	for _, id := range []string{"", "..", "../rr-cache/" + kept, "rr-cache", kept + "/postimage", strings.Repeat("g", 40), "-rf"} {
		if err := repo.ForgetRerere(id); err == nil {
			t.Errorf("ForgetRerere(%q) = nil, want an error", id)
		}
	}
	if got := rrCacheFiles(t, dir); !slices.Equal(got, keptFiles) {
		t.Errorf("rr-cache after the refused forgets holds %q, want %q", got, keptFiles)
	}
}

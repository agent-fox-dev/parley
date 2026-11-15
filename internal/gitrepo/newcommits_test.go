package gitrepo_test

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
)

func TestNewCommitsAndChangedPaths_ReadTheObjectsOfAQuarantine(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo, _, _, c3 := threeCommits(t)
	// A fork of the repository holds its new objects alone, as a quarantine
	// does: it reads the repository's through its alternates.
	quarantine := newFork(t, repo.Dir())
	stream := "commit refs/heads/q\ncommitter P <p@example.invalid> now\ndata 0\nfrom " + c3 + "\nM 100644 inline src/q.go\ndata 1\nq\n" +
		"commit refs/heads/q\ncommitter P <p@example.invalid> now\ndata 0\nM 100644 inline docs/b.md\ndata 1\nb\nM 100644 inline docs/a.md\ndata 1\na\n"
	if _, err := gitrepotest.Run(quarantine, stream, "fast-import", "--quiet", "--date-format=now"); err != nil {
		t.Fatal(err)
	}
	c5 := gitrepotest.Git(t, quarantine, "rev-parse", "q")
	c4 := gitrepotest.Git(t, quarantine, "rev-parse", "q~1")

	if _, err := repo.NewCommits(ctx, []string{c5}, ""); err == nil {
		t.Error("the repository alone listed commits it holds no objects of")
	}
	view, err := gitrepo.Git{Env: []string{"GIT_ALTERNATE_OBJECT_DIRECTORIES=" + filepath.Join(quarantine, "objects")}}.Open(ctx, repo.Dir())
	if err != nil {
		t.Fatal(err)
	}
	if got, err := view.NewCommits(ctx, []string{c5}, ""); err != nil || !slices.Equal(got, []string{c4, c5}) {
		t.Errorf("NewCommits = %v, %v, want [%s %s] oldest first", got, err, c4, c5)
	}
	for _, tc := range []struct {
		commit string
		want   []string
	}{
		{c4, []string{"src/q.go"}},
		{c5, []string{"docs/a.md", "docs/b.md"}},
	} {
		if got, err := view.ChangedPaths(ctx, tc.commit); err != nil || !slices.Equal(got, tc.want) {
			t.Errorf("ChangedPaths(%.7s) = %v, %v, want %s", tc.commit, got, err, strings.Join(tc.want, " "))
		}
	}
}

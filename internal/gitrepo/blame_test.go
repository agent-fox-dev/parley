package gitrepo_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
)

func TestBlame_GroupsTheLinesOfAFileByTheCommitThatLastChangedThem(t *testing.T) {
	dir := t.TempDir()
	gitrepotest.Git(t, dir, "init", "--quiet", "--bare", "--initial-branch=main", dir)
	if _, err := gitrepotest.Run(dir, gitrepotest.BlameStream(), "fast-import", "--quiet"); err != nil {
		t.Fatal(err)
	}
	repo, err := (gitrepo.Git{}).Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	ids := strings.Fields(gitrepotest.Git(t, dir, "rev-parse", "main~3", "main~2", "main~1", "main"))
	c1, c2, c3, c4 := ids[0], ids[1], ids[2], ids[3]
	type r = gitrepo.BlameRange
	for _, c := range []struct {
		name, path  string
		first, last int
		want        []r
	}{
		{"a whole file", "f.txt", 0, 0, []r{{1, 2, c1, "f.txt", 1}, {3, 3, c3, "f.txt", 3}, {4, 6, c2, "f.txt", 4}}},
		{"a span cuts the runs at its ends", "f.txt", 2, 4, []r{{2, 2, c1, "f.txt", 2}, {3, 3, c3, "f.txt", 3}, {4, 4, c2, "f.txt", 4}}},
		{"a span to the last line", "f.txt", 5, 0, []r{{5, 6, c2, "f.txt", 5}}},
		{"lines whose original lines are apart", "g.txt", 0, 0, []r{{1, 1, c1, "g.txt", 1}, {2, 2, c1, "g.txt", 3}}},
		{"lines from before a rename", "renamed.txt", 0, 0, []r{{1, 4, c1, "before.txt", 1}, {5, 5, c4, "renamed.txt", 5}}},
		{"an empty file", "empty.txt", 0, 0, []r{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := repo.Blame(context.Background(), c4, c.path, c.first, c.last)
			if err != nil || len(got) != len(c.want) || (len(got) > 0 && !reflect.DeepEqual(got, c.want)) {
				t.Errorf("Blame(%s, %d, %d) = %+v, %v, want %+v", c.path, c.first, c.last, got, err, c.want)
			}
		})
	}
}

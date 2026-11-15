package gitrepo_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
)

func TestQuarantine_HoldsTheObjectsOfAPackUntilTheyAreMovedOrDiscarded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo, _, _, c3 := threeCommits(t)
	client := newFork(t, repo.Dir())
	for _, c := range []struct {
		name             string
		mangle           func(pack string) string
		migrate, invalid bool
	}{
		{"moved into the repository", nil, true, false},
		{"discarded", nil, false, false},
		{"not a pack", func(string) string { return "twenty bytes, no pack" }, false, true},
		{"a truncated pack", func(p string) string { return p[:len(p)/2] }, false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			c4 := gitrepotest.Git(t, client, "commit-tree", "-p", c3, "-m", c.name, c3+"^{tree}")
			pack := gitrepotest.Pack(t, client, c4, "^"+c3)
			if c.mangle != nil {
				pack = c.mangle(pack)
			}
			q, err := repo.NewQuarantine()
			if err != nil {
				t.Fatal(err)
			}
			if err := q.Receive(ctx, strings.NewReader(pack)); (err != nil) != c.invalid {
				t.Fatalf("Receive = %v, want an error %t", err, c.invalid)
			}
			if !c.invalid {
				if missing, err := q.View().Missing(ctx, []string{c3, c4}); err != nil || len(missing) != 0 {
					t.Errorf("the view misses %v, %v, want both the repository's and the pack's commits", missing, err)
				}
				if missing, err := repo.Missing(ctx, []string{c3, c4}); err != nil || !slices.Equal(missing, []string{c4}) {
					t.Errorf("the repository misses %v, %v, want the pack's commit %s before it is moved", missing, err, c4)
				}
			}
			if c.migrate {
				if err := q.Migrate(); err != nil {
					t.Fatal(err)
				}
			}
			if err := q.Discard(); err != nil {
				t.Fatal(err)
			}
			if got := readable(repo.Dir(), c4); got != c.migrate {
				t.Errorf("the pack's commit readable in the repository = %t, want %t", got, c.migrate)
			}
			if strays := gitrepotest.Quarantines(repo.Dir()); len(strays) != 0 {
				t.Errorf("objects/ holds %v, want no quarantine left", strays)
			}
		})
	}
}

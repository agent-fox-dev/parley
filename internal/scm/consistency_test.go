package scm_test

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
)

func TestRead_AnswersForOneCommitWhileItsRefMoves(t *testing.T) {
	w := newWorld(t)
	c1, c2 := w.id["c1"], w.id["c2"]
	trees := map[string]string{}
	for _, c := range []string{c1, c2} {
		trees[c] = strings.TrimSpace(gitrepotest.Git(t, w.repo, "rev-parse", c+"^{tree}"))
	}
	content := map[string]string{c1: "c1\n", c2: "c2\n"}
	repo, err := gitrepo.Git{}.Open(context.Background(), w.repo)
	if err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		path string
		// answer is the commit an answer names and whether the rest of it
		// was computed from that commit.
		answer func(r reply) (sha string, consistent bool)
	}{
		{"/file?ref=main&path=f.txt", func(r reply) (string, bool) {
			sha := r.header.Get("X-Parley-Resolved-Sha")
			return sha, string(r.body) == content[sha]
		}},
		{"/tree?ref=main&recursive=true", func(r reply) (string, bool) {
			sha, _ := r.json["resolved_sha"].(string)
			return sha, r.json["tree_sha"] == trees[sha]
		}},
	} {
		t.Run(c.path, func(t *testing.T) {
			ctx, stop := context.WithCancel(context.Background())
			var wg sync.WaitGroup
			var moves int
			var moveErr error
			wg.Go(func() {
				from, to := c2, c1
				for ctx.Err() == nil {
					if moveErr = repo.CompareAndSwapRef(context.Background(), "refs/heads/main", to, from); moveErr != nil {
						return
					}
					from, to = to, from
					moves++
				}
			})
			seen := map[string]int{}
			for range 200 {
				r := w.get(t, scmPath("demo", c.path), w.ownerKey)
				sha, consistent := c.answer(r)
				if r.status != http.StatusOK || (sha != c1 && sha != c2) || !consistent {
					t.Errorf("%d %q (resolved %q), want 200 computed from c1 or c2 alone", r.status, r.body, sha)
					break
				}
				seen[sha]++
			}
			stop()
			wg.Wait()
			if moveErr != nil || moves == 0 {
				t.Fatalf("the ref moved %d times, then %v", moves, moveErr)
			}
			if moves%2 == 1 {
				if err := repo.CompareAndSwapRef(context.Background(), "refs/heads/main", c2, c1); err != nil {
					t.Fatal(err)
				}
			}
			t.Logf("%d moves; answers from c1 %d, from c2 %d", moves, seen[c1], seen[c2])
		})
	}
}

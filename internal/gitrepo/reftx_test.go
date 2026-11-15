package gitrepo_test

import (
	"context"
	"errors"
	"maps"
	"path/filepath"
	"testing"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
)

func TestPrepareRefs(t *testing.T) {
	t.Parallel()
	const unknown = "1111111111111111111111111111111111111111"
	type u = gitrepo.RefUpdate
	cases := []struct {
		name    string
		updates func(c1, c2, c3 string) []u
		// held is a transaction prepared before this one and still open.
		held    func(c1, c2, c3 string) []u
		commit  bool
		wantErr error
		wantRef string
		// moved is what Commit leaves on top of a=c1, b=c2, main=c3.
		moved func(c1, c2, c3 string) map[string]string
	}{
		{name: "every expectation holds and the transaction commits", commit: true,
			updates: func(c1, c2, c3 string) []u {
				return []u{{"refs/heads/a", c1, c2}, {"refs/heads/n", "", c3}, {"refs/heads/b", c2, ""}}
			},
			moved: func(_, c2, c3 string) map[string]string { return map[string]string{"a": c2, "n": c3, "b": ""} }},
		{name: "every expectation holds and the transaction aborts",
			updates: func(c1, c2, c3 string) []u { return []u{{"refs/heads/a", c1, c2}, {"refs/heads/n", "", c3}} }},
		{name: "a ref moved", wantErr: gitrepo.ErrRefMoved, wantRef: "refs/heads/a",
			updates: func(_, c2, c3 string) []u { return []u{{"refs/heads/n", "", c3}, {"refs/heads/a", c2, c3}} }},
		{name: "a before no object has", wantErr: gitrepo.ErrRefMoved, wantRef: "refs/heads/a",
			updates: func(_, c2, _ string) []u { return []u{{"refs/heads/a", unknown, c2}} }},
		{name: "a create of a ref that exists", wantErr: gitrepo.ErrRefMoved, wantRef: "refs/heads/b",
			updates: func(_, _, c3 string) []u { return []u{{"refs/heads/n", "", c3}, {"refs/heads/b", "", c3}} }},
		{name: "an update of a missing ref", wantErr: gitrepo.ErrRefMoved, wantRef: "refs/heads/gone",
			updates: func(c1, c2, _ string) []u { return []u{{"refs/heads/gone", c1, c2}} }},
		{name: "a delete of a missing ref", wantErr: gitrepo.ErrRefMoved, wantRef: "refs/heads/gone",
			updates: func(c1, _, _ string) []u { return []u{{"refs/heads/gone", c1, ""}} }},
		{name: "a ref another transaction holds", wantErr: gitrepo.ErrRefMoved, wantRef: "refs/heads/a",
			held:    func(c1, _, c3 string) []u { return []u{{"refs/heads/a", c1, c3}} },
			updates: func(c1, c2, _ string) []u { return []u{{"refs/heads/a", c1, c2}} }},
		{name: "a name another ref is in the way of", wantErr: gitrepo.ErrRefNameConflict, wantRef: "refs/heads/a/x",
			updates: func(_, _, c3 string) []u { return []u{{"refs/heads/a/x", "", c3}} }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			repo, c1, c2, c3 := threeCommits(t)
			gitrepotest.Git(t, repo.Dir(), "update-ref", "refs/heads/a", c1)
			gitrepotest.Git(t, repo.Dir(), "update-ref", "refs/heads/b", c2)
			before := gitrepotest.Refs(t, repo.Dir(), "refs/heads/")
			if c.held != nil {
				held, err := repo.PrepareRefs(ctx, c.held(c1, c2, c3))
				if err != nil {
					t.Fatalf("the held transaction: %v", err)
				}
				defer held.Abort()
			}
			tx, err := repo.PrepareRefs(ctx, c.updates(c1, c2, c3))
			var refErr *gitrepo.RefError
			switch {
			case c.wantErr == nil && err != nil:
				t.Fatalf("PrepareRefs = %v, want a prepared transaction", err)
			case c.wantErr != nil && (!errors.Is(err, c.wantErr) || !errors.As(err, &refErr) || refErr.Ref != c.wantRef):
				t.Fatalf("PrepareRefs = %v, want %v naming %s", err, c.wantErr, c.wantRef)
			}
			if got := gitrepotest.Refs(t, repo.Dir(), "refs/heads/"); !maps.Equal(got, before) {
				t.Errorf("refs once prepared = %v, want them unchanged %v", got, before)
			}
			want := maps.Clone(before)
			if c.wantErr == nil && c.commit {
				if err := tx.Commit(); err != nil {
					t.Fatalf("Commit = %v", err)
				}
				for name, id := range c.moved(c1, c2, c3) {
					want[name] = id
					if id == "" {
						delete(want, name)
					}
				}
			} else if c.wantErr == nil {
				if err := tx.Abort(); err != nil {
					t.Fatalf("Abort = %v", err)
				}
			}
			if got := gitrepotest.Refs(t, repo.Dir(), "refs/heads/"); !maps.Equal(got, want) {
				t.Errorf("refs = %v, want %v", got, want)
			}
		})
	}
}

func TestPrepareRefs_ReadsTheNewObjectsOfAQuarantine(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo, _, _, c3 := threeCommits(t)
	quarantine := newFork(t, repo.Dir())
	stream := "commit refs/heads/q\ncommitter P <p@example.invalid> now\ndata 0\nfrom " + c3 + "\nM 100644 inline q\ndata 1\nq\n"
	if _, err := gitrepotest.Run(quarantine, stream, "fast-import", "--quiet", "--date-format=now"); err != nil {
		t.Fatal(err)
	}
	q := gitrepotest.Git(t, quarantine, "rev-parse", "q")
	update := []gitrepo.RefUpdate{{Ref: "refs/heads/main", Old: c3, New: q}}
	if _, err := repo.PrepareRefs(ctx, update); err == nil || errors.Is(err, gitrepo.ErrRefMoved) {
		t.Errorf("PrepareRefs without the quarantine = %v, want git's error about an object it lacks", err)
	}
	view, err := gitrepo.Git{Env: []string{"GIT_ALTERNATE_OBJECT_DIRECTORIES=" + filepath.Join(quarantine, "objects")}}.Open(ctx, repo.Dir())
	if err != nil {
		t.Fatal(err)
	}
	tx, err := view.PrepareRefs(ctx, update)
	if err != nil {
		t.Fatalf("PrepareRefs over the quarantine = %v", err)
	}
	if err := tx.Abort(); err != nil {
		t.Fatalf("Abort = %v", err)
	}
	if got := gitrepotest.Git(t, repo.Dir(), "rev-parse", "main"); got != c3 {
		t.Errorf("main = %s, want %s", got, c3)
	}
}

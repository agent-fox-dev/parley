package carrypatch_test

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
)

func TestRebuild_RebaseReplaysCommitsInOrder(t *testing.T) {
	t.Parallel()
	f := startRebuildFixture(t)
	authors := map[string]string{
		"a1": "Ada <ada@example.com> 1600000000 +0200",
		"a2": "Ada <ada@example.com> 1600000100 +0200",
		"b1": "Bo <bo@example.com> 1600000200 -0500",
	}
	a1 := commitAs(t, f.copy, "a1", authors["a1"], map[string]string{"a1.txt": "a1\n"}, f.base)
	a2 := commitAs(t, f.copy, "a2", authors["a2"], map[string]string{"a2.txt": "a2\n"}, a1)
	b1 := commitAs(t, f.copy, "b1", authors["b1"], map[string]string{"b1.txt": "b1\n"}, f.base)
	f.putPatch(t, "p1", a2)
	f.putPatch(t, "p2", b1)
	// The branches were cut from the base before upstream moved on.
	base := f.moveUpstream(t, "U2", map[string]string{"u2.txt": "u2\n"})

	out, id := f.attempt(t, nil)
	if out.Kind != attemptCompleted {
		t.Fatalf("the attempt = %+v, want completed", out)
	}
	deploy := refIn(f.copy, "refs/heads/deploy")
	log := commitsIn(t, f.copy, base, deploy, "%H|%P|%s|%an <%ae> %ad|%cn <%ce> %cd")
	if len(log) != 3 {
		t.Fatalf("base..deploy = %q, want the three replayed commits", log)
	}
	ids := make([]string, len(log))
	parent := base
	for i, msg := range []string{"a1", "a2", "b1"} {
		fields := strings.Split(log[i], "|")
		ids[i] = fields[0]
		want := []string{fields[0], parent, msg, authors[msg], fmt.Sprintf("Parley <parley@localhost> %d +0000", f.clock().Unix())}
		if !slices.Equal(fields, want) {
			t.Errorf("commit %d of base..deploy = %q, want %q", i, fields, want)
		}
		parent = fields[0]
	}
	results := f.patchResults(t, id)
	if len(results) != 2 {
		t.Fatalf("patch_results = %v, want two", results)
	}
	for i, want := range []string{ids[1], deploy} {
		if r := results[i]; r["status"] != "success" || r["new_head_sha"] != want {
			t.Errorf("patch_results[%d] = %v, want success with new_head_sha %s", i, r, want)
		}
	}
}

func TestRebuild_RebaseLeavesOutMergesAndDropsPresentCommits(t *testing.T) {
	t.Parallel()
	const author = "Ada <ada@example.com> 1600000000 +0200"
	cases := []struct {
		name string
		// setup stores the patches and returns the index of the patch whose
		// added commits are checked and their messages.
		setup func(t *testing.T, f *rebuildFixture) (int, []string)
	}{
		{"a merge of upstream into the branch is left out", func(t *testing.T, f *rebuildFixture) (int, []string) {
			c1 := commitAs(t, f.copy, "c1", author, map[string]string{"c1.txt": "c1\n"}, f.base)
			f.moveUpstream(t, "U2", map[string]string{"u2.txt": "u2\n"})
			// The developer fetched upstream's new head and merged it.
			if err := f.comp.EnsureSetup(context.Background(), f.record(t)); err != nil {
				t.Fatal(err)
			}
			u2 := refIn(f.copy, "refs/remotes/upstream/main")
			merge := commitAs(t, f.copy, "merge upstream", author, map[string]string{"u2.txt": "u2\n"}, c1, u2)
			f.putPatch(t, "p1", commitAs(t, f.copy, "c2", author, map[string]string{"c2.txt": "c2\n"}, merge))
			return 0, []string{"c1", "c2"}
		}},
		{"a commit whose change upstream holds is dropped", func(t *testing.T, f *rebuildFixture) (int, []string) {
			c1 := commitAs(t, f.copy, "c1", author, map[string]string{"x.txt": "x\n"}, f.base)
			f.putPatch(t, "p1", commitAs(t, f.copy, "c2", author, map[string]string{"y.txt": "y\n"}, c1))
			f.moveUpstream(t, "upstream takes c1", map[string]string{"x.txt": "x\n"})
			return 0, []string{"c2"}
		}},
		{"a commit of an earlier patch is dropped", func(t *testing.T, f *rebuildFixture) (int, []string) {
			a1 := commitAs(t, f.copy, "a1", author, map[string]string{"a1.txt": "a1\n"}, f.base)
			f.putPatch(t, "p1", a1)
			// p2 was cut from the branch of p1.
			f.putPatch(t, "p2", commitAs(t, f.copy, "b1", author, map[string]string{"b1.txt": "b1\n"}, a1))
			return 1, []string{"b1"}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := startRebuildFixture(t)
			i, want := tc.setup(t, f)

			out, id := f.attempt(t, nil)
			if out.Kind != attemptCompleted {
				t.Fatalf("the attempt = %+v, want completed", out)
			}
			res, results := f.result(t, id), f.patchResults(t, id)
			if len(results) <= i {
				t.Fatalf("patch_results = %v, want a result for patch %d", results, i)
			}
			before, _ := res["upstream_head_sha"].(string)
			if i > 0 {
				before, _ = results[i-1]["new_head_sha"].(string)
			}
			after, _ := results[i]["new_head_sha"].(string)
			if got := commitsIn(t, f.copy, before, after, "%s"); !slices.Equal(got, want) {
				t.Errorf("the commits added for patch %d = %q, want %q", i, got, want)
			}
			base, _ := res["upstream_head_sha"].(string)
			for _, line := range commitsIn(t, f.copy, base, refIn(f.copy, "refs/heads/deploy"), "%H %P") {
				fields := strings.Fields(line)
				if len(fields) != 2 {
					t.Errorf("the commit %s added by the run has the parents %q, want one", fields[0], fields[1:])
					continue
				}
				if diff := gitrepotest.Git(t, f.copy, "diff-tree", "--no-commit-id", "--name-only", "-r", fields[0]); diff == "" {
					t.Errorf("the commit %s added by the run changes nothing", fields[0])
				}
			}
		})
	}
}

func TestRebuild_BuildsOutOfSight(t *testing.T) {
	t.Parallel()
	f := startRebuildFixture(t)
	f.addPatch(t, "p1")
	f.addPatch(t, "p2")
	before, err := visibleRefs(f.copy)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var advertised []string
	look := func(at string) {
		refs, err := visibleRefs(f.copy)
		if err != nil {
			t.Errorf("read the refs %s: %v", at, err)
			return
		}
		if !maps.Equal(refs, before) {
			t.Errorf("the refs %s = %v, want them as before the attempt, %v", at, refs, before)
		}
		out, err := gitrepotest.Run(f.copy, "", "ls-remote", f.copy)
		if err != nil {
			t.Errorf("list the advertised refs %s: %v", at, err)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		advertised = append(advertised, out)
	}
	f.hooks.beforePatch(func(i int) {
		if i == 1 {
			look("before patch 2")
		}
	})
	f.hooks.beforePublish(func() { look("before the publish") })

	out, id := f.attempt(t, nil)
	if out.Kind != attemptCompleted {
		t.Fatalf("the attempt = %+v, want completed", out)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(advertised) != 2 {
		t.Fatalf("looked at the repository %d times during the attempt, want before patch 2 and before the publish", len(advertised))
	}
	results := f.patchResults(t, id)
	var made []string
	for _, r := range results {
		if h, ok := r["new_head_sha"].(string); ok {
			made = append(made, h)
		}
	}
	if len(made) != 2 {
		t.Fatalf("patch_results = %v, want a new head for each patch", results)
	}
	for i, ad := range advertised {
		for _, h := range made {
			if strings.Contains(ad, h) {
				t.Errorf("the repository advertised the run's commit %s before the publish (look %d): %s", h, i, ad)
			}
		}
	}
	if got := refIn(f.copy, "refs/heads/deploy"); got != made[1] {
		t.Errorf("deploy = %s after the publish, want the run's head %s", got, made[1])
	}
}

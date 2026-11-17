package carrypatch_test

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/agent-fox-dev/parley/internal/carrypatch"
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

func TestRebuild_NothingToApply(t *testing.T) {
	t.Parallel()
	const author = "Ada <ada@example.com> 1600000000 +0200"
	cases := []struct {
		name     string
		strategy string
		setup    func(t *testing.T, f *rebuildFixture)
	}{
		{"rebase: the patch branch points at the upstream base", "rebase", func(t *testing.T, f *rebuildFixture) {
			f.putPatch(t, "p1", f.base)
		}},
		{"rebase: upstream holds the change of the patch's only commit as another commit", "rebase", func(t *testing.T, f *rebuildFixture) {
			f.putPatch(t, "p1", commitAs(t, f.copy, "c1", author, map[string]string{"x.txt": "x\n"}, f.base))
			f.moveUpstream(t, "upstream takes c1", map[string]string{"x.txt": "x\n"})
		}},
		{"merge: the patch branch points at an ancestor of the upstream base", "merge", func(t *testing.T, f *rebuildFixture) {
			f.putPatch(t, "p1", f.base)
			f.moveUpstream(t, "U2", map[string]string{"u2.txt": "u2\n"})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := startRebuildFixture(t)
			tc.setup(t, f)

			out, id := f.attemptWith(t, carrypatch.RebuildOptions{Strategy: &tc.strategy}, nil)
			if out.Kind != attemptCompleted {
				t.Fatalf("the attempt = %+v, want completed", out)
			}
			results := f.patchResults(t, id)
			if len(results) != 1 {
				t.Fatalf("patch_results = %v, want one", results)
			}
			if r := results[0]; r["status"] != "skipped" || r["skipped_reason"] != "nothing_to_apply" || r["new_head_sha"] != nil {
				t.Errorf("the result of p1 = %v, want skipped with nothing_to_apply and no new_head_sha", r)
			}
			base, _ := f.result(t, id)["upstream_head_sha"].(string)
			if got := refIn(f.copy, "refs/heads/deploy"); base == "" || got != base {
				t.Errorf("deploy = %s, want the upstream base %q", got, base)
			}
		})
	}
}

func TestRebuild_MergeAlwaysMakesAMergeCommit(t *testing.T) {
	t.Parallel()
	const author = "Ada <ada@example.com> 1600000000 +0200"
	cases := []struct {
		name         string
		moveUpstream bool
	}{
		{"the patch branch could fast-forward", false},
		{"upstream has moved so a real merge is needed", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := startRebuildFixture(t)
			source := commitAs(t, f.copy, "x1", author, map[string]string{"x.txt": "x\n"}, f.base)
			f.putPatch(t, "feature/x", source)
			if tc.moveUpstream {
				f.moveUpstream(t, "U2", map[string]string{"u2.txt": "u2\n"})
			}

			merge := "merge"
			out, id := f.attemptWith(t, carrypatch.RebuildOptions{Strategy: &merge}, nil)
			if out.Kind != attemptCompleted {
				t.Fatalf("the attempt = %+v, want completed", out)
			}
			results := f.patchResults(t, id)
			if len(results) != 1 || results[0]["status"] != "success" {
				t.Fatalf("patch_results = %v, want one success", results)
			}
			m, _ := results[0]["new_head_sha"].(string)
			before, _ := f.result(t, id)["upstream_head_sha"].(string)
			if m == "" || m == before || m == source {
				t.Fatalf("new_head_sha = %q, want a merge commit of its own (base %s, source %s)", m, before, source)
			}
			want := []string{before + " " + source, "Merge branch 'feature/x'", "Parley <parley@localhost>", "Parley <parley@localhost>"}
			if got := strings.Split(show(t, f.copy, m, "%P|%s|%an <%ae>|%cn <%ce>"), "|"); !slices.Equal(got, want) {
				t.Errorf("parents|subject|author|committer of the merge = %q, want %q", got, want)
			}
			if got := refIn(f.copy, "refs/heads/deploy"); got != m {
				t.Errorf("deploy = %s, want the merge commit %s", got, m)
			}
		})
	}
}

// The test sets variables of the process's environment, which no other
// test may see: it does not run in parallel.
func TestRebuild_CommitterComesFromConfiguration(t *testing.T) {
	const (
		author = "Ada <ada@example.com>"
		hubBot = "Hub Bot <bot@hub.example>"
	)
	for _, strategy := range []string{"rebase", "merge"} {
		t.Run(strategy, func(t *testing.T) {
			f := startRebuildFixtureOn(t, "[land]\ncommitter_name = \"Hub Bot\"\ncommitter_email = \"bot@hub.example\"\n", nil)
			gitrepotest.Git(t, f.copy, "config", "user.name", "Mallory")
			gitrepotest.Git(t, f.copy, "config", "user.email", "mallory@example.com")
			a1 := commitAs(t, f.copy, "a1", author+" 1600000000 +0200", map[string]string{"a1.txt": "a1\n"}, f.base)
			f.putPatch(t, "p1", commitAs(t, f.copy, "a2", author+" 1600000100 +0200", map[string]string{"a2.txt": "a2\n"}, a1))
			f.moveUpstream(t, "U2", map[string]string{"u2.txt": "u2\n"})
			// Set once every commit of the test's own is written: the git
			// the test runs reads them too.
			for _, v := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
				t.Setenv(v, "Mallory")
			}
			for _, v := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
				t.Setenv(v, "mallory@example.com")
			}

			out, id := f.attemptWith(t, carrypatch.RebuildOptions{Strategy: &strategy}, nil)
			if out.Kind != attemptCompleted {
				t.Fatalf("the attempt = %+v, want completed", out)
			}
			base, _ := f.result(t, id)["upstream_head_sha"].(string)
			deploy := refIn(f.copy, "refs/heads/deploy")
			// The commits the run made: the replayed ones, or the merge, whose
			// second parent brings the patch's own commits.
			made := gitrepotest.Git(t, f.copy, "log", "--first-parent", "--format=%an <%ae>|%cn <%ce>", base+".."+deploy)
			want := map[string]string{"rebase": author + "|" + hubBot + "\n" + author + "|" + hubBot, "merge": hubBot + "|" + hubBot}[strategy]
			if made != want {
				t.Errorf("author|committer of the commits the run made = %q, want %q", made, want)
			}
			if raw := gitrepotest.Git(t, f.copy, "log", "--format=raw", base+".."+deploy); strings.Contains(raw, "allory") {
				t.Errorf("a commit of base..deploy names Mallory:\n%s", raw)
			}
		})
	}
}

// show is the commit rev of the repository dir with the git log format
// given.
func show(t *testing.T, dir, rev, format string) string {
	t.Helper()
	return gitrepotest.Git(t, dir, "log", "-1", "--date=raw", "--format="+format, rev)
}

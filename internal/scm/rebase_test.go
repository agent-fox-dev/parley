package scm_test

import (
	"context"
	"fmt"
	"io/fs"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/gitserver/pushrule"
	"github.com/agent-fox-dev/parley/internal/scm"
)

// rebaseStream is the history of the workspace the rebases work on: main
// is m1, writing a.txt, then m2, changing its first line; topic is t1 and
// t2 on top of m1, by authors in zones other than UTC, adding t1.txt and
// t2.txt; clash is k1, changing the first line of a.txt as m2 does not,
// and k2 on top of m1; gone deletes a.txt on top of m1; picks is m2 with
// t1.txt and t2.txt added as topic adds them; merged merges clash into
// topic; behind is at m1, as is the tag v1.
const rebaseStream = "commit refs/heads/main\nmark :1\n" + testCommitter + "data 2\nm1\nM 100644 inline a.txt\ndata 8\none\ntwo\n\n" +
	"commit refs/heads/main\nmark :2\n" + testCommitter + "data 2\nm2\nfrom :1\nM 100644 inline a.txt\ndata 8\nONE\ntwo\n\n" +
	"commit refs/heads/topic\nmark :3\nauthor Ann <ann@example.invalid> 1600000000 +0200\n" + testCommitter +
	"data 3\nt1\n\nfrom :1\nM 100644 inline t1.txt\ndata 3\nt1\n\n" +
	"commit refs/heads/topic\nmark :4\nauthor Bob <bob@example.invalid> 1600000100 -0500\n" + testCommitter +
	"data 9\nt2\n\nbody\nM 100644 inline t2.txt\ndata 3\nt2\n\n" +
	"commit refs/heads/clash\nmark :5\n" + testCommitter + "data 2\nk1\nfrom :1\nM 100644 inline a.txt\ndata 8\nuno\ntwo\n\n" +
	"commit refs/heads/clash\nmark :6\n" + testCommitter + "data 2\nk2\nM 100644 inline k.txt\ndata 2\nk\n\n" +
	"commit refs/heads/gone\n" + testCommitter + "data 2\ng1\nfrom :1\nD a.txt\n\n" +
	"commit refs/heads/picks\n" + testCommitter + "data 2\np1\nfrom :2\nM 100644 inline t1.txt\ndata 3\nt1\n\n" +
	"commit refs/heads/picks\n" + testCommitter + "data 2\np2\nM 100644 inline t2.txt\ndata 3\nt2\n\n" +
	"commit refs/heads/merged\n" + testCommitter + "data 2\nmg\nfrom :4\nmerge :6\nM 100644 inline k.txt\ndata 2\nk\n\n" +
	"reset refs/heads/behind\nfrom :1\n\nreset refs/tags/v1\nfrom :1\n\n"

// rebaseWorld is a writeWorld with the workspace rb of the owner, its
// repository made by rebaseStream, whose git directory is dir and whose
// commits id names by their messages; rules counts the requests the rule
// engine decides.
type rebaseWorld struct {
	*writeWorld
	dir   string
	id    map[string]string
	rules *countingRules
	path  string
}

func newRebaseWorld(t *testing.T) *rebaseWorld {
	t.Helper()
	rules := &countingRules{}
	w := &rebaseWorld{rules: rules, path: scmPath("rb", "/rebase"), id: map[string]string{}}
	w.writeWorld = newWriteWorld(t, "", func(d *scm.Deps) { rules.Rules, d.Rules = d.Rules, rules })
	w.dir = w.addWorkspace(t, "rb", w.org, rebaseStream)
	ids := strings.Fields(gitrepotest.Git(t, w.dir, "rev-parse", "main~1", "main", "topic~1", "topic", "clash~1", "clash", "picks"))
	for i, name := range []string{"m1", "m2", "t1", "t2", "k1", "k2", "p2"} {
		w.id[name] = ids[i]
	}
	return w
}

// countingRules are rules that count the requests they decide.
type countingRules struct {
	scm.Rules
	n atomic.Int64
}

func (c *countingRules) Evaluate(ctx context.Context, req pushrule.Request) pushrule.Verdict {
	c.n.Add(1)
	return c.Rules.Evaluate(ctx, req)
}

// head is the commit the branch name is at.
func (w *rebaseWorld) head(t *testing.T, name string) string {
	t.Helper()
	return gitrepotest.Refs(t, w.dir, "refs/heads/")[name]
}

// reset puts the branch name at the commit id.
func (w *rebaseWorld) reset(t *testing.T, name, id string) {
	t.Helper()
	gitrepotest.Git(t, w.dir, "update-ref", "refs/heads/"+name, id)
}

// rebase sends a rebase request with body, as the owner.
func (w *rebaseWorld) rebase(t *testing.T, body map[string]any) reply {
	t.Helper()
	return w.sendJSON(t, http.MethodPost, w.path, w.ownerKey, body)
}

// side is the file path of the commit rev as a side of a conflict report,
// as JSON decodes it.
func (w *rebaseWorld) side(t *testing.T, rev, path string) map[string]any {
	t.Helper()
	f := strings.Fields(gitrepotest.Git(t, w.dir, "ls-tree", rev, "--", path))
	if len(f) != 4 {
		t.Fatalf("ls-tree %s %s answered %q", rev, path, f)
	}
	return map[string]any{"sha": f[2], "mode": f[0], "path": path}
}

// checkRebased checks that r is a 200 rebased answer moving branch from
// old onto the commit onto, and returns the new head.
func (w *rebaseWorld) checkRebased(t *testing.T, name string, r reply, branch, old, onto string) string {
	t.Helper()
	newHead, _ := r.json["new_head_sha"].(string)
	b, _ := r.json["branch"].(map[string]any)
	if r.status != http.StatusOK || r.json["status"] != "rebased" || r.json["old_head_sha"] != old ||
		r.json["onto_sha"] != onto || len(newHead) != 40 {
		t.Fatalf("%s: %d %s, want 200 rebased from %s onto %s", name, r.status, r.body, old, onto)
	}
	if b["name"] != "refs/heads/"+branch || b["sha"] != newHead {
		t.Errorf("%s: branch %v, want refs/heads/%s at %s", name, b, branch, newHead)
	}
	if got := w.head(t, branch); got != newHead {
		t.Errorf("%s: %s is at %s, want the new head %s", name, branch, got, newHead)
	}
	if seen := w.observed.reset(); !slices.Equal(seen, []string{"rebase ok"}) {
		t.Errorf("%s: the recorder saw %q, want [rebase ok]", name, seen)
	}
	return newHead
}

// checkPush checks that the events after seq are one push of branch from
// before to after, forced as forced says, via api.
func (w *rebaseWorld) checkPush(t *testing.T, name string, seq int64, branch, before, after string, forced bool) {
	t.Helper()
	_, pushes := w.eventsAfter(t, seq)
	if len(pushes) != 1 || pushes[0].Ref != "refs/heads/"+branch || pushes[0].Before == nil || *pushes[0].Before != before ||
		pushes[0].After == nil || *pushes[0].After != after || pushes[0].Forced != forced || pushes[0].Via != pushrule.ViaAPI {
		t.Errorf("%s: push events %+v, want one moving %s from %s to %s, forced %v, via api", name, pushes, branch, before, after, forced)
	}
}

// TestRebase checks POST …/scm/rebase on one workspace, each behaviour
// putting the branches it uses where it expects them.
func TestRebase(t *testing.T) {
	t.Parallel()
	w := newRebaseWorld(t)
	m1, m2, t1, t2 := w.id["m1"], w.id["m2"], w.id["t1"], w.id["t2"]

	t.Run("replays the branch onto the onto commit", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			body map[string]any
		}{
			{"at the expected head", map[string]any{"branch": "topic", "onto": "main", "expected_head_sha": t2}},
			{"by full name without an expected head", map[string]any{"branch": "refs/heads/topic", "onto": "main"}},
		} {
			w.reset(t, "topic", t2)
			w.observed.reset()
			seq := w.lastSeq(t)
			r := w.rebase(t, tc.body)
			newHead := w.checkRebased(t, tc.name, r, "topic", t2, m2)
			if newHead == t2 {
				t.Errorf("%s: the new head is the old one", tc.name)
			}
			if v, ok := r.json["change"]; !ok || v != nil {
				t.Errorf("%s: change is %v (present %v), want null", tc.name, v, ok)
			}
			if n := gitrepotest.Git(t, w.dir, "rev-list", "--count", "main..topic"); n != "2" {
				t.Errorf("%s: main..topic holds %q commits, want 2", tc.name, n)
			}
			if base := gitrepotest.Git(t, w.dir, "merge-base", "main", "topic"); base != m2 {
				t.Errorf("%s: the merge base of main and topic is %q, want m2 %s", tc.name, base, m2)
			}
			show := func(rev, format string) string {
				return gitrepotest.Git(t, w.dir, "log", "-1", "--date=raw", "--format="+format, rev)
			}
			for i, pair := range [][2]string{{t1, newHead + "~1"}, {t2, newHead}} {
				old, replayed := pair[0], pair[1]
				if a, b := show(old, "%an <%ae> %ad%n%B"), show(replayed, "%an <%ae> %ad%n%B"); a != b {
					t.Errorf("%s: commit %d has author and message %q, want %q", tc.name, i, b, a)
				}
				if c := show(replayed, "%cn <%ce>"); c != "parley <parley@localhost>" {
					t.Errorf("%s: commit %d has the committer %q, want the hub", tc.name, i, c)
				}
				if a, b := gitrepotest.Git(t, w.dir, "diff", old+"^", old), gitrepotest.Git(t, w.dir, "diff", replayed+"^", replayed); a != b {
					t.Errorf("%s: commit %d changes\n%s\nwant\n%s", tc.name, i, b, a)
				}
			}
			w.checkPush(t, tc.name, seq, "topic", t2, newHead, true)
		}
	})

	t.Run("answers up_to_date when the branch holds onto", func(t *testing.T) {
		w.reset(t, "topic", t2)
		for _, onto := range []struct{ name, rev, sha string }{
			{"an ancestor", m1, m1},
			{"the head itself", t2, t2},
			{"the branch's own name", "topic", t2},
		} {
			w.observed.reset()
			w.stage.reset()
			seq, decided := w.lastSeq(t), w.rules.n.Load()
			r := w.rebase(t, map[string]any{"branch": "topic", "onto": onto.rev})
			b, _ := r.json["branch"].(map[string]any)
			if r.status != http.StatusOK || r.json["status"] != "up_to_date" || r.json["old_head_sha"] != t2 ||
				r.json["new_head_sha"] != t2 || r.json["onto_sha"] != onto.sha || b["sha"] != t2 {
				t.Errorf("%s: %d %s, want 200 up_to_date at %s onto %s", onto.name, r.status, r.body, t2, onto.sha)
			}
			if got := w.head(t, "topic"); got != t2 {
				t.Errorf("%s: topic moved to %s", onto.name, got)
			}
			if types, _ := w.eventsAfter(t, seq); len(types) != 0 {
				t.Errorf("%s: events %q, want none", onto.name, types)
			}
			if n, seen := w.rules.n.Load()-decided, w.stage.reset(); n != 0 || len(seen) != 0 {
				t.Errorf("%s: the rules decided %d requests and the stage ran %d times, want none", onto.name, n, len(seen))
			}
			if seen := w.observed.reset(); !slices.Equal(seen, []string{"rebase ok"}) {
				t.Errorf("%s: the recorder saw %q, want [rebase ok]", onto.name, seen)
			}
		}
	})

	t.Run("moves a branch whose commits onto holds to onto", func(t *testing.T) {
		for _, tc := range []struct {
			name, branch, from, onto, want string
			forced                         bool
		}{
			{"a branch behind onto", "behind", m1, "main", m2, false},
			{"a branch whose commits were picked onto onto", "topic", t2, "picks", w.id["p2"], true},
		} {
			w.reset(t, tc.branch, tc.from)
			w.observed.reset()
			seq := w.lastSeq(t)
			r := w.rebase(t, map[string]any{"branch": tc.branch, "onto": tc.onto})
			if newHead := w.checkRebased(t, tc.name, r, tc.branch, tc.from, tc.want); newHead != tc.want {
				t.Errorf("%s: new_head_sha %s, want onto's %s", tc.name, newHead, tc.want)
			}
			w.checkPush(t, tc.name, seq, tc.branch, tc.from, tc.want, tc.forced)
		}
	})

	t.Run("reports a conflict and moves nothing", func(t *testing.T) {
		k1, k2 := w.id["k1"], w.id["k2"]
		gone := w.head(t, "gone")
		for _, tc := range []struct {
			name, branch, head, commit string
			file                       map[string]any
		}{
			{"both sides change a line", "clash", k2, k1, map[string]any{"path": "a.txt", "kind": "content",
				"base": w.side(t, m1, "a.txt"), "ours": w.side(t, m2, "a.txt"), "theirs": w.side(t, k1, "a.txt"), "landed_by": []any{}}},
			{"the branch deletes what onto changes", "gone", gone, gone, map[string]any{"path": "a.txt", "kind": "modify_delete",
				"base": w.side(t, m1, "a.txt"), "ours": w.side(t, m2, "a.txt"), "theirs": nil, "landed_by": []any{}}},
		} {
			refs, files := gitrepotest.Git(t, w.dir, "for-each-ref"), gitFiles(t, w.dir)
			w.observed.reset()
			seq := w.lastSeq(t)
			r := w.rebase(t, map[string]any{"branch": tc.branch, "onto": "main"})
			d := errorDetails(r)
			want := map[string]any{"target_sha": m2, "head_sha": tc.head, "base_sha": m1, "commit_sha": tc.commit,
				"files": []any{tc.file}, "landed_changes": []any{}, "change": nil}
			if r.status != http.StatusConflict || r.errorType() != "rebase_conflict" || fmt.Sprint(d) != fmt.Sprint(want) {
				t.Errorf("%s: %d %s, want 409 rebase_conflict with the details %v", tc.name, r.status, r.body, want)
			}
			if after := gitrepotest.Git(t, w.dir, "for-each-ref"); after != refs {
				t.Errorf("%s: the refs changed from\n%s\nto\n%s", tc.name, refs, after)
			}
			if types, _ := w.eventsAfter(t, seq); len(types) != 0 {
				t.Errorf("%s: events %q, want none", tc.name, types)
			}
			if after := gitFiles(t, w.dir); !slices.Equal(after, files) {
				t.Errorf("%s: the repository's files outside objects/ were %q, then %q", tc.name, files, after)
			}
			if seen := w.observed.reset(); !slices.Equal(seen, []string{"rebase error"}) {
				t.Errorf("%s: the recorder saw %q, want [rebase error]", tc.name, seen)
			}
		}
	})

	t.Run("refuses an invalid rebase and moves nothing", func(t *testing.T) {
		w.reset(t, "topic", t2)
		for _, tc := range []struct {
			name      string
			body      map[string]any
			status    int
			errorType string
		}{
			{"a merge commit to replay", map[string]any{"branch": "merged", "onto": "main"}, http.StatusBadRequest, "merge_commits_unsupported"},
			{"no branch", map[string]any{"onto": "main"}, http.StatusBadRequest, "invalid_request"},
			{"no onto", map[string]any{"branch": "topic"}, http.StatusBadRequest, "invalid_request"},
			{"an unknown branch", map[string]any{"branch": "nope", "onto": "main"}, http.StatusNotFound, "ref_not_found"},
			{"an unknown onto", map[string]any{"branch": "topic", "onto": "nope"}, http.StatusNotFound, "ref_not_found"},
			{"a tag for the branch", map[string]any{"branch": "refs/tags/v1", "onto": "main"}, http.StatusBadRequest, "invalid_request"},
			{"an abbreviated expected head", map[string]any{"branch": "topic", "onto": "main", "expected_head_sha": t2[:7]},
				http.StatusBadRequest, "invalid_request"},
		} {
			refs := gitrepotest.Git(t, w.dir, "for-each-ref")
			seq := w.lastSeq(t)
			r := w.rebase(t, tc.body)
			if r.status != tc.status || r.errorType() != tc.errorType {
				t.Errorf("%s: %d %s, want %d %s", tc.name, r.status, r.body, tc.status, tc.errorType)
			}
			if after := gitrepotest.Git(t, w.dir, "for-each-ref"); after != refs {
				t.Errorf("%s: the refs changed from\n%s\nto\n%s", tc.name, refs, after)
			}
			if types, _ := w.eventsAfter(t, seq); len(types) != 0 {
				t.Errorf("%s: events %q, want none", tc.name, types)
			}
		}
	})

	t.Run("keeps the head another writer gave the branch", func(t *testing.T) {
		w.reset(t, "topic", t2)
		other := commit(t, w.dir, t2, "x.txt", "x")
		var mu sync.Mutex
		var hookErr error
		for _, tc := range []struct {
			name    string
			body    map[string]any
			hook    func()
			current string
		}{
			{"a stale expected head", map[string]any{"branch": "topic", "onto": "main", "expected_head_sha": t1}, nil, t2},
			{"a branch moved while the commits are replayed", map[string]any{"branch": "topic", "onto": "main"}, func() {
				mu.Lock()
				defer mu.Unlock()
				_, hookErr = gitrepotest.Run(w.dir, "", "update-ref", "refs/heads/topic", other)
			}, other},
		} {
			scm.SetRebaseHook(w.scm, tc.hook)
			w.observed.reset()
			seq := w.lastSeq(t)
			r := w.rebase(t, tc.body)
			scm.SetRebaseHook(w.scm, nil)
			mu.Lock()
			if hookErr != nil {
				t.Fatalf("%s: the hook could not move topic: %v", tc.name, hookErr)
			}
			mu.Unlock()
			if r.status != http.StatusConflict || r.errorType() != "ref_moved" || errorDetails(r)["current_sha"] != tc.current {
				t.Errorf("%s: %d %s, want 409 ref_moved with details.current_sha %s", tc.name, r.status, r.body, tc.current)
			}
			if got := w.head(t, "topic"); got != tc.current {
				t.Errorf("%s: topic is at %s, want %s", tc.name, got, tc.current)
			}
			if types, _ := w.eventsAfter(t, seq); len(types) != 0 {
				t.Errorf("%s: events %q, want none", tc.name, types)
			}
			if seen := w.observed.reset(); !slices.Equal(seen, []string{"rebase ref_moved"}) {
				t.Errorf("%s: the recorder saw %q, want [rebase ref_moved]", tc.name, seen)
			}
		}
	})

	t.Run("names the change the rebase made a version of", func(t *testing.T) {
		w.reset(t, "topic", t2)
		w.stage.mu.Lock()
		w.stage.versions = map[string]*pushrule.Version{"refs/heads/topic": {WorkspaceSlug: "rb", Number: 7, Version: 3}}
		w.stage.mu.Unlock()
		w.stage.reset()
		w.observed.reset()
		r := w.rebase(t, map[string]any{"branch": "topic", "onto": "main"})
		newHead := w.checkRebased(t, "rebased", r, "topic", t2, m2)
		if want := map[string]any{"workspace_slug": "rb", "number": 7.0, "version": 3.0}; fmt.Sprint(r.json["change"]) != fmt.Sprint(want) {
			t.Errorf("rebased: change %v, want %v", r.json["change"], want)
		}
		seen := w.stage.reset()
		if len(seen) != 1 || seen[0].VersionKind != pushrule.VersionKindRebase || seen[0].Via != pushrule.ViaAPI ||
			len(seen[0].Updates) != 1 || seen[0].Updates[0].Ref != "refs/heads/topic" ||
			seen[0].Updates[0].Before != t2 || seen[0].Updates[0].After != newHead {
			t.Errorf("rebased: the stage saw %+v, want one call with version kind rebase from %s to %s", seen, t2, newHead)
		}

		w.linker.answer("topic", &scm.LinkedChange{WorkspaceSlug: "rb", Number: 7, Version: 4, State: scm.ChangeOpen})
		defer w.linker.answer("topic", nil)
		r = w.rebase(t, map[string]any{"branch": "topic", "onto": "main"})
		if want := map[string]any{"workspace_slug": "rb", "number": 7.0, "version": 4.0}; r.status != http.StatusOK ||
			r.json["status"] != "up_to_date" || fmt.Sprint(r.json["change"]) != fmt.Sprint(want) {
			t.Errorf("up_to_date: %d %s, want 200 up_to_date with change %v", r.status, r.body, want)
		}
		if seen := w.stage.reset(); len(seen) != 0 {
			t.Errorf("up_to_date: the stage saw %+v, want no call", seen)
		}
	})
}

// gitFiles are the paths of the files of the git directory dir outside
// objects/, such as an index, a lock or a work tree a rebase could leave.
func gitFiles(t *testing.T, dir string) []string {
	t.Helper()
	var paths []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		if d.IsDir() && rel == "objects" {
			return filepath.SkipDir
		}
		if !d.IsDir() {
			paths = append(paths, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(paths)
	return paths
}

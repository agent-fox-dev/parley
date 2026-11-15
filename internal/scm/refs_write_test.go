package scm_test

import (
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/gitserver/pushrule"
	"github.com/agent-fox-dev/parley/internal/scm"
)

// refsWorld is a writeWorld with the workspace refs of the owner, its
// repository made by refsStream, whose git directory is dir and whose
// commits m1 and m2 are main's.
type refsWorld struct {
	*writeWorld
	dir, m1, m2 string
}

func newRefsWorld(t *testing.T) *refsWorld {
	t.Helper()
	w := &refsWorld{writeWorld: newWriteWorld(t, "")}
	w.dir = w.addWorkspace(t, "refs", w.org, refsStream)
	ids := strings.Fields(gitrepotest.Git(t, w.dir, "rev-parse", "main~1", "main"))
	w.m1, w.m2 = ids[0], ids[1]
	return w
}

// head is what the ref name of the refs workspace points at, "" when it
// does not exist.
func (w *refsWorld) head(t *testing.T, name string) string {
	t.Helper()
	return gitrepotest.Refs(t, w.dir, "refs/")[strings.TrimPrefix(name, "refs/")]
}

// refsResult is the result a write of refs reports to the recorder for an
// answer of status and errorType.
func refsResult(status int, errorType string) string {
	switch {
	case errorType == "ref_moved":
		return scm.ResultRefMoved
	case status == http.StatusForbidden:
		return scm.ResultRefused
	case status >= http.StatusBadRequest:
		return scm.ResultError
	}
	return scm.ResultOK
}

// checkObserved checks that the write was reported once, under op, with the
// result of its answer r.
func (w *refsWorld) checkObserved(t *testing.T, name, op string, r reply) {
	t.Helper()
	if seen, want := w.observed.reset(), []string{op + " " + refsResult(r.status, r.errorType())}; !slices.Equal(seen, want) {
		t.Errorf("%s: the recorder saw %q, want %q", name, seen, want)
	}
}

// TestRefCreate checks POST …/scm/refs: a valid name that does not exist
// and one target that is a commit make the ref there, through the ref
// writer, and anything else is refused without a ref or an event.
func TestRefCreate(t *testing.T) {
	t.Parallel()
	w := newRefsWorld(t)
	path := scmPath("refs", "/refs")

	t.Run("creates the ref at the named commit", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			body map[string]any
			full string
			kind string
		}{
			{"bare name by sha", map[string]any{"name": "x", "sha": w.m2}, "refs/heads/x", "branch"},
			{"full branch name by ref", map[string]any{"name": "refs/heads/feat/y", "ref": "main"}, "refs/heads/feat/y", "branch"},
			{"tag by ref", map[string]any{"name": "refs/tags/v1", "ref": "main"}, "refs/tags/v1", "tag"},
			{"bare name by an abbreviated id", map[string]any{"name": "z", "ref": w.m2[:7]}, "refs/heads/z", "branch"},
		} {
			seq := w.lastSeq(t)
			r := w.sendJSON(t, http.MethodPost, path, w.ownerKey, tc.body)
			if r.status != http.StatusCreated || r.json["name"] != tc.full || r.json["kind"] != tc.kind || r.json["sha"] != w.m2 {
				t.Errorf("%s: %d %s, want 201 with name %s, kind %s, sha %s", tc.name, r.status, r.body, tc.full, tc.kind, w.m2)
			}
			w.checkObserved(t, tc.name, "ref_create", r)
			if got := w.head(t, tc.full); got != w.m2 {
				t.Errorf("%s: %s is at %q, want %s", tc.name, tc.full, got, w.m2)
			}
			_, pushes := w.eventsAfter(t, seq)
			if len(pushes) != 1 || pushes[0].Ref != tc.full || pushes[0].Before != nil || pushes[0].After == nil ||
				*pushes[0].After != w.m2 || !pushes[0].Created || pushes[0].Deleted || pushes[0].Via != pushrule.ViaAPI {
				t.Errorf("%s: push events %+v, want one creating %s at %s via api", tc.name, pushes, tc.full, w.m2)
			}
		}
		if kind := strings.TrimSpace(gitrepotest.Git(t, w.dir, "cat-file", "-t", "refs/tags/v1")); kind != "commit" {
			t.Errorf("refs/tags/v1 points at a %s, want a commit (a lightweight tag)", kind)
		}
	})

	t.Run("refuses an invalid creation and creates nothing", func(t *testing.T) {
		tree := strings.TrimSpace(gitrepotest.Git(t, w.dir, "rev-parse", "main^{tree}"))
		sha := w.m2
		for _, tc := range []struct {
			name      string
			body      map[string]any
			status    int
			errorType string
		}{
			{"no name", map[string]any{"sha": sha}, http.StatusBadRequest, "invalid_request"},
			{"empty name", map[string]any{"name": "", "sha": sha}, http.StatusBadRequest, "invalid_request"},
			{"name with ..", map[string]any{"name": "a..b", "sha": sha}, http.StatusBadRequest, "invalid_request"},
			{"name ending in .lock", map[string]any{"name": "x.lock", "sha": sha}, http.StatusBadRequest, "invalid_request"},
			{"namespace alone", map[string]any{"name": "refs/heads/", "sha": sha}, http.StatusBadRequest, "invalid_request"},
			{"notes namespace", map[string]any{"name": "refs/notes/n", "sha": sha}, http.StatusBadRequest, "invalid_request"},
			{"remotes namespace", map[string]any{"name": "refs/remotes/o/m", "sha": sha}, http.StatusBadRequest, "invalid_request"},
			{"HEAD", map[string]any{"name": "HEAD", "sha": sha}, http.StatusBadRequest, "invalid_request"},
			{"both sha and ref", map[string]any{"name": "n", "sha": sha, "ref": "main"}, http.StatusBadRequest, "invalid_request"},
			{"neither sha nor ref", map[string]any{"name": "n"}, http.StatusBadRequest, "invalid_request"},
			{"abbreviated sha", map[string]any{"name": "n", "sha": sha[:7]}, http.StatusBadRequest, "invalid_request"},
			{"sha not hexadecimal", map[string]any{"name": "n", "sha": strings.Repeat("g", 40)}, http.StatusBadRequest, "invalid_request"},
			{"sha naming no object", map[string]any{"name": "n", "sha": strings.Repeat("1", 40)}, http.StatusNotFound, "ref_not_found"},
			{"sha naming a tree", map[string]any{"name": "n", "sha": tree}, http.StatusNotFound, "ref_not_found"},
			{"ref naming nothing", map[string]any{"name": "n", "ref": "nope"}, http.StatusNotFound, "ref_not_found"},
			{"existing branch by bare name", map[string]any{"name": "main", "sha": w.m1}, http.StatusConflict, "ref_exists"},
			{"existing branch by full name", map[string]any{"name": "refs/heads/main", "sha": w.m1}, http.StatusConflict, "ref_exists"},
			{"hub-owned ref", map[string]any{"name": "refs/parley/changes/1/1", "sha": sha}, http.StatusForbidden, "hub_owned_ref"},
		} {
			refs, seq := gitrepotest.Git(t, w.dir, "for-each-ref"), w.lastSeq(t)
			r := w.sendJSON(t, http.MethodPost, path, w.ownerKey, tc.body)
			if r.status != tc.status || r.errorType() != tc.errorType {
				t.Errorf("%s: %d %s, want %d %s", tc.name, r.status, r.body, tc.status, tc.errorType)
			}
			w.checkObserved(t, tc.name, "ref_create", r)
			if after := gitrepotest.Git(t, w.dir, "for-each-ref"); after != refs {
				t.Errorf("%s: refs are\n%s\nwant them unchanged\n%s", tc.name, after, refs)
			}
			if types, _ := w.eventsAfter(t, seq); len(types) != 0 {
				t.Errorf("%s: events %q, want none", tc.name, types)
			}
		}
	})
}

// TestRefMove checks PATCH …/scm/refs/*name: a ref at expected_sha moves to
// sha by compare-and-swap through the ref writer, and a move that names no
// ref, no commit or a stale expected_sha leaves it where it is.
func TestRefMove(t *testing.T) {
	t.Parallel()
	w := newRefsWorld(t)
	w.stage.versions = map[string]*pushrule.Version{"refs/heads/work": {WorkspaceSlug: "refs", Number: 1, Version: 2}}
	sibling := commit(t, w.dir, w.m1, "side.txt", "side")
	child := commit(t, w.dir, w.m2, "child.txt", "child")
	// reset puts work at m2 and the tag v1 at m1, as each row expects them.
	reset := func(t *testing.T) {
		gitrepotest.Git(t, w.dir, "update-ref", "refs/heads/work", w.m2)
		gitrepotest.Git(t, w.dir, "update-ref", "refs/tags/v1", w.m1)
		w.stage.reset()
	}

	t.Run("moves the ref by compare-and-swap", func(t *testing.T) {
		for _, address := range []string{"work", "refs/heads/work"} {
			for _, tc := range []struct {
				name, ref, path, from, to string
				forced                    bool
			}{
				{"to a descendant", "refs/heads/work", address, w.m2, child, false},
				{"to its parent", "refs/heads/work", address, w.m2, w.m1, true},
				{"to a sibling", "refs/heads/work", address, w.m2, sibling, true},
				{"a tag", "refs/tags/v1", "refs/tags/v1", w.m1, w.m2, false},
				{"to its own commit", "refs/heads/work", address, w.m2, w.m2, false},
			} {
				name := address + ": " + tc.name
				reset(t)
				seq := w.lastSeq(t)
				r := w.sendJSON(t, http.MethodPatch, scmPath("refs", "/refs/"+tc.path), w.ownerKey,
					map[string]any{"sha": tc.to, "expected_sha": tc.from})
				if r.status != http.StatusOK || r.json["sha"] != tc.to || r.json["name"] != tc.ref {
					t.Errorf("%s: %d %s, want 200 with %s at %s", name, r.status, r.body, tc.ref, tc.to)
				}
				w.checkObserved(t, name, "ref_move", r)
				if got := w.head(t, tc.ref); got != tc.to {
					t.Errorf("%s: %s is at %s, want %s", name, tc.ref, got, tc.to)
				}
				_, pushes := w.eventsAfter(t, seq)
				seen := w.stage.reset()
				if tc.from == tc.to {
					if len(pushes) != 0 || len(seen) != 0 {
						t.Errorf("%s: push events %+v and stage calls %d, want none", name, pushes, len(seen))
					}
					continue
				}
				if len(pushes) != 1 || pushes[0].Ref != tc.ref || pushes[0].Before == nil || *pushes[0].Before != tc.from ||
					pushes[0].After == nil || *pushes[0].After != tc.to || pushes[0].Forced != tc.forced ||
					pushes[0].Created || pushes[0].Deleted || pushes[0].Via != pushrule.ViaAPI {
					t.Errorf("%s: push events %+v, want one moving %s from %s to %s, forced %v, via api", name, pushes, tc.ref, tc.from, tc.to, tc.forced)
				}
				if len(seen) != 1 || seen[0].Via != pushrule.ViaAPI || seen[0].VersionKind != pushrule.VersionKindRef ||
					len(seen[0].Updates) != 1 || seen[0].Updates[0].Ref != tc.ref {
					t.Errorf("%s: the stage saw %+v, want one call for %s via api with version kind ref", name, seen, tc.ref)
				}
			}
		}
	})

	t.Run("refuses a move and leaves the ref where it is", func(t *testing.T) {
		for _, tc := range []struct {
			name, path string
			body       map[string]any
			status     int
			errorType  string
		}{
			{"expected_sha differs", "work", map[string]any{"sha": child, "expected_sha": w.m1}, http.StatusConflict, "ref_moved"},
			{"no such ref", "nope", map[string]any{"sha": child, "expected_sha": w.m2}, http.StatusNotFound, "ref_not_found"},
			{"no expected_sha", "work", map[string]any{"sha": child}, http.StatusBadRequest, "invalid_request"},
			{"no sha", "work", map[string]any{"expected_sha": w.m2}, http.StatusBadRequest, "invalid_request"},
			{"abbreviated sha", "work", map[string]any{"sha": child[:7], "expected_sha": w.m2}, http.StatusBadRequest, "invalid_request"},
			{"sha naming no commit", "work", map[string]any{"sha": strings.Repeat("1", 40), "expected_sha": w.m2}, http.StatusNotFound, "ref_not_found"},
			{"notes namespace", "refs/notes/n", map[string]any{"sha": child, "expected_sha": w.m2}, http.StatusBadRequest, "invalid_request"},
		} {
			reset(t)
			seq := w.lastSeq(t)
			r := w.sendJSON(t, http.MethodPatch, scmPath("refs", "/refs/"+tc.path), w.ownerKey, tc.body)
			if r.status != tc.status || r.errorType() != tc.errorType {
				t.Errorf("%s: %d %s, want %d %s", tc.name, r.status, r.body, tc.status, tc.errorType)
			}
			w.checkObserved(t, tc.name, "ref_move", r)
			if tc.errorType == "ref_moved" {
				e, _ := r.json["error"].(map[string]any)
				if details, _ := e["details"].(map[string]any); details["current_sha"] != w.m2 {
					t.Errorf("%s: error %v, want details.current_sha %s", tc.name, e, w.m2)
				}
			}
			if got := w.head(t, "refs/heads/work"); got != w.m2 {
				t.Errorf("%s: work is at %s, want it left at %s", tc.name, got, w.m2)
			}
			if types, _ := w.eventsAfter(t, seq); len(types) != 0 {
				t.Errorf("%s: events %q, want none", tc.name, types)
			}
		}
	})

	t.Run("lets one of two concurrent moves from one commit through", func(t *testing.T) {
		reset(t)
		var wg sync.WaitGroup
		replies := make([]reply, 2)
		for i, to := range []string{child, sibling} {
			wg.Go(func() {
				replies[i] = w.sendJSON(t, http.MethodPatch, scmPath("refs", "/refs/work"), w.ownerKey,
					map[string]any{"sha": to, "expected_sha": w.m2})
			})
		}
		wg.Wait()
		w.observed.reset()
		statuses := []int{replies[0].status, replies[1].status}
		slices.Sort(statuses)
		winner, loser := replies[0], replies[1]
		if replies[1].status == http.StatusOK {
			winner, loser = replies[1], replies[0]
		}
		if !slices.Equal(statuses, []int{http.StatusOK, http.StatusConflict}) || loser.errorType() != "ref_moved" {
			t.Fatalf("answers %d %s and %d %s, want one 200 and one 409 ref_moved",
				replies[0].status, replies[0].body, replies[1].status, replies[1].body)
		}
		if got := w.head(t, "refs/heads/work"); got != winner.json["sha"] {
			t.Errorf("work is at %s, want the winner's %v", got, winner.json["sha"])
		}
	})
}

// TestRefDelete checks DELETE …/scm/refs/*name: an unprotected branch that
// is no change's source, or a tag, is deleted by compare-and-swap through
// the ref writer, its commits kept, and any other delete keeps the ref.
func TestRefDelete(t *testing.T) {
	t.Parallel()
	w := newRefsWorld(t)

	t.Run("deletes the ref by compare-and-swap", func(t *testing.T) {
		for _, ref := range []string{"refs/heads/tmp", "refs/heads/tmp2"} {
			gitrepotest.Git(t, w.dir, "update-ref", ref, w.m2)
		}
		gitrepotest.Git(t, w.dir, "update-ref", "refs/tags/v1", w.m1)
		for _, tc := range []struct{ path, ref, before string }{
			{"/refs/tmp", "refs/heads/tmp", w.m2},
			{"/refs/refs/heads/tmp2?expected_sha=" + w.m2, "refs/heads/tmp2", w.m2},
			{"/refs/refs/tags/v1", "refs/tags/v1", w.m1},
		} {
			seq := w.lastSeq(t)
			r := w.send(t, http.MethodDelete, scmPath("refs", tc.path), w.ownerKey)
			if r.status != http.StatusNoContent || len(r.body) != 0 {
				t.Errorf("DELETE %s: %d %q, want 204 with no body", tc.path, r.status, r.body)
			}
			w.checkObserved(t, tc.path, "ref_delete", r)
			if _, err := gitrepotest.Run(w.dir, "", "rev-parse", "--verify", "--quiet", tc.ref); err == nil {
				t.Errorf("DELETE %s: %s still exists", tc.path, tc.ref)
			}
			_, pushes := w.eventsAfter(t, seq)
			if len(pushes) != 1 || pushes[0].Ref != tc.ref || pushes[0].Before == nil || *pushes[0].Before != tc.before ||
				pushes[0].After != nil || !pushes[0].Deleted || pushes[0].Forced || pushes[0].Via != pushrule.ViaAPI {
				t.Errorf("DELETE %s: push events %+v, want one deleting %s from %s via api", tc.path, pushes, tc.ref, tc.before)
			}
			if _, err := gitrepotest.Run(w.dir, "", "cat-file", "-e", tc.before); err != nil {
				t.Errorf("DELETE %s: the commit %s is no longer readable: %v", tc.path, tc.before, err)
			}
		}
	})

	t.Run("refuses a delete and keeps the ref", func(t *testing.T) {
		for _, tc := range []struct {
			name, path string
			change     *scm.LinkedChange
			settings   string
			status     int
			errorType  string
			message    string
		}{
			{name: "expected_sha differs", path: "/refs/work?expected_sha=" + w.m1, status: http.StatusConflict, errorType: "ref_moved"},
			{name: "source of a draft change", path: "/refs/work", status: http.StatusConflict, errorType: "ref_in_use",
				change: &scm.LinkedChange{WorkspaceSlug: "refs", Number: 3, Version: 1, State: scm.ChangeDraft}},
			{name: "source of an open change", path: "/refs/refs/heads/work", status: http.StatusConflict, errorType: "ref_in_use",
				change: &scm.LinkedChange{WorkspaceSlug: "refs", Number: 4, Version: 2, State: scm.ChangeOpen}},
			{name: "no such ref", path: "/refs/nope", status: http.StatusNotFound, errorType: "ref_not_found"},
			{name: "abbreviated expected_sha", path: "/refs/work?expected_sha=" + w.m2[:7], status: http.StatusBadRequest, errorType: "invalid_request"},
			{name: "the unprotected default branch", path: "/refs/main", settings: `{"protected_branches":[]}`,
				status: http.StatusForbidden, errorType: "default_branch", message: "main is the default branch and cannot be deleted"},
		} {
			gitrepotest.Git(t, w.dir, "update-ref", "refs/heads/work", w.m2)
			w.linker.answer("work", tc.change)
			if tc.settings != "" {
				w.set(t, "refs", "settings", tc.settings)
			}
			refs, seq := gitrepotest.Git(t, w.dir, "for-each-ref"), w.lastSeq(t)
			r := w.send(t, http.MethodDelete, scmPath("refs", tc.path), w.ownerKey)
			e, _ := r.json["error"].(map[string]any)
			if r.status != tc.status || r.errorType() != tc.errorType || tc.message != "" && e["message"] != tc.message {
				t.Errorf("%s: %d %s, want %d %s %q", tc.name, r.status, r.body, tc.status, tc.errorType, tc.message)
			}
			w.checkObserved(t, tc.name, "ref_delete", r)
			if tc.errorType == "ref_moved" {
				if details, _ := e["details"].(map[string]any); details["current_sha"] != w.m2 {
					t.Errorf("%s: error %v, want details.current_sha %s", tc.name, e, w.m2)
				}
			}
			if after := gitrepotest.Git(t, w.dir, "for-each-ref"); after != refs {
				t.Errorf("%s: refs are\n%s\nwant them unchanged\n%s", tc.name, after, refs)
			}
			if types, _ := w.eventsAfter(t, seq); len(types) != 0 {
				t.Errorf("%s: events %q, want none", tc.name, types)
			}
		}
	})

	t.Run("deletes the source of a landed change", func(t *testing.T) {
		gitrepotest.Git(t, w.dir, "update-ref", "refs/heads/work", w.m2)
		w.linker.answer("work", &scm.LinkedChange{WorkspaceSlug: "refs", Number: 5, Version: 1, State: "landed"})
		if r := w.send(t, http.MethodDelete, scmPath("refs", "/refs/work"), w.ownerKey); r.status != http.StatusNoContent {
			t.Errorf("%d %s, want 204", r.status, r.body)
		}
		w.observed.reset()
		if got := w.head(t, "refs/heads/work"); got != "" {
			t.Errorf("work is at %s, want it deleted", got)
		}
	})
}

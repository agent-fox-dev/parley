package change_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/gitserver/pushrule"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/identity"
)

// anySession is a session-linking rule of the source control API that
// accepts every session.
type anySession struct{}

func (anySession) Check(_ context.Context, _ identity.Principal, id, _ string) (string, error) {
	return strings.ToLower(id), nil
}

// newSCMWorld is a world whose source control API accepts every session,
// so that the change component's session linker is the one that judges a
// write's session when its version is recorded.
func newSCMWorld(t *testing.T) *world {
	t.Helper()
	return newWorldWith(t, hubtest.Config{Compose: func(c *hub.Composition) { c.SCM().RegisterSessionLinker(anySession{}) }})
}

// latest reads change number of demo with its versions and fails the test
// unless it is at version want, at head, the latest version of kind;
// it returns that version.
func (w *world) latest(t *testing.T, number int64, want int, head, kind string) map[string]any {
	t.Helper()
	_, ch, _ := w.get(t, "/workspaces/demo/changes/"+strconv.FormatInt(number, 10)+"?include=versions", w.ownerKey)
	vs, _ := ch["versions"].([]any)
	if len(vs) != want || ch["version"] != float64(want) || ch["head_sha"] != head {
		t.Fatalf("change %d = version %v at %v with %d versions, want version %d at %s", number, ch["version"], ch["head_sha"], len(vs), want, head)
	}
	v, _ := vs[want-1].(map[string]any)
	if v["kind"] != kind || v["head_sha"] != head {
		t.Fatalf("version %d = %v, want kind %s at %s", want, v, kind, head)
	}
	return v
}

// named is the change member of a write's answer.
func named(out map[string]any) any { return out["change"] }

func TestRefUpdatedStage_RecordsTheVersionOfEveryWriteThatMovesTheSource(t *testing.T) {
	t.Parallel()
	w := newSCMWorld(t)
	w.branch(t, "feat", "Feat\n\nWhy.\n")
	w.openBranch(t, "feat", nil)
	clone := w.clone(t, "demo")
	clientGit(t, clone, "checkout", "--quiet", "feat")
	heads := func() map[string]string { return gitrepotest.Refs(t, w.demo, "refs/heads/") }
	var head string

	t.Run("a git push", func(t *testing.T) {
		clientGit(t, clone, "commit", "--quiet", "--allow-empty", "-m", "Pushed")
		clientGit(t, clone, "push", "--quiet", "origin", "feat")
		head = clientGit(t, clone, "rev-parse", "HEAD")
		w.latest(t, 1, 2, head, change.KindPush)
	})
	t.Run("an API commit", func(t *testing.T) {
		status, out := w.send(t, http.MethodPost, "/workspaces/demo/scm/commits", w.ownerKey, map[string]any{
			"branch": "feat", "expected_head_sha": head, "message": "Committed\n",
			"operations": []any{map[string]any{"op": "put", "path": "c.txt", "content": "c\n"}}})
		if status != http.StatusCreated {
			t.Fatalf("POST commits = %d %v, want 201", status, out)
		}
		head = out["commit"].(map[string]any)["sha"].(string)
		w.latest(t, 1, 3, head, change.KindCommit)
		if want := map[string]any{"workspace_slug": "demo", "number": 1.0, "version": 3.0}; !reflect.DeepEqual(named(out), want) {
			t.Errorf("the commit's change = %v, want %v", named(out), want)
		}
	})
	t.Run("a ref move", func(t *testing.T) {
		spare := commit(t, w.demo, "spare", w.base, "Spare\n", map[string]string{"s.txt": "s\n"})
		status, out := w.send(t, http.MethodPatch, "/workspaces/demo/scm/refs/feat", w.ownerKey,
			map[string]any{"sha": spare, "expected_sha": head})
		if status != http.StatusOK {
			t.Fatalf("PATCH refs/feat = %d %v, want 200", status, out)
		}
		head = spare
		w.latest(t, 1, 4, head, change.KindRef)
		if want := map[string]any{"workspace_slug": "demo", "number": 1.0}; !reflect.DeepEqual(named(out), want) {
			t.Errorf("the ref's change = %v, want %v", named(out), want)
		}
	})
	t.Run("a rebase", func(t *testing.T) {
		commit(t, w.demo, "main", w.base, "Main moves\n", map[string]string{"m.txt": "m\n"})
		status, out := w.send(t, http.MethodPost, "/workspaces/demo/scm/rebase", w.ownerKey, map[string]any{"branch": "feat", "onto": "main"})
		if status != http.StatusOK {
			t.Fatalf("POST rebase = %d %v, want 200", status, out)
		}
		head = out["new_head_sha"].(string)
		if v := w.latest(t, 1, 5, head, change.KindRebase); v["patch_equivalent_to"] != 4.0 {
			t.Errorf("patch_equivalent_to = %v, want 4", v["patch_equivalent_to"])
		}
		if want := map[string]any{"workspace_slug": "demo", "number": 1.0, "version": 5.0}; !reflect.DeepEqual(named(out), want) {
			t.Errorf("the rebase's change = %v, want %v", named(out), want)
		}
	})
	t.Run("the reads name the change", func(t *testing.T) {
		_, ref := w.send(t, http.MethodGet, "/workspaces/demo/scm/refs/feat", w.ownerKey, nil)
		if want := map[string]any{"workspace_slug": "demo", "number": 1.0}; !reflect.DeepEqual(named(ref), want) {
			t.Errorf("the ref object's change = %v, want %v", named(ref), want)
		}
		_, cm := w.send(t, http.MethodGet, "/workspaces/demo/scm/commits/"+head, w.ownerKey, nil)
		if want := map[string]any{"workspace_slug": "demo", "number": 1.0, "version": 5.0}; !reflect.DeepEqual(named(cm), want) {
			t.Errorf("the commit object's change = %v, want %v", named(cm), want)
		}
	})
	t.Run("a session the change component refuses", func(t *testing.T) {
		status, out := w.send(t, http.MethodPost, "/workspaces/demo/scm/commits", w.ownerKey, map[string]any{
			"branch": "feat", "expected_head_sha": head, "message": "Refused\n", "session_id": sessionOther,
			"operations": []any{map[string]any{"op": "put", "path": "r.txt", "content": "r\n"}}})
		if status != http.StatusBadRequest || errorType(out) != "unknown_session" {
			t.Errorf("POST commits = %d %v, want 400 unknown_session", status, out)
		}
		if got := heads()["feat"]; got != head {
			t.Errorf("feat moved to %s, want it at %s", got, head)
		}
		w.latest(t, 1, 5, head, change.KindRebase)
	})
	t.Run("a push that deletes another branch and moves the source", func(t *testing.T) {
		gitrepotest.Git(t, w.demo, "update-ref", "refs/heads/gone", w.base)
		gitrepotest.Git(t, w.demo, "pack-refs", "--all")
		clientGit(t, clone, "fetch", "--quiet", "origin")
		clientGit(t, clone, "reset", "--quiet", "--hard", "origin/feat")
		clientGit(t, clone, "commit", "--quiet", "--allow-empty", "-m", "Pushed again")
		clientGit(t, clone, "push", "--quiet", "origin", "feat", ":gone")
		head = clientGit(t, clone, "rev-parse", "HEAD")
		w.latest(t, 1, 6, head, change.KindPush)
		if _, found := heads()["gone"]; found {
			t.Error("gone is still there")
		}
	})
	t.Run("deleting the source", func(t *testing.T) {
		seq := w.seq(t)
		clientGit(t, clone, "push", "--quiet", "origin", "--delete", "feat")
		if ch := w.read(t, 1); ch.State != change.StateAbandoned || ch.Version != 6 {
			t.Errorf("change 1 = %s at version %d, want abandoned at version 6", ch.State, ch.Version)
		}
		var reasons []any
		for _, e := range w.eventsAfter(t, seq) {
			if e.Type == change.EventAbandon {
				reasons = append(reasons, metadata(t, e)["reason"])
			}
		}
		if want := []any{change.ReasonSourceDeleted}; !reflect.DeepEqual(reasons, want) {
			t.Errorf("abandon reasons = %v, want %v", reasons, want)
		}
	})
}

func TestChangeLinker_ChecksAndOpensTheChangeACommitAsksFor(t *testing.T) {
	t.Parallel()
	w := newSCMWorld(t)
	commitTo := func(branch, message string, ch map[string]any) (int, map[string]any) {
		return w.send(t, http.MethodPost, "/workspaces/demo/scm/commits", w.ownerKey, map[string]any{
			"branch": branch, "base_ref": "main", "message": message, "change": ch,
			"operations": []any{map[string]any{"op": "put", "path": branch + ".txt", "content": branch + "\n"}}})
	}

	t.Run("a change object the change component accepts", func(t *testing.T) {
		status, out := commitTo("topic", "Topic\n", map[string]any{"title": "The topic", "intent": "Because."})
		if status != http.StatusCreated {
			t.Fatalf("POST commits = %d %v, want 201", status, out)
		}
		sha := out["commit"].(map[string]any)["sha"].(string)
		if want := map[string]any{"workspace_slug": "demo", "number": 1.0, "version": 1.0}; !reflect.DeepEqual(named(out), want) {
			t.Errorf("the commit's change = %v, want %v", named(out), want)
		}
		w.latest(t, 1, 1, sha, change.KindCommit)
		if ch := w.read(t, 1); ch.Title != "The topic" || ch.Intent != "Because." || ch.Source.Branch != "topic" {
			t.Errorf("change 1 = %q %q from %s, want the commit's title, intent and branch", ch.Title, ch.Intent, ch.Source.Branch)
		}
	})
	t.Run("a change object the change component refuses", func(t *testing.T) {
		status, out := commitTo("bare", "Bare\n", map[string]any{})
		if status != http.StatusBadRequest || errorType(out) != "intent_required" {
			t.Errorf("POST commits = %d %v, want 400 intent_required", status, out)
		}
		if _, found := gitrepotest.Refs(t, w.demo, "refs/heads/")["bare"]; found {
			t.Error("the refused commit created its branch")
		}
		if n := w.rows(t)["changes"]; n != 1 {
			t.Errorf("%d changes, want 1", n)
		}
	})
	t.Run("the source of an open change is in use", func(t *testing.T) {
		status, out := w.send(t, http.MethodDelete, "/workspaces/demo/scm/refs/topic", w.ownerKey, nil)
		if status != http.StatusConflict || errorType(out) != "ref_in_use" {
			t.Errorf("DELETE refs/topic = %d %v, want 409 ref_in_use", status, out)
		}
	})
}

// activityFunc is a fork activity recorder of the ref writer as a function.
type activityFunc func(ctx context.Context, slug string) error

func (f activityFunc) RecordActivity(ctx context.Context, slug string) error { return f(ctx, slug) }

// sendFromHub sends a request as send does, from a goroutine of the hub's,
// which may not fail the test: it returns the status and the answer, or
// the error as the answer.
func (w *world) sendFromHub(method, path string, body any) (int, string) {
	var data io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, err.Error()
		}
		data = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, w.URL("/api/v1"+path), data)
	if err != nil {
		return 0, err.Error()
	}
	req.Header.Set("Authorization", "Bearer "+w.ownerKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer res.Body.Close()
	answer, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(answer)
}

// A write that moves or deletes the branch a commit moved, after the
// commit's refs moved and before the change the commit asks for is opened,
// finds no change in its ref-updated stage: the open reads the branch in
// its own transaction, and records the move as the next version of kind
// ref by the hub, or abandons the change whose source is gone.
func TestChangeLinker_RecordsAWriteToTheSourceBetweenTheCommitAndTheOpen(t *testing.T) {
	t.Parallel()
	var between atomic.Pointer[func()]
	w := newWorldWith(t, hubtest.Config{Compose: func(c *hub.Composition) {
		c.SCM().RegisterSessionLinker(anySession{})
		forks := c.Forks()
		// The ref writer tells the fork activity recorder of a write to a
		// fork once its refs moved and before the write goes on, which is
		// the gap between a commit and the open of its change.
		c.RefWriter().RegisterForkActivity(activityFunc(func(ctx context.Context, slug string) error {
			if f := between.Swap(nil); f != nil {
				(*f)()
			}
			return forks.RecordActivity(ctx, slug)
		}))
	}})
	m1 := w.addFork(t, "demo--m1", "main", nil, nil)
	// inGap has write run once in the gap of the next write to the fork,
	// given the head of branch; the channel gives what it answered.
	inGap := func(branch string, write func(head string) (int, string)) <-chan string {
		done := make(chan string, 1)
		f := func() {
			out, err := exec.Command("git", "--git-dir", m1, "rev-parse", "refs/heads/"+branch).Output()
			if err != nil {
				done <- err.Error()
				return
			}
			status, answer := write(strings.TrimSpace(string(out)))
			done <- fmt.Sprintf("%d %s", status, answer)
		}
		between.Store(&f)
		return done
	}
	// commitTo commits to the new branch, asking for a change, which is
	// change number of demo, and returns the commit.
	commitTo := func(t *testing.T, branch string, number int64) string {
		t.Helper()
		status, out := w.send(t, http.MethodPost, "/workspaces/demo--m1/scm/commits", w.ownerKey, map[string]any{
			"branch": branch, "base_ref": w.base, "message": "Topic\n", "change": map[string]any{"title": "The topic", "intent": "Because."},
			"operations": []any{map[string]any{"op": "put", "path": branch + ".txt", "content": branch + "\n"}}})
		if status != http.StatusCreated {
			t.Fatalf("POST commits = %d %v, want 201", status, out)
		}
		if ch, _ := named(out).(map[string]any); ch["number"] != float64(number) {
			t.Fatalf("the commit's change = %v, want change %d", named(out), number)
		}
		return out["commit"].(map[string]any)["sha"].(string)
	}

	t.Run("a commit moves the branch", func(t *testing.T) {
		gap := inGap("topic", func(head string) (int, string) {
			return w.sendFromHub(http.MethodPost, "/workspaces/demo--m1/scm/commits", map[string]any{
				"branch": "topic", "expected_head_sha": head, "message": "Later\n",
				"operations": []any{map[string]any{"op": "put", "path": "later.txt", "content": "later\n"}}})
		})
		first := commitTo(t, "topic", 1)
		if answer := <-gap; !strings.HasPrefix(answer, "201 ") {
			t.Fatalf("the commit in the gap = %s, want 201", answer)
		}
		later := gitrepotest.Refs(t, m1, "refs/heads/")["topic"]
		if later == first {
			t.Fatal("the commit in the gap left topic at the first commit")
		}
		w.latest(t, 1, 2, later, change.KindRef)
		if v1 := w.versions(t, "demo", 1)[0]; v1["head_sha"] != first || v1["kind"] != change.KindCommit {
			t.Errorf("version 1 = %v, want kind commit at %s", v1, first)
		}
	})
	t.Run("a write deletes the branch", func(t *testing.T) {
		gap := inGap("gone", func(head string) (int, string) {
			return w.sendFromHub(http.MethodDelete, "/workspaces/demo--m1/scm/refs/gone?expected_sha="+head, nil)
		})
		commitTo(t, "gone", 2)
		if answer := <-gap; !strings.HasPrefix(answer, "204 ") {
			t.Fatalf("the delete in the gap = %s, want 204", answer)
		}
		if _, found := gitrepotest.Refs(t, m1, "refs/heads/")["gone"]; found {
			t.Fatal("the delete in the gap left the branch")
		}
		if ch := w.read(t, 2); ch.State != change.StateAbandoned || ch.Version != 1 {
			t.Errorf("change 2 = %s at version %d, want abandoned at version 1", ch.State, ch.Version)
		}
	})
}

// Each update moves feat, the source of change 1, to h2; only the last
// row is one the stage records a version for.
func TestRefUpdated_RecordsAVersionOnlyForAnAllowedMoveOfABranchThatNamesAKind(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	h1 := w.branch(t, "feat", "Feat\n\nWhy.\n")
	w.openBranch(t, "feat", nil)
	h2 := commit(t, w.demo, "feat", h1, "Two\n", map[string]string{"two.txt": "2\n"})
	caller := pushrule.Caller{Level: access.LevelOwner, CredentialType: change.CredentialAPIKey, CredentialID: "k1", UserID: w.owner}
	move := func(ref string, direct, allowed bool) []pushrule.Decision {
		return []pushrule.Decision{{Update: pushrule.Update{Ref: ref, Before: h1, After: h2}, Allowed: allowed, Direct: direct}}
	}
	for _, c := range []struct {
		name, via, kind string
		updates         []pushrule.Decision
		want            *pushrule.Version
	}{
		{"a direct push", pushrule.ViaGit, pushrule.VersionKindPush, move("refs/heads/feat", true, true), nil},
		{"a refused update", pushrule.ViaGit, pushrule.VersionKindPush, move("refs/heads/feat", false, false), nil},
		{"a land", pushrule.ViaLand, pushrule.VersionKindNone, move("refs/heads/feat", false, true), nil},
		{"a write that names no kind", pushrule.ViaAPI, pushrule.VersionKindNone, move("refs/heads/feat", false, true), nil},
		{"a tag", pushrule.ViaGit, pushrule.VersionKindPush, move("refs/tags/feat", false, true), nil},
		{"a push of the source", pushrule.ViaGit, pushrule.VersionKindPush, move("refs/heads/feat", false, true),
			&pushrule.Version{WorkspaceSlug: "demo", Number: 1, Version: 2}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var out []*pushrule.Version
			w.write(t, func(ctx context.Context, tx *sql.Tx) error {
				var err error
				out, err = w.changes.RefUpdated(ctx, tx, pushrule.StageInput{Workspace: pushrule.Workspace{Slug: "demo", Kind: access.KindRepository},
					Caller: caller, Via: c.via, VersionKind: c.kind, Updates: c.updates})
				return err
			})
			// A land is answered before any update is looked at.
			var want []*pushrule.Version
			if c.via != pushrule.ViaLand {
				want = []*pushrule.Version{c.want}
			}
			if !reflect.DeepEqual(out, want) {
				t.Errorf("RefUpdated = %v, want %v", out, want)
			}
			wantVersion := int64(1)
			if c.want != nil {
				wantVersion = 2
			}
			if ch := w.read(t, 1); ch.Version != wantVersion {
				t.Errorf("change 1 is at version %d, want %d", ch.Version, wantVersion)
			}
		})
	}
}

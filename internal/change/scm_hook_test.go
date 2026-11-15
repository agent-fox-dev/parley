package change_test

import (
	"context"
	"database/sql"
	"net/http"
	"reflect"
	"strconv"
	"strings"
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

package scm_test

import (
	"fmt"
	"maps"
	"net/http"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/gitserver/pushrule"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/scm"
)

// The messages of the protected-branch rule for main.
const (
	msgMainViaChange = "main is protected; push another branch and open a change (git push -o parley.change)"
	msgMainRewrite   = "main is protected and cannot be force-pushed or deleted"
)

// TestDirectPush checks how the writes meet a protected branch with the
// hub's own push rule engine: the owner and the admin token move it forward
// as a direct push, which opens and asks for no change, and nobody creates,
// rewinds, rewrites or deletes it. Each behaviour has a workspace of its
// own, made by refsStream, whose main, m1 <- m2, is protected by default.
func TestDirectPush(t *testing.T) {
	t.Parallel()
	w := newWriteWorld(t, "")
	// The linker names an open change whose source is main, so that a write
	// that asked it would find one.
	w.linker.answer("main", &scm.LinkedChange{WorkspaceSlug: "any", Number: 7, Version: 2, State: scm.ChangeOpen})
	seed := func(t *testing.T, slug string) (dir, m1, m2 string) {
		t.Helper()
		dir = w.addWorkspace(t, slug, w.org, refsStream)
		ids := strings.Fields(gitrepotest.Git(t, dir, "rev-parse", "main~1", "main"))
		return dir, ids[0], ids[1]
	}
	pat := func(t *testing.T, spec identity.PATSpec) string {
		t.Helper()
		spec.UserID, spec.Permissions = w.owner, []string{"git:write"}
		return w.IssuePAT(t, spec)
	}
	main := func(t *testing.T, dir string) string {
		t.Helper()
		return gitrepotest.Refs(t, dir, "refs/heads/")["main"]
	}
	// noLinkerCalls checks that no write since the last check asked the
	// change linker anything.
	noLinkerCalls := func(t *testing.T, name string) {
		t.Helper()
		if asked, opened := w.linker.calls(); len(asked) != 0 || len(opened) != 0 {
			t.Errorf("%s: the change linker was asked about %q and to open %q, want nothing", name, asked, opened)
		}
	}
	n := 0
	commitTo := func(t *testing.T, slug, credential, head string, extra map[string]any) reply {
		t.Helper()
		n++
		body := map[string]any{"branch": "main", "expected_head_sha": head, "message": "direct",
			"operations": []any{put(fmt.Sprintf("docs/%d.md", n), "new\n")}}
		maps.Copy(body, extra)
		return w.sendJSON(t, http.MethodPost, scmPath(slug, "/commits"), credential, body)
	}
	checkRefused := func(t *testing.T, name string, r reply, message string) {
		t.Helper()
		e, _ := r.json["error"].(map[string]any)
		if r.status != http.StatusForbidden || r.errorType() != pushrule.ErrorTypeProtectedBranch || e["message"] != message {
			t.Errorf("%s: %d %s, want 403 protected_branch %q", name, r.status, r.body, message)
		}
	}

	t.Run("accepts a commit to the protected branch from an exempt caller as a direct push", func(t *testing.T) {
		dir, _, _ := seed(t, "direct")
		// A version a stage reported for main would still name no change.
		w.stage.mu.Lock()
		w.stage.versions = map[string]*pushrule.Version{"refs/heads/main": {WorkspaceSlug: "direct", Number: 7, Version: 3}}
		w.stage.mu.Unlock()
		t.Cleanup(func() { w.stage.mu.Lock(); w.stage.versions = nil; w.stage.mu.Unlock() })
		w.linker.calls()
		for _, caller := range []struct{ name, credential string }{
			{"the owner's API key", w.ownerKey},
			{"the admin token", w.Token},
			{"the owner's unbound PAT without paths", w.writePAT},
		} {
			head, seq := main(t, dir), w.lastSeq(t)
			r := commitTo(t, "direct", caller.credential, head, nil)
			if r.status != http.StatusCreated {
				t.Errorf("%s: %d %s, want 201", caller.name, r.status, r.body)
				continue
			}
			sha, _ := answerCommit(r)["sha"].(string)
			if got := main(t, dir); got != sha {
				t.Errorf("%s: main is at %s, want the new commit %s", caller.name, got, sha)
			}
			if _, pushes := w.eventsAfter(t, seq); len(pushes) != 1 || !pushes[0].Direct || pushes[0].Forced ||
				pushes[0].Via != pushrule.ViaAPI || pushes[0].After == nil || *pushes[0].After != sha {
				t.Errorf("%s: push events %+v, want one direct, not forced, to %s via api", caller.name, pushes, sha)
			}
			if c, ok := r.json["change"]; !ok || c != nil {
				t.Errorf("%s: change %v, want null", caller.name, c)
			}
			if c, ok := r.json["change_error"]; !ok || c != nil {
				t.Errorf("%s: change_error %v, want null", caller.name, c)
			}
			noLinkerCalls(t, caller.name)
			ws := w.get(t, "/workspaces/direct", w.Token)
			if ws.json["head_sha"] != sha {
				t.Errorf("%s: the workspace's head_sha is %v, want the new commit %s", caller.name, ws.json["head_sha"], sha)
			}
		}
	})

	t.Run("refuses a commit to the protected branch from a caller the exemption does not cover", func(t *testing.T) {
		dir, _, _ := seed(t, "covered")
		noPush, _, _ := seed(t, "noexempt")
		w.set(t, "noexempt", "settings", `{"protect.owner_push":false}`)
		for _, tc := range []struct {
			name, slug, dir, credential string
		}{
			{"the owner's PAT bound to the workspace", "covered", dir, pat(t, identity.PATSpec{Workspace: "covered"})},
			{"the owner's PAT with paths", "covered", dir, pat(t, identity.PATSpec{Paths: []string{"**"}})},
			{"the owner's API key without owner_push", "noexempt", noPush, w.ownerKey},
			{"the admin token without owner_push", "noexempt", noPush, w.Token},
		} {
			head, seq := main(t, tc.dir), w.lastSeq(t)
			r := commitTo(t, tc.slug, tc.credential, head, nil)
			checkRefused(t, tc.name, r, msgMainViaChange)
			if got := main(t, tc.dir); got != head {
				t.Errorf("%s: main moved from %s to %s", tc.name, head, got)
			}
			if types, _ := w.eventsAfter(t, seq); len(types) != 0 {
				t.Errorf("%s: events %q, want none", tc.name, types)
			}
		}
	})

	t.Run("refuses a change object on a protected branch before anything else", func(t *testing.T) {
		dir, m1, m2 := seed(t, "nochange")
		gitrepotest.Git(t, dir, "update-ref", "refs/heads/work", m2)
		bound := pat(t, identity.PATSpec{Workspace: "nochange"})
		change := map[string]any{"change": map[string]any{"target_branch": "release"}}
		w.linker.calls()
		for _, caller := range []struct{ name, credential string }{
			{"the owner's API key", w.ownerKey},
			{"the admin token", w.Token},
			{"the owner's bound PAT", bound},
		} {
			objects, refs := gitrepotest.Git(t, dir, "count-objects", "-v"), gitrepotest.Git(t, dir, "for-each-ref")
			// The stale expected_head_sha would be 409 ref_moved, and the
			// bound PAT's commit 403 protected_branch, were the parent or
			// the rules asked first.
			r := commitTo(t, "nochange", caller.credential, m1, change)
			if r.status != http.StatusBadRequest || r.errorType() != "invalid_request" {
				t.Errorf("%s: %d %s, want 400 invalid_request", caller.name, r.status, r.body)
			}
			if after := gitrepotest.Git(t, dir, "count-objects", "-v"); after != objects {
				t.Errorf("%s: the objects went from\n%s\nto\n%s", caller.name, objects, after)
			}
			if after := gitrepotest.Git(t, dir, "for-each-ref"); after != refs {
				t.Errorf("%s: the refs changed from\n%s\nto\n%s", caller.name, refs, after)
			}
			noLinkerCalls(t, caller.name)
		}
		// On a branch nothing protects the change object is no reason to
		// refuse: the parent is resolved, and the stale head is refused.
		r := w.sendJSON(t, http.MethodPost, scmPath("nochange", "/commits"), w.ownerKey, map[string]any{"branch": "work",
			"expected_head_sha": m1, "message": "m", "operations": []any{put("docs/a.md", "a\n")},
			"change": map[string]any{"target_branch": "main"}})
		if r.status != http.StatusConflict || r.errorType() != "ref_moved" {
			t.Errorf("a change object on work with a stale head: %d %s, want 409 ref_moved", r.status, r.body)
		}
	})

	t.Run("moves the protected branch forward by a ref move and by a rebase as a direct push", func(t *testing.T) {
		dir, _, m2 := seed(t, "forward")
		bound := pat(t, identity.PATSpec{Workspace: "forward"})
		c := commit(t, dir, m2, "work.txt", "work\n")
		gitrepotest.Git(t, dir, "update-ref", "refs/heads/work", c)
		d := commit(t, dir, c, "next.txt", "next\n")
		e := commit(t, dir, d, "last.txt", "last\n")
		w.stage.mu.Lock()
		w.stage.versions = map[string]*pushrule.Version{"refs/heads/main": {WorkspaceSlug: "forward", Number: 7, Version: 3}}
		w.stage.mu.Unlock()
		t.Cleanup(func() { w.stage.mu.Lock(); w.stage.versions = nil; w.stage.mu.Unlock() })
		w.linker.calls()
		checkDirect := func(t *testing.T, name string, seq int64, before, after string) {
			t.Helper()
			if _, pushes := w.eventsAfter(t, seq); len(pushes) != 1 || !pushes[0].Direct || pushes[0].Forced ||
				pushes[0].Before == nil || *pushes[0].Before != before || pushes[0].After == nil || *pushes[0].After != after {
				t.Errorf("%s: push events %+v, want one direct, not forced, from %s to %s", name, pushes, before, after)
			}
			noLinkerCalls(t, name)
		}

		seq := w.lastSeq(t)
		r := w.sendJSON(t, http.MethodPatch, scmPath("forward", "/refs/refs/heads/main"), w.ownerKey,
			map[string]any{"sha": c, "expected_sha": m2})
		if r.status != http.StatusOK || r.json["sha"] != c {
			t.Fatalf("PATCH main forward: %d %s, want 200 at %s", r.status, r.body, c)
		}
		checkDirect(t, "PATCH main forward", seq, m2, c)

		seq = w.lastSeq(t)
		r = w.sendJSON(t, http.MethodPost, scmPath("forward", "/rebase"), w.ownerKey, map[string]any{"branch": "main", "onto": d})
		if r.status != http.StatusOK || r.json["status"] != "rebased" || r.json["old_head_sha"] != c || r.json["new_head_sha"] != d {
			t.Fatalf("a rebase of main onto a descendant: %d %s, want 200 rebased from %s to %s", r.status, r.body, c, d)
		}
		if ch, ok := r.json["change"]; !ok || ch != nil {
			t.Errorf("a rebase of main onto a descendant: change %v, want null", ch)
		}
		checkDirect(t, "a rebase of main onto a descendant", seq, c, d)

		seq = w.lastSeq(t)
		r = w.sendJSON(t, http.MethodPatch, scmPath("forward", "/refs/main"), bound, map[string]any{"sha": e, "expected_sha": d})
		checkRefused(t, "PATCH main forward with the bound PAT", r, msgMainViaChange)
		r = w.sendJSON(t, http.MethodPost, scmPath("forward", "/rebase"), bound, map[string]any{"branch": "main", "onto": e})
		checkRefused(t, "a rebase of main with the bound PAT", r, msgMainViaChange)
		if got := main(t, dir); got != d {
			t.Errorf("main is at %s after the bound PAT's writes, want %s", got, d)
		}
		if types, _ := w.eventsAfter(t, seq); len(types) != 0 {
			t.Errorf("the bound PAT's writes emitted %q, want nothing", types)
		}
	})

	t.Run("refuses to create, rewind, rewrite or delete a protected branch for every caller", func(t *testing.T) {
		dir, m1, m2 := seed(t, "guarded")
		w.set(t, "guarded", "settings", `{"protected_branches":["main","release/*"]}`)
		gitrepotest.Git(t, dir, "update-ref", "refs/heads/release/0", m1)
		sibling := commit(t, dir, m1, "side.txt", "side\n")
		type request struct {
			name, method, path string
			body               map[string]any
			message            string
		}
		for _, rq := range []request{
			{"a creation of release/1", http.MethodPost, "/refs", map[string]any{"name": "release/1", "sha": m2},
				"release/1 is protected; push another branch and open a change (git push -o parley.change)"},
			{"a move of main to its parent", http.MethodPatch, "/refs/main", map[string]any{"sha": m1, "expected_sha": m2}, msgMainRewrite},
			{"a move of main to a sibling", http.MethodPatch, "/refs/main", map[string]any{"sha": sibling, "expected_sha": m2}, msgMainRewrite},
			{"a delete of release/0", http.MethodDelete, "/refs/refs/heads/release/0?expected_sha=" + m1, nil,
				"release/0 is protected and cannot be force-pushed or deleted"},
			{"a rebase of main onto a divergent commit", http.MethodPost, "/rebase", map[string]any{"branch": "main", "onto": sibling}, msgMainRewrite},
		} {
			for _, caller := range []struct{ name, credential string }{{"the owner's API key", w.ownerKey}, {"the admin token", w.Token}} {
				name := fmt.Sprintf("%s by %s", rq.name, caller.name)
				refs, seq := gitrepotest.Git(t, dir, "for-each-ref"), w.lastSeq(t)
				r := w.sendJSON(t, rq.method, scmPath("guarded", rq.path), caller.credential, rq.body)
				checkRefused(t, name, r, rq.message)
				if after := gitrepotest.Git(t, dir, "for-each-ref"); after != refs {
					t.Errorf("%s: the refs changed from\n%s\nto\n%s", name, refs, after)
				}
				if types, _ := w.eventsAfter(t, seq); len(types) != 0 {
					t.Errorf("%s: events %q, want none", name, types)
				}
			}
		}
	})
}

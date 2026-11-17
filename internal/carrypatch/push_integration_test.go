package carrypatch_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/carrypatch"
	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/gitserver"
	"github.com/agent-fox-dev/parley/internal/gitserver/pushrule"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// The pushes of a stock git client to a carry-patch workspace over the
// hub's git route: the push rules with the carry-patch controller the hub
// registers, the ref writer with its forwards to origin, the event log and
// the store, none of them stubbed.

func TestGitPush_HoldsTheIntegrationBranchHubOwnedRefsAndProtectedPatchBranches(t *testing.T) {
	t.Parallel()
	f := startPushFixture(t)
	tips := f.registerPatches(t, "patch/a")
	backup := tips["patch/a"]
	gitrepotest.SetRef(t, f.copy, "refs/parley/replaced/patch/a", backup)
	pushes := f.eventCount(t, "parley.git.push")

	t.Run("the integration branch with another branch, by the admin token", func(t *testing.T) {
		deploy := f.commit(t, f.base, "deploy")
		feature := f.commit(t, f.base, "feature")
		out, err := f.push(t, f.Token, deploy+":refs/heads/deploy", feature+":refs/heads/feature/x")
		wantPushRefused(t, out, err,
			"integration branch deploy is maintained by the hub; push a patch branch and rebuild instead",
			"not updated: another ref in this push was refused")
		if got := refIn(f.copy, "refs/heads/deploy"); got != f.base {
			t.Errorf("hub's deploy = %s after the refused push, want %s", got, f.base)
		}
		if got := refIn(f.copy, "refs/heads/feature/x"); got != "" {
			t.Errorf("hub's feature/x = %s after the refused push, want none", got)
		}
		if f.control.asked.Load() == 0 {
			t.Error("the controller was not asked about the integration branch")
		}

		out, err = f.push(t, f.Token, ":refs/heads/deploy")
		wantPushRefused(t, out, err, "integration branch deploy is maintained by the hub; push a patch branch and rebuild instead")
		if got := refIn(f.copy, "refs/heads/deploy"); got != f.base {
			t.Errorf("hub's deploy = %q after the refused delete, want %s", got, f.base)
		}
		if got := f.eventCount(t, "parley.git.push"); got != pushes {
			t.Errorf("%d parley.git.push events after the refused pushes, want %d", got, pushes)
		}
	})

	t.Run("a ref under refs/parley/replaced/, by the owner", func(t *testing.T) {
		next := f.commit(t, backup, "backup")
		for _, tc := range []struct{ name, refspec string }{
			{"update", next + ":refs/parley/replaced/patch/a"},
			{"create", next + ":refs/parley/replaced/new"},
			{"delete", ":refs/parley/replaced/patch/a"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				asked := f.control.asked.Load()
				out, err := f.push(t, f.key, tc.refspec)
				wantPushRefused(t, out, err, "refs/parley/ is maintained by the hub and cannot be pushed to")
				if got := refIn(f.copy, "refs/parley/replaced/patch/a"); got != backup {
					t.Errorf("hub's refs/parley/replaced/patch/a = %q, want %s", got, backup)
				}
				if got := refIn(f.copy, "refs/parley/replaced/new"); got != "" {
					t.Errorf("hub's refs/parley/replaced/new = %s, want none", got)
				}
				if n := f.control.asked.Load() - asked; n != 0 {
					t.Errorf("the controller was asked %d times, want 0", n)
				}
			})
		}
	})

	f.setSettings(t, map[string]any{"protected_branches": []string{"main", "patch/*"}})
	// Members only read a repository workspace; who writes it without the
	// owner exemption is the owner with a PAT bound to it.
	bound := f.IssuePAT(t, identity.PATSpec{UserID: f.owner, Workspace: f.slug, Permissions: []string{"git:read", "git:write"}})

	t.Run("a protected patch branch, without the owner exemption", func(t *testing.T) {
		asked := f.control.asked.Load()
		for _, tc := range []struct{ name, credential, refspec, want string }{
			{"a fast-forward with a bound PAT", bound, f.commit(t, backup, "ahead") + ":refs/heads/patch/a",
				"patch/a is protected; push another branch and open a change (git push -o parley.change)"},
			{"a forced update with a bound PAT", bound, "+" + f.commit(t, f.base, "rewritten") + ":refs/heads/patch/a",
				"patch/a is protected and cannot be force-pushed or deleted"},
			{"a delete with the owner's API key", f.key, ":refs/heads/patch/a",
				"patch/a is protected and cannot be force-pushed or deleted"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				out, err := f.push(t, tc.credential, tc.refspec)
				wantPushRefused(t, out, err, tc.want)
				if got := refIn(f.copy, "refs/heads/patch/a"); got != backup {
					t.Errorf("hub's patch/a = %q, want %s", got, backup)
				}
			})
		}
		if n := f.control.asked.Load() - asked; n != 0 {
			t.Errorf("the controller was asked %d times, want 0", n)
		}
	})

	t.Run("the owner fast-forwards a protected patch branch", func(t *testing.T) {
		first := f.commit(t, backup, "direct")
		if out, err := f.push(t, f.key, first+":refs/heads/patch/a"); err != nil {
			t.Fatalf("the owner's push from the hub's authority failed: %v\n%s", err, out)
		}
		if got := refIn(f.copy, "refs/heads/patch/a"); got != first {
			t.Errorf("hub's patch/a = %s, want the pushed %s", got, first)
		}
		evs := f.rebuildEvents(t, f.slug, "parley.git.push")
		if len(evs) == 0 || evs[len(evs)-1].Metadata["direct"] != true {
			t.Errorf("parley.git.push events = %+v, want the last one direct", evs)
		}

		f.setSettings(t, map[string]any{"patch_branch_source": "origin"})
		out, err := f.push(t, f.key, f.commit(t, first, "again")+":refs/heads/patch/a")
		wantPushRefused(t, out, err, "branch is synced from origin; push to "+f.origin.URL()+" instead")
		if got := refIn(f.copy, "refs/heads/patch/a"); got != first {
			t.Errorf("hub's patch/a = %s after the refused push, want %s", got, first)
		}
	})
}

func TestGitPush_ForwardsPatchBranchesToOriginAndRefusesThePushWhenOriginRefusesOne(t *testing.T) {
	t.Parallel()
	f := startPushFixture(t, func(w *workspace.Workspace) {
		w.Settings = map[string]json.RawMessage{
			"patch_branch_source":    json.RawMessage(`"origin"`),
			"push_patches_to_origin": json.RawMessage(`true`),
		}
	})
	if got := f.comp.ForwardLimit(); got != 120*time.Second {
		t.Errorf("the hub's carry-patch component forwards within %v, want 2m0s", got)
	}
	f.secrets.set(workspace.SecretGitPAT, "tok-12345")
	tips := f.registerPatches(t, "patch/a", "patch/b")
	f.origin.RejectRef("refs/heads/patch/b", "frozen")
	pushes := f.eventCount(t, "parley.git.push")

	a := f.commit(t, tips["patch/a"], "a2")
	b := f.commit(t, tips["patch/b"], "b2")
	out, err := f.push(t, f.key, a+":refs/heads/patch/a", b+":refs/heads/patch/b")
	wantPushRefused(t, out, err, "origin rejected push: frozen", "not updated: another ref in this push was refused")
	for branch, tip := range tips {
		if got := refIn(f.copy, "refs/heads/"+branch); got != tip {
			t.Errorf("hub's %s = %s after the refused push, want %s", branch, got, tip)
		}
	}
	if got := f.origin.Head("patch/a"); got != a {
		t.Errorf("origin's patch/a = %q, want the forwarded %s", got, a)
	}
	if got := f.origin.Head("patch/b"); got != "" {
		t.Errorf("origin's patch/b = %s, want none", got)
	}
	if got := f.eventCount(t, "parley.git.push"); got != pushes {
		t.Errorf("%d parley.git.push events after the refused push, want %d", got, pushes)
	}

	// Once a forwarded push is accepted its commit is in the hub's copy,
	// which records it as origin's head.
	a3 := f.commit(t, a, "a3")
	if out, err := f.push(t, f.key, a3+":refs/heads/patch/a"); err != nil {
		t.Fatalf("the forwarded push of patch/a failed: %v\n%s", err, out)
	}
	if got := f.origin.Head("patch/a"); got != a3 {
		t.Errorf("origin's patch/a = %q, want the forwarded %s", got, a3)
	}
	waitFor(t, "origin's head of patch/a recorded in the hub's copy", func() bool {
		return refIn(f.copy, "refs/remotes/origin/patch/a") == a3
	})
}

func TestGitPush_QueuesARebuildOnlyAfterTheClientHasItsAnswer(t *testing.T) {
	t.Parallel()
	const session = "6f1c2e3a-4b5d-4e6f-8a9b-0c1d2e3f4a5b"
	held := &heldRebuilds{recordingRebuilds: &recordingRebuilds{}, entered: make(chan string, 4), release: make(chan struct{}),
		hold: func(_ string, opts carrypatch.RebuildOptions) bool { return opts.SubmittedBy == "system:push" }}
	f := startPushFixtureWith(t, func(c *hub.Composition) {
		c.CarryPatch().RegisterRebuildEnqueuer(held)
		c.PushOptions().RegisterSessionLinker(anyPushSession{})
	})
	tips := f.registerPatches(t, "patch/a")
	pushed := func() []wantCall {
		var out []wantCall
		for _, c := range held.callsOf(f.slug) {
			if c.opts.SubmittedBy == "system:push" {
				out = append(out, wantCall{c.slug, c.opts.SubmittedBy, c.opts.SessionID})
			}
		}
		return out
	}

	a2 := f.commit(t, tips["patch/a"], "a2")
	answered := make(chan error, 1)
	var out string
	go func() {
		var err error
		out, err = f.push(t, f.key, a2+":refs/heads/patch/a")
		answered <- err
	}()
	select {
	case err := <-answered:
		if err != nil {
			t.Fatalf("git push of patch/a failed: %v\n%s", err, out)
		}
	case <-time.After(30 * time.Second):
		close(held.release)
		t.Fatal("git push did not exit while the rebuild enqueue was held")
	}
	hubtest.WaitClosed(t, held.enteredBy(f.slug), "the trigger held in the rebuild enqueue")
	if got := refIn(f.copy, "refs/heads/patch/a"); got != a2 {
		t.Errorf("hub's patch/a = %s while the trigger is held, want the pushed %s", got, a2)
	}
	if got := pushed(); len(got) != 0 {
		t.Errorf("rebuilds %v queued while the enqueue is held, want none yet", got)
	}
	close(held.release)
	waitFor(t, "the rebuild of the push", func() bool { return len(pushed()) == 1 })
	if got, want := fmt.Sprint(pushed()), fmt.Sprint([]wantCall{{f.slug, "system:push", nil}}); got != want {
		t.Errorf("rebuild calls %s, want %s", got, want)
	}

	a3 := f.commit(t, a2, "a3")
	if out, err := f.push(t, f.key, "--push-option=parley.session="+session, a3+":refs/heads/patch/a"); err != nil {
		t.Fatalf("git push of patch/a with a session failed: %v\n%s", err, out)
	}
	waitFor(t, "the rebuild of the push with a session", func() bool { return len(pushed()) == 2 })
	if got, want := pushed()[1].String(), (wantCall{f.slug, "system:push", ptr(session)}).String(); got != want {
		t.Errorf("the second rebuild call %s, want %s", got, want)
	}
}

func TestLand_IsBlockedOntoAPatchBranchSyncedFromOriginWithoutForwarding(t *testing.T) {
	t.Parallel()
	f := startPushFixture(t, func(w *workspace.Workspace) {
		w.Settings = map[string]json.RawMessage{"patch_branch_source": json.RawMessage(`"origin"`)}
	})
	tips := f.registerPatches(t, "patch/a")
	fix := gitrepotest.Commit(t, f.copy, "fix tls", map[string]string{"tls.txt": "fix\n"}, tips["patch/a"])
	gitrepotest.SetRef(t, f.copy, "refs/heads/fix/tls", fix)
	if status, raw := f.Call(t, http.MethodPost, "/api/v1/workspaces/"+f.slug+"/changes", f.key, map[string]any{
		"source": map[string]any{"branch": "fix/tls"}, "target_branch": "patch/a",
		"title": "Fix TLS patch", "intent": "Fix the TLS patch.",
	}); status != http.StatusCreated {
		t.Fatalf("open the change from fix/tls onto patch/a = %d %s, want 201", status, raw)
	}

	status, raw := f.Call(t, http.MethodPost, "/api/v1/workspaces/"+f.slug+"/changes/1/land", f.key, map[string]any{})
	var answer struct {
		Error struct {
			ErrorType string `json:"error_type"`
			Details   struct {
				Status string `json:"status"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &answer); err != nil || status != http.StatusConflict ||
		answer.Error.ErrorType != "policy_blocked" || answer.Error.Details.Status != "blocked" {
		t.Errorf("land the change = %d %s, want 409 policy_blocked with the attempt blocked", status, raw)
	}
	// The attempt does not show a guard's reason; its land_fail event does.
	want := "branch is synced from origin; push to " + f.origin.URL() + " instead"
	if evs := f.rebuildEvents(t, f.slug, "parley.change.land_fail"); len(evs) != 1 || evs[0].Metadata["reason"] != want {
		t.Errorf("parley.change.land_fail events = %+v, want one with the reason %q", evs, want)
	}
	if got := refIn(f.copy, "refs/heads/patch/a"); got != tips["patch/a"] {
		t.Errorf("hub's patch/a = %s after the blocked land, want %s", got, tips["patch/a"])
	}
	if got := f.rebuildJobs(t); len(got) != 0 {
		t.Errorf("rebuild jobs %+v after the blocked land, want none", got)
	}
}

func TestGitPush_APatchBranchIsRebuiltAndMirroredAndTheIntegrationBranchStaysTheHubs(t *testing.T) {
	t.Parallel()
	f := startPushFixture(t, func(w *workspace.Workspace) {
		w.Settings = map[string]json.RawMessage{
			"patch_branch_source": json.RawMessage(`"hub"`), "push_patches_to_origin": json.RawMessage(`true`),
			"auto_rebuild_after_push": json.RawMessage(`true`),
		}
	})
	tips := f.registerPatches(t, "patch/tls")
	pat := f.IssuePAT(t, identity.PATSpec{UserID: f.owner, Permissions: []string{"git:read", "git:write", "rebuilds:read"}})

	tls := f.commit(t, tips["patch/tls"], "tls2")
	if out, err := f.push(t, pat, tls+":refs/heads/patch/tls"); err != nil {
		t.Fatalf("the agent's push of patch/tls failed: %v\n%s", err, out)
	}
	if got := refIn(f.copy, "refs/heads/patch/tls"); got != tls {
		t.Errorf("hub's patch/tls = %s, want the pushed %s", got, tls)
	}
	waitFor(t, "the mirror of patch/tls to origin", func() bool { return f.origin.Head("patch/tls") == tls })
	if got := f.listRebuilds(t, pat); len(got) != 1 || got[0].SubmittedBy != "system:push" {
		t.Errorf("rebuilds of %s = %+v, want one submitted by system:push", f.slug, got)
	}

	deploy, originDeploy := refIn(f.copy, "refs/heads/deploy"), f.origin.Head("deploy")
	out, err := f.push(t, pat, f.commit(t, tls, "on deploy")+":refs/heads/deploy")
	wantPushRefused(t, out, err, "integration branch deploy is maintained by the hub; push a patch branch and rebuild instead")
	if got := refIn(f.copy, "refs/heads/deploy"); got != deploy {
		t.Errorf("hub's deploy = %s after the refused push, want %s", got, deploy)
	}
	if got := f.origin.Head("deploy"); got != originDeploy {
		t.Errorf("origin's deploy = %q after the refused push, want %q", got, originDeploy)
	}
	if n := f.eventCount(t, carrypatch.EventPatchMirrorFailed); n != 0 {
		t.Errorf("%d %s events, want none", n, carrypatch.EventPatchMirrorFailed)
	}
}

func TestGitPush_ADeveloperIsPointedToOriginThenForwardsThroughTheHub(t *testing.T) {
	t.Parallel()
	const token = "tok-path-2"
	f := startPushFixture(t, func(w *workspace.Workspace) {
		w.Settings = map[string]json.RawMessage{
			"patch_branch_source": json.RawMessage(`"origin"`), "push_patches_to_origin": json.RawMessage(`false`),
			"auto_rebuild_after_push": json.RawMessage(`false`),
		}
	})
	tips := f.registerPatches(t, "patch/tls")
	f.secrets.set(workspace.SecretGitPAT, token)
	f.origin.Accept("x-token-auth", token)
	h0, o0 := tips["patch/tls"], f.origin.Head("patch/tls")
	pushes := f.eventCount(t, "parley.git.push")

	tls := f.commit(t, h0, "tls2")
	out, err := f.push(t, f.key, tls+":refs/heads/patch/tls")
	wantPushRefused(t, out, err, "branch is synced from origin; push to "+f.origin.URL()+" instead")
	if strings.Contains(out, token) {
		t.Errorf("git's output shows the origin credential:\n%s", out)
	}
	if got := refIn(f.copy, "refs/heads/patch/tls"); got != h0 {
		t.Errorf("hub's patch/tls = %s after the refused push, want %s", got, h0)
	}
	if got := f.origin.Head("patch/tls"); got != o0 {
		t.Errorf("origin's patch/tls = %q after the refused push, want %q", got, o0)
	}

	f.setSettings(t, map[string]any{"push_patches_to_origin": true})
	if out, err := f.push(t, f.key, tls+":refs/heads/patch/tls"); err != nil {
		t.Fatalf("the forwarded push of patch/tls failed: %v\n%s", err, out)
	}
	if got, hub := f.origin.Head("patch/tls"), refIn(f.copy, "refs/heads/patch/tls"); got != tls || hub != tls {
		t.Errorf("origin's patch/tls = %q and hub's = %s, want both the pushed %s", got, hub, tls)
	}
	var tlsPushes int
	for _, ev := range f.rebuildEvents(t, f.slug, "parley.git.push")[pushes:] {
		if ev.Metadata["ref"] == "refs/heads/patch/tls" {
			tlsPushes++
		}
	}
	if tlsPushes != 1 {
		t.Errorf("%d parley.git.push events for refs/heads/patch/tls, want 1", tlsPushes)
	}
	if got := f.listRebuilds(t, f.key); len(got) != 0 {
		t.Errorf("rebuilds of %s = %+v, want none", f.slug, got)
	}
}

func TestLand_APatchBranchLandedOnIsRebuiltWithTheLandingsSessionAndMirrored(t *testing.T) {
	t.Parallel()
	const session = "0b8f3d6e-2c4a-4f1e-9d7b-5a6c8e0f1a2b"
	f := startPushFixtureWith(t, func(c *hub.Composition) { c.Land().RegisterSessionLinker(anyLandSession{}) },
		func(w *workspace.Workspace) {
			w.Settings = map[string]json.RawMessage{
				"patch_branch_source": json.RawMessage(`"hub"`), "push_patches_to_origin": json.RawMessage(`true`),
				"auto_rebuild_after_push": json.RawMessage(`true`),
			}
		})
	tips := f.registerPatches(t, "patch/tls")
	pat := f.IssuePAT(t, identity.PATSpec{UserID: f.owner, Permissions: []string{"git:read", "git:write", "changes:write", "rebuilds:read"}})

	fix := f.commit(t, tips["patch/tls"], "fix tls")
	if out, err := f.push(t, pat, fix+":refs/heads/fix/tls"); err != nil {
		t.Fatalf("the push of fix/tls failed: %v\n%s", err, out)
	}
	if got := f.listRebuilds(t, pat); len(got) != 0 {
		t.Fatalf("rebuilds of %s = %+v after the push of fix/tls, want none", f.slug, got)
	}
	if status, raw := f.Call(t, http.MethodPost, "/api/v1/workspaces/"+f.slug+"/changes", pat, map[string]any{
		"source": map[string]any{"branch": "fix/tls"}, "target_branch": "patch/tls",
		"title": "Fix TLS patch", "intent": "Fix the TLS patch.",
	}); status != http.StatusCreated {
		t.Fatalf("open the change from fix/tls onto patch/tls = %d %s, want 201", status, raw)
	}

	status, raw := f.Call(t, http.MethodPost, "/api/v1/workspaces/"+f.slug+"/changes/1/land", pat, map[string]any{"session_id": session})
	var attempt struct {
		Status    string `json:"status"`
		LandedSHA string `json:"landed_sha"`
	}
	if err := json.Unmarshal(raw, &attempt); err != nil || status != http.StatusOK || attempt.Status != "landed" || attempt.LandedSHA == "" {
		t.Fatalf("land the change = %d %s, want 200 with the attempt landed", status, raw)
	}
	if got := refIn(f.copy, "refs/heads/patch/tls"); got != attempt.LandedSHA {
		t.Errorf("hub's patch/tls = %s, want the landed %s", got, attempt.LandedSHA)
	}
	waitFor(t, "the rebuild of the land", func() bool { return len(f.listRebuilds(t, pat)) == 1 })
	if got := f.listRebuilds(t, pat); got[0].SubmittedBy != "system:land" {
		t.Errorf("rebuilds of %s = %+v, want one submitted by system:land", f.slug, got)
	}
	if evs := f.rebuildEvents(t, f.slug, carrypatch.EventRebuildEnqueue); len(evs) != 1 || evs[0].SessionID == nil || *evs[0].SessionID != session {
		t.Errorf("%s events = %+v, want one of the session %s", carrypatch.EventRebuildEnqueue, evs, session)
	}
	waitFor(t, "the mirror of patch/tls to origin", func() bool { return f.origin.Head("patch/tls") == attempt.LandedSHA })
}

// listRebuilds is the rebuild list of the workspace as GET
// /workspaces/:slug/rebuilds answers it to credential.
func (f *pushFixture) listRebuilds(t *testing.T, credential string) []listedRebuild {
	t.Helper()
	status, raw := f.Call(t, http.MethodGet, "/api/v1/workspaces/"+f.slug+"/rebuilds", credential, nil)
	var page struct {
		Items []listedRebuild `json:"items"`
	}
	if err := json.Unmarshal(raw, &page); err != nil || status != http.StatusOK {
		t.Fatalf("list the rebuilds of %s = %d %s, want 200", f.slug, status, raw)
	}
	return page.Items
}

// listedRebuild is what the smoke tests read of a listed rebuild.
type listedRebuild struct {
	ID          string `json:"id"`
	SubmittedBy string `json:"submitted_by"`
}

// anyLandSession is a land session linker that accepts every session and
// records nothing.
type anyLandSession struct{}

func (anyLandSession) Check(_ context.Context, _ change.Caller, sessionID, _ string) (string, error) {
	return strings.ToLower(sessionID), nil
}

func (anyLandSession) Record(context.Context, *sql.Tx, string, string, string) error { return nil }

// anyPushSession is a push session linker that accepts every session.
type anyPushSession struct{}

func (anyPushSession) Check(_ context.Context, _ gitserver.PushCaller, sessionID, _ string) (string, error) {
	return strings.ToLower(sessionID), nil
}

// pushFixture is the sync fixture's workspace acme with a git client of
// it, and the hub's carry-patch controller wrapped by a counter.
type pushFixture struct {
	*syncFixture
	control *countingControl
	// client is the git directory of a bare clone of the hub's copy that
	// shares the copy's objects, as git clone --shared leaves one.
	client string
}

// startPushFixture starts the sync fixture with the workspace's record
// changed by edit before it is stored, wraps the controller the hub
// registered with its push rules by a counter, and makes the client
// without a git process.
func startPushFixture(t *testing.T, edit ...func(w *workspace.Workspace)) *pushFixture {
	t.Helper()
	return startPushFixtureWith(t, nil, edit...)
}

// startPushFixtureWith is startPushFixture with the composition changed by
// compose, when it is not nil, after the fixture's own changes.
func startPushFixtureWith(t *testing.T, compose func(c *hub.Composition), edit ...func(w *workspace.Workspace)) *pushFixture {
	t.Helper()
	f := &pushFixture{control: &countingControl{}}
	f.syncFixture = startSyncFixtureWith(t, syncOptions{compose: func(c *hub.Composition) {
		f.control.inner = c.PushControl()
		c.PushRules().RegisterController(f.control)
		if compose != nil {
			compose(c)
		}
	}}, edit...)
	f.client = filepath.Join(t.TempDir(), "client.git")
	gitrepotest.InitBare(t, f.client, "")
	alternates := filepath.Join(f.client, "objects", "info", "alternates")
	if err := os.WriteFile(alternates, []byte(filepath.Join(f.copy, "objects")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return f
}

// registerPatches gives each branch, in the hub's copy, one commit on the
// upstream base that adds a file of its own, registers them as patches,
// in order, through POST /workspaces/:slug/patches as the owner, and
// returns their tips by branch.
func (f *pushFixture) registerPatches(t *testing.T, branches ...string) map[string]string {
	t.Helper()
	tips := map[string]string{}
	var body []map[string]any
	for _, branch := range branches {
		file := strings.ReplaceAll(branch, "/", "-") + ".txt"
		tips[branch] = gitrepotest.Commit(t, f.copy, branch, map[string]string{file: branch + "\n"}, f.base)
		gitrepotest.SetRef(t, f.copy, "refs/heads/"+branch, tips[branch])
		body = append(body, map[string]any{"branch_name": branch})
	}
	if status, raw := f.Call(t, http.MethodPost, "/api/v1/workspaces/"+f.slug+"/patches", f.key, body); status != http.StatusCreated {
		t.Fatalf("register the patches %v = %d %s, want 201", branches, status, raw)
	}
	return tips
}

// commit writes into the client a commit on parent with the message msg
// that adds a file named for it, and returns its id.
func (f *pushFixture) commit(t *testing.T, parent, msg string) string {
	t.Helper()
	return gitrepotest.Commit(t, f.client, msg, map[string]string{msg + ".txt": msg + "\n"}, parent)
}

// push runs git push from the client to the workspace over the hub's git
// route with refspecs, sending credential as the Basic password, and
// returns git's output and its error. An option of git push, such as
// --push-option=..., may come among the refspecs.
func (f *pushFixture) push(t *testing.T, credential string, refspecs ...string) (string, error) {
	t.Helper()
	u, err := url.Parse(f.URL("/git/acme/" + f.slug + ".git"))
	if err != nil {
		t.Fatal(err)
	}
	u.User = url.UserPassword("x", credential)
	cmd := exec.Command("git", append([]string{"push", u.String()}, refspecs...)...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "GIT_DIR=" + f.client,
		"GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "LC_ALL=C"}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// wantPushRefused fails the test unless git exited non-zero with every text
// of want in its output.
func wantPushRefused(t *testing.T, out string, err error, want ...string) {
	t.Helper()
	if err == nil {
		t.Errorf("git push succeeded, want it refused:\n%s", out)
		return
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("git's output lacks %q:\n%s", w, out)
		}
	}
}

// countingControl is a carry-patch controller that counts the updates it
// is asked about and hands each to inner.
type countingControl struct {
	inner *carrypatch.PushControl
	asked atomic.Int64
}

func (c *countingControl) Decide(ctx context.Context, ws pushrule.Workspace, caller pushrule.Caller, u pushrule.Update) (pushrule.Answer, error) {
	c.asked.Add(1)
	if c.inner == nil {
		return pushrule.Answer{}, errors.New("the hub registered no carry-patch controller")
	}
	return c.inner.Decide(ctx, ws, caller, u)
}

package carrypatch_test

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/gitserver/pushrule"
	"github.com/agent-fox-dev/parley/internal/hub"
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
	f := &pushFixture{control: &countingControl{}}
	f.syncFixture = startSyncFixtureWith(t, syncOptions{compose: func(c *hub.Composition) {
		f.control.inner = c.PushControl()
		c.PushRules().RegisterController(f.control)
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
// returns git's output and its error.
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

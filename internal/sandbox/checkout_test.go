package sandbox_test

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/sandbox"
)

func TestCheckout_ForkGetsWorkBranch(t *testing.T) {
	t.Parallel()
	h, release := startCheckoutWorld(t)
	shas := map[string]string{"main": h.base, "release": release}
	for _, tc := range []struct {
		name            string
		checkout        map[string]any
		wantRef, wantBr string
	}{
		{"no checkout member", nil, "main", "t1"},
		{"an empty checkout", map[string]any{}, "main", "t1"},
		{"a ref", map[string]any{"ref": "release"}, "release", "t1"},
		{"a branch", map[string]any{"branch": "work"}, "main", "work"},
		{"a ref and a branch", map[string]any{"ref": "release", "branch": "work"}, "release", "work"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string]any{"command": []string{"true"}}
			if tc.checkout != nil {
				body["checkout"] = tc.checkout
			}
			o, rec := h.provisioned(t, "demo--t1", "", body)
			want := map[string]any{"path": "/workspace", "ref": tc.wantRef, "sha": shas[tc.wantRef], "branch": tc.wantBr}
			if !reflect.DeepEqual(o["checkout"], want) {
				t.Errorf("201 checkout = %v; want %v", o["checkout"], want)
			}
			runs := h.hooks.runs(rec.ID)
			checkCloneAndOrder(t, runs, "https://hub.test/git/acme/demo--t1.git")
			clone := firstRun(runs, isClone)
			create := firstRun(runs, func(argv []string) bool {
				return isCheckout(argv) && createsBranch(argv) && slices.Contains(argv, tc.wantBr) && slices.Contains(argv, "origin/"+tc.wantRef)
			})
			if create < 0 || create < clone {
				t.Errorf("commands %v; want one after the clone that creates %s at origin/%s and checks it out", runs, tc.wantBr, tc.wantRef)
			}
			if rec.State != sandbox.StateReady || rec.Checkout == nil || rec.Checkout.SHA != checkoutHead {
				t.Errorf("sandbox = %s %q, checkout %+v; want ready with sha %s, the sandbox's HEAD", rec.State, rec.StateReason, rec.Checkout, checkoutHead)
			}
			ev := h.sandboxEvents(t, sandbox.EventReady, rec.ID)
			if len(ev) != 1 || ev[0].metadata["checkout"].(map[string]any)["sha"] != checkoutHead {
				t.Errorf("ready events %+v; want one whose checkout has sha %s", ev, checkoutHead)
			}
		})
	}
}

func TestCheckout_ExistingBranchIsKept(t *testing.T) {
	t.Parallel()
	h, _ := startCheckoutWorld(t)
	// The fork shares the parent's objects, so the commit is written there
	// and the fork's branch t1 is set to it.
	x := gitrepotest.Commit(t, h.workspaces.RepoPath("demo"), "work on t1", map[string]string{"WORK": "t1\n"}, h.base)
	gitrepotest.SetRef(t, h.workspaces.RepoPath("demo--t1"), "refs/heads/t1", x)

	o, rec := h.provisioned(t, "demo--t1", "", map[string]any{})
	c, _ := o["checkout"].(map[string]any)
	if c["branch"] != "t1" || c["sha"] != x || c["ref"] != "main" {
		t.Errorf("201 checkout = %v; want branch t1 at %s with ref main", c, x)
	}
	if rec.State != sandbox.StateReady {
		t.Fatalf("sandbox = %s %q; want ready", rec.State, rec.StateReason)
	}
	runs := h.hooks.runs(rec.ID)
	if firstRun(runs, func(argv []string) bool { return isCheckout(argv) && slices.Contains(argv, "t1") }) < 0 {
		t.Errorf("commands %v; want one that checks out t1", runs)
	}
	for _, argv := range gitRuns(runs) {
		if createsBranch(argv) || slices.Contains(argv, "reset") || slices.Contains(argv, "origin/main") {
			t.Errorf("%v creates or resets a branch; want t1 checked out as it is", argv)
		}
	}
}

func TestCheckout_RepositoryWorkspace(t *testing.T) {
	t.Parallel()
	h, release := startCheckoutWorld(t)
	for _, tc := range []struct {
		name     string
		checkout map[string]any
		want     string
	}{
		{"no checkout member", nil, "main"},
		{"a ref", map[string]any{"ref": "release"}, "release"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string]any{}
			if tc.checkout != nil {
				body["checkout"] = tc.checkout
			}
			sha := map[string]string{"main": h.base, "release": release}[tc.want]
			o, rec := h.provisioned(t, "demo", "", body)
			want := map[string]any{"path": "/workspace", "ref": tc.want, "sha": sha, "branch": nil}
			if !reflect.DeepEqual(o["checkout"], want) {
				t.Errorf("201 checkout = %v; want %v", o["checkout"], want)
			}
			if rec.State != sandbox.StateReady {
				t.Fatalf("sandbox = %s %q; want ready", rec.State, rec.StateReason)
			}
			runs := h.hooks.runs(rec.ID)
			checkCloneAndOrder(t, runs, "https://hub.test/git/acme/demo.git")
			if firstRun(runs, func(argv []string) bool {
				return isCheckout(argv) && (slices.Contains(argv, tc.want) || slices.Contains(argv, "origin/"+tc.want))
			}) < firstRun(runs, isClone) {
				t.Errorf("commands %v; want one after the clone that checks out %s", runs, tc.want)
			}
			for _, argv := range gitRuns(runs) {
				if createsBranch(argv) {
					t.Errorf("%v creates a branch; want none on a repository", argv)
				}
			}
		})
	}
}

func TestCheckout_ConfiguresGit(t *testing.T) {
	t.Parallel()
	h, _ := startCheckoutWorld(t)
	if _, err := h.Store.Main().Exec(`UPDATE users SET full_name = 'Dana Doe' WHERE id = ?`, h.dana); err != nil {
		t.Fatal(err)
	}
	_, rec := h.provisioned(t, "demo--t1", "", map[string]any{})
	if rec.State != sandbox.StateReady {
		t.Fatalf("sandbox = %s %q; want ready", rec.State, rec.StateReason)
	}
	runs := h.hooks.runs(rec.ID)
	helper := firstRun(runs, func(argv []string) bool {
		return len(argv) > 1 && argv[1] == "config" && slices.Contains(argv, "--global") && slices.Contains(argv, "credential.https://hub.test.helper")
	})
	clone := firstRun(runs, isClone)
	if helper < 0 || clone < 0 || helper > clone {
		t.Fatalf("commands %v; want a global git config of credential.https://hub.test.helper before the clone", runs)
	}
	if joined := strings.Join(runs[helper].argv, " "); !strings.Contains(joined, "PARLEY_API_KEY") || !strings.Contains(joined, "x-token-auth") {
		t.Errorf("credential helper %v; want one that answers x-token-auth and the content of PARLEY_API_KEY", runs[helper].argv)
	}
	for _, kv := range [][2]string{{"user.name", "Dana Doe"}, {"user.email", "dana@example.com"}} {
		i := firstRun(runs, func(argv []string) bool {
			return len(argv) > 3 && argv[1] == "config" && slices.Equal(argv[len(argv)-2:], kv[:]) && !slices.Contains(argv, "--global")
		})
		if i < clone {
			t.Errorf("commands %v; want git config %s %q after the clone", runs, kv[0], kv[1])
		}
	}
	hubp, ok := h.rt.Provider("parley", rec.ID+"-hub")
	if !ok || hubp.Value == "" {
		t.Fatal("the runtime holds no hub credential provider for the sandbox")
	}
	for _, r := range runs {
		if strings.Contains(strings.Join(r.argv, " "), hubp.Value) {
			t.Errorf("%v holds the hub credential's value", r.argv)
		}
	}
}

func TestCheckout_Skipped(t *testing.T) {
	t.Parallel()
	h, _ := startCheckoutWorld(t)
	pat := h.IssuePAT(t, identity.PATSpec{UserID: h.dana, Permissions: []string{"sandboxes:write", "changes:read"}})
	for _, tc := range []struct {
		name, cred string
		body       map[string]any
	}{
		{"checkout false", "", map[string]any{"checkout": false}},
		{"a profile without hub access", "", map[string]any{"profile": "plain"}},
		{"a hub credential without git:read", pat, map[string]any{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.body["command"] = []string{"true"}
			o, rec := h.provisioned(t, "demo--t1", tc.cred, tc.body)
			if o["checkout"] != nil {
				t.Errorf("201 checkout = %v; want null", o["checkout"])
			}
			if rec.State != sandbox.StateReady || rec.Checkout != nil {
				t.Errorf("sandbox = %s %q, checkout %+v; want ready without a checkout", rec.State, rec.StateReason, rec.Checkout)
			}
			if git := gitRuns(h.hooks.runs(rec.ID)); len(git) != 0 {
				t.Errorf("git commands %v; want none", git)
			}
			if main, ok := h.hooks.main(rec.ID); !ok || !slices.Equal(main, []string{"true"}) {
				t.Errorf("main process %v (started %t); want [true]", main, ok)
			}
		})
	}
}

func TestCheckout_RestoreFrom(t *testing.T) {
	t.Parallel()
	h, _ := startCheckoutWorld(t)
	old := h.seedIn(t, "demo--t1", json.RawMessage(`{"ok":true,"refs":[{"branch":"t1","sha":"`+h.base+`","dirty":true}]}`))
	restorer := &stubRestorer{hooks: h.hooks}
	h.svc.RegisterRestorer(restorer)
	t.Cleanup(func() { h.svc.RegisterRestorer(nil) })

	o, rec := h.provisioned(t, "demo--t1", "", map[string]any{"restore_from": old, "command": []string{"true"}})
	if o["restored_from"] != old {
		t.Errorf("201 restored_from = %v; want %s", o["restored_from"], old)
	}
	if rec.State != sandbox.StateReady || rec.RestoredFrom == nil || *rec.RestoredFrom != old {
		t.Errorf("sandbox = %s %q, restored from %v; want ready, restored from %s", rec.State, rec.StateReason, rec.RestoredFrom, old)
	}
	runs := h.hooks.runs(rec.ID)
	var restores []int
	lastGit, main := -1, -1
	for i, r := range runs {
		switch {
		case r.kind == "restore":
			restores = append(restores, i)
			if !slices.Equal(r.argv, []string{rec.ID, old}) {
				t.Errorf("restore of %v; want of %s from %s", r.argv, rec.ID, old)
			}
		case r.kind == "main":
			main = i
		case r.kind == "exec":
			lastGit = i
		}
	}
	if len(restores) != 1 || lastGit < 0 || restores[0] < lastGit || main < restores[0] {
		t.Errorf("log %v; want one restore after the last checkout command and before the main process", runs)
	}
	if ev := h.sandboxEvents(t, sandbox.EventCreate, rec.ID); len(ev) != 1 || ev[0].metadata["restored_from"] != old {
		t.Errorf("create events %+v; want one with restored_from %s", ev, old)
	}

	t.Run("without a restorer", func(t *testing.T) {
		h.svc.RegisterRestorer(nil)
		_, rec := h.provisioned(t, "demo--t1", "", map[string]any{"restore_from": old, "command": []string{"true"}})
		if rec.State != sandbox.StateFailed || rec.StateReason != sandbox.ReasonProvisionFailed {
			t.Errorf("sandbox = %s %q; want failed provision_failed", rec.State, rec.StateReason)
		}
		if _, started := h.hooks.main(rec.ID); started {
			t.Error("the main process was started")
		}
	})
}

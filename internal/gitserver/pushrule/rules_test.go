package pushrule_test

import (
	"testing"

	"github.com/agent-fox-dev/parley/internal/gitserver/pushrule"
)

func TestEvaluate_RefusesEveryWriteToAHubOwnedRef(t *testing.T) {
	r := newRepo(t)
	e := engine(pushrule.EngineDeps{Settings: protecting("main")})
	want := refused("refs/parley/ is maintained by the hub and cannot be pushed to", "hub_owned_ref")
	for _, ref := range []string{"refs/parley/changes/3/1", "refs/parley/replaced/patch-a", "refs/parley/sandboxes/sb1/main", "refs/parley/x"} {
		for _, u := range []pushrule.Update{create(ref, r.head), move(ref, r.base, r.head), remove(ref, r.head)} {
			for _, caller := range []pushrule.Caller{adminToken, ownerKey, ownerPAT} {
				if got := decide(t, e, r, demo, caller, u); got != want {
					t.Errorf("%s by %s: %+v, want %+v", describe(u), caller.CredentialType, got, want)
				}
			}
		}
	}
}

func TestEvaluate_RefusesDeletingTheDefaultBranch(t *testing.T) {
	r := newRepo(t)
	e := engine(pushrule.EngineDeps{Settings: protecting()})
	trunk := demo
	trunk.DefaultBranch = "trunk"
	for _, caller := range []pushrule.Caller{adminToken, ownerKey} {
		for _, tc := range []struct {
			u    pushrule.Update
			want outcome
		}{
			{remove("refs/heads/trunk", r.head), refused("trunk is the default branch and cannot be deleted", "default_branch")},
			{move("refs/heads/trunk", r.head, r.side), outcome{Allowed: true, Forced: true}},
			{remove("refs/tags/trunk", r.head), outcome{Allowed: true, Deleted: true}},
		} {
			if got := decide(t, e, r, trunk, caller, tc.u); got != tc.want {
				t.Errorf("%s by %s: %+v, want %+v", describe(tc.u), caller.CredentialType, got, tc.want)
			}
		}
	}
}

func TestEvaluate_RefusesEveryWriteToATrackingBranchOfAFork(t *testing.T) {
	r := newRepo(t)
	lookup := &tracking{branches: []string{"dev", "release/1"}}
	e := engine(pushrule.EngineDeps{Settings: protecting(), Tracking: lookup})
	for _, branch := range []string{"dev", "release/1"} {
		ref := "refs/heads/" + branch
		want := refused(branch+" tracks the parent workspace and is read-only in this fork; push a new branch", "tracking_branch")
		for _, u := range []pushrule.Update{create(ref, r.head), move(ref, r.base, r.head), remove(ref, r.head)} {
			for _, caller := range []pushrule.Caller{adminToken, ownerKey, boundPAT} {
				if got := decide(t, e, r, demoFk, caller, u); got != want {
					t.Errorf("%s by %s: %+v, want %+v", describe(u), caller.CredentialType, got, want)
				}
			}
		}
	}
	if got, want := decide(t, e, r, demoFk, boundPAT, create("refs/heads/task-1", r.head)), (outcome{Allowed: true, Created: true}); got != want {
		t.Errorf("create of a new branch in the fork: %+v, want %+v", got, want)
	}
	lookup.asked = nil
	if got, want := decide(t, e, r, demo, ownerKey, move("refs/heads/dev", r.base, r.head)), (outcome{Allowed: true}); got != want {
		t.Errorf("update of dev in the repository: %+v, want %+v", got, want)
	}
	if len(lookup.asked) != 0 {
		t.Errorf("the lookup was asked %v for a repository workspace", lookup.asked)
	}
}

func describe(u pushrule.Update) string {
	switch {
	case u.Before == pushrule.ZeroID:
		return "create of " + u.Ref
	case u.After == pushrule.ZeroID:
		return "delete of " + u.Ref
	}
	return "update of " + u.Ref
}

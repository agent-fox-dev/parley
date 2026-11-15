package pushrule_test

import (
	"context"
	"testing"

	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/gitserver/pushrule"
)

func TestEvaluate_AllowsWhatNoRuleRefusesAndReportsHowTheRefMoves(t *testing.T) {
	r := newRepo(t)
	e := engine(pushrule.EngineDeps{Settings: protecting("main")})
	for _, tc := range []struct {
		u    pushrule.Update
		want outcome
	}{
		{create("refs/heads/feat", r.head), outcome{Allowed: true, Created: true}},
		{move("refs/heads/feat", r.base, r.head), outcome{Allowed: true}},
		{move("refs/heads/feat", r.head, r.side), outcome{Allowed: true, Forced: true}},
		{remove("refs/heads/feat", r.head), outcome{Allowed: true, Deleted: true}},
		{create("refs/tags/v1", r.base), outcome{Allowed: true, Created: true}},
		{move("refs/tags/v1", r.base, r.head), outcome{Allowed: true}},
		{move("refs/tags/v1", r.head, r.base), outcome{Allowed: true, Forced: true}},
		{remove("refs/tags/v1", r.head), outcome{Allowed: true, Deleted: true}},
		{create("refs/notes/commits", r.head), outcome{Allowed: true, Created: true}},
		{create("refs/custom/x", r.head), outcome{Allowed: true, Created: true}},
		{create("refs/heads/parley/x", r.head), outcome{Allowed: true, Created: true}},
		{create("refs/parleyx/y", r.head), outcome{Allowed: true, Created: true}},
	} {
		if got := decide(t, e, r, demo, ownerKey, tc.u); got != tc.want {
			t.Errorf("%s from %.7s to %.7s: %+v, want %+v", describe(tc.u), tc.u.Before, tc.u.After, got, tc.want)
		}
	}
}

func TestEvaluate_DecidesEveryUpdateInOrderAndWritesNothing(t *testing.T) {
	r := newRepo(t)
	lookup := &tracking{}
	e := engine(pushrule.EngineDeps{Settings: protecting("main"), Tracking: lookup})
	refsBefore := gitrepotest.Git(t, r.dir, "for-each-ref")
	updates := []pushrule.Update{
		create("refs/heads/topic", r.side),
		create("refs/parley/x", r.head),
		move("refs/heads/feat", r.head, r.side),
	}
	v := e.Evaluate(context.Background(), pushrule.Request{Workspace: demo, Caller: ownerKey, Updates: updates, Repo: r.view})
	if len(v.Decisions) != len(updates) {
		t.Fatalf("got %d decisions for %d updates", len(v.Decisions), len(updates))
	}
	wants := []outcome{
		{Allowed: true, Created: true},
		refused("refs/parley/ is maintained by the hub and cannot be pushed to", "hub_owned_ref"),
		{Allowed: true, Forced: true},
	}
	for i, d := range v.Decisions {
		if d.Update != updates[i] || outcomeOf(d) != wants[i] {
			t.Errorf("decision %d = %+v, want %+v about %+v", i, d, wants[i], updates[i])
		}
	}
	if !v.Refused() {
		t.Error("Refused() = false for a request with a refused update")
	}
	if got := gitrepotest.Git(t, r.dir, "for-each-ref"); got != refsBefore {
		t.Errorf("refs after evaluating:\n%s\nwant:\n%s", got, refsBefore)
	}
}

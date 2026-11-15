package pushrule_test

import (
	"context"
	"testing"

	"github.com/agent-fox-dev/parley/internal/gitserver/pushrule"
)

func TestEvaluate_TakesTheCarryPatchControllersAnswerForWhatRulesOneToThreeAllowed(t *testing.T) {
	r := newRepo(t)
	const integration = "integration branch integration is maintained by the hub; push a patch branch and rebuild instead"
	for _, tc := range []struct {
		ws          pushrule.Workspace
		u           pushrule.Update
		answer      string // refuse, allow or forward
		want        outcome
		wantForward bool
		wantAsked   int
	}{
		{demoCP, move("refs/heads/integration", r.base, r.head), "refuse", refused(integration, "integration_branch"), false, 1},
		{demoCP, move("refs/heads/patch/a", r.base, r.head), "allow", outcome{Allowed: true}, false, 1},
		{demoCP, move("refs/heads/patch/a", r.base, r.head), "forward", outcome{Allowed: true}, true, 1},
		{demoCP, create("refs/tags/v1", r.head), "forward", outcome{Allowed: true, Created: true}, true, 1},
		{demoCP, create("refs/parley/x", r.head), "forward", refused("refs/parley/ is maintained by the hub and cannot be pushed to", "hub_owned_ref"), false, 0},
		{demoCP, remove("refs/heads/main", r.head), "forward", refused("main is the default branch and cannot be deleted", "default_branch"), false, 0},
		{demo, move("refs/heads/patch/a", r.base, r.head), "refuse", outcome{Allowed: true}, false, 0},
	} {
		cp := &controller{}
		cp.answers = map[string]pushrule.Answer{tc.u.Ref: map[string]pushrule.Answer{
			"refuse":  pushrule.Refuse(integration, pushrule.ErrorTypeIntegrationBranch),
			"allow":   pushrule.Allow(),
			"forward": pushrule.AllowAfterForward(cp.forward),
		}[tc.answer]}
		e := engine(pushrule.EngineDeps{Settings: protecting()})
		e.RegisterController(cp)
		v := e.Evaluate(context.Background(), pushrule.Request{Workspace: tc.ws, Caller: ownerKey, Updates: []pushrule.Update{tc.u}, Repo: r.view})
		d := v.Decisions[0]
		name := describe(tc.u) + " on " + tc.ws.Mode + " answered " + tc.answer
		if got := outcomeOf(d); got != tc.want {
			t.Errorf("%s: %+v, want %+v", name, got, tc.want)
		}
		if asked, _ := cp.counts(); asked != tc.wantAsked || (asked == 1 && cp.asked[0] != "demo:key-owner:"+tc.u.Ref) {
			t.Errorf("%s: controller asked %q, want %d question about the update", name, cp.asked, tc.wantAsked)
		}
		if (d.Forward != nil) != tc.wantForward {
			t.Errorf("%s: pending forward %v, want %v", name, d.Forward != nil, tc.wantForward)
		}
		if d.Forward == nil {
			continue
		}
		if _, forwarded := cp.counts(); forwarded != 0 {
			t.Errorf("%s: evaluating ran the forward", name)
		}
		if err := d.Forward(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, forwarded := cp.counts(); forwarded != 1 {
			t.Errorf("%s: the pending forward is not the controller's", name)
		}
	}
}

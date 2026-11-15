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
	cp := &controller{}
	cp.answers = map[string]pushrule.Answer{"refs/heads/patch/a": pushrule.AllowAfterForward(cp.forward)}
	e.RegisterController(cp)
	refsBefore := gitrepotest.Git(t, r.dir, "for-each-ref")
	updates := []pushrule.Update{
		create("refs/heads/topic", r.side),
		create("refs/parley/x", r.head),
		move("refs/heads/patch/a", r.head, r.side),
	}
	v := e.Evaluate(context.Background(), pushrule.Request{Workspace: demoCP, Caller: ownerKey, Updates: updates, Repo: r.view})
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
	if v.Decisions[2].Forward == nil {
		t.Error("the patch branch update carries no pending forward")
	}
	if !v.Refused() {
		t.Error("Refused() = false for a request with a refused update")
	}
	if _, forwarded := cp.counts(); forwarded != 0 {
		t.Errorf("evaluating ran %d forwards, want none", forwarded)
	}
	if got := gitrepotest.Git(t, r.dir, "for-each-ref"); got != refsBefore {
		t.Errorf("refs after evaluating:\n%s\nwant:\n%s", got, refsBefore)
	}
}

func TestEvaluate_AnswersWithTheFirstRuleThatRefuses(t *testing.T) {
	r := newRepo(t)
	src := commit(t, r.dir, []string{r.head}, "M src/a.go")
	const patchMsg = "patch/a is rebuilt by the hub; push a new patch branch"
	for _, tc := range []struct {
		ws        pushrule.Workspace
		protected []string
		caller    pushrule.Caller
		u         pushrule.Update
		want      outcome
		asked     int
	}{
		{demo, []string{"main"}, ownerKey, remove("refs/heads/main", r.head),
			refused("main is protected and cannot be force-pushed or deleted", "protected_branch"), 0},
		{demoFk, []string{"main"}, ownerKey, create("refs/heads/main", r.head),
			refused("main is protected; push another branch and open a change (git push -o parley.change)", "protected_branch"), 0},
		{demoFk, []string{"main"}, ownerKey, move("refs/heads/main", r.base, r.head),
			refused("main tracks the parent workspace and is read-only in this fork; push a new branch", "tracking_branch"), 0},
		{demoFk, nil, ownerKey, remove("refs/heads/main", r.head),
			refused("main is the default branch and cannot be deleted", "default_branch"), 0},
		{demoCP, []string{"main", "patch/*"}, ownerKey, move("refs/heads/patch/a", r.base, r.head),
			refused(patchMsg, "patch_branch"), 1},
		{demoCP, []string{"main", "patch/*"}, memberKey, move("refs/heads/patch/a", r.base, r.head),
			refused("patch/a is protected; push another branch and open a change (git push -o parley.change)", "protected_branch"), 0},
		{demoFk, []string{"main"}, docsPAT, move("refs/heads/dev", r.head, src),
			refused("dev tracks the parent workspace and is read-only in this fork; push a new branch", "tracking_branch"), 0},
	} {
		cp := &controller{answers: map[string]pushrule.Answer{"refs/heads/patch/a": pushrule.Refuse(patchMsg, "patch_branch")}}
		e := engine(pushrule.EngineDeps{Settings: protecting(tc.protected...), Tracking: &tracking{branches: []string{"main", "dev"}}})
		e.RegisterController(cp)
		got := decide(t, e, r, tc.ws, tc.caller, tc.u)
		if asked, _ := cp.counts(); got != tc.want || asked != tc.asked {
			t.Errorf("%s on %s by %s: %+v after %d controller questions, want %+v after %d",
				describe(tc.u), tc.ws.Slug, tc.caller.CredentialID, got, asked, tc.want, tc.asked)
		}
	}
}

func TestEvaluate_RefusesAsInternalWhatARuleCannotEvaluate(t *testing.T) {
	r := newRepo(t)
	ff := move("refs/heads/feat", r.base, r.head)
	added := move("refs/heads/feat", r.head, commit(t, r.dir, []string{r.head}, "M docs/added.md"))
	none := protecting()
	for _, tc := range []struct {
		fault      string
		ws         pushrule.Workspace
		deps       pushrule.EngineDeps
		controller pushrule.Controller
		fails      string
		caller     pushrule.Caller
		u          pushrule.Update
	}{
		{"settings reader fails", demo, pushrule.EngineDeps{Settings: settings{err: errInjected}}, nil, "", ownerKey, ff},
		{"is-ancestor fails on a protected branch", demo, pushrule.EngineDeps{Settings: protecting("main")}, nil, "IsAncestor", ownerKey, move("refs/heads/main", r.base, r.head)},
		{"tracking lookup fails on a fork", demoFk, pushrule.EngineDeps{Settings: none, Tracking: &tracking{err: errInjected}}, nil, "", ownerKey, ff},
		{"controller fails", demoCP, pushrule.EngineDeps{Settings: none}, &controller{err: errInjected}, "", ownerKey, ff},
		{"no controller on a carry_patch workspace", demoCP, pushrule.EngineDeps{Settings: none}, nil, "", ownerKey, ff},
		{"version-files lookup fails", demo, pushrule.EngineDeps{Settings: none, VersionFiles: &versionFiles{err: errInjected}}, nil, "", docsPAT, ff},
		{"new-commits question fails", demo, pushrule.EngineDeps{Settings: none}, nil, "NewCommits", docsPAT, added},
		{"changed-paths question fails", demo, pushrule.EngineDeps{Settings: none}, nil, "ChangedPaths", docsPAT, added},
		{"object question fails", demo, pushrule.EngineDeps{Settings: none}, nil, "ObjectPaths", docsPAT, ff},
	} {
		e := engine(tc.deps)
		e.RegisterController(tc.controller)
		v := e.Evaluate(context.Background(), pushrule.Request{Workspace: tc.ws, Caller: tc.caller, Updates: []pushrule.Update{tc.u}, Repo: &view{View: r.view, fails: tc.fails}})
		if len(v.Decisions) != 1 || outcomeOf(v.Decisions[0]) != refused("internal error; try again", "internal") {
			t.Errorf("%s: %+v, want one refused as internal", tc.fault, v.Decisions)
		}
	}
}

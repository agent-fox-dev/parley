package pushrule_test

import (
	"fmt"
	"testing"

	"github.com/agent-fox-dev/parley/internal/gitserver/pushrule"
)

func TestProtected_IsWhatTheBranchGlobMatcherAnswers(t *testing.T) {
	for _, tc := range []struct {
		branch string
		globs  []string
		want   bool
	}{
		{"main", []string{"main"}, true},
		{"main2", []string{"main"}, false},
		{"mai", []string{"main"}, false},
		{"Main", []string{"main"}, false},
		{"release/1.0", []string{"release/*"}, true},
		{"release/1.0/hotfix", []string{"release/*"}, false},
		{"release/1.0/hotfix", []string{"release/**"}, true},
		{"a", []string{"*"}, true},
		{"feature/a", []string{"*"}, false},
		{"feature/a", []string{"**"}, true},
		{"x", []string{}, false},
		{"x", nil, false},
		{"dev", []string{"main", "dev"}, true},
		{"rel-1", []string{"rel-*"}, true},
	} {
		if got := pushrule.Protected(tc.branch, tc.globs); got != tc.want {
			t.Errorf("Protected(%q, %q) = %v, want %v", tc.branch, tc.globs, got, tc.want)
		}
	}

	r := newRepo(t)
	e := engine(pushrule.EngineDeps{Settings: protecting("main")})
	if got, want := decide(t, e, r, demo, memberKey, move("refs/tags/main", r.head, r.side)), (outcome{Allowed: true, Forced: true}); got != want {
		t.Errorf("forced update of refs/tags/main protecting main: %+v, want %+v", got, want)
	}
}

func TestEvaluate_HoldsProtectedBranchesToTheOwnerExemption(t *testing.T) {
	r := newRepo(t)
	const (
		rewrite = "main is protected and cannot be force-pushed or deleted"
		viaLand = "%s is protected; push another branch and open a change (git push -o parley.change)"
	)
	direct := outcome{Allowed: true, Direct: true}
	type row struct {
		ws        pushrule.Workspace
		caller    pushrule.Caller
		ownerPush bool
		u         pushrule.Update
		want      outcome
	}
	var rows []row
	for _, caller := range []pushrule.Caller{adminToken, ownerKey, ownerPAT, boundPAT} {
		for _, ownerPush := range []bool{true, false} {
			rows = append(rows,
				row{demo, caller, ownerPush, move("refs/heads/main", r.head, r.side), refused(rewrite, "protected_branch")},
				row{demo, caller, ownerPush, remove("refs/heads/main", r.head), refused(rewrite, "protected_branch")},
				row{demo, caller, ownerPush, create("refs/heads/release/2", r.head), refused(fmt.Sprintf(viaLand, "release/2"), "protected_branch")},
			)
		}
	}
	fastForward := move("refs/heads/main", r.base, r.head)
	for _, tc := range []struct {
		caller    pushrule.Caller
		ownerPush bool
	}{
		{adminToken, false}, {ownerKey, false}, {memberKey, true}, {boundPAT, true}, {pathsPAT, true}, {memberPAT, true},
	} {
		rows = append(rows, row{demo, tc.caller, tc.ownerPush, fastForward, refused(fmt.Sprintf(viaLand, "main"), "protected_branch")})
	}
	rows = append(rows,
		row{demo, adminToken, true, fastForward, direct},
		row{demo, ownerKey, true, fastForward, direct},
		row{demo, ownerPAT, true, fastForward, direct},
		row{demoFk, ownerKey, true, fastForward, direct},
		// The exemption lifts rule 2 only: the rules after it still refuse,
		// and an unprotected branch is never a direct push.
		row{demoFk, ownerKey, true, move("refs/heads/dev", r.base, r.head), refused("dev tracks the parent workspace and is read-only in this fork; push a new branch", "tracking_branch")},
		row{demo, ownerKey, true, move("refs/heads/feat", r.base, r.head), outcome{Allowed: true}},
	)
	for _, tc := range rows {
		s := settings{Settings: pushrule.Settings{ProtectedBranches: []string{"main", "release/*", "dev"}, OwnerPush: tc.ownerPush}}
		e := engine(pushrule.EngineDeps{Settings: s, Tracking: &tracking{branches: []string{"dev"}}})
		if got := decide(t, e, r, tc.ws, tc.caller, tc.u); got != tc.want {
			t.Errorf("%s on %s by %s/%s (paths %q, bound %v), owner_push %v: %+v, want %+v",
				describe(tc.u), tc.ws.Slug, tc.caller.Level, tc.caller.CredentialType, tc.caller.Paths, tc.caller.Bound, tc.ownerPush, got, tc.want)
		}
	}
}

func TestEvaluate_ProtectsTheDefaultBranchWithoutASettingsReader(t *testing.T) {
	r := newRepo(t)
	var e pushrule.Engine
	for _, tc := range []struct {
		u    pushrule.Update
		want outcome
	}{
		{move("refs/heads/main", r.head, r.side), refused("main is protected and cannot be force-pushed or deleted", "protected_branch")},
		{move("refs/heads/main", r.base, r.head), outcome{Allowed: true, Direct: true}},
		{move("refs/heads/feat", r.head, r.side), outcome{Allowed: true, Forced: true}},
	} {
		if got := decide(t, &e, r, demo, ownerKey, tc.u); got != tc.want {
			t.Errorf("%s: %+v, want %+v", describe(tc.u), got, tc.want)
		}
	}
}

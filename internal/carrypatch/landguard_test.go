package carrypatch_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/carrypatch"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/land"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// guardOf is a landing guard over the in-memory readers and the forward
// stub of f.
func guardOf(f *controlFixture) *carrypatch.LandGuard {
	return &carrypatch.LandGuard{Records: f.records, Settings: f.settings, Patches: f.patches, Forwarder: f.forwards}
}

// landOnto is what the land engine asks a guard about a land onto branch
// of demo that would move it from controlBase to controlFF.
func landOnto(branch string) land.TargetInput {
	return land.TargetInput{Workspace: "demo", Branch: branch, Old: controlBase, New: controlFF}
}

func TestLandGuard_AnswersALandFromThePatchSettingsAndContactsOriginOnlyToForward(t *testing.T) {
	t.Parallel()
	standard := controlRecord(nil, ptr("https://example.test/acme/demo.git"))
	standard.Mode = workspace.ModeStandard
	withUserinfo := controlRecord(nil, ptr("https://bot:s3cret@example.test/acme/demo.git"))
	settings := func(source string, forward bool) map[string]any {
		return controlSettings{source, forward}.values()
	}
	active := controlPatch("patch/a", carrypatch.StatusActive, 1)
	refused := "branch is synced from origin; push to https://example.test/acme/demo.git instead"
	for _, tc := range []struct {
		name     string
		rec      workspace.Workspace
		settings map[string]any
		patches  []carrypatch.Patch
		branch   string
		// forwardErr is what the forward fails with, nil for success.
		forwardErr error
		// noPatchRead is set where the guard must not read the patch list.
		noPatchRead bool
		want        land.TargetAnswer
		wantForward bool
	}{
		{name: "a standard workspace, onto main", rec: standard, settings: settings("origin", false),
			branch: "main", noPatchRead: true},
		{name: "a carry-patch workspace, onto a branch that is no patch's", rec: withUserinfo,
			settings: settings("origin", false), patches: []carrypatch.Patch{active}, branch: "main"},
		{name: "onto the branch of a deleted patch, from origin without forwarding", rec: withUserinfo,
			settings: settings("origin", false), patches: []carrypatch.Patch{controlPatch("patch/old", carrypatch.StatusDeleted, 0)},
			branch: "patch/old"},
		{name: "onto a patch branch, from the hub without forwarding", rec: withUserinfo,
			settings: settings("hub", false), patches: []carrypatch.Patch{active}, branch: "patch/a"},
		{name: "onto a patch branch, from the hub with forwarding", rec: withUserinfo,
			settings: settings("hub", true), patches: []carrypatch.Patch{active}, branch: "patch/a"},
		{name: "onto a patch branch, from origin without forwarding", rec: withUserinfo,
			settings: settings("origin", false), patches: []carrypatch.Patch{active}, branch: "patch/a",
			want: land.TargetAnswer{Refuse: refused}},
		{name: "onto the branch of a disabled patch, from origin without forwarding", rec: withUserinfo,
			settings: settings("origin", false), patches: []carrypatch.Patch{controlPatch("patch/a", carrypatch.StatusDisabled, 1)},
			branch: "patch/a", want: land.TargetAnswer{Refuse: refused}},
		{name: "onto a patch branch, from origin with forwarding that origin takes", rec: withUserinfo,
			settings: settings("origin", true), patches: []carrypatch.Patch{active}, branch: "patch/a", wantForward: true},
		{name: "onto a patch branch, from origin with forwarding that origin rejects", rec: withUserinfo,
			settings: settings("origin", true), patches: []carrypatch.Patch{active}, branch: "patch/a",
			forwardErr: errors.New("origin rejected push: frozen"),
			want:       land.TargetAnswer{OriginRejected: "origin rejected push: frozen"}, wantForward: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newControl(t, tc.rec, tc.settings, tc.patches...)
			f.patches.forbidden = tc.noPatchRead
			f.forwards.err = tc.forwardErr
			got, err := guardOf(f).Check(context.Background(), landOnto(tc.branch))
			if err != nil || got != tc.want {
				t.Errorf("the guard answered %+v, %v; want %+v, nil", got, err, tc.want)
			}
			var want []string
			if tc.wantForward {
				want = []string{"demo " + tc.branch + " " + controlFF}
			}
			if asked := f.forwards.asked(); fmt.Sprint(asked) != fmt.Sprint(want) {
				t.Errorf("the guard ran the forwards %q, want %q", asked, want)
			}
			if tc.wantForward && len(f.forwards.repos) > 0 && f.forwards.repos[0] != nil {
				t.Error("the guard forwarded from a repository of its own, want the hub's copy (nil)")
			}
		})
	}
}

func TestLandGuard_FailsInsteadOfAnsweringWhenItCannotDecide(t *testing.T) {
	t.Parallel()
	rec := controlRecord(nil, ptr("https://example.test/acme/demo.git"))
	for _, tc := range []struct {
		name  string
		fault func(f *controlFixture)
	}{
		{"the settings cannot be read", func(f *controlFixture) { f.settings.err = errors.New("settings unreadable") }},
		{"the patch list cannot be read", func(f *controlFixture) { f.patches.err = errors.New("patch store unreadable") }},
		{"the workspace record cannot be read", func(f *controlFixture) { f.records.err = errors.New("record unreadable") }},
		{"the workspace has no record", func(f *controlFixture) { delete(f.records.records, "demo") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newControl(t, rec, controlSettings{"origin", true}.values(), controlPatch("patch/a", carrypatch.StatusActive, 1))
			tc.fault(f)
			got, err := guardOf(f).Check(context.Background(), landOnto("patch/a"))
			if err == nil || got != (land.TargetAnswer{}) {
				t.Errorf("the guard answered %+v, %v; want no answer and an error", got, err)
			}
			if asked := f.forwards.asked(); len(asked) != 0 {
				t.Errorf("the guard ran the forwards %q, want none", asked)
			}
		})
	}
}

func TestLandGuard_LetsALandPassOnlyOnceOriginTookTheNewHead(t *testing.T) {
	t.Parallel()
	f := newForwardFixture(t)
	dead := gitrepotest.NewOrigin(t)
	dead.Close()
	deadURL := dead.URL()
	// guard is a landing guard of demo, whose patch/a is synced from origin
	// with forwarding, which forwards with the fixture's component bound
	// with limit, over the workspace record rec.
	guard := func(rec workspace.Workspace, limit time.Duration) *carrypatch.LandGuard {
		return &carrypatch.LandGuard{
			Records:   &recordStub{records: map[string]workspace.Workspace{"demo": rec}},
			Settings:  &settingsStub{values: controlSettings{"origin", true}.values()},
			Patches:   &patchStub{t: t, patches: []carrypatch.Patch{controlPatch("patch/a", carrypatch.StatusActive, 1)}},
			Forwarder: f.component(limit),
		}
	}
	in := land.TargetInput{Workspace: "demo", Branch: "patch/a", Old: f.base, New: f.commit}

	for _, tc := range []struct {
		name  string
		setup func(rec *workspace.Workspace)
		limit time.Duration
		want  string
	}{
		{name: "a hook of origin refuses",
			setup: func(*workspace.Workspace) { f.origin.RejectRef("refs/heads/patch/a", "frozen") },
			want:  "origin rejected push: frozen"},
		{name: "origin's branch moved to a commit the new head lacks",
			setup: func(*workspace.Workspace) { f.origin.Commit("patch/a", "elsewhere") },
			want:  "origin rejected push: non-fast-forward"},
		{name: "nothing listens at origin's address",
			setup: func(rec *workspace.Workspace) { rec.GitURL = &deadURL },
			want:  "origin rejected push: origin unreachable"},
		{name: "the origin credential cannot be read",
			setup: func(*workspace.Workspace) { f.secrets.set("demo", "", errors.New("secret store sealed")) },
			want:  "failed to resolve origin credentials"},
		{name: "origin stalls past the limit",
			setup: func(*workspace.Workspace) { f.origin.StallRef("refs/heads/patch/a") },
			limit: 200 * time.Millisecond,
			want:  "origin rejected push: timed out"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := f.ws
			tc.setup(&rec)
			t.Cleanup(func() {
				f.origin.Reject("")
				f.secrets.set("demo", forwardToken, nil)
				gitrepotest.SetRef(t, f.origin.Dir(), "refs/heads/patch/a", f.base)
			})
			originBefore := f.origin.Head("patch/a")
			start := time.Now()
			got, err := guard(rec, tc.limit).Check(context.Background(), in)
			if err != nil || got != (land.TargetAnswer{OriginRejected: tc.want}) {
				t.Errorf("the guard answered %+v, %v; want origin rejected with %q", got, err, tc.want)
			}
			if took := time.Since(start); tc.limit > 0 && took >= tc.limit+5*time.Second {
				t.Errorf("the guard answered after %v, want within the limit %v and 5s", took, tc.limit)
			}
			if got := f.origin.Head("patch/a"); got != originBefore {
				t.Errorf("origin's patch/a = %.7s, want it left at %.7s", got, originBefore)
			}
			if got := f.recorded("patch/a"); got != f.base {
				t.Errorf("the recorded origin head of patch/a = %.7s, want it left at %.7s", got, f.base)
			}
		})
	}

	t.Run("origin takes the new head", func(t *testing.T) {
		got, err := guard(f.ws, 0).Check(context.Background(), in)
		if err != nil || got != (land.TargetAnswer{}) {
			t.Fatalf("the guard answered %+v, %v; want it to allow the land", got, err)
		}
		if got := f.origin.Head("patch/a"); got != f.commit {
			t.Errorf("origin's patch/a = %.7s, want the new head %.7s", got, f.commit)
		}
		if got := f.recorded("patch/a"); got != f.commit {
			t.Errorf("the recorded origin head of patch/a = %.7s, want the new head %.7s", got, f.commit)
		}
		if got := gitrepotest.Ref(f.copy, "refs/heads/patch/a"); got != f.base {
			t.Errorf("the copy's patch/a = %.7s, want it left at %.7s for the land to move", got, f.base)
		}
	})
}

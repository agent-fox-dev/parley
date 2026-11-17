package carrypatch_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/carrypatch"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/jobs"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

func TestRebuild_PushesTheIntegrationBranch(t *testing.T) {
	t.Parallel()
	const token = "ghp_ORIGINTOKEN"
	cases := []struct {
		name string
		edit []func(w *workspace.Workspace)
		// prepare readies origin and the hub before the attempt and returns
		// the lease the push must carry.
		prepare func(t *testing.T, f *rebuildFixture) string
	}{
		{"over origin's deploy at the recorded value", []func(w *workspace.Workspace){pushOn},
			func(t *testing.T, f *rebuildFixture) string { return f.onOrigin(t, true) }},
		{"origin has no deploy and none is recorded", []func(w *workspace.Workspace){pushOn},
			func(*testing.T, *rebuildFixture) string { return "" }},
		{"the setting turned on during the attempt", nil,
			func(t *testing.T, f *rebuildFixture) string {
				f.hooks.beforePublish(func() {
					if err := f.setSettingNow(pushSetting, true); err != nil {
						t.Errorf("turn the push on: %v", err)
					}
				})
				return ""
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pusher := &recordingPusher{}
			f := startRebuildFixtureWith(t, pusher.pushWith(nil), tc.edit...)
			f.addPatch(t, "p1")
			f.secrets.set(workspace.SecretGitPAT, token)
			f.origin.Accept("x-token-auth", token)
			lease := tc.prepare(t, f)

			out, id := f.attempt(t, nil)
			if out.Kind != attemptCompleted {
				t.Fatalf("the attempt = %+v, want completed", out)
			}
			j := f.job(t, id)
			res := f.result(t, id)
			head, _ := res["integration_head_sha"].(string)
			if j.Status != jobs.StatusCompleted || res["integration_branch_pushed"] != true {
				t.Errorf("the job has status %s and integration_branch_pushed %v, want completed and true", j.Status, res["integration_branch_pushed"])
			}
			if head == "" || head == f.base {
				t.Fatalf("integration_head_sha = %v, want the head with p1", res["integration_head_sha"])
			}
			if got := f.origin.Head("deploy"); got != head {
				t.Errorf("origin's deploy = %q, want the job's integration_head_sha %s", got, head)
			}
			if got := refIn(f.copy, "refs/remotes/origin/deploy"); got != head {
				t.Errorf("refs/remotes/origin/deploy in the hub = %q, want %s", got, head)
			}
			reqs := pusher.requests()
			if len(reqs) != 1 {
				t.Fatalf("%d pushes, want one", len(reqs))
			}
			r := reqs[0]
			if !r.Force || r.Lease != lease || r.Branch != "deploy" || r.Commit != head || r.URL != f.origin.URL() {
				t.Errorf("the push = force %v, lease %q of %s to %s at %s; want a forced push under the lease %q of %s to deploy at %s",
					r.Force, r.Lease, r.Commit, r.Branch, r.URL, lease, head, f.origin.URL())
			}
			if r.Credential != (gitrepo.Credential{Username: "x-token-auth", Password: token}) {
				t.Errorf("the push presented %+v, want the workspace's origin credential", r.Credential)
			}
			if e := f.rebuildEvents(t, f.slug, carrypatch.EventRebuildPushFailed); len(e) != 0 {
				t.Errorf("%d parley.rebuild.push_failed events, want none", len(e))
			}
		})
	}
}

func TestRebuild_PushFailureDoesNotFailTheRebuild(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		deps func(d *carrypatch.Deps)
		// prepare readies origin and the hub before the attempt and returns
		// what origin's deploy must be after it.
		prepare func(t *testing.T, f *rebuildFixture) string
	}{
		{"origin's deploy moved after the hub recorded it", nil,
			func(t *testing.T, f *rebuildFixture) string {
				f.onOrigin(t, true)
				return f.origin.Commit("deploy", "Z")
			}},
		{"origin cannot be reached", func(d *carrypatch.Deps) {
			g := d.Git
			d.Push = func(ctx context.Context, req gitrepo.PushRequest) (gitrepo.PushResult, error) {
				res, err := g.Push(ctx, req)
				if err != nil {
					err = fmt.Errorf("fatal: unable to access 'https://bob:s3cret@host/x.git/': %w (s3cret)", err)
				}
				return res, err
			}
		}, func(t *testing.T, f *rebuildFixture) string {
			f.secrets.set(workspace.SecretGitUsername, "bob")
			f.secrets.set(workspace.SecretGitPassword, "s3cret")
			if err := os.RemoveAll(f.origin.Dir()); err != nil {
				t.Fatal(err)
			}
			return ""
		}},
		{"the push outlasts its time limit", func(d *carrypatch.Deps) { d.RebuildPushDeadline = 50 * time.Millisecond },
			func(t *testing.T, f *rebuildFixture) string {
				f.origin.Block()
				return ""
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := startRebuildFixtureWith(t, tc.deps, pushOn)
			f.addPatch(t, "p1")
			session := "6f1d2c3b-4a5e-4f60-8172-93a4b5c6d7e8"
			wantOrigin := tc.prepare(t, f)
			recorded := refIn(f.copy, "refs/remotes/origin/deploy")

			out, id := f.attemptWith(t, carrypatch.RebuildOptions{SessionID: &session}, nil)
			if out.Kind != attemptCompleted {
				t.Fatalf("the attempt = %+v, want completed", out)
			}
			j := f.job(t, id)
			res := f.result(t, id)
			if j.Status != jobs.StatusCompleted || res["integration_branch_pushed"] != false {
				t.Errorf("the job has status %s and integration_branch_pushed %v, want completed and false", j.Status, res["integration_branch_pushed"])
			}
			head, _ := res["integration_head_sha"].(string)
			if head == "" || refIn(f.copy, "refs/heads/deploy") != head {
				t.Errorf("deploy in the hub = %q, want the job's integration_head_sha %v", refIn(f.copy, "refs/heads/deploy"), res["integration_head_sha"])
			}
			if got := refIn(f.copy, "refs/remotes/origin/deploy"); got != recorded {
				t.Errorf("refs/remotes/origin/deploy in the hub = %q, want it left at %q", got, recorded)
			}
			if wantOrigin != "" {
				if got := f.origin.Head("deploy"); got != wantOrigin {
					t.Errorf("origin's deploy = %q, want it left at %s", got, wantOrigin)
				}
			}

			events := f.rebuildEvents(t, f.slug, carrypatch.EventRebuildPushFailed)
			if len(events) != 1 {
				t.Fatalf("%d parley.rebuild.push_failed events, want one", len(events))
			}
			e := events[0]
			if e.ResourceType != "rebuild" || e.ResourceID != id || e.ActorType != event.ActorSystem || e.ActorID != nil {
				t.Errorf("the event is about %s %s by %s %v, want rebuild %s by system", e.ResourceType, e.ResourceID, e.ActorType, e.ActorID, id)
			}
			if e.SessionID == nil || *e.SessionID != session {
				t.Errorf("the event's session_id = %v, want %s", e.SessionID, session)
			}
			reason, _ := e.Metadata["reason"].(string)
			if e.Metadata["integration_branch"] != "deploy" || reason == "" || len(e.Metadata) != 2 {
				t.Errorf("the event's metadata = %v, want integration_branch deploy and a reason", e.Metadata)
			}
			for _, hidden := range []string{"s3cret", "bob:"} {
				if strings.Contains(reason, hidden) {
					t.Errorf("the event's reason %q holds %q", reason, hidden)
				}
			}
		})
	}
}

func TestRebuild_NoPushWhenTheSettingIsOff(t *testing.T) {
	t.Parallel()
	pusher := &recordingPusher{}
	f := startRebuildFixtureWith(t, pusher.pushWith(nil))
	f.addPatch(t, "p1")

	out, id := f.attempt(t, nil)
	if out.Kind != attemptCompleted {
		t.Fatalf("the attempt = %+v, want completed", out)
	}
	if n := len(pusher.requests()); n != 0 {
		t.Errorf("%d pushes, want none", n)
	}
	if n := f.origin.Requests(); n != 0 {
		t.Errorf("origin received %d requests, want none", n)
	}
	if res := f.result(t, id); res["integration_branch_pushed"] != false {
		t.Errorf("integration_branch_pushed = %v, want false", res["integration_branch_pushed"])
	}
	if e := f.rebuildEvents(t, f.slug, carrypatch.EventRebuildPushFailed); len(e) != 0 {
		t.Errorf("%d parley.rebuild.push_failed events, want none", len(e))
	}
}

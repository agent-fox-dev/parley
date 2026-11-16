package carrypatch_test

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/carrypatch"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// The steps of the set-up a fake can fail.
const (
	stepRecord = "record"
	stepFetch  = "fetch"
	stepRerere = "rerere"
	stepBranch = "branch"
)

// fakeSetup is the workspace store, the upstream fetcher and the copy of a
// set-up: it counts the calls of each step and fails the one fail names.
type fakeSetup struct {
	fail   string
	calls  map[string]int
	target string // the id the integration branch was written at
}

func (f *fakeSetup) step(name string) error {
	f.calls[name]++
	if f.fail == name {
		return errors.New("the " + name + " step broke")
	}
	return nil
}

func (f *fakeSetup) DefaultIntegrationBranch(_ context.Context, _, branch string) error {
	if branch != "deploy" {
		return errors.New("wrote " + branch + ", want deploy")
	}
	return f.step(stepRecord)
}

func (f *fakeSetup) FetchUpstream(context.Context, workspace.Workspace) error {
	return f.step(stepFetch)
}

func (f *fakeSetup) UpstreamBase(context.Context, workspace.Workspace) (string, error) {
	return "base", nil
}

func (f *fakeSetup) SetConfig(_ context.Context, key, value string) error {
	if key != "rerere.enabled" || value != "true" {
		return errors.New("set " + key + "=" + value + ", want rerere.enabled=true")
	}
	return f.step(stepRerere)
}

func (f *fakeSetup) SetRef(_ context.Context, ref, id string) error {
	if ref != "refs/heads/deploy" {
		return errors.New("wrote " + ref + ", want refs/heads/deploy")
	}
	f.target = id
	return f.step(stepBranch)
}

func (f *fakeSetup) CompareAndSwapRef(context.Context, string, string, string) error {
	return errors.New("the post-clone set-up compared and swapped, want an unconditional write")
}

func TestSetup_ContinuesAfterAFailedStep(t *testing.T) {
	t.Parallel()
	head := "head"
	w := workspace.Workspace{Slug: "acme", Mode: workspace.ModeCarryPatch, HeadSHA: &head}
	cases := []struct {
		fail   string
		named  string // what the error names
		target string // where the integration branch goes
	}{
		{stepRecord, "integration_branch", "base"},
		{stepFetch, "upstream fetch", "head"},
		{stepRerere, "rerere.enabled", "base"},
		{stepBranch, "refs/heads/deploy", "base"},
	}
	for _, c := range cases {
		t.Run(c.fail, func(t *testing.T) {
			t.Parallel()
			f := &fakeSetup{fail: c.fail, calls: map[string]int{}}
			err := carrypatch.Setup{Record: f, Upstream: f, Copy: f, Replace: true}.Run(context.Background(), w)
			if err == nil || !strings.Contains(err.Error(), c.named) {
				t.Errorf("Run = %v, want an error naming %q", err, c.named)
			}
			if got := errors.Is(err, carrypatch.ErrUpstreamFetch); got != (c.fail == stepFetch) {
				t.Errorf("errors.Is(err, ErrUpstreamFetch) = %v, want %v", got, c.fail == stepFetch)
			}
			want := map[string]int{stepRecord: 1, stepFetch: 1, stepRerere: 1, stepBranch: 1}
			if !maps.Equal(f.calls, want) {
				t.Errorf("the steps were called %v, want each once %v", f.calls, want)
			}
			if c.fail != stepBranch && f.target != c.target {
				t.Errorf("the integration branch was written at %q, want %q", f.target, c.target)
			}
		})
	}
}

func TestSetup_RunsAfterFreshCloneBeforeReady(t *testing.T) {
	t.Parallel()
	origin := gitrepotest.NewOrigin(t)
	origin.Commit("main", "o1")
	up := gitrepotest.NewOrigin(t)
	u := up.Commit("main", "u1")
	release := up.Commit("release", "r1")
	h := startCarryHub(t, origin, up)

	h.create(t, "variant", origin, carryPatch(up, map[string]any{"integration_branch": "integration"}))
	h.create(t, "plain", origin, nil)

	w := h.waitReady(t, "variant")
	if got := h.observedStatus("variant"); got != workspace.StatusCloning {
		t.Errorf("the set-up observed status %q, want cloning", got)
	}
	if w.CloneError != nil || w.UpstreamHeadSHA != nil {
		t.Errorf("clone_error %s, upstream_head_sha %s; want both null", str(w.CloneError), str(w.UpstreamHeadSHA))
	}
	dir := h.copyOf("variant")
	if got := refIn(dir, "refs/heads/integration"); got != u {
		t.Errorf("refs/heads/integration = %q, want the upstream base %s", got, u)
	}
	if got := configIn(dir, "rerere.enabled"); got != "true" {
		t.Errorf("rerere.enabled = %q, want true", got)
	}
	wantUpstream := map[string]string{"main": u, "release": release}
	if got := gitrepotest.Refs(t, dir, "refs/remotes/upstream/"); !maps.Equal(got, wantUpstream) {
		t.Errorf("the upstream refs = %v, want %v", got, wantUpstream)
	}
	if got := h.eventTypes(t, "variant"); !slices.Equal(got, []string{"parley.workspace.create"}) {
		t.Errorf("the events of the workspace = %v, want its create alone", got)
	}

	h.waitReady(t, "plain")
	plain := h.copyOf("plain")
	if got := gitrepotest.Refs(t, plain, "refs/remotes/upstream/"); len(got) != 0 {
		t.Errorf("the standard workspace's copy has upstream refs %v, want none", got)
	}
	if got := refIn(plain, "refs/heads/integration"); got != "" {
		t.Errorf("the standard workspace's copy has an integration branch at %s, want none", got)
	}
}

func TestSetup_DefaultsIntegrationBranchToDeploy(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		stored *string
		want   string
	}{
		{nil, "deploy"},
		{new("integration"), "integration"},
	} {
		if got := carrypatch.IntegrationBranch(workspace.Workspace{IntegrationBranch: c.stored}); got != c.want {
			t.Errorf("IntegrationBranch with %s stored = %q, want %q", str(c.stored), got, c.want)
		}
	}

	origin := gitrepotest.NewOrigin(t)
	origin.Commit("main", "o1")
	up := gitrepotest.NewOrigin(t)
	u := up.Commit("main", "u1")
	h := startCarryHub(t, origin, up)

	// The clone waits for origin, so that the record is read before the
	// set-up runs.
	origin.Block()
	h.create(t, "variant", origin, carryPatch(up, nil))
	before := h.record(t, "variant")
	origin.Release()
	if before.IntegrationBranch != nil || carrypatch.IntegrationBranch(before) != "deploy" {
		t.Errorf("before the set-up: stored %s, IntegrationBranch %q; want null and deploy",
			str(before.IntegrationBranch), carrypatch.IntegrationBranch(before))
	}
	w := h.waitReady(t, "variant")
	if str(w.IntegrationBranch) != "deploy" {
		t.Errorf("the stored integration_branch = %s, want deploy", str(w.IntegrationBranch))
	}
	if got := refIn(h.copyOf("variant"), "refs/heads/deploy"); got != u {
		t.Errorf("refs/heads/deploy = %q, want upstream's main %s", got, u)
	}

	h.create(t, "named", origin, carryPatch(up, map[string]any{"integration_branch": "integration"}))
	w = h.waitReady(t, "named")
	if str(w.IntegrationBranch) != "integration" || carrypatch.IntegrationBranch(w) != "integration" {
		t.Errorf("stored %s, IntegrationBranch %q; want integration both", str(w.IntegrationBranch), carrypatch.IntegrationBranch(w))
	}
	if got := refIn(h.copyOf("named"), "refs/heads/integration"); got != u {
		t.Errorf("refs/heads/integration = %q, want upstream's main %s", got, u)
	}
	if got := refIn(h.copyOf("named"), "refs/heads/deploy"); got != "" {
		t.Errorf("refs/heads/deploy = %q in a workspace whose integration branch is integration, want none", got)
	}
}

func TestSetup_UpstreamFailureStillReady(t *testing.T) {
	t.Parallel()
	const wrongToken = "upstream-token-wrong-7c1d"
	origin := gitrepotest.NewOrigin(t)
	head := origin.Commit("main", "o1")
	missing := gitrepotest.NewOrigin(t)
	missing.Commit("main", "m1")
	refusing := gitrepotest.NewOrigin(t, gitrepotest.WithBasicAuth("x-token-auth", "upstream-token-right"))
	refusing.Commit("main", "r1")
	devOnly := gitrepotest.NewOrigin(t)
	devOnly.Commit("dev", "d1") // HEAD names main, which it lacks
	h := startCarryHub(t, origin, missing, refusing, devOnly)

	cases := []struct {
		name, slug, upstream string
	}{
		{"the upstream URL names no repository", "nowhere", strings.TrimSuffix(missing.URL(), "origin.git") + "missing.git"},
		{"the upstream refuses the credential", "refused", refusing.URL()},
		{"no upstream base after the fetch", "devonly", devOnly.URL()},
	}
	// The clones wait for origin, so that the secret is stored before the
	// set-up reads it.
	origin.Block()
	for _, c := range cases {
		h.create(t, c.slug, origin, map[string]any{"workspace_mode": "carry_patch", "upstream_url": c.upstream})
	}
	h.putSecret(t, "refused", carrypatch.SecretUpstreamGitPAT, wrongToken)
	origin.Release()

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := h.waitReady(t, c.slug)
			if w.CloneError != nil {
				t.Errorf("clone_error %s, want null", *w.CloneError)
			}
			dir := h.copyOf(c.slug)
			if got := refIn(dir, "refs/heads/deploy"); got != head || str(w.HeadSHA) != head {
				t.Errorf("refs/heads/deploy = %q, head_sha %s; want origin's main %s for both", got, str(w.HeadSHA), head)
			}
			if got := configIn(dir, "rerere.enabled"); got != "true" {
				t.Errorf("rerere.enabled = %q, want true", got)
			}
			var lines []string
			for line := range strings.Lines(h.Log.String()) {
				var rec map[string]any
				if json.Unmarshal([]byte(line), &rec) == nil && rec["slug"] == c.slug && rec["msg"] == "workspace post-clone step failed" {
					lines = append(lines, line)
				}
			}
			if len(lines) != 1 || !strings.Contains(lines[0], "set-up") {
				t.Errorf("the log has %d lines of the failed step of %s, want one naming the set-up: %q", len(lines), c.slug, lines)
			}
		})
	}
	if strings.Contains(h.Log.String(), wrongToken) {
		t.Errorf("the hub's log holds the upstream token:\n%s", h.Log.String())
	}
}

func TestSetup_IntegrationBranchDoesNotFollowOrigin(t *testing.T) {
	t.Parallel()
	origin := gitrepotest.NewOrigin(t)
	origin.Commit("main", "o1")
	x := origin.Commit("deploy", "the origin's own deploy")
	up := gitrepotest.NewOrigin(t)
	u := up.Commit("main", "u1")
	h := startCarryHub(t, origin, up)

	h.create(t, "variant", origin, carryPatch(up, nil))
	h.waitReady(t, "variant")
	dir := h.copyOf("variant")
	if got := refIn(dir, "refs/heads/deploy"); got != u || got == x {
		t.Errorf("refs/heads/deploy = %q, want the upstream base %s, not origin's deploy %s", got, u, x)
	}
	if got := refIn(dir, "refs/remotes/origin/deploy"); got != x {
		t.Errorf("refs/remotes/origin/deploy = %q, want origin's deploy %s as the clone recorded it", got, x)
	}
}

func TestEnsureSetup_RepeatsOnlyWhatIsMissing(t *testing.T) {
	t.Parallel()
	origin := gitrepotest.NewOrigin(t)
	head := origin.Commit("main", "o1")
	up := gitrepotest.NewOrigin(t)
	u := up.Commit("main", "u1")
	up.Refuse(503)
	h := startCarryHub(t, origin, up)
	ctx := context.Background()

	h.create(t, "variant", origin, carryPatch(up, nil))
	h.waitReady(t, "variant")
	dir := h.copyOf("variant")
	if got, upstream := refIn(dir, "refs/heads/deploy"), gitrepotest.Refs(t, dir, "refs/remotes/upstream/"); got != head || len(upstream) != 0 {
		t.Fatalf("after the set-up with upstream unreachable: deploy = %q, upstream refs %v; want %s and none", got, upstream, head)
	}

	up.Refuse(0)
	if err := h.comp.EnsureSetup(ctx, h.record(t, "variant")); err != nil {
		t.Errorf("EnsureSetup with upstream reachable = %v, want nil", err)
	}
	if got := refIn(dir, "refs/remotes/upstream/main"); got != u {
		t.Errorf("refs/remotes/upstream/main = %q, want %s", got, u)
	}
	if got := refIn(dir, "refs/heads/deploy"); got != head {
		t.Errorf("refs/heads/deploy = %q, want it left at %s", got, head)
	}

	gitrepotest.Git(t, dir, "update-ref", "-d", "refs/heads/deploy")
	gitrepotest.Git(t, dir, "config", "--local", "--unset", "rerere.enabled")
	if err := h.comp.EnsureSetup(ctx, h.record(t, "variant")); err != nil {
		t.Errorf("EnsureSetup with the branch and rerere gone = %v, want nil", err)
	}
	if got := refIn(dir, "refs/heads/deploy"); got != u {
		t.Errorf("refs/heads/deploy = %q, want it created at the upstream base %s", got, u)
	}
	if got := configIn(dir, "rerere.enabled"); got != "true" {
		t.Errorf("rerere.enabled = %q, want true", got)
	}

	before := gitrepotest.Refs(t, dir, "")
	if err := h.comp.EnsureSetup(ctx, h.record(t, "variant")); err != nil {
		t.Errorf("EnsureSetup on a complete set-up = %v, want nil", err)
	}
	if after := gitrepotest.Refs(t, dir, ""); !maps.Equal(after, before) {
		t.Errorf("the refs after a repeat on a complete set-up = %v, want them unchanged %v", after, before)
	}
}

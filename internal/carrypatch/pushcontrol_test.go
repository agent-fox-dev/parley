package carrypatch_test

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/carrypatch"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/gitserver/pushrule"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// The commits the decision tables move refs between: controlFF descends
// from controlBase, controlForced does not.
const (
	controlBase   = "1111111111111111111111111111111111111111"
	controlFF     = "2222222222222222222222222222222222222222"
	controlForced = "3333333333333333333333333333333333333333"
)

// controlUpdate is one kind of ref update, by name.
type controlUpdate struct {
	name          string
	before, after string
}

var (
	updCreate  = controlUpdate{"create", pushrule.ZeroID, controlFF}
	updFF      = controlUpdate{"fast-forward", controlBase, controlFF}
	updForced  = controlUpdate{"forced", controlBase, controlForced}
	updDelete  = controlUpdate{"delete", controlBase, pushrule.ZeroID}
	allUpdates = []controlUpdate{updCreate, updFF, updForced, updDelete}
)

// of is the update of ref of this kind.
func (k controlUpdate) of(ref string) pushrule.Update {
	return pushrule.Update{Ref: ref, Before: k.before, After: k.after}
}

// controlCaller is a caller level, by name.
type controlCaller struct {
	name   string
	caller pushrule.Caller
}

var allCallers = []controlCaller{
	{"admin", pushrule.Caller{Level: access.LevelAdmin, CredentialType: pushrule.CredentialAdminToken}},
	{"owner", pushrule.Caller{Level: access.LevelOwner, CredentialType: pushrule.CredentialAPIKey, CredentialID: "key-owner", UserID: "u-owner"}},
	{"member", pushrule.Caller{Level: access.LevelMember, CredentialType: pushrule.CredentialPAT, CredentialID: "pat-member", UserID: "u-member"}},
}

// controlSettings are the two patch settings of a decision.
type controlSettings struct {
	source  string
	forward bool
}

func (s controlSettings) values() map[string]any {
	return map[string]any{"patch_branch_source": s.source, "push_patches_to_origin": s.forward}
}

func (s controlSettings) String() string {
	if s.forward {
		return s.source + "/true"
	}
	return s.source + "/false"
}

var allSettings = []controlSettings{{"hub", false}, {"hub", true}, {"origin", false}, {"origin", true}}

// lineage is the repository the engine asks whether an update is forced:
// controlBase is an ancestor of controlFF and of nothing else.
type lineage struct{}

func (lineage) IsAncestor(_ context.Context, ancestor, descendant string) (bool, error) {
	return ancestor == descendant || (ancestor == controlBase && descendant == controlFF), nil
}

func (lineage) NewCommits(context.Context, []string, string) ([]string, error) { return nil, nil }

func (lineage) ChangedPaths(context.Context, string) ([]string, error) { return nil, nil }

func (lineage) ObjectPaths(context.Context, string) (string, []string, error) {
	return "commit", nil, nil
}

// recordStub is the workspace record lookup of the controller.
type recordStub struct {
	mu      sync.Mutex
	records map[string]workspace.Workspace
	err     error
}

func (r *recordStub) WorkspaceRecord(_ context.Context, slug string) (workspace.Workspace, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return workspace.Workspace{}, false, r.err
	}
	w, ok := r.records[slug]
	return w, ok, nil
}

// settingsStub is an in-memory settings reader that counts its reads.
type settingsStub struct {
	mu     sync.Mutex
	values map[string]any
	err    error
	reads  int
}

func (s *settingsStub) EffectiveSettings(context.Context, workspace.Workspace) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	if s.err != nil {
		return nil, s.err
	}
	return maps.Clone(s.values), nil
}

func (s *settingsStub) set(key string, v any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[key] = v
}

func (s *settingsStub) readCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reads
}

// patchStub is a patch list held in memory, which answers for live patches
// only and counts its reads; with forbidden set, a read fails the test.
type patchStub struct {
	t         *testing.T
	mu        sync.Mutex
	patches   []carrypatch.Patch
	err       error
	forbidden bool
	reads     int
}

func (p *patchStub) LivePatchOfBranch(_ context.Context, slug, branch string) (carrypatch.Patch, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reads++
	if p.forbidden {
		p.t.Errorf("the patch list was read for %s of %s", branch, slug)
	}
	if p.err != nil {
		return carrypatch.Patch{}, false, p.err
	}
	for _, q := range p.patches {
		if q.Workspace == slug && q.BranchName == branch && q.Status != carrypatch.StatusDeleted {
			return q, true, nil
		}
	}
	return carrypatch.Patch{}, false, nil
}

func (p *patchStub) readCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.reads
}

// forwardStub records the forwards it is asked to run, and the repository
// each was handed.
type forwardStub struct {
	mu    sync.Mutex
	calls []string
	repos []*gitrepo.Repo
	err   error
}

func (f *forwardStub) Forward(_ context.Context, repo *gitrepo.Repo, w workspace.Workspace, branch, commit string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, w.Slug+" "+branch+" "+commit)
	f.repos = append(f.repos, repo)
	return f.err
}

func (f *forwardStub) asked() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// controlFixture is a controller over in-memory readers for the carry-patch
// workspace demo, whose default branch is main.
type controlFixture struct {
	records  *recordStub
	settings *settingsStub
	patches  *patchStub
	forwards *forwardStub
	control  *carrypatch.PushControl
}

// demoPushWorkspace is the workspace as the engine hands it over.
var demoPushWorkspace = pushrule.Workspace{Slug: "demo", Kind: access.KindRepository, DefaultBranch: "main", Mode: workspace.ModeCarryPatch}

// controlRecord is the record of demo with the integration branch and the
// origin URL given; nil leaves the field unset.
func controlRecord(integration, gitURL *string) workspace.Workspace {
	main := "main"
	return workspace.Workspace{
		Slug: "demo", Kind: access.KindRepository, Mode: workspace.ModeCarryPatch,
		DefaultBranch: &main, IntegrationBranch: integration, GitURL: gitURL,
	}
}

func ptr(s string) *string { return &s }

func newControl(t *testing.T, rec workspace.Workspace, settings map[string]any, patches ...carrypatch.Patch) *controlFixture {
	t.Helper()
	f := &controlFixture{
		records:  &recordStub{records: map[string]workspace.Workspace{rec.Slug: rec}},
		settings: &settingsStub{values: settings},
		patches:  &patchStub{t: t, patches: patches},
		forwards: &forwardStub{},
	}
	f.control = &carrypatch.PushControl{Records: f.records, Settings: f.settings, Patches: f.patches, Forwarder: f.forwards}
	return f
}

// controlPatch is the patch of branch in demo with status and position.
func controlPatch(branch, status string, position int) carrypatch.Patch {
	p := carrypatch.Patch{ID: "patch-" + branch, Workspace: "demo", BranchName: branch, Status: status}
	if status != carrypatch.StatusDeleted {
		p.Position = &position
	}
	return p
}

// decide asks the real rule engine, with the controller registered, about
// u by caller, and returns its decision, failing the test when deciding ran
// a forward.
func (f *controlFixture) decide(t *testing.T, caller pushrule.Caller, u pushrule.Update) pushrule.Decision {
	t.Helper()
	e := &pushrule.Engine{}
	e.RegisterController(f.control)
	before := len(f.forwards.asked())
	v := e.Evaluate(context.Background(), pushrule.Request{
		Workspace: demoPushWorkspace, Caller: caller, Updates: []pushrule.Update{u}, Repo: lineage{},
	})
	if n := len(f.forwards.asked()); n != before {
		t.Errorf("deciding %s ran %d forwards, want none", u.Ref, n-before)
	}
	return v.Decisions[0]
}

// wantRefused fails the test unless d refuses its update with message and
// errorType and carries no forward.
func wantRefused(t *testing.T, name string, d pushrule.Decision, message, errorType string) {
	t.Helper()
	if d.Allowed || d.Message != message || d.ErrorType != errorType || d.Forward != nil {
		t.Errorf("%s: allowed %v, %q (%s), forward %v; want refused with %q (%s) and no forward",
			name, d.Allowed, d.Message, d.ErrorType, d.Forward != nil, message, errorType)
	}
}

// wantAllowed fails the test unless d allows its update, with a forward
// when forward is set and without one otherwise.
func wantAllowed(t *testing.T, name string, d pushrule.Decision, forward bool) {
	t.Helper()
	if !d.Allowed || (d.Forward != nil) != forward {
		t.Errorf("%s: allowed %v (%q %s), forward %v; want allowed with forward %v",
			name, d.Allowed, d.Message, d.ErrorType, d.Forward != nil, forward)
	}
}

func TestPushControl_RefusesEveryWriteOfTheIntegrationBranchWithoutReadingThePatchList(t *testing.T) {
	t.Parallel()
	const deployMsg = "integration branch deploy is maintained by the hub; push a patch branch and rebuild instead"
	for _, rec := range []struct {
		name        string
		integration *string
		ref, want   string
	}{
		{"deploy", ptr("deploy"), "refs/heads/deploy", deployMsg},
		{"release/int", ptr("release/int"), "refs/heads/release/int",
			"integration branch release/int is maintained by the hub; push a patch branch and rebuild instead"},
		{"none recorded", nil, "refs/heads/deploy", deployMsg},
	} {
		for _, s := range allSettings {
			f := newControl(t, controlRecord(rec.integration, ptr("https://example.test/acme/demo.git")), s.values(),
				controlPatch("patch/a", carrypatch.StatusActive, 1))
			f.patches.forbidden = true
			for _, u := range allUpdates {
				for _, c := range allCallers {
					name := rec.name + " " + s.String() + " " + u.name + " by " + c.name
					wantRefused(t, name, f.decide(t, c.caller, u.of(rec.ref)), rec.want, pushrule.ErrorTypeIntegrationBranch)
				}
			}
			if n := f.settings.readCount(); n != 0 {
				t.Errorf("%s %s: the settings were read %d times, want none", rec.name, s, n)
			}
		}
	}
}

func TestPushControl_AllowsWhatIsNeitherTheIntegrationBranchNorALivePatchBranch(t *testing.T) {
	t.Parallel()
	for _, ref := range []string{"refs/tags/deploy", "refs/notes/deploy", "refs/heads/patch/old", "refs/heads/feature/x", "refs/heads/deploy-2"} {
		for _, u := range []controlUpdate{updCreate, updFF, updDelete} {
			f := newControl(t, controlRecord(ptr("deploy"), ptr("https://example.test/acme/demo.git")),
				controlSettings{"origin", false}.values(),
				controlPatch("patch/a", carrypatch.StatusActive, 1), controlPatch("patch/old", carrypatch.StatusDeleted, 0))
			wantAllowed(t, ref+" "+u.name, f.decide(t, allCallers[1].caller, u.of(ref)), false)
			if !strings.HasPrefix(ref, "refs/heads/") && f.patches.readCount() != 0 {
				t.Errorf("%s %s: the patch list was read %d times, want none", ref, u.name, f.patches.readCount())
			}
		}
	}
}

func TestPushControl_AllowsEveryWriteOfAPatchBranchWhenTheHubIsItsAuthority(t *testing.T) {
	t.Parallel()
	patches := []carrypatch.Patch{
		controlPatch("patch/a", carrypatch.StatusActive, 1),
		controlPatch("patch/b", carrypatch.StatusConflict, 2),
		controlPatch("patch/c", carrypatch.StatusDisabled, 3),
		controlPatch("patch/d", carrypatch.StatusMergedUpstream, 4),
	}
	for _, forward := range []bool{false, true} {
		s := controlSettings{"hub", forward}
		f := newControl(t, controlRecord(ptr("deploy"), ptr("https://example.test/acme/demo.git")), s.values(), patches...)
		want := slices.Clone(patches)
		for _, p := range patches {
			for _, u := range allUpdates {
				name := s.String() + " " + p.BranchName + " (" + p.Status + ") " + u.name
				wantAllowed(t, name, f.decide(t, allCallers[2].caller, u.of("refs/heads/"+p.BranchName)), false)
			}
		}
		// The controller only reads: a delete leaves the patch where it was.
		f.patches.mu.Lock()
		if !slices.EqualFunc(f.patches.patches, want, func(a, b carrypatch.Patch) bool {
			return a.BranchName == b.BranchName && a.Status == b.Status && *a.Position == *b.Position
		}) {
			t.Errorf("%s: the patch list is %+v after the decisions, want %+v", s, f.patches.patches, want)
		}
		f.patches.mu.Unlock()
	}
}

func TestPushControl_RefusesAPatchBranchSyncedFromOriginWithItsURLWithoutUserInformation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ gitURL, shown string }{
		{"https://example.test/acme/demo.git", "https://example.test/acme/demo.git"},
		{"https://bot:s3cret@example.test/acme/demo.git", "https://example.test/acme/demo.git"},
		{"git@example.test:acme/demo.git", "git@example.test:acme/demo.git"},
	} {
		f := newControl(t, controlRecord(ptr("deploy"), ptr(tc.gitURL)), controlSettings{"origin", false}.values(),
			controlPatch("patch/a", carrypatch.StatusActive, 1))
		want := "branch is synced from origin; push to " + tc.shown + " instead"
		for _, u := range allUpdates {
			for _, c := range allCallers {
				name := tc.gitURL + " " + u.name + " by " + c.name
				d := f.decide(t, c.caller, u.of("refs/heads/patch/a"))
				wantRefused(t, name, d, want, pushrule.ErrorTypePatchBranch)
				if strings.Contains(d.Message, "s3cret") || strings.Contains(d.Message, "bot") {
					t.Errorf("%s: the message %q shows the URL's user information", name, d.Message)
				}
			}
		}
	}
}

func TestPushControl_ReadsThePatchSettingsAtEveryDecision(t *testing.T) {
	t.Parallel()
	f := newControl(t, controlRecord(ptr("deploy"), ptr("https://example.test/acme/demo.git")), map[string]any{},
		controlPatch("patch/a", carrypatch.StatusActive, 1))
	u := updFF.of("refs/heads/patch/a")
	owner := allCallers[1].caller

	wantAllowed(t, "no settings", f.decide(t, owner, u), false)
	f.settings.set("patch_branch_source", "origin")
	wantRefused(t, "origin/missing", f.decide(t, owner, u),
		"branch is synced from origin; push to https://example.test/acme/demo.git instead", pushrule.ErrorTypePatchBranch)
	f.settings.set("push_patches_to_origin", true)
	wantAllowed(t, "origin/true", f.decide(t, owner, u), true)
	f.settings.set("patch_branch_source", "hub")
	wantAllowed(t, "hub/true", f.decide(t, owner, u), false)

	if n := f.settings.readCount(); n != 4 {
		t.Errorf("the settings were read %d times in 4 decisions, want once each", n)
	}
}

func TestPushControl_ForwardsAPatchBranchSyncedFromOriginOnlyAfterDeciding(t *testing.T) {
	t.Parallel()
	for _, u := range []controlUpdate{updCreate, updFF, updForced} {
		f := newControl(t, controlRecord(ptr("deploy"), ptr("https://example.test/acme/demo.git")), controlSettings{"origin", true}.values(),
			controlPatch("patch/a", carrypatch.StatusActive, 1))
		d := f.decide(t, allCallers[2].caller, u.of("refs/heads/patch/a"))
		wantAllowed(t, u.name, d, true)
		if d.Forward == nil {
			continue
		}
		// The forward is bound to the workspace, the branch and the update's
		// after commit, pushes from the repository the ref writer hands it,
		// and its failure is the forward's.
		f.forwards.err = errors.New("origin rejected push: frozen")
		repo := new(gitrepo.Repo)
		if err := d.Forward(context.Background(), repo); err == nil || err.Error() != "origin rejected push: frozen" {
			t.Errorf("%s: the forward returned %v, want the forwarder's error", u.name, err)
		}
		if got, want := f.forwards.asked(), []string{"demo patch/a " + u.after}; !slices.Equal(got, want) {
			t.Errorf("%s: forwarded %q, want %q", u.name, got, want)
		}
		if len(f.forwards.repos) != 1 || f.forwards.repos[0] != repo {
			t.Errorf("%s: the forwarder was handed %v, want the ref writer's repository", u.name, f.forwards.repos)
		}
	}

	f := newControl(t, controlRecord(ptr("deploy"), ptr("https://example.test/acme/demo.git")), controlSettings{"origin", true}.values(),
		controlPatch("patch/a", carrypatch.StatusActive, 1))
	wantRefused(t, "delete", f.decide(t, allCallers[1].caller, updDelete.of("refs/heads/patch/a")),
		"branch is synced from origin; delete it on origin instead", pushrule.ErrorTypePatchBranch)
	if got := f.forwards.asked(); len(got) != 0 {
		t.Errorf("delete: forwarded %q, want nothing", got)
	}
}

func TestPushControl_FailsInsteadOfAllowingWhenItCannotDecide(t *testing.T) {
	t.Parallel()
	fault := errors.New("disk on fire")
	for _, tc := range []struct {
		name    string
		ref     string
		breakIt func(f *controlFixture)
		// refusal is the engine's answer: internal unless the controller
		// decided without the broken reader.
		message, errorType string
		decideErr          bool
	}{
		{"settings unreadable", "refs/heads/feature/x", func(f *controlFixture) { f.settings.err = fault },
			"internal error; try again", pushrule.ErrorTypeInternal, true},
		{"patch list unreadable", "refs/heads/feature/x", func(f *controlFixture) { f.patches.err = fault },
			"internal error; try again", pushrule.ErrorTypeInternal, true},
		{"record unreadable", "refs/heads/feature/x", func(f *controlFixture) { f.records.err = fault },
			"internal error; try again", pushrule.ErrorTypeInternal, true},
		{"record missing", "refs/heads/feature/x", func(f *controlFixture) { f.records.records = nil },
			"internal error; try again", pushrule.ErrorTypeInternal, true},
		{"no forwarder for a forward", "refs/heads/patch/a", func(f *controlFixture) {
			f.settings.values = controlSettings{"origin", true}.values()
			f.control.Forwarder = nil
		}, "internal error; try again", pushrule.ErrorTypeInternal, true},
		{"patch list unreadable, integration branch", "refs/heads/deploy", func(f *controlFixture) { f.patches.err = fault },
			"integration branch deploy is maintained by the hub; push a patch branch and rebuild instead", pushrule.ErrorTypeIntegrationBranch, false},
	} {
		f := newControl(t, controlRecord(ptr("deploy"), ptr("https://example.test/acme/demo.git")), controlSettings{"hub", false}.values(),
			controlPatch("patch/a", carrypatch.StatusActive, 1))
		tc.breakIt(f)
		u := updCreate.of(tc.ref)
		_, err := f.control.Decide(context.Background(), demoPushWorkspace, allCallers[0].caller, u)
		if (err != nil) != tc.decideErr {
			t.Errorf("%s: Decide returned %v, want an error %v", tc.name, err, tc.decideErr)
		}
		wantRefused(t, tc.name, f.decide(t, allCallers[0].caller, u), tc.message, tc.errorType)
	}
}

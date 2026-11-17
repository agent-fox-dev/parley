package carrypatch_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/carrypatch"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/gitserver/pushrule"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// The kinds of update the trigger tables make, of a branch by name.
func updateOf(branch string) pushrule.Update {
	return updFF.of(branchRef(branch))
}

func createOf(branch string) pushrule.Update {
	return updCreate.of(branchRef(branch))
}

func deleteOf(branch string) pushrule.Update {
	return updDelete.of(branchRef(branch))
}

func branchRef(branch string) string {
	if strings.HasPrefix(branch, "refs/") {
		return branch
	}
	return "refs/heads/" + branch
}

// triggerFixture is the trigger of a bare component over in-memory readers:
// the carry-patch workspace demo with patch/a and patch/b active and
// patch/old deleted, the carry-patch workspace other with patch/a, the
// standard workspace std and the task fork fork of demo, a recording
// rebuild enqueuer, a mirror that records what it is asked to push in
// place of the component's, an event writer that counts its calls, and a
// log that keeps every record.
type triggerFixture struct {
	comp     *carrypatch.Component
	trigger  *carrypatch.Trigger
	records  *recordStub
	settings *settingsStub
	patches  *patchStub
	rebuilds *recordingRebuilds
	mirrors  *recordingMirror
	events   *countingEvents
	log      *hubtest.LogBuffer
}

func newTrigger(t *testing.T, settings map[string]any) *triggerFixture {
	t.Helper()
	demo := controlRecord(ptr("deploy"), ptr("https://example.test/acme/demo.git"))
	other := demo
	other.Slug = "other"
	std := demo
	std.Slug, std.Mode = "std", workspace.ModeStandard
	fork := std
	fork.Slug, fork.Kind, fork.ParentSlug = "fork", access.KindFork, ptr("demo")
	otherPatch := controlPatch("patch/a", carrypatch.StatusActive, 1)
	otherPatch.Workspace = "other"
	f := &triggerFixture{
		records: &recordStub{records: map[string]workspace.Workspace{
			"demo": demo, "other": other, "std": std, "fork": fork,
		}},
		settings: &settingsStub{values: settings},
		patches: &patchStub{t: t, patches: []carrypatch.Patch{
			controlPatch("patch/a", carrypatch.StatusActive, 1),
			controlPatch("patch/b", carrypatch.StatusActive, 2),
			controlPatch("patch/old", carrypatch.StatusDeleted, 0),
			otherPatch,
		}},
		rebuilds: &recordingRebuilds{},
		mirrors:  &recordingMirror{},
		events:   &countingEvents{},
		log:      &hubtest.LogBuffer{},
	}
	f.comp = &carrypatch.Component{}
	f.comp.Bind(carrypatch.Deps{
		Events: f.events,
		Logger: slog.New(slog.NewTextHandler(f.log, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	f.comp.RegisterRebuildEnqueuer(f.rebuilds)
	f.trigger = f.comp.Trigger()
	f.trigger.Records, f.trigger.Settings, f.trigger.Patches = f.records, f.settings, f.patches
	f.trigger.Mirror = f.mirrors.mirror
	return f
}

// recordingMirror records each mirror the trigger runs as the workspace
// and the branches it is asked to push.
type recordingMirror struct {
	mu   sync.Mutex
	runs []string
}

func (m *recordingMirror) mirror(_ context.Context, w workspace.Workspace, patches []carrypatch.Patch) {
	branches := make([]string, len(patches))
	for i, p := range patches {
		branches[i] = p.BranchName
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.runs = append(m.runs, w.Slug+" "+strings.Join(branches, ","))
}

// wantMirrors fails the test unless the mirrors run are want, each as
// "<workspace> <branch>,<branch>...".
func (m *recordingMirror) wantMirrors(t *testing.T, want ...string) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if fmt.Sprint(m.runs) != fmt.Sprint(want) {
		t.Errorf("mirrors run %q, want %q", m.runs, want)
	}
}

// hubPatches are the patch settings with the hub as the authority and
// auto_rebuild_after_push as given; nil leaves the key out.
func hubPatches(autoRebuild any) map[string]any {
	s := map[string]any{"patch_branch_source": "hub", "push_patches_to_origin": false}
	if autoRebuild != nil {
		s["auto_rebuild_after_push"] = autoRebuild
	}
	return s
}

// after runs the trigger and fails the test if it panicked.
func (f *triggerFixture) after(t *testing.T, slug string, updates []pushrule.Update, source carrypatch.TriggerSource, session *string) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("AfterUpdates panicked: %v", r)
		}
	}()
	f.trigger.AfterUpdates(context.Background(), slug, updates, source, session)
}

// logged are the lines of the log at level, such as "WARN" or "ERROR".
func (f *triggerFixture) logged(level string) []string {
	var out []string
	for line := range strings.Lines(f.log.String()) {
		if strings.Contains(line, "level="+level+" ") {
			out = append(out, line)
		}
	}
	return out
}

// wantCall is an enqueue the trigger should make: of slug, submitted by,
// with session.
type wantCall struct {
	slug, submittedBy string
	session           *string
}

func (w wantCall) String() string {
	s := "nil"
	if w.session != nil {
		s = *w.session
	}
	return fmt.Sprintf("{%s %s %s}", w.slug, w.submittedBy, s)
}

// wantCalls fails the test unless the enqueues made are want, each by the
// system with no actor id.
func wantCalls(t *testing.T, name string, made []rebuildCall, want []wantCall) {
	t.Helper()
	got := make([]wantCall, len(made))
	for i, c := range made {
		got[i] = wantCall{c.slug, c.opts.SubmittedBy, c.opts.SessionID}
		if c.opts.ActorType != event.ActorSystem || c.opts.ActorID != nil {
			t.Errorf("%s: call %d by actor %q %v, want the system and no actor id", name, i, c.opts.ActorType, c.opts.ActorID)
		}
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("%s: enqueue calls %v, want %v", name, got, want)
	}
}

func (r *recordingRebuilds) all() []rebuildCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]rebuildCall(nil), r.made...)
}

func TestTrigger_QueuesOneRebuildPerRequestThatMovedAPatchBranch(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		updates     []pushrule.Update
		source      carrypatch.TriggerSource
		session     *string
		autoRebuild any
		want        []wantCall
	}{
		{"an update of a patch branch by a push", []pushrule.Update{updateOf("patch/a")}, carrypatch.SourcePush, nil, true,
			[]wantCall{{"demo", "system:push", nil}}},
		{"two patch branches and another branch in one push", []pushrule.Update{updateOf("patch/a"), updateOf("patch/b"), createOf("feature/x")},
			carrypatch.SourcePush, ptr("sess-1"), true, []wantCall{{"demo", "system:push", ptr("sess-1")}}},
		{"a create of a patch branch with the setting missing", []pushrule.Update{createOf("patch/b")}, carrypatch.SourcePush, nil, nil,
			[]wantCall{{"demo", "system:push", nil}}},
		{"an update of a patch branch by a land", []pushrule.Update{updateOf("patch/a")}, carrypatch.SourceLand, ptr("sess-2"), true,
			[]wantCall{{"demo", "system:land", ptr("sess-2")}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newTrigger(t, hubPatches(tc.autoRebuild))
			f.after(t, "demo", tc.updates, tc.source, tc.session)
			wantCalls(t, tc.name, f.rebuilds.all(), tc.want)
		})
	}
}

func TestTrigger_QueuesNoRebuildWithoutAMovedPatchBranchOrWithTheSettingOff(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		slug        string
		updates     []pushrule.Update
		autoRebuild bool
		noPatchRead bool
	}{
		{"auto_rebuild_after_push off", "demo", []pushrule.Update{updateOf("patch/a")}, false, false},
		{"a delete of a patch branch", "demo", []pushrule.Update{deleteOf("patch/a")}, true, false},
		{"an update of a deleted patch's branch", "demo", []pushrule.Update{updateOf("patch/old")}, true, false},
		{"an unregistered branch and a tag", "demo", []pushrule.Update{createOf("feature/x"), createOf("refs/tags/v1")}, true, false},
		{"a standard workspace", "std", []pushrule.Update{updateOf("patch/a")}, true, true},
		{"a task fork of the carry-patch workspace", "fork", []pushrule.Update{updateOf("patch/a")}, true, true},
		{"no update", "demo", nil, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newTrigger(t, hubPatches(tc.autoRebuild))
			f.patches.forbidden = tc.noPatchRead
			f.after(t, tc.slug, tc.updates, carrypatch.SourcePush, nil)
			if n := f.rebuilds.calls(); n != 0 {
				t.Errorf("the rebuild enqueue was called %d times, want 0", n)
			}
			if tc.noPatchRead && f.patches.readCount() != 0 {
				t.Errorf("the patch list was read %d times, want 0", f.patches.readCount())
			}
		})
	}
}

func TestTrigger_SkipsARebuildAlreadyQueuedSilently(t *testing.T) {
	t.Parallel()
	f := newTrigger(t, map[string]any{"patch_branch_source": "hub", "push_patches_to_origin": true, "auto_rebuild_after_push": true})
	f.rebuilds.answer("demo", carrypatch.RebuildResult{ExistingJobID: "job-1"}, nil)
	f.after(t, "demo", []pushrule.Update{updateOf("patch/a")}, carrypatch.SourcePush, nil)
	if n := f.rebuilds.calls(); n != 1 {
		t.Errorf("the rebuild enqueue was called %d times, want 1", n)
	}
	f.mirrors.wantMirrors(t, "demo patch/a")
	if n := f.events.count.Load(); n != 0 {
		t.Errorf("the trigger wrote %d events, want none", n)
	}
	if lines := append(f.logged("WARN"), f.logged("ERROR")...); len(lines) != 0 {
		t.Errorf("the trigger logged above debug level:\n%s", strings.Join(lines, ""))
	}
}

func TestTrigger_LogsOneErrorAndReturnsWhenAStepFails(t *testing.T) {
	t.Parallel()
	errRead := errors.New("disk I/O error")
	// After a failed enqueue the trigger still mirrors; after a failed
	// read it has nothing to mirror with.
	for _, tc := range []struct {
		name        string
		fault       func(f *triggerFixture)
		wantCalls   int
		wantMirrors []string
	}{
		{"the workspace record cannot be read", func(f *triggerFixture) { f.records.err = errRead }, 0, nil},
		{"the settings cannot be read", func(f *triggerFixture) { f.settings.err = errRead }, 0, nil},
		{"the patch list cannot be read", func(f *triggerFixture) { f.patches.err = errRead }, 0, nil},
		{"the enqueue returns an error", func(f *triggerFixture) { f.rebuilds.answer("demo", carrypatch.RebuildResult{}, errRead) }, 1,
			[]string{"demo patch/a"}},
		{"the enqueue panics", func(f *triggerFixture) {
			f.comp.RegisterRebuildEnqueuer(panickingRebuilds{f.rebuilds})
		}, 1, []string{"demo patch/a"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newTrigger(t, map[string]any{"patch_branch_source": "hub", "push_patches_to_origin": true, "auto_rebuild_after_push": true})
			tc.fault(f)
			f.after(t, "demo", []pushrule.Update{updateOf("patch/a")}, carrypatch.SourcePush, nil)
			errs := f.logged("ERROR")
			if len(errs) != 1 || !strings.Contains(errs[0], "demo") {
				t.Errorf("error records %q, want one that names demo", errs)
			}
			if n := f.rebuilds.calls(); n != tc.wantCalls {
				t.Errorf("the rebuild enqueue was called %d times, want %d", n, tc.wantCalls)
			}
			f.mirrors.wantMirrors(t, tc.wantMirrors...)
		})
	}
}

func TestTrigger_EndedByTheHubsStopLogsNoError(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		fault func(f *triggerFixture, cancel context.CancelFunc)
	}{
		{"while the workspace record is read", func(f *triggerFixture, cancel context.CancelFunc) {
			f.trigger.Records = cancellingRecords{cancel}
		}},
		{"while the rebuild is queued", func(f *triggerFixture, cancel context.CancelFunc) {
			f.comp.RegisterRebuildEnqueuer(cancellingRebuilds{cancel})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newTrigger(t, map[string]any{"patch_branch_source": "hub", "push_patches_to_origin": false})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			tc.fault(f, cancel)
			f.trigger.AfterUpdates(ctx, "demo", []pushrule.Update{updateOf("patch/a")}, carrypatch.SourcePush, nil)
			if errs := f.logged("ERROR"); len(errs) != 0 {
				t.Errorf("error records %q for a trigger the hub's stop ended, want none", errs)
			}
		})
	}
}

// cancellingRecords is a record reader that ends the trigger's context, as
// the hub's stop does, and fails with the context's error.
type cancellingRecords struct{ cancel context.CancelFunc }

func (r cancellingRecords) WorkspaceRecord(ctx context.Context, _ string) (workspace.Workspace, bool, error) {
	r.cancel()
	return workspace.Workspace{}, false, ctx.Err()
}

// cancellingRebuilds is a rebuild enqueuer that ends the trigger's context,
// as the hub's stop does, and fails with the context's error.
type cancellingRebuilds struct{ cancel context.CancelFunc }

func (r cancellingRebuilds) EnqueueRebuild(ctx context.Context, _ string, _ carrypatch.RebuildOptions) (carrypatch.RebuildResult, error) {
	r.cancel()
	return carrypatch.RebuildResult{}, ctx.Err()
}

func TestTrigger_RunsTheTriggersOfOneWorkspaceOneAtATime(t *testing.T) {
	t.Parallel()
	f := newTrigger(t, hubPatches(true))
	held := &heldRebuilds{recordingRebuilds: f.rebuilds, entered: make(chan string, 4), release: make(chan struct{}),
		hold: func(slug string, _ carrypatch.RebuildOptions) bool { return slug == "demo" }}
	f.comp.RegisterRebuildEnqueuer(held)
	var done sync.WaitGroup
	run := func(slug string) {
		done.Go(func() {
			f.trigger.AfterUpdates(context.Background(), slug, []pushrule.Update{updateOf("patch/a")}, carrypatch.SourcePush, nil)
		})
	}
	run("demo")
	hubtest.WaitClosed(t, held.enteredBy("demo"), "the first trigger of demo in the enqueue")
	run("demo")
	run("other")
	waitFor(t, "the trigger of other in the enqueue", func() bool { return len(f.rebuilds.callsOf("other")) == 1 })
	time.Sleep(20 * time.Millisecond)
	if n := len(held.entered); n != 0 {
		t.Errorf("a second trigger of demo reached the enqueue while the first was still in it")
	}
	close(held.release)
	done.Wait()
	if n := len(f.rebuilds.callsOf("demo")); n != 2 {
		t.Errorf("demo's rebuild enqueue was called %d times, want 2", n)
	}
}

func TestTrigger_PostPushListenerTriggersForGitPushesAndAPIWritesAndNotForLands(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		via     string
		updates []pushrule.Update
		session *string
		want    []wantCall
	}{
		{"an API write that updates a patch branch", pushrule.ViaAPI, []pushrule.Update{updateOf("patch/a")}, ptr("sess-3"),
			[]wantCall{{"demo", "system:push", ptr("sess-3")}}},
		{"an API write that creates a patch branch", pushrule.ViaAPI, []pushrule.Update{createOf("patch/a")}, nil,
			[]wantCall{{"demo", "system:push", nil}}},
		{"an API write of another branch", pushrule.ViaAPI, []pushrule.Update{updateOf("feature/x")}, nil, nil},
		{"a git push that updates a patch branch", pushrule.ViaGit, []pushrule.Update{updateOf("patch/b")}, nil,
			[]wantCall{{"demo", "system:push", nil}}},
		{"a land", pushrule.ViaLand, []pushrule.Update{updateOf("patch/a")}, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newTrigger(t, hubPatches(true))
			decisions := make([]pushrule.Decision, len(tc.updates))
			for i, u := range tc.updates {
				decisions[i] = pushrule.Decision{Update: u, Allowed: true}
			}
			var listen pushrule.PostPushListener = f.trigger.PostPush
			if err := listen(context.Background(), pushrule.PostPush{Workspace: "demo", Updates: decisions, Via: tc.via, SessionID: tc.session}); err != nil {
				t.Errorf("the listener returned %v, want nil", err)
			}
			wantCalls(t, tc.name, f.rebuilds.all(), tc.want)
		})
	}
}

// panickingRebuilds records each call and then panics.
type panickingRebuilds struct{ *recordingRebuilds }

func (p panickingRebuilds) EnqueueRebuild(ctx context.Context, slug string, opts carrypatch.RebuildOptions) (carrypatch.RebuildResult, error) {
	_, _ = p.recordingRebuilds.EnqueueRebuild(ctx, slug, opts)
	panic("enqueue exploded")
}

// heldRebuilds is a recording enqueuer that holds each call hold picks,
// after telling entered of its workspace, until release is closed, and
// then records it.
type heldRebuilds struct {
	*recordingRebuilds
	hold    func(slug string, opts carrypatch.RebuildOptions) bool
	entered chan string
	release chan struct{}
}

func (h *heldRebuilds) EnqueueRebuild(ctx context.Context, slug string, opts carrypatch.RebuildOptions) (carrypatch.RebuildResult, error) {
	if h.hold(slug, opts) {
		h.entered <- slug
		<-h.release
	}
	return h.recordingRebuilds.EnqueueRebuild(ctx, slug, opts)
}

// enteredBy is a channel closed once a held call of slug has entered.
func (h *heldRebuilds) enteredBy(slug string) <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		for s := range h.entered {
			if s == slug {
				close(ch)
				return
			}
		}
	}()
	return ch
}

// countingEvents is an event writer that counts its calls and writes
// nothing.
type countingEvents struct{ count atomic.Int64 }

func (e *countingEvents) Emit(context.Context, *sql.Tx, event.Input) (event.Record, error) {
	e.count.Add(1)
	return event.Record{}, nil
}

// waitFor fails the test unless cond holds within 10 s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

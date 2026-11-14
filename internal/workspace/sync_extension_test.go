package workspace_test

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// stageCall is one call of a stage of a sync extension: the stage, the error
// of its context when it returned, the run, the results the second stage
// was given and the refs of the hub's copy when it was called.
type stageCall struct {
	stage   string
	ctxErr  error
	run     workspace.SyncRun
	results []workspace.BranchResult
	refs    map[string]string
}

// recordingExtension is a sync extension that records every call of its
// stages, runs during inside each, and answers firstErr from its first stage
// and add and secondErr from its second.
type recordingExtension struct {
	t   *testing.T
	dir string

	mu        sync.Mutex
	during    func(stage string)
	firstErr  error
	secondErr error
	add       workspace.SyncAddition
	calls     []stageCall
}

func (e *recordingExtension) AfterFetch(ctx context.Context, run workspace.SyncRun) error {
	err, _, _ := e.record(ctx, "first", run, nil)
	return err
}

func (e *recordingExtension) BeforeCompletion(ctx context.Context, run workspace.SyncRun, results []workspace.BranchResult) (workspace.SyncAddition, error) {
	_, add, err := e.record(ctx, "second", run, results)
	return add, err
}

func (e *recordingExtension) record(ctx context.Context, stage string, run workspace.SyncRun, results []workspace.BranchResult) (firstErr error, add workspace.SyncAddition, secondErr error) {
	refs := refsUnder(e.t, e.dir, "refs/")
	e.mu.Lock()
	during := e.during
	e.mu.Unlock()
	if during != nil {
		during(stage)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, stageCall{stage, ctx.Err(), run, results, refs})
	return e.firstErr, e.add, e.secondErr
}

// set changes the extension with fn, between syncs.
func (e *recordingExtension) set(fn func(e *recordingExtension)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	fn(e)
}

// recorded are the calls so far and the names of their stages.
func (e *recordingExtension) recorded() ([]stageCall, []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var stages []string
	for _, c := range e.calls {
		stages = append(stages, c.stage)
	}
	return slices.Clone(e.calls), stages
}

// newExtensionWorld starts a sync world whose workspace is carry_patch, with
// ext registered as its sync extension, origin's main a commit ahead of the
// hub's and the new tag v2 on origin only. old is the hub's main, theirs
// origin's.
func newExtensionWorld(t *testing.T, ext *recordingExtension) (s *syncWorld, old, theirs string) {
	t.Helper()
	s = newSyncWorld(t)
	s.setMode(t, workspace.ModeCarryPatch, "")
	ext.t, ext.dir = t, s.copy
	s.svc.RegisterSyncExtension(ext)
	old = s.origin.Head("main")
	theirs = grow(t, s.origin.Dir(), "main", old, "origin")
	gitrepotest.Git(t, s.origin.Dir(), "tag", "v2", theirs)
	return s, old, theirs
}

func TestSyncExtension_StagesRunInOrder(t *testing.T) {
	t.Parallel()
	ext := &recordingExtension{}
	s, old, theirs := newExtensionWorld(t, ext)

	s.postSync(t, s.uKey, syncSlug, "").decode(t, http.StatusOK)
	calls, stages := ext.recorded()
	if !slices.Equal(stages, []string{"first", "second"}) {
		t.Fatalf("stages called %v, want [first second]", stages)
	}
	first, second := calls[0], calls[1]
	if first.refs["remotes/origin/main"] != theirs || first.refs["heads/main"] != old || first.refs["tags/v2"] != "" {
		t.Errorf("at the first stage origin's main %s, the hub's main %s, tag v2 %q; want %s, %s and no tag",
			first.refs["remotes/origin/main"], first.refs["heads/main"], first.refs["tags/v2"], theirs, old)
	}
	run := first.run
	if run.Workspace.Slug != syncSlug || run.ResetToOrigin || run.OriginHeads["main"] != theirs ||
		run.Actor.User == nil || run.Actor.User.ID != s.u {
		t.Errorf("the run = %+v, want workspace %s, the caller u, no reset, origin's main %s", run, syncSlug, theirs)
	}
	ff := workspace.BranchResult{Name: "main", Result: workspace.BranchFastForward, LocalSHA: theirs, OriginSHA: theirs}
	if !slices.Contains(second.results, ff) || second.refs["heads/main"] != theirs || second.refs["tags/v2"] == "" {
		t.Errorf("the second stage got %+v with the hub's main %s and tag v2 %q, want main fast_forward, moved, and the tag",
			second.results, second.refs["heads/main"], second.refs["tags/v2"])
	}
	if !reflect.DeepEqual(second.run, run) {
		t.Errorf("the second stage's run %+v differs from the first's %+v", second.run, run)
	}

	// A caller who disconnects during the first stage stops neither stage.
	started, release := make(chan struct{}), make(chan struct{})
	ext.set(func(e *recordingExtension) {
		e.during = func(stage string) {
			if stage == "first" {
				close(started)
				<-release
			}
		}
	})
	s.postSyncAndLeave(t, s.uKey, syncSlug, started)
	close(release)
	eventually(t, "the sync completes after the disconnect", func() bool { return len(s.eventsOf(t, syncEvent)) == 2 })
	calls, stages = ext.recorded()
	if !slices.Equal(stages, []string{"first", "second", "first", "second"}) {
		t.Fatalf("stages called %v, want [first second] for each sync", stages)
	}
	if calls[2].ctxErr != nil || calls[3].ctxErr != nil {
		t.Errorf("after the disconnect the stages had the context errors %v, %v; want a live context", calls[2].ctxErr, calls[3].ctxErr)
	}
	ext.set(func(e *recordingExtension) { e.during = nil })

	// Neither stage runs for a standard workspace, and without an extension
	// the answer is the workspace object and branches alone.
	s.setMode(t, workspace.ModeStandard, "")
	s.postSync(t, s.uKey, syncSlug, "").decode(t, http.StatusOK)
	if _, stages := ext.recorded(); len(stages) != 4 {
		t.Errorf("a standard workspace's sync called the stages %v", stages[4:])
	}
	s.setMode(t, workspace.ModeCarryPatch, "")
	s.svc.RegisterSyncExtension(nil)
	syncBranches(t, s.postSync(t, s.uKey, syncSlug, ""))
}

func TestSyncExtension_FirstStageFailure(t *testing.T) {
	t.Parallel()
	ext := &recordingExtension{firstErr: errors.New("upstream fetch failed: https://user:pw@up.example/x")}
	s, _, _ := newExtensionWorld(t, ext)
	before, _ := s.workspace(t, syncSlug)
	heads, tags, events := refsUnder(t, s.copy, "refs/heads/"), refsUnder(t, s.copy, "refs/tags/"), s.eventCount(t)

	a := s.postSync(t, s.uKey, syncSlug, "")
	typ, msg := a.refusal(t)
	if a.status != http.StatusBadGateway || typ != "upstream_error" || !strings.Contains(msg, "upstream fetch failed") ||
		strings.Contains(msg, "pw") || strings.Contains(msg, "origin fetch failed") {
		t.Errorf("answer = %d %s, want 502 upstream_error with the scrubbed text of the stage and no origin prefix", a.status, a.body)
	}
	rec, _ := s.workspace(t, syncSlug)
	if deref(rec.SyncError) != msg || !reflect.DeepEqual(rec.LastSyncAt, before.LastSyncAt) ||
		!reflect.DeepEqual(rec.HeadSHA, before.HeadSHA) || !reflect.DeepEqual(rec.OriginHeadSHA, before.OriginHeadSHA) {
		t.Errorf("the record has sync_error %q, last_sync_at %v, heads %v %v; want the message and the rest unchanged",
			deref(rec.SyncError), rec.LastSyncAt, deref(rec.HeadSHA), deref(rec.OriginHeadSHA))
	}
	if !maps.Equal(refsUnder(t, s.copy, "refs/heads/"), heads) || !maps.Equal(refsUnder(t, s.copy, "refs/tags/"), tags) {
		t.Errorf("the hub's branches or tags changed")
	}
	if _, stages := ext.recorded(); !slices.Equal(stages, []string{"first"}) {
		t.Errorf("stages called %v, want [first]", stages)
	}
	if n := s.eventCount(t); n != events {
		t.Errorf("the log grew by %d events, want none", n-events)
	}
}

func TestSyncExtension_SecondStageAddsMembers(t *testing.T) {
	t.Parallel()
	h := strings.Repeat("ab", 20)
	ext := &recordingExtension{add: workspace.SyncAddition{UpstreamHeadSHA: &h, Members: map[string]json.RawMessage{
		"origin_fetched": json.RawMessage(`true`), "patches_diverged": json.RawMessage(`[]`),
		"slug": json.RawMessage(`"other"`), "branches": json.RawMessage(`"other"`),
	}}}
	s, _, _ := newExtensionWorld(t, ext)

	body := s.postSync(t, s.uKey, syncSlug, "").decode(t, http.StatusOK)
	if body["origin_fetched"] != true || !reflect.DeepEqual(body["patches_diverged"], []any{}) ||
		body["slug"] != syncSlug || body["upstream_head_sha"] != h {
		t.Errorf("answer origin_fetched %v, patches_diverged %v, slug %v, upstream_head_sha %v; want true, [], %s, %s",
			body["origin_fetched"], body["patches_diverged"], body["slug"], body["upstream_head_sha"], syncSlug, h)
	}
	if branches, ok := body["branches"].([]any); !ok || len(branches) == 0 {
		t.Errorf("branches = %v, want the per-branch list", body["branches"])
	}
	if len(body) != len(objectMembers)+3 {
		t.Errorf("the answer has %d members, want the workspace object's, branches and the 2 new ones", len(body))
	}
	rec, _ := s.workspace(t, syncSlug)
	if deref(rec.UpstreamHeadSHA) != h || rec.LastSyncAt == nil {
		t.Errorf("the record has upstream_head_sha %q, last_sync_at %v; want %s and set", deref(rec.UpstreamHeadSHA), rec.LastSyncAt, h)
	}

	// A second stage that returns no value leaves upstream_head_sha alone.
	ext.set(func(e *recordingExtension) { e.add = workspace.SyncAddition{} })
	s.postSync(t, s.uKey, syncSlug, "").decode(t, http.StatusOK)
	if rec, _ := s.workspace(t, syncSlug); deref(rec.UpstreamHeadSHA) != h {
		t.Errorf("upstream_head_sha = %q after a sync without a value, want %s kept", deref(rec.UpstreamHeadSHA), h)
	}
}

func TestSyncExtension_SecondStageFailure(t *testing.T) {
	t.Parallel()
	ext := &recordingExtension{secondErr: errors.New("refresh of patch branch p1 failed")}
	s, _, theirs := newExtensionWorld(t, ext)
	if _, err := s.Store.Main().Exec(`UPDATE workspaces SET sync_error = 'old', upstream_head_sha = 'up' WHERE slug = ?`, syncSlug); err != nil {
		t.Fatal(err)
	}
	before, _ := s.workspace(t, syncSlug)
	events := s.eventCount(t)

	a := s.postSync(t, s.uKey, syncSlug, "")
	if typ, msg := a.refusal(t); a.status != http.StatusInternalServerError || typ != "internal" || !strings.Contains(msg, "p1") {
		t.Errorf("answer = %d %s, want 500 internal naming p1", a.status, a.body)
	}
	rec, _ := s.workspace(t, syncSlug)
	if !reflect.DeepEqual(rec.LastSyncAt, before.LastSyncAt) || deref(rec.SyncError) != "old" ||
		deref(rec.UpstreamHeadSHA) != "up" || !reflect.DeepEqual(rec.HeadSHA, before.HeadSHA) {
		t.Errorf("the record has last_sync_at %v, sync_error %q, upstream_head_sha %q, head_sha %q; want them unchanged",
			rec.LastSyncAt, deref(rec.SyncError), deref(rec.UpstreamHeadSHA), deref(rec.HeadSHA))
	}
	if got := ref(t, s.copy, "refs/heads/main"); got != theirs {
		t.Errorf("the hub's main = %s, want origin's %s kept", got, theirs)
	}
	if n := s.eventCount(t); n != events {
		t.Errorf("the log grew by %d events, want none", n-events)
	}
}

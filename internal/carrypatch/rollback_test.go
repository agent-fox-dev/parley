package carrypatch_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"uuid"

	"github.com/agent-fox-dev/parley/internal/carrypatch"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/jobs"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// rollbackCase is a ready carry-patch workspace of the rollback tests: the
// copy made by rollbackRepo, deploy at H2 whose parent is H1, and the
// completed rebuild J2 that moved deploy from H1 to H2.
type rollbackCase struct {
	slug, dir, h1, h2, j2 string
}

// rollbackCarry stores the ready carry-patch workspace slug with no patch
// and gives it the rollback case.
func (h *rebuildHub) rollbackCarry(t *testing.T, slug string) rollbackCase {
	t.Helper()
	h.carryWith(t, slug, nil)
	return h.rollbackCase(t, slug)
}

// rollbackCase gives the stored workspace slug the copy and the job of a
// rollback case.
func (h *rebuildHub) rollbackCase(t *testing.T, slug string) rollbackCase {
	t.Helper()
	dir, h1, h2 := h.rollbackRepo(t, slug)
	return rollbackCase{slug: slug, dir: dir, h1: h1, h2: h2, j2: h.settledRebuild(t, slug, rebuiltFrom(h1, h2), nil)}
}

// deploy is the head of the case's integration branch, "" when it has none.
func (c rollbackCase) deploy() string { return refIn(c.dir, "refs/heads/deploy") }

// rolledBack decodes the answer of a rollback.
func rolledBack(t *testing.T, a answer) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(a.raw, &m); err != nil {
		t.Fatalf("decode the rollback's answer %s: %v", a.raw, err)
	}
	return m
}

func TestRollback_MovesIntegrationBranch(t *testing.T) {
	t.Parallel()
	h := startRebuildHub(t)
	c := h.rollbackCarry(t, "my-fork")

	a := h.rollback(t, h.key, c.slug, c.j2)
	if a.status != http.StatusOK {
		t.Fatalf("rollback = %d %s, want 200", a.status, a.raw)
	}
	if got, want := rolledBack(t, a), map[string]any{"rolled_back_to": c.h1, "previous_head_sha": c.h2}; !reflect.DeepEqual(got, want) {
		t.Errorf("rollback answered %v, want %v", got, want)
	}
	if got := c.deploy(); got != c.h1 {
		t.Errorf("deploy is at %s after the rollback, want H1 %s", got, c.h1)
	}

	// The branch is at the target already: the swap holds all the same.
	again := h.rollback(t, h.key, c.slug, c.j2)
	if again.status != http.StatusOK {
		t.Fatalf("the repeated rollback = %d %s, want 200", again.status, again.raw)
	}
	if got, want := rolledBack(t, again), map[string]any{"rolled_back_to": c.h1, "previous_head_sha": c.h1}; !reflect.DeepEqual(got, want) {
		t.Errorf("the repeated rollback answered %v, want %v", got, want)
	}
	if got := c.deploy(); got != c.h1 {
		t.Errorf("deploy is at %s after the repeated rollback, want H1 %s", got, c.h1)
	}

	admin := h.rollbackCarry(t, "admin-fork")
	if a := h.rollback(t, h.Token, admin.slug, admin.j2); a.status != http.StatusOK {
		t.Errorf("rollback with the admin token = %d %s, want 200", a.status, a.raw)
	}
	if got := admin.deploy(); got != admin.h1 {
		t.Errorf("deploy is at %s after the admin's rollback, want H1 %s", got, admin.h1)
	}
}

func TestRollback_LosesCompareAndSwap(t *testing.T) {
	t.Parallel()
	h := startRebuildHub(t)
	c := h.rollbackCarry(t, "my-fork")
	h3 := gitrepotest.Commit(t, c.dir, "H3", map[string]string{"a.txt": "3\n"}, c.h2)
	h.onRollbackSwap(c.slug, func() {
		// As a publishing rebuild would.
		if _, err := gitrepotest.Run(c.dir, "", "update-ref", "refs/heads/deploy", h3); err != nil {
			t.Errorf("move deploy to H3: %v", err)
		}
	})

	a := h.rollback(t, h.key, c.slug, c.j2)
	var envelope struct {
		Error struct {
			Details struct {
				CurrentSHA *string `json:"current_sha"`
			} `json:"details"`
		} `json:"error"`
	}
	_ = json.Unmarshal(a.raw, &envelope)
	if a.status != http.StatusConflict || a.Error.ErrorType != "ref_moved" || str(envelope.Error.Details.CurrentSHA) != h3 {
		t.Errorf("rollback = %d %s, want 409 ref_moved with current_sha H3 %s", a.status, a.raw, h3)
	}
	if got := c.deploy(); got != h3 {
		t.Errorf("deploy is at %s, want H3 %s as the other writer left it", got, h3)
	}
	if got := h.rebuildEvents(t, c.slug, carrypatch.EventRebuildRollback); len(got) != 0 {
		t.Errorf("the lost rollback wrote the events %+v, want none", got)
	}
}

func TestRollback_CreatesMissingIntegrationBranch(t *testing.T) {
	t.Parallel()
	h := startRebuildHub(t)
	c := h.rollbackCarry(t, "my-fork")
	gitrepotest.SetRef(t, c.dir, "refs/heads/keep", c.h2)
	gitrepotest.Git(t, c.dir, "update-ref", "-d", "refs/heads/deploy")

	a := h.rollback(t, h.key, c.slug, c.j2)
	if a.status != http.StatusOK {
		t.Fatalf("rollback = %d %s, want 200", a.status, a.raw)
	}
	if got, want := rolledBack(t, a), map[string]any{"rolled_back_to": c.h1, "previous_head_sha": nil}; !reflect.DeepEqual(got, want) {
		t.Errorf("rollback answered %s, want %v", a.raw, want)
	}
	if got := c.deploy(); got != c.h1 {
		t.Errorf("deploy is at %q after the rollback, want H1 %s", got, c.h1)
	}

	// A ref whose name is in the way of the branch's keeps it from being
	// created: 409 conflict, and nothing changes.
	blocked := h.rollbackCarry(t, "blocked-fork")
	gitrepotest.Git(t, blocked.dir, "update-ref", "-d", "refs/heads/deploy")
	gitrepotest.SetRef(t, blocked.dir, "refs/heads/deploy/next", blocked.h2)
	if a := h.rollback(t, h.key, blocked.slug, blocked.j2); a.status != http.StatusConflict || a.Error.ErrorType != "conflict" {
		t.Errorf("rollback with refs/heads/deploy/next in the way = %d %s, want 409 conflict", a.status, a.raw)
	}
	if got := refIn(blocked.dir, "refs/heads/deploy/next"); got != blocked.h2 {
		t.Errorf("refs/heads/deploy/next is at %q after the refused rollback, want H2 %s", got, blocked.h2)
	}
	if got := h.rebuildEvents(t, blocked.slug, carrypatch.EventRebuildRollback); len(got) != 0 {
		t.Errorf("the refused rollback wrote the events %+v, want none", got)
	}
}

func TestRollback_ChangesIntegrationBranchOnly(t *testing.T) {
	t.Parallel()
	h := startRebuildHub(t)
	origin := t.TempDir()
	gitrepotest.InitBare(t, origin, "")
	gitrepotest.SetRef(t, origin, "refs/heads/deploy", gitrepotest.Commit(t, origin, "origin's deploy", map[string]string{"o.txt": "o\n"}))
	const slug = "my-fork"
	h.add(t, slug, workspace.ModeCarryPatch, workspace.StatusReady, workspace.MarkerNone, func(w *workspace.Workspace) {
		w.IntegrationBranch, w.GitURL = new("deploy"), &origin
		w.Settings = map[string]json.RawMessage{}
		for _, key := range []string{"rebuild_push_integration_branch", "auto_rebuild_after_push", "auto_rebuild_after_sync"} {
			w.Settings[key] = json.RawMessage("true")
		}
	})
	a, b := livePatch(slug, "a", 1), livePatch(slug, "b", 2)
	b.Status, b.ConflictFiles = carrypatch.StatusConflict, []string{"x.txt"}
	h.put(t, a, b, deletedPatch(slug, "c", at(10, 0)))
	c := h.rollbackCase(t, slug)
	for _, branch := range []string{"a", "b", "c"} {
		gitrepotest.SetRef(t, c.dir, "refs/heads/"+branch, gitrepotest.Commit(t, c.dir, branch, map[string]string{branch + ".txt": branch + "\n"}, c.h1))
	}
	gitrepotest.SetRef(t, c.dir, carrypatch.ReplacedRefPrefix+"a", gitrepotest.Commit(t, c.dir, "replaced a", nil, c.h1))

	type held struct {
		Refs, OriginRefs string
		Patches          []carrypatch.Patch
		Jobs             []jobs.Job
		JobCount         int
	}
	state := func() held {
		t.Helper()
		ctx := context.Background()
		live, err := carrypatch.ListLivePatches(ctx, h.Store.Main(), slug)
		if err != nil {
			t.Fatal(err)
		}
		deleted, err := carrypatch.ListDeletedPatches(ctx, h.Store.Main(), slug)
		if err != nil {
			t.Fatal(err)
		}
		s := held{
			Refs:       gitrepotest.Git(t, c.dir, "for-each-ref", "--format=%(refname) %(objectname)"),
			OriginRefs: gitrepotest.Git(t, origin, "for-each-ref", "--format=%(refname) %(objectname)"),
			Patches:    append(live, deleted...), Jobs: h.jobsOf(t, slug),
		}
		if err := h.Store.Main().QueryRow(`SELECT count(*) FROM jobs`).Scan(&s.JobCount); err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := state()

	if r := h.rollback(t, h.key, slug, c.j2); r.status != http.StatusOK {
		t.Fatalf("rollback = %d %s, want 200", r.status, r.raw)
	}
	after := state()
	if want := strings.Replace(before.Refs, "refs/heads/deploy "+c.h2, "refs/heads/deploy "+c.h1, 1); after.Refs != want {
		t.Errorf("the copy's refs after the rollback are\n%s\nwant\n%s", after.Refs, want)
	}
	if after.OriginRefs != before.OriginRefs {
		t.Errorf("origin's refs after the rollback are\n%s\nwant\n%s", after.OriginRefs, before.OriginRefs)
	}
	if !reflect.DeepEqual(after.Patches, before.Patches) {
		t.Errorf("the patches after the rollback are %+v, want %+v", after.Patches, before.Patches)
	}
	if !reflect.DeepEqual(after.Jobs, before.Jobs) || after.JobCount != before.JobCount {
		t.Errorf("the jobs after the rollback are %d %+v, want %d %+v", after.JobCount, after.Jobs, before.JobCount, before.Jobs)
	}
	queued, err := h.queue.List(context.Background(), carrypatch.RebuildJobType, slug, jobs.StatusQueued, nil, 10)
	if err != nil || len(queued) != 0 {
		t.Errorf("the queue holds the queued rebuilds %+v (%v), want none", queued, err)
	}
}

func TestRollback_EmitsEvent(t *testing.T) {
	t.Parallel()
	h := startRebuildHub(t)
	want := func(c rollbackCase, previous any) []rebuildEvent {
		return []rebuildEvent{{ResourceType: "rebuild", ResourceID: c.j2, ActorType: "api_key", ActorID: &h.owner,
			Metadata: map[string]any{"integration_branch": "deploy", "rolled_back_to": c.h1, "previous_head_sha": previous}}}
	}

	moved := h.rollbackCarry(t, "my-fork")
	if a := h.rollback(t, h.key, moved.slug, moved.j2); a.status != http.StatusOK {
		t.Fatalf("rollback = %d %s, want 200", a.status, a.raw)
	}
	if got, want := h.rebuildEvents(t, moved.slug, carrypatch.EventRebuildRollback), want(moved, moved.h2); !reflect.DeepEqual(got, want) {
		t.Errorf("the rollback events are %+v, want %+v", got, want)
	}

	created := h.rollbackCarry(t, "created-fork")
	gitrepotest.Git(t, created.dir, "update-ref", "-d", "refs/heads/deploy")
	if a := h.rollback(t, h.key, created.slug, created.j2); a.status != http.StatusOK {
		t.Fatalf("rollback of a missing branch = %d %s, want 200", a.status, a.raw)
	}
	if got, want := h.rebuildEvents(t, created.slug, carrypatch.EventRebuildRollback), want(created, nil); !reflect.DeepEqual(got, want) {
		t.Errorf("the rollback events of a created branch are %+v, want %+v", got, want)
	}

	// A move no event reports is taken back.
	unrecorded := h.rollbackCarry(t, "unrecorded-fork")
	h.failEvents.Store(true)
	a := h.rollback(t, h.key, unrecorded.slug, unrecorded.j2)
	h.failEvents.Store(false)
	if a.status != http.StatusInternalServerError {
		t.Errorf("rollback with the event log refusing = %d %s, want 500", a.status, a.raw)
	}
	if got := unrecorded.deploy(); got != unrecorded.h2 {
		t.Errorf("deploy is at %s after the unrecorded rollback, want H2 %s", got, unrecorded.h2)
	}
	if got := h.rebuildEvents(t, unrecorded.slug, carrypatch.EventRebuildRollback); len(got) != 0 {
		t.Errorf("the unrecorded rollback wrote the events %+v, want none", got)
	}

	// A branch the unrecorded rollback created is removed again.
	uncreated := h.rollbackCarry(t, "uncreated-fork")
	gitrepotest.Git(t, uncreated.dir, "update-ref", "-d", "refs/heads/deploy")
	h.failEvents.Store(true)
	a = h.rollback(t, h.key, uncreated.slug, uncreated.j2)
	h.failEvents.Store(false)
	if a.status != http.StatusInternalServerError {
		t.Errorf("rollback of a missing branch with the event log refusing = %d %s, want 500", a.status, a.raw)
	}
	if got := uncreated.deploy(); got != "" {
		t.Errorf("deploy is at %s after the unrecorded rollback created it, want it missing again", got)
	}
}

func TestRollback_ProceedsBesideARebuild(t *testing.T) {
	t.Parallel()
	h := startRebuildHub(t)
	// The cases pass the queue in turn, so they run one after the other.
	for _, status := range []string{jobs.StatusQueued, jobs.StatusRunning, jobs.StatusFailed} {
		t.Run(status, func(t *testing.T) {
			c := h.rollbackCarry(t, "w-"+status)
			other := h.queueRebuild(t, c.slug)
			if status != jobs.StatusQueued {
				h.drive(t, other, status)
			}
			before := h.job(t, other)

			if a := h.rollback(t, h.key, c.slug, c.j2); a.status != http.StatusOK {
				t.Fatalf("rollback beside a %s rebuild = %d %s, want 200", status, a.status, a.raw)
			}
			if got := c.deploy(); got != c.h1 {
				t.Errorf("deploy is at %s, want H1 %s", got, c.h1)
			}
			if after := h.job(t, other); !reflect.DeepEqual(after, before) {
				t.Errorf("the other job is %+v after the rollback, want %+v", after, before)
			}
		})
	}
}

func TestRollback_Unavailable(t *testing.T) {
	t.Parallel()
	h := startRebuildHub(t)
	// The cases pass the queue in turn, so they run one after the other.
	for _, c := range []struct {
		name string
		// job makes the job of slug the rollback names; h1 and h2 are its
		// commits and blob a blob of its copy.
		job func(t *testing.T, slug, h1, h2, blob string) string
	}{
		{"queued", func(t *testing.T, slug, _, _, _ string) string { return h.queueRebuild(t, slug) }},
		{"running with a previous head", func(t *testing.T, slug, h1, h2, _ string) string {
			id := h.queueRebuild(t, slug)
			h.drive(t, id, jobs.StatusRunning)
			if err := h.queue.SetResult(context.Background(), id, json.RawMessage(rebuiltFrom(h1, h2))); err != nil {
				t.Fatal(err)
			}
			return id
		}},
		{"failed with a previous head", func(t *testing.T, slug, h1, h2, _ string) string {
			return h.settledRebuild(t, slug, rebuiltFrom(h1, h2), errors.New("the upstream is unreachable"))
		}},
		{"dead-lettered with a previous head", func(t *testing.T, slug, h1, h2, _ string) string {
			return h.settledRebuild(t, slug, rebuiltFrom(h1, h2), jobs.Permanent(errors.New("the patch list cannot be applied")))
		}},
		{"cancelled", func(t *testing.T, slug, _, _, _ string) string {
			id := h.queueRebuild(t, slug)
			h.drive(t, id, jobs.StatusCancelled)
			return id
		}},
		{"completed with no previous head", func(t *testing.T, slug, _, h2, _ string) string {
			return h.settledRebuild(t, slug, rebuiltFrom("", h2), nil)
		}},
		{"completed with a previous head that names no object", func(t *testing.T, slug, _, h2, _ string) string {
			return h.settledRebuild(t, slug, rebuiltFrom(strings.Repeat("ab", 20), h2), nil)
		}},
		{"completed with a previous head that names a blob", func(t *testing.T, slug, _, h2, blob string) string {
			return h.settledRebuild(t, slug, rebuiltFrom(blob, h2), nil)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			slug := "w-" + strings.ReplaceAll(c.name, " ", "-")
			h.carryWith(t, slug, nil)
			dir, h1, h2 := h.rollbackRepo(t, slug)
			blob, err := gitrepotest.Run(dir, "a blob\n", "hash-object", "-w", "--stdin")
			if err != nil {
				t.Fatal(err)
			}
			id := c.job(t, slug, h1, h2, blob)

			if a := h.rollback(t, h.key, slug, id); a.status != http.StatusConflict || a.Error.ErrorType != "rollback_unavailable" {
				t.Errorf("rollback = %d %s, want 409 rollback_unavailable", a.status, a.raw)
			}
			if got := refIn(dir, "refs/heads/deploy"); got != h2 {
				t.Errorf("deploy is at %s, want H2 %s", got, h2)
			}
			if got := h.rebuildEvents(t, slug, carrypatch.EventRebuildRollback); len(got) != 0 {
				t.Errorf("the refused rollback wrote the events %+v, want none", got)
			}
		})
	}
}

func TestRollbackRoute_UnknownJob(t *testing.T) {
	t.Parallel()
	h := startRebuildHub(t)
	mine := h.rollbackCarry(t, "my-fork")
	other := h.rollbackCarry(t, "other-fork")
	for _, id := range []string{uuid.NewV4().String(), "not-a-uuid", other.j2} {
		t.Run(id, func(t *testing.T) {
			a := h.rollback(t, h.key, mine.slug, id)
			if a.status != http.StatusNotFound || a.Error.ErrorType != "not_found" || a.Error.Message != "rebuild job not found" {
				t.Errorf("rollback = %d %s, want 404 not_found 'rebuild job not found'", a.status, a.raw)
			}
		})
	}
	for _, c := range []rollbackCase{mine, other} {
		if got := c.deploy(); got != c.h2 {
			t.Errorf("deploy of %s is at %s, want H2 %s", c.slug, got, c.h2)
		}
		if got := h.rebuildEvents(t, c.slug, carrypatch.EventRebuildRollback); len(got) != 0 {
			t.Errorf("%s has the rollback events %+v, want none", c.slug, got)
		}
	}
}

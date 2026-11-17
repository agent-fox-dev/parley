package carrypatch_test

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"testing"
	"uuid"

	"github.com/agent-fox-dev/parley/internal/carrypatch"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/jobs"
)

func TestEnqueueRebuild_WritesOneEnqueueEventPerQueuedJob(t *testing.T) {
	t.Parallel()
	h := startRebuildHub(t)
	session := uuid.NewV4().String()
	system := func(by string, strategy *string) carrypatch.RebuildOptions {
		return carrypatch.RebuildOptions{Strategy: strategy, SubmittedBy: by, ActorType: event.ActorSystem, SessionID: &session}
	}
	// running is the job the follow-up follows.
	var running string
	// The cases pass the queue in turn, so they run one after the other.
	for i, c := range []struct {
		name string
		// setup, when set, prepares slug before the source queues.
		setup func(t *testing.T, slug string)
		// queue queues a job of slug as the source does and returns its id,
		// ok false for a duplicate.
		queue              func(t *testing.T, slug string) (id string, ok bool)
		actorType          string
		actorID, sessionID *string
		submittedBy        string
		strategy           string
	}{
		{name: "the route", actorType: "api_key", actorID: &h.owner, submittedBy: h.owner, strategy: "rebase",
			queue: func(t *testing.T, slug string) (string, bool) {
				a := h.submit(t, slug, nil, nil)
				id, _ := a.patch["id"].(string)
				return id, a.status == http.StatusAccepted
			}},
		{name: "a push", actorType: event.ActorSystem, sessionID: &session, submittedBy: "system:push", strategy: "merge",
			queue: func(t *testing.T, slug string) (string, bool) {
				return h.enqueue(t, slug, system("system:push", new("merge")))
			}},
		{name: "a landing", actorType: event.ActorSystem, sessionID: &session, submittedBy: "system:land", strategy: "rebase",
			queue: func(t *testing.T, slug string) (string, bool) {
				return h.enqueue(t, slug, system("system:land", nil))
			}},
		{name: "a follow-up", actorType: event.ActorSystem, sessionID: &session, submittedBy: "system:followup", strategy: "rebase",
			setup: func(t *testing.T, slug string) {
				running, _ = h.enqueue(t, slug, system("system:push", nil))
				h.drive(t, running, jobs.StatusRunning)
			},
			queue: func(t *testing.T, slug string) (string, bool) {
				opts := system("system:followup", nil)
				opts.FollowUpOf = &running
				return h.enqueue(t, slug, opts)
			}},
	} {
		t.Run(c.name, func(t *testing.T) {
			slug := fmt.Sprintf("w%d", i)
			h.carryWith(t, slug, nil, carrypatch.StatusActive, carrypatch.StatusConflict, carrypatch.StatusDisabled, carrypatch.StatusDeleted)
			if c.setup != nil {
				c.setup(t, slug)
			}
			before := len(h.rebuildEvents(t, slug, carrypatch.EventRebuildEnqueue))
			id, ok := c.queue(t, slug)
			if !ok {
				t.Fatalf("no job was queued")
			}
			events := h.rebuildEvents(t, slug, carrypatch.EventRebuildEnqueue)
			if len(events) != before+1 {
				t.Fatalf("%s has %d enqueue events, want %d", slug, len(events), before+1)
			}
			want := rebuildEvent{ResourceType: "rebuild", ResourceID: id, ActorType: c.actorType, ActorID: c.actorID, SessionID: c.sessionID,
				Metadata: map[string]any{"submitted_by": c.submittedBy, "strategy": c.strategy, "fail_mode": "fail_fast", "patch_count": 2.0}}
			if got := events[len(events)-1]; !reflect.DeepEqual(got, want) {
				t.Errorf("the enqueue event is %+v, want %+v", got, want)
			}

			if _, ok := c.queue(t, slug); ok {
				t.Fatalf("a second job was queued while one is pending")
			}
			if got := h.rebuildEvents(t, slug, carrypatch.EventRebuildEnqueue); len(got) != len(events) {
				t.Errorf("%s has %d enqueue events after the duplicate, want %d", slug, len(got), len(events))
			}
		})
	}
	t.Run("an enqueue whose event fails", func(t *testing.T) {
		h.carryWith(t, "refused", nil, carrypatch.StatusActive)
		h.failEvents.Store(true)
		defer h.failEvents.Store(false)
		if _, err := h.comp.Rebuilds().EnqueueRebuild(context.Background(), "refused", system("system:push", nil)); err == nil {
			t.Errorf("the enqueue succeeded while the event log refuses its write")
		}
		if js, events := h.jobsOf(t, "refused"), h.rebuildEvents(t, "refused", carrypatch.EventRebuildEnqueue); len(js) != 0 || len(events) != 0 {
			t.Errorf("refused has %d jobs and %d enqueue events, want none", len(js), len(events))
		}
	})
}

// enqueue queues a rebuild of slug through the hub's rebuild enqueuer with
// opts, and returns the id of the job queued and true, or of the pending
// job and false for a duplicate.
func (h *rebuildHub) enqueue(t *testing.T, slug string, opts carrypatch.RebuildOptions) (string, bool) {
	t.Helper()
	res, err := h.comp.Rebuilds().EnqueueRebuild(context.Background(), slug, opts)
	if err != nil {
		t.Fatalf("enqueue a rebuild of %s: %v", slug, err)
	}
	if res.Queued {
		if res.JobID == "" || res.ExistingJobID != "" {
			t.Fatalf("enqueue a rebuild of %s = %+v, want the job's id alone", slug, res)
		}
		return res.JobID, true
	}
	if res.JobID != "" || res.ExistingJobID == "" {
		t.Fatalf("enqueue a rebuild of %s = %+v, want the pending job's id alone", slug, res)
	}
	return res.ExistingJobID, false
}

func TestEnqueueRebuild_QueuesWithoutTheActivePatchCheckAndReportsADuplicate(t *testing.T) {
	t.Parallel()
	h := startRebuildHub(t)
	// The cases pass the queue in turn, so they run one after the other.
	for i, c := range []struct {
		submittedBy, strategy, failMode string
		opts                            carrypatch.RebuildOptions
	}{
		{"system:push", "merge", "continue", carrypatch.RebuildOptions{}},
		{"system:land", "rebase", "continue", carrypatch.RebuildOptions{Strategy: new("rebase"), FailMode: new("")}},
		{h.owner, "merge", "fail_fast", carrypatch.RebuildOptions{FailMode: new("fail_fast"), ActorType: "api_key", ActorID: &h.owner}},
	} {
		t.Run(c.submittedBy, func(t *testing.T) {
			slug := fmt.Sprintf("w%d", i)
			h.carryWith(t, slug, mergeAndContinue, carrypatch.StatusDisabled)
			opts := c.opts
			opts.SubmittedBy = c.submittedBy
			if opts.ActorType == "" {
				opts.ActorType = event.ActorSystem
			}
			first, queued := h.enqueue(t, slug, opts)
			if !queued {
				t.Fatalf("the first enqueue reported the pending job %s", first)
			}
			r := h.recordOf(t, first)
			if r["status"] != "queued" || r["submitted_by"] != c.submittedBy || r["strategy"] != c.strategy || r["fail_mode"] != c.failMode {
				t.Errorf("the job is %v, want queued, submitted by %s, with %s and %s", r, c.submittedBy, c.strategy, c.failMode)
			}

			if again, queued := h.enqueue(t, slug, opts); queued || again != first {
				t.Errorf("the second enqueue = %s queued %v, want the pending job %s and nothing queued", again, queued, first)
			}
			if js := h.jobsOf(t, slug); len(js) != 1 {
				t.Errorf("%s has %d jobs after the duplicate, want 1", slug, len(js))
			}

			h.drive(t, first, jobs.StatusRunning)
			follow := carrypatch.RebuildOptions{SubmittedBy: "system:followup", ActorType: event.ActorSystem, FollowUpOf: &first}
			next, queued := h.enqueue(t, slug, follow)
			if !queued {
				t.Fatalf("the follow-up reported the pending job %s", next)
			}
			h.wantStatus(t, next, jobs.StatusQueued)
			if js := h.jobsOf(t, slug); len(js) != 2 {
				t.Errorf("%s has %d jobs after the follow-up, want 2", slug, len(js))
			}
		})
	}
}

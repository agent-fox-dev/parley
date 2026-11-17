package carrypatch_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/carrypatch"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/jobs"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// mergeAndContinue are the rebuild settings that are not the defaults.
var mergeAndContinue = map[string]string{"rebuild_strategy": "merge", "rebuild_fail_mode": "continue"}

// queuedRecord is the record of the rebuild job id of slug just queued by
// the owner with strategy and failMode, stamped with the hub's clock: every
// member present, those of an attempt at their defaults.
func (h *rebuildHub) queuedRecord(id, slug, strategy, failMode string) map[string]any {
	stamp := h.clock().Truncate(time.Second).Format(time.RFC3339)
	return map[string]any{
		"id": id, "workspace_slug": slug, "status": "queued", "strategy": strategy, "fail_mode": failMode,
		"integration_branch": "deploy", "submitted_by": h.owner, "retry_count": 0.0, "next_attempt_at": nil,
		"error": nil, "conflict": nil, "created_at": stamp, "updated_at": stamp, "completed_at": nil,
		"upstream_head_sha": nil, "integration_head_sha": nil, "previous_integration_head_sha": nil,
		"patches_applied": 0.0, "patches_skipped": 0.0, "patches_conflicted": 0.0, "patches_removed": 0.0,
		"integration_branch_pushed": false, "patch_results": []any{},
	}
}

func TestSubmitRebuild_QueuesAJobAndAnswersItsRecord(t *testing.T) {
	t.Parallel()
	h := startRebuildHub(t)
	bodies := []struct {
		name               string
		body               any
		strategy, failMode string
	}{
		{"no body", nil, "", ""},
		{"an empty body", "", "", ""},
		{"white space", " \n", "", ""},
		{"an empty object", "{}", "", ""},
		{"empty and null", `{"strategy":"","fail_mode":null}`, "", ""},
		{"strategy merge", `{"strategy":"merge"}`, "merge", ""},
		{"fail_mode continue", `{"fail_mode":"continue"}`, "", "continue"},
		{"merge and continue", `{"strategy":"merge","fail_mode":"continue"}`, "merge", "continue"},
		{"rebase and fail_fast", `{"strategy":"rebase","fail_mode":"fail_fast"}`, "rebase", "fail_fast"},
	}
	settings := []struct {
		name               string
		stored             map[string]string
		strategy, failMode string
	}{
		{"settings unset", nil, "rebase", "fail_fast"},
		{"settings merge and continue", mergeAndContinue, "merge", "continue"},
	}
	type submitCase struct {
		name               string
		body               any
		stored             map[string]string
		patches            []string
		strategy, failMode string
	}
	var cases []submitCase
	for _, s := range settings {
		for _, b := range bodies {
			c := submitCase{name: s.name + ", " + b.name, body: b.body, stored: s.stored, patches: []string{carrypatch.StatusActive},
				strategy: s.strategy, failMode: s.failMode}
			if b.strategy != "" {
				c.strategy = b.strategy
			}
			if b.failMode != "" {
				c.failMode = b.failMode
			}
			cases = append(cases, c)
		}
	}
	cases = append(cases, submitCase{name: "a patch in conflict only", patches: []string{carrypatch.StatusConflict, carrypatch.StatusDeleted},
		strategy: "rebase", failMode: "fail_fast"})
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			slug := fmt.Sprintf("w%d", i)
			h.carryWith(t, slug, c.stored, c.patches...)
			a := h.submit(t, slug, c.body, nil)
			id, _ := a.patch["id"].(string)
			if a.status != http.StatusAccepted {
				t.Fatalf("submit = %d %s, want 202", a.status, a.raw)
			}
			if want := h.queuedRecord(id, slug, c.strategy, c.failMode); !reflect.DeepEqual(a.patch, want) {
				t.Errorf("submit answered %s, want %v", a.raw, want)
			}
			if r := h.readRebuild(t, slug, id); r.status != http.StatusOK || !reflect.DeepEqual(r.patch, a.patch) {
				t.Errorf("GET the job = %d %s, want 200 with the record answered %s", r.status, r.raw, a.raw)
			}
		})
	}
}

func TestSubmitRebuild_RefusesAnInvalidBody(t *testing.T) {
	t.Parallel()
	h := startRebuildHub(t)
	h.carryWith(t, "w", nil, carrypatch.StatusActive)
	for _, body := range []string{
		`{"strategy":"squash"}`, `{"strategy":"REBASE"}`, `{"fail_mode":"stop"}`, `{"strategy":1}`,
		`{"fail_mode":true}`, `{"strategy":"merge","fail_mode":"x"}`,
	} {
		t.Run(body, func(t *testing.T) {
			a := h.submit(t, "w", body, nil)
			var envelope struct {
				Error struct {
					Code int `json:"code"`
				} `json:"error"`
			}
			_ = json.Unmarshal(a.raw, &envelope)
			if a.status != http.StatusBadRequest || a.Error.ErrorType != "invalid_request" || envelope.Error.Code != http.StatusBadRequest {
				t.Errorf("submit = %d %s, want 400 invalid_request", a.status, a.raw)
			}
		})
	}
	if js, events := h.jobsOf(t, "w"), h.rebuildEvents(t, "w", carrypatch.EventRebuildEnqueue); len(js) != 0 || len(events) != 0 {
		t.Errorf("w has %d jobs and %d enqueue events after the refusals, want none", len(js), len(events))
	}
}

func TestSubmitRebuild_RefusesAWorkspaceWithoutActiveOrConflictPatches(t *testing.T) {
	t.Parallel()
	h := startRebuildHub(t)
	for i, patches := range [][]string{
		nil,
		{carrypatch.StatusDisabled},
		{carrypatch.StatusMergedUpstream},
		{carrypatch.StatusDeleted},
		{carrypatch.StatusDisabled, carrypatch.StatusMergedUpstream, carrypatch.StatusDeleted},
	} {
		t.Run(fmt.Sprint(patches), func(t *testing.T) {
			t.Parallel()
			slug := fmt.Sprintf("w%d", i)
			h.carryWith(t, slug, nil, patches...)
			a := h.submit(t, slug, nil, nil)
			if a.status != http.StatusBadRequest || a.Error.ErrorType != "no_active_patches" {
				t.Errorf("submit = %d %s, want 400 no_active_patches", a.status, a.raw)
			}
			if js, events := h.jobsOf(t, slug), h.rebuildEvents(t, slug, carrypatch.EventRebuildEnqueue); len(js) != 0 || len(events) != 0 {
				t.Errorf("%s has %d jobs and %d enqueue events, want none", slug, len(js), len(events))
			}
		})
	}
}

func TestSubmitRebuild_RefusesWhileARebuildIsPending(t *testing.T) {
	t.Parallel()
	h := startRebuildHub(t)
	// The cases pass the queue in turn, so they run one after the other.
	for i, c := range []struct {
		existing string
		// elsewhere puts the existing job on another workspace.
		elsewhere bool
		want      int
	}{
		{jobs.StatusQueued, false, http.StatusConflict},
		{jobs.StatusRunning, false, http.StatusConflict},
		{jobs.StatusFailed, false, http.StatusConflict},
		{jobs.StatusCompleted, false, http.StatusAccepted},
		{jobs.StatusDeadLetter, false, http.StatusAccepted},
		{jobs.StatusCancelled, false, http.StatusAccepted},
		{jobs.StatusQueued, true, http.StatusAccepted},
	} {
		t.Run(fmt.Sprintf("%s elsewhere %v", c.existing, c.elsewhere), func(t *testing.T) {
			slug, other := fmt.Sprintf("w%d", i), fmt.Sprintf("other%d", i)
			h.carryWith(t, slug, nil, carrypatch.StatusActive)
			h.carryWith(t, other, nil, carrypatch.StatusActive)
			at := slug
			if c.elsewhere {
				at = other
			}
			existing := h.mustSubmit(t, at)
			if c.existing != jobs.StatusQueued {
				h.drive(t, existing, c.existing)
			}
			before := h.job(t, existing)

			a := h.submit(t, slug, nil, nil)
			if a.status != c.want {
				t.Fatalf("submit = %d %s, want %d", a.status, a.raw, c.want)
			}
			jobsAfter, events := 1, 1
			if c.elsewhere {
				jobsAfter, events = 0, 0
			}
			if c.want == http.StatusConflict {
				if a.Error.ErrorType != "concurrent_rebuild" || !strings.Contains(a.Error.Message, existing) {
					t.Errorf("submit = %s, want concurrent_rebuild naming %s", a.raw, existing)
				}
			} else {
				jobsAfter, events = jobsAfter+1, events+1
			}
			if js := h.jobsOf(t, slug); len(js) != jobsAfter {
				t.Errorf("%s has %d jobs, want %d", slug, len(js), jobsAfter)
			}
			if got := h.rebuildEvents(t, slug, carrypatch.EventRebuildEnqueue); len(got) != events {
				t.Errorf("%s has %d enqueue events, want %d", slug, len(got), events)
			}
			if after := h.job(t, existing); !reflect.DeepEqual(after, before) {
				t.Errorf("the existing job is %+v after the submit, want %+v", after, before)
			}
		})
	}
}

func TestRebuild_KeepsTheOptionsItWasQueuedWith(t *testing.T) {
	t.Parallel()
	h := startRebuildHub(t)
	h.carryWith(t, "w", nil, carrypatch.StatusActive)
	id := h.mustSubmit(t, "w")
	h.configure(t, "w", mergeAndContinue)
	want := map[string]any{"strategy": "rebase", "fail_mode": "fail_fast", "integration_branch": "deploy"}
	captured := func(when string) {
		t.Helper()
		r := h.recordOf(t, id)
		for k, v := range want {
			if r[k] != v {
				t.Errorf("%s the record shows %s %v, want %v", when, k, r[k], v)
			}
		}
	}
	captured("queued, after the settings changed,")

	h.drive(t, id, jobs.StatusRunning)
	var payload map[string]any
	if err := json.Unmarshal(h.runner.payloadOf(id), &payload); err != nil {
		t.Fatalf("the attempt's payload %s: %v", h.runner.payloadOf(id), err)
	}
	for k, v := range want {
		if payload[k] != v {
			t.Errorf("the attempt was given %s %v, want %v", k, payload[k], v)
		}
	}
	captured("running,")

	h.finish(t, id, jobs.Permanent(errors.New("the patch list cannot be applied")))
	h.wantStatus(t, id, jobs.StatusDeadLetter)
	captured("dead-lettered,")

	if a := h.requeue(t, "w", id); a.status != http.StatusOK {
		t.Fatalf("requeue = %d %s, want 200", a.status, a.raw)
	}
	captured("requeued,")
	first := h.runner.payloadOf(id)
	h.drive(t, id, jobs.StatusCompleted)
	if again := h.runner.payloadOf(id); string(again) != string(first) {
		t.Errorf("the attempt after the requeue was given %s, want %s", again, first)
	}
	captured("completed after its requeue,")

	// A job queued now takes the settings as they are now.
	if a := h.submit(t, "w", nil, nil); a.status != http.StatusAccepted || a.patch["strategy"] != "merge" || a.patch["fail_mode"] != "continue" {
		t.Errorf("a later submit = %d %s, want 202 with merge and continue", a.status, a.raw)
	}
}

func TestSubmitRebuild_ReplaysARepeatedIdempotencyKey(t *testing.T) {
	t.Parallel()
	h := startRebuildHub(t)
	h.carryWith(t, "w", nil, carrypatch.StatusActive)
	key := http.Header{"Idempotency-Key": {"k1"}}
	first := h.submit(t, "w", nil, key)
	if first.status != http.StatusAccepted {
		t.Fatalf("submit = %d %s, want 202", first.status, first.raw)
	}
	again := h.submit(t, "w", nil, key)
	if again.status != http.StatusOK || string(again.raw) != string(first.raw) {
		t.Errorf("the repeated submit = %d %s, want 200 %s", again.status, again.raw, first.raw)
	}
	if js, events := h.jobsOf(t, "w"), h.rebuildEvents(t, "w", carrypatch.EventRebuildEnqueue); len(js) != 1 || len(events) != 1 {
		t.Errorf("w has %d jobs and %d enqueue events, want 1 and 1", len(js), len(events))
	}
}

func TestReadRebuild_AnswersTheWholeRecordInEveryStatus(t *testing.T) {
	t.Parallel()
	h := startRebuildHub(t)
	ctx := context.Background()
	setResult := func(t *testing.T, id, doc string) {
		t.Helper()
		if err := h.queue.SetResult(ctx, id, json.RawMessage(doc)); err != nil {
			t.Fatalf("store the result of %s: %v", id, err)
		}
	}
	const conflict = `{"files":["a.go"],"kind":"content"}`
	const full = `{"upstream_head_sha":"u1","integration_head_sha":"i1","previous_integration_head_sha":"i0",` +
		`"patches_applied":2,"patches_skipped":1,"patches_conflicted":0,"patches_removed":1,` +
		`"integration_branch_pushed":true,"patch_results":[{"patch_id":"p1"},{"patch_id":"p2"}]}`
	defaults := map[string]any{
		"error": nil, "conflict": nil, "retry_count": 0.0, "upstream_head_sha": nil, "integration_head_sha": nil,
		"patches_applied": 0.0, "integration_branch_pushed": false, "patch_results": []any{},
	}
	decode := func(doc string) map[string]any {
		var m map[string]any
		if err := json.Unmarshal([]byte(doc), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	// The cases pass the queue in turn, so they run one after the other.
	for i, c := range []struct {
		name, status string
		reach        func(t *testing.T, id string)
		want         map[string]any
	}{
		{"queued", jobs.StatusQueued, func(*testing.T, string) {}, defaults},
		{"running with no result", jobs.StatusRunning, func(t *testing.T, id string) { h.drive(t, id, jobs.StatusRunning) }, defaults},
		{"running with a result", jobs.StatusRunning, func(t *testing.T, id string) {
			h.drive(t, id, jobs.StatusRunning)
			setResult(t, id, `{"patch_results":[{"patch_id":"p1"}],"upstream_head_sha":"S"}`)
		}, map[string]any{"patch_results": []any{map[string]any{"patch_id": "p1"}}, "upstream_head_sha": "S",
			"integration_head_sha": nil, "patches_applied": 0.0, "integration_branch_pushed": false}},
		{"failed", jobs.StatusFailed, func(t *testing.T, id string) {
			h.drive(t, id, jobs.StatusRunning)
			h.finish(t, id, errors.New("fetch failed"))
		}, map[string]any{"error": "fetch failed", "retry_count": 1.0, "conflict": nil}},
		{"dead-lettered with a detail", jobs.StatusDeadLetter, func(t *testing.T, id string) {
			h.drive(t, id, jobs.StatusRunning)
			h.finish(t, id, jobs.WithDetail(jobs.Permanent(errors.New("patch 2 conflicts")), json.RawMessage(conflict)))
		}, map[string]any{"error": "patch 2 conflicts", "conflict": decode(conflict), "retry_count": 0.0}},
		{"completed with a full result", jobs.StatusCompleted, func(t *testing.T, id string) {
			h.drive(t, id, jobs.StatusRunning)
			setResult(t, id, full)
			h.finish(t, id, nil)
		}, func() map[string]any {
			m := decode(full)
			m["error"], m["conflict"] = nil, nil
			return m
		}()},
		{"cancelled", jobs.StatusCancelled, func(t *testing.T, id string) { h.drive(t, id, jobs.StatusCancelled) }, defaults},
		{"requeued", jobs.StatusQueued, func(t *testing.T, id string) {
			h.fail(t, id, 1)
			h.attempt(t, id, jobs.WithDetail(jobs.Permanent(errors.New("patch 2 conflicts")), json.RawMessage(conflict)))
			if a := h.requeue(t, h.job(t, id).Workspace, id); a.status != http.StatusOK {
				t.Fatalf("requeue = %d %s, want 200", a.status, a.raw)
			}
		}, defaults},
	} {
		t.Run(c.name, func(t *testing.T) {
			slug := fmt.Sprintf("w%d", i)
			h.carryWith(t, slug, nil, carrypatch.StatusActive)
			id := h.mustSubmit(t, slug)
			c.reach(t, id)
			h.wantStatus(t, id, c.status)

			r := h.readRebuild(t, slug, id)
			if r.status != http.StatusOK || r.patch["status"] != c.status {
				t.Fatalf("GET the job = %d %s, want 200 %s", r.status, r.raw, c.status)
			}
			if got, want := slices.Sorted(maps.Keys(r.patch)), slices.Sorted(maps.Keys(h.queuedRecord("", "", "", ""))); !slices.Equal(got, want) {
				t.Errorf("the record has the members %v, want %v", got, want)
			}
			if failed := c.status == jobs.StatusFailed; (r.patch["next_attempt_at"] != nil) != failed {
				t.Errorf("next_attempt_at is %v in %s", r.patch["next_attempt_at"], c.status)
			}
			if ended := jobs.Final(c.status); (r.patch["completed_at"] != nil) != ended {
				t.Errorf("completed_at is %v in %s", r.patch["completed_at"], c.status)
			}
			for k, v := range c.want {
				if !reflect.DeepEqual(r.patch[k], v) {
					t.Errorf("%s is %v, want %v", k, r.patch[k], v)
				}
			}
		})
	}
}

// rebuildIDs are the ids of a list's items, in order.
func rebuildIDs(a answer) []string {
	out := make([]string, 0, len(a.Items))
	for _, it := range a.Items {
		out = append(out, fmt.Sprint(it["id"]))
	}
	return out
}

func TestListRebuilds_NewestFirstFilteredAndPaged(t *testing.T) {
	t.Parallel()
	h := startRebuildHub(t)
	for _, slug := range []string{"w", "w2", "empty"} {
		h.carryWith(t, slug, nil, carrypatch.StatusActive)
	}
	// Five jobs of w, made t, t+1s, t+1s, t+2s and t+3s, and one of w2.
	var made []string
	for _, step := range []struct {
		advance time.Duration
		status  string
	}{
		{0, jobs.StatusCompleted}, {time.Second, jobs.StatusDeadLetter}, {0, jobs.StatusCancelled},
		{time.Second, jobs.StatusCompleted}, {time.Second, jobs.StatusQueued},
	} {
		h.advance(step.advance)
		id := h.mustSubmit(t, "w")
		if step.status != jobs.StatusQueued {
			h.drive(t, id, step.status)
		}
		made = append(made, id)
	}
	h.mustSubmit(t, "w2")
	sameSecond := []string{made[1], made[2]}
	slices.Sort(sameSecond)
	newestFirst := []string{made[4], made[3], sameSecond[1], sameSecond[0], made[0]}

	path := "/workspaces/w/rebuilds"
	a := h.get(t, path)
	if a.status != http.StatusOK || !slices.Equal(rebuildIDs(a), newestFirst) || a.Next != nil {
		t.Fatalf("GET %s = %d %v next %v (%s), want 200 %v and no next_cursor", path, a.status, rebuildIDs(a), a.Next, a.raw, newestFirst)
	}
	for _, it := range a.Items {
		if want := h.recordOf(t, fmt.Sprint(it["id"])); !reflect.DeepEqual(it, want) {
			t.Errorf("the item %v, want the job's record %v", it, want)
		}
	}
	if a := h.get(t, path+"?status=completed"); !slices.Equal(rebuildIDs(a), []string{made[3], made[0]}) || a.Next != nil {
		t.Errorf("GET ?status=completed = %d %s, want the two completed jobs newest first", a.status, a.raw)
	}
	for _, p := range []string{path + "?status=running", "/workspaces/empty/rebuilds"} {
		if a := h.get(t, p); a.status != http.StatusOK || !strings.Contains(string(a.raw), `"items":[]`) || a.Next != nil {
			t.Errorf("GET %s = %d %s, want 200 with no items and no next_cursor", p, a.status, a.raw)
		}
	}

	var pages [][]string
	query := "?limit=2"
	for range 5 {
		a := h.get(t, path+query)
		if a.status != http.StatusOK {
			t.Fatalf("GET %s%s = %d %s, want 200", path, query, a.status, a.raw)
		}
		pages = append(pages, rebuildIDs(a))
		if a.Next == nil {
			break
		}
		query = "?limit=2&cursor=" + url.QueryEscape(*a.Next)
	}
	if want := [][]string{newestFirst[:2], newestFirst[2:4], newestFirst[4:]}; !slices.EqualFunc(pages, want, slices.Equal) {
		t.Errorf("the pages of 2 are %v, want %v", pages, want)
	}
}

func TestListRebuilds_RefusesAStatusOutsideTheSix(t *testing.T) {
	t.Parallel()
	h := startRebuildHub(t)
	h.carryWith(t, "w", nil, carrypatch.StatusActive)
	for _, query := range []string{"status=done", "status=QUEUED", "status=", "status=queued,failed", "status=queued&status=failed"} {
		t.Run(query, func(t *testing.T) {
			t.Parallel()
			if a := h.get(t, "/workspaces/w/rebuilds?"+query); a.status != http.StatusBadRequest || a.Error.ErrorType != "invalid_request" {
				t.Errorf("GET ?%s = %d %s, want 400 invalid_request", query, a.status, a.raw)
			}
		})
	}
}

func TestRebuildJob_AnswersNotFoundForAJobTheWorkspaceDoesNotHold(t *testing.T) {
	t.Parallel()
	h := startRebuildHub(t)
	ctx := context.Background()
	h.carryWith(t, "w", nil, carrypatch.StatusActive)
	h.carryWith(t, "w2", nil, carrypatch.StatusActive)
	deadLetter := h.mustSubmit(t, "w2")
	h.drive(t, deadLetter, jobs.StatusDeadLetter)
	queued := h.mustSubmit(t, "w2")
	var otherType jobs.Job
	if err := h.Store.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		otherType, err = h.queue.Enqueue(ctx, tx, "probe", "w", json.RawMessage(`{}`))
		return err
	}); err != nil {
		t.Fatalf("queue a job of another type: %v", err)
	}
	before := map[string]jobs.Job{deadLetter: h.job(t, deadLetter), queued: h.job(t, queued), otherType.ID: h.job(t, otherType.ID)}

	routes := []struct{ method, suffix string }{{http.MethodGet, ""}, {http.MethodDelete, ""}, {http.MethodPost, "/requeue"}}
	ids := map[string]string{
		"not a UUID": "abc", "an unknown UUID": uuid.NewV4().String(), "a queued job of w2": queued,
		"a dead-lettered job of w2": deadLetter, "a job of another type": otherType.ID,
	}
	for _, rt := range routes {
		for name, id := range ids {
			t.Run(rt.method+" "+name, func(t *testing.T) {
				t.Parallel()
				a := h.call(t, h.key, rt.method, "/workspaces/w/rebuilds/"+id+rt.suffix, nil, nil)
				if a.status != http.StatusNotFound || a.Error.ErrorType != "not_found" || a.Error.Message != "rebuild job not found" {
					t.Errorf("%s %s = %d %s, want 404 not_found 'rebuild job not found'", rt.method, id, a.status, a.raw)
				}
			})
		}
	}
	t.Cleanup(func() {
		for id, j := range before {
			if after := h.job(t, id); !reflect.DeepEqual(after, j) {
				t.Errorf("the job %s is %+v after the refusals, want %+v", id, after, j)
			}
		}
		for _, slug := range []string{"w", "w2"} {
			for _, typ := range []string{carrypatch.EventRebuildCancel, carrypatch.EventRebuildRequeue} {
				if got := h.rebuildEvents(t, slug, typ); len(got) != 0 {
					t.Errorf("%s has %d %s events, want none", slug, len(got), typ)
				}
			}
		}
	})
}

// callerEvent is the one event of typ the owner's cancel or requeue of the
// job id writes: the caller as actor, no metadata.
func (h *rebuildHub) callerEvent(id string) []rebuildEvent {
	return []rebuildEvent{{ResourceType: "rebuild", ResourceID: id, ActorType: "api_key", ActorID: &h.owner, Metadata: map[string]any{}}}
}

func TestCancelRebuild_CancelsAQueuedJob(t *testing.T) {
	t.Parallel()
	h := startRebuildHub(t)
	h.carryWith(t, "w", nil, carrypatch.StatusActive)
	id := h.mustSubmit(t, "w")

	a := h.cancel(t, "w", id)
	if a.status != http.StatusOK || a.patch["status"] != jobs.StatusCancelled || a.patch["completed_at"] == nil {
		t.Fatalf("cancel = %d %s, want 200 cancelled with completed_at", a.status, a.raw)
	}
	if want := h.recordOf(t, id); !reflect.DeepEqual(a.patch, want) {
		t.Errorf("cancel answered %s, want the job's record %v", a.raw, want)
	}
	if got, want := h.rebuildEvents(t, "w", carrypatch.EventRebuildCancel), h.callerEvent(id); !reflect.DeepEqual(got, want) {
		t.Errorf("the cancel events are %+v, want %+v", got, want)
	}
	h.pass(t)
	h.wantStatus(t, id, jobs.StatusCancelled)
	if n := h.runner.callsOf(id); n != 0 {
		t.Errorf("the handler was called %d times for the cancelled job, want never", n)
	}
	if a := h.submit(t, "w", nil, nil); a.status != http.StatusAccepted {
		t.Errorf("a submit after the cancel = %d %s, want 202", a.status, a.raw)
	}
}

func TestCancelRebuild_RefusesAJobThatIsNotQueued(t *testing.T) {
	t.Parallel()
	h := startRebuildHub(t)
	// The cases pass the queue in turn, so they run one after the other.
	for i, status := range []string{jobs.StatusRunning, jobs.StatusFailed, jobs.StatusCompleted, jobs.StatusDeadLetter, jobs.StatusCancelled} {
		t.Run(status, func(t *testing.T) {
			slug := fmt.Sprintf("w%d", i)
			h.carryWith(t, slug, nil, carrypatch.StatusActive)
			id := h.mustSubmit(t, slug)
			h.drive(t, id, status)
			before, events := h.job(t, id), len(h.rebuildEvents(t, slug, carrypatch.EventRebuildCancel))

			if a := h.cancel(t, slug, id); a.status != http.StatusConflict || a.Error.ErrorType != "job_not_cancellable" {
				t.Errorf("cancel = %d %s, want 409 job_not_cancellable", a.status, a.raw)
			}
			if after := h.job(t, id); !reflect.DeepEqual(after, before) {
				t.Errorf("the job is %+v after the refusal, want %+v", after, before)
			}
			if got := len(h.rebuildEvents(t, slug, carrypatch.EventRebuildCancel)); got != events {
				t.Errorf("%s has %d cancel events after the refusal, want %d", slug, got, events)
			}
		})
	}
}

func TestCancelRebuild_EitherCancelsOrLosesToTheClaim(t *testing.T) {
	t.Parallel()
	h := startRebuildHub(t)
	h.carryWith(t, "w", nil, carrypatch.StatusActive)
	// Every other pass is preceded by a request like the cancel, a read of
	// the job, so that the claim reaches the store first in some rounds and
	// the cancel in others.
	transport := &http.Transport{}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport}
	outcomes := map[int]int{}
	for i := range 100 {
		id := h.mustSubmit(t, "w")
		attempt := h.runner.expect(id)
		attempt.outcome <- nil
		passed := make(chan error, 1)
		go func() {
			req, err := http.NewRequest(http.MethodGet, h.URL("/api/v1/workspaces/w/rebuilds/"+id), nil)
			if err == nil && i%2 == 1 {
				req.Header.Set("Authorization", "Bearer "+h.key)
				var res *http.Response
				if res, err = client.Do(req); err == nil {
					err = res.Body.Close()
				}
			}
			if err != nil {
				passed <- err
				return
			}
			passed <- h.queue.Pass(context.Background())
		}()
		a := h.cancel(t, "w", id)
		if err := <-passed; err != nil {
			t.Fatalf("pass the job queue: %v", err)
		}
		outcomes[a.status]++
		switch {
		case a.status == http.StatusOK && a.patch["status"] == jobs.StatusCancelled:
			h.wantStatus(t, id, jobs.StatusCancelled)
			if n := h.runner.callsOf(id); n != 0 {
				t.Fatalf("the handler was called %d times for the cancelled job %s, want never", n, id)
			}
		case a.status == http.StatusConflict && a.Error.ErrorType == "job_not_cancellable":
			h.settled(t, attempt)
			h.wantStatus(t, id, jobs.StatusCompleted)
			if n := h.runner.callsOf(id); n != 1 {
				t.Fatalf("the handler was called %d times for the claimed job %s, want once", n, id)
			}
		default:
			t.Fatalf("cancel = %d %s, want 200 cancelled or 409 job_not_cancellable", a.status, a.raw)
		}
	}
	t.Logf("cancels answered by status: %v", outcomes)
}

func TestRequeueRebuild_QueuesADeadLetteredJobAgain(t *testing.T) {
	t.Parallel()
	h := startRebuildHub(t)
	const detail = `{"files":["a.go"],"kind":"content"}`
	// The cases pass the queue in turn, so they run one after the other.
	for i, c := range []struct {
		name       string
		deadLetter func(t *testing.T, id string)
		retries    int
	}{
		{"a permanent error with a detail", func(t *testing.T, id string) {
			h.attempt(t, id, jobs.WithDetail(jobs.Permanent(errors.New("patch 2 conflicts")), json.RawMessage(detail)))
		}, 0},
		{"its retries exhausted", func(t *testing.T, id string) {
			h.fail(t, id, jobs.MaxRetries)
			h.attempt(t, id, errors.New("fetch failed"))
		}, jobs.MaxRetries},
	} {
		t.Run(c.name, func(t *testing.T) {
			slug := fmt.Sprintf("w%d", i)
			h.carryWith(t, slug, nil, carrypatch.StatusActive)
			id := h.mustSubmit(t, slug)
			c.deadLetter(t, id)
			before := h.recordOf(t, id)
			if before["status"] != jobs.StatusDeadLetter || before["retry_count"] != float64(c.retries) || before["error"] == nil {
				t.Fatalf("the job before the requeue is %v, want dead_letter with an error at retry_count %d", before, c.retries)
			}
			payload, calls := h.runner.payloadOf(id), h.runner.callsOf(id)

			a := h.requeue(t, slug, id)
			if a.status != http.StatusOK {
				t.Fatalf("requeue = %d %s, want 200", a.status, a.raw)
			}
			want := map[string]any{"status": jobs.StatusQueued, "retry_count": 0.0, "error": nil, "conflict": nil, "completed_at": nil, "next_attempt_at": nil}
			for _, k := range []string{"id", "created_at", "strategy", "fail_mode", "integration_branch", "submitted_by"} {
				want[k] = before[k]
			}
			for k, v := range want {
				if !reflect.DeepEqual(a.patch[k], v) {
					t.Errorf("requeue answered %s %v, want %v", k, a.patch[k], v)
				}
			}
			if stored := h.recordOf(t, id); !reflect.DeepEqual(a.patch, stored) {
				t.Errorf("requeue answered %s, want the job's record %v", a.raw, stored)
			}
			if got, want := h.rebuildEvents(t, slug, carrypatch.EventRebuildRequeue), h.callerEvent(id); !reflect.DeepEqual(got, want) {
				t.Errorf("the requeue events are %+v, want %+v", got, want)
			}

			h.drive(t, id, jobs.StatusCompleted)
			if n, again := h.runner.callsOf(id), h.runner.payloadOf(id); n != calls+1 || string(again) != string(payload) {
				t.Errorf("after the requeue the handler was called %d more times with %s, want once with %s", n-calls, again, payload)
			}
		})
	}
}

func TestRequeueRebuild_RefusesAJobThatIsNotDeadLettered(t *testing.T) {
	t.Parallel()
	h := startRebuildHub(t)
	// The cases pass the queue in turn, so they run one after the other.
	for i, c := range []struct {
		status string
		// otherQueued queues another job of the workspace before the requeue.
		otherQueued bool
	}{
		{jobs.StatusQueued, false}, {jobs.StatusRunning, false}, {jobs.StatusFailed, false},
		{jobs.StatusCompleted, false}, {jobs.StatusCancelled, false},
		{jobs.StatusCompleted, true}, {jobs.StatusCancelled, true},
	} {
		t.Run(fmt.Sprintf("%s another queued %v", c.status, c.otherQueued), func(t *testing.T) {
			slug := fmt.Sprintf("w%d", i)
			h.carryWith(t, slug, nil, carrypatch.StatusActive)
			id := h.mustSubmit(t, slug)
			if c.status != jobs.StatusQueued {
				h.drive(t, id, c.status)
			}
			if c.otherQueued {
				h.mustSubmit(t, slug)
			}
			before := h.job(t, id)

			if a := h.requeue(t, slug, id); a.status != http.StatusConflict || a.Error.ErrorType != "job_not_requeueable" {
				t.Errorf("requeue = %d %s, want 409 job_not_requeueable", a.status, a.raw)
			}
			if after := h.job(t, id); !reflect.DeepEqual(after, before) {
				t.Errorf("the job is %+v after the refusal, want %+v", after, before)
			}
			if got := h.rebuildEvents(t, slug, carrypatch.EventRebuildRequeue); len(got) != 0 {
				t.Errorf("%s has %d requeue events, want none", slug, len(got))
			}
		})
	}
}

func TestRequeueRebuild_RefusesWhileAnotherRebuildIsPending(t *testing.T) {
	t.Parallel()
	h := startRebuildHub(t)
	// The cases pass the queue in turn, so they run one after the other.
	for i, c := range []struct {
		other string
		want  int
	}{
		{jobs.StatusQueued, http.StatusConflict}, {jobs.StatusRunning, http.StatusConflict}, {jobs.StatusFailed, http.StatusConflict},
		{jobs.StatusCompleted, http.StatusOK}, {jobs.StatusCancelled, http.StatusOK}, {jobs.StatusDeadLetter, http.StatusOK},
	} {
		t.Run("the other "+c.other, func(t *testing.T) {
			slug := fmt.Sprintf("w%d", i)
			h.carryWith(t, slug, nil, carrypatch.StatusActive)
			id := h.mustSubmit(t, slug)
			h.drive(t, id, jobs.StatusDeadLetter)
			other := h.mustSubmit(t, slug)
			if c.other != jobs.StatusQueued {
				h.drive(t, other, c.other)
			}
			before, otherBefore := h.job(t, id), h.job(t, other)

			a := h.requeue(t, slug, id)
			if a.status != c.want {
				t.Fatalf("requeue = %d %s, want %d", a.status, a.raw, c.want)
			}
			if after := h.job(t, other); !reflect.DeepEqual(after, otherBefore) {
				t.Errorf("the other job is %+v after the requeue, want %+v", after, otherBefore)
			}
			if c.want == http.StatusOK {
				h.wantStatus(t, id, jobs.StatusQueued)
				return
			}
			if a.Error.ErrorType != "concurrent_rebuild" || !strings.Contains(a.Error.Message, other) {
				t.Errorf("requeue = %s, want concurrent_rebuild naming %s", a.raw, other)
			}
			if after := h.job(t, id); !reflect.DeepEqual(after, before) {
				t.Errorf("the job is %+v after the refusal, want %+v", after, before)
			}
			if got := h.rebuildEvents(t, slug, carrypatch.EventRebuildRequeue); len(got) != 0 {
				t.Errorf("%s has %d requeue events, want none", slug, len(got))
			}
		})
	}
}

// jobRoute is one of the rebuild routes as the refusal tests send it.
type jobRoute struct {
	method string
	// path follows /workspaces/<slug>; :id stands for the job the route
	// names.
	path  string
	write bool
	// names is the status of the job the route names, "" for none.
	names string
	// ready is set on a route that refuses outside a ready workspace: every
	// write route, and the preview, a read route that reads the copy rather
	// than stored state.
	ready bool
}

func (rt jobRoute) String() string { return rt.method + " " + rt.path }

// jobRoutes are the rebuild routes. The cancel names a queued job, the
// requeue a dead-lettered one and the rollback a completed one with a
// previous head, so that only the refusal under test stands between the
// request and a change.
var jobRoutes = []jobRoute{
	{http.MethodPost, "/rebuild", true, "", true},
	{http.MethodGet, "/rebuilds", false, "", false},
	{http.MethodGet, "/rebuilds/:id", false, jobs.StatusQueued, false},
	{http.MethodDelete, "/rebuilds/:id", true, jobs.StatusQueued, true},
	{http.MethodPost, "/rebuilds/:id/requeue", true, jobs.StatusDeadLetter, true},
	{http.MethodPost, "/rebuilds/:id/rollback", true, jobs.StatusCompleted, true},
	{http.MethodGet, "/rebuild-preview", false, "", true},
}

// send sends the route for slug with credential and body; the job it
// names is that of ids under the route's status.
func (rt jobRoute) send(t *testing.T, h *rebuildHub, credential, slug string, ids map[string]string, body any) answer {
	t.Helper()
	return h.call(t, credential, rt.method, "/workspaces/"+slug+strings.ReplaceAll(rt.path, ":id", ids[rt.names]), body, nil)
}

// rebuildState is what the rebuild routes change in a workspace: its
// rebuild jobs, how many parley.rebuild.* events of each type it has, and
// the head of its integration branch deploy, "" when it has none.
type rebuildState struct {
	Jobs   []jobs.Job
	Events map[string]int
	Deploy string
}

func (h *rebuildHub) rebuildState(t *testing.T, slug string) rebuildState {
	t.Helper()
	s := rebuildState{Jobs: h.jobsOf(t, slug), Events: map[string]int{}, Deploy: refIn(h.repoPath(slug), "refs/heads/deploy")}
	for _, typ := range []string{carrypatch.EventRebuildEnqueue, carrypatch.EventRebuildCancel, carrypatch.EventRebuildRequeue, carrypatch.EventRebuildRollback} {
		s.Events[typ] = len(h.rebuildEvents(t, slug, typ))
	}
	return s
}

// unchanged fails the test when the rebuild state of slug differs from
// before.
func (h *rebuildHub) unchanged(t *testing.T, slug string, before rebuildState) {
	t.Helper()
	if after := h.rebuildState(t, slug); !reflect.DeepEqual(after, before) {
		t.Errorf("the rebuild jobs and events of %s changed:\n got %+v\nwant %+v", slug, after, before)
	}
}

// routeJobs gives slug a copy made by rollbackRepo, whose upstream branch
// main, the workspace's default branch, is at H1 so that a preview has a
// base, and the jobs the routes name: a completed rebuild that moved
// deploy from H1 to H2, a dead-lettered one, then a queued one; it returns
// their ids by status.
func (h *rebuildHub) routeJobs(t *testing.T, slug string) map[string]string {
	t.Helper()
	dir, h1, h2 := h.rollbackRepo(t, slug)
	gitrepotest.Git(t, dir, "update-ref", "refs/remotes/upstream/main", h1)
	completed := h.settledRebuild(t, slug, rebuiltFrom(h1, h2), nil)
	deadLetter := h.queueRebuild(t, slug)
	h.drive(t, deadLetter, jobs.StatusDeadLetter)
	return map[string]string{jobs.StatusCompleted: completed, jobs.StatusDeadLetter: deadLetter, jobs.StatusQueued: h.queueRebuild(t, slug)}
}

func TestRebuildRoutes_RefuseCallersWithoutTheScopeOrAccess(t *testing.T) {
	t.Parallel()
	h := startRebuildHub(t)
	h.carryWith(t, "w", nil, carrypatch.StatusActive)
	ids := h.routeJobs(t, "w")
	before := h.rebuildState(t, "w")
	member := h.CreateUser(t, "member")
	h.AddMember(t, h.org, member)
	memberKey := h.IssueKey(t, member)
	outsiderKey := h.IssueKey(t, h.CreateUser(t, "outsider"))
	patchPAT := h.IssuePAT(t, identity.PATSpec{UserID: h.owner, Permissions: []string{"patches:read", "patches:write"}})
	readPAT := h.IssuePAT(t, identity.PATSpec{UserID: h.owner, Permissions: []string{"rebuilds:read"}})

	type want struct {
		status    int
		errorType string
	}
	served := want{http.StatusOK, ""}
	scope := want{http.StatusForbidden, "insufficient_scope"}
	notFound := want{http.StatusNotFound, "not_found"}
	callers := []struct {
		name, credential, slug string
		write, read            want
	}{
		{"a PAT with the patch scopes only", patchPAT, "w", scope, scope},
		{"a PAT with the patch scopes only on an unknown slug", patchPAT, "missing", scope, scope},
		{"a PAT with rebuilds:read only", readPAT, "w", scope, served},
		{"a user outside the organization", outsiderKey, "w", notFound, notFound},
		{"the owner on an unknown slug", h.key, "missing", notFound, notFound},
		{"a member with read access", memberKey, "w", want{http.StatusForbidden, "forbidden"}, served},
	}
	bodies := map[string]map[jobRoute]string{}
	for _, c := range callers {
		bodies[c.name] = map[jobRoute]string{}
		for _, rt := range jobRoutes {
			t.Run(c.name+"/"+rt.String(), func(t *testing.T) {
				w := c.read
				if rt.write {
					w = c.write
				}
				gits := len(h.gitCommands())
				a := rt.send(t, h, c.credential, c.slug, ids, nil)
				if a.status != w.status || a.Error.ErrorType != w.errorType {
					t.Errorf("%s = %d %s, want %d %s", rt, a.status, a.raw, w.status, w.errorType)
				}
				if ran := h.gitCommands()[gits:]; w.status != http.StatusOK && len(ran) != 0 {
					t.Errorf("the refused %s ran git: %q", rt, ran)
				}
				bodies[c.name][rt] = string(a.raw)
				h.unchanged(t, "w", before)
			})
		}
	}
	// An outsider learns nothing a caller naming no workspace would not.
	for _, rt := range jobRoutes {
		if outsider, unknown := bodies["a user outside the organization"][rt], bodies["the owner on an unknown slug"][rt]; outsider != unknown {
			t.Errorf("%s answered the outsider %s and the unknown slug %s, want the same", rt, outsider, unknown)
		}
	}
	t.Run("a member's invalid submit is refused for the access first", func(t *testing.T) {
		if a := jobRoutes[0].send(t, h, memberKey, "w", ids, map[string]any{"strategy": "squash"}); a.status != http.StatusForbidden || a.Error.ErrorType != "forbidden" {
			t.Errorf("submit = %d %s, want 403 forbidden", a.status, a.raw)
		}
		h.unchanged(t, "w", before)
	})
}

func TestRebuildRoutes_RefuseAWorkspaceNotInCarryPatchMode(t *testing.T) {
	t.Parallel()
	h := startRebuildHub(t)
	h.carryWith(t, "w", nil, carrypatch.StatusActive)
	h.add(t, "plain", workspace.ModeStandard, workspace.StatusReady, workspace.MarkerNone)
	// The mode is answered before the state.
	h.add(t, "plain-archived", workspace.ModeStandard, workspace.StatusArchived, workspace.MarkerNone)
	// A task fork is always in mode standard, whatever its parent's mode.
	h.add(t, "w-fork", workspace.ModeStandard, workspace.StatusReady, workspace.MarkerNone, func(w *workspace.Workspace) {
		w.Kind, w.ParentSlug, w.GitURL = access.KindFork, new("w"), nil
	})
	// Ids that name no job: the mode is answered before the job is looked
	// up.
	ids := map[string]string{jobs.StatusQueued: uuid.NewV4().String(), jobs.StatusDeadLetter: uuid.NewV4().String(), jobs.StatusCompleted: uuid.NewV4().String()}
	for _, slug := range []string{"plain", "plain-archived", "w-fork"} {
		for _, rt := range jobRoutes {
			t.Run(slug+"/"+rt.String(), func(t *testing.T) {
				if a := rt.send(t, h, h.key, slug, ids, nil); a.status != http.StatusBadRequest || a.Error.ErrorType != "workspace_mode_mismatch" {
					t.Errorf("%s on %s = %d %s, want 400 workspace_mode_mismatch", rt, slug, a.status, a.raw)
				}
			})
		}
	}
	t.Run("an invalid submit is refused as such first", func(t *testing.T) {
		if a := jobRoutes[0].send(t, h, h.key, "plain", ids, map[string]any{"strategy": "squash"}); a.status != http.StatusBadRequest || a.Error.ErrorType != "invalid_request" {
			t.Errorf("submit on plain = %d %s, want 400 invalid_request", a.status, a.raw)
		}
	})
}

// The writes, and the preview, which reads the copy, are refused outside a
// ready workspace without running git; the reads of stored jobs are served.
func TestRebuildRoutes_RefuseWritesButServeReadsOutsideAReadyWorkspace(t *testing.T) {
	t.Parallel()
	h := startRebuildHub(t)
	// No patch: a submit that got past the workspace's state would be
	// refused for that.
	h.carryWith(t, "w", nil)
	h.drive(t, h.queueRebuild(t, "w"), jobs.StatusCompleted)
	ids := h.routeJobs(t, "w")
	if a := jobRoutes[0].send(t, h, h.key, "w", ids, nil); a.status != http.StatusBadRequest || a.Error.ErrorType != "no_active_patches" {
		t.Fatalf("submit on the ready workspace = %d %s, want 400 no_active_patches", a.status, a.raw)
	}
	kept := map[jobRoute][]byte{}
	for _, rt := range jobRoutes {
		if !rt.write {
			a := rt.send(t, h, h.key, "w", ids, nil)
			if a.status != http.StatusOK {
				t.Fatalf("%s on the ready workspace = %d %s, want 200", rt, a.status, a.raw)
			}
			kept[rt] = a.raw
		}
	}
	before := h.rebuildState(t, "w")
	unknown := map[string]string{jobs.StatusQueued: uuid.NewV4().String(), jobs.StatusDeadLetter: uuid.NewV4().String(), jobs.StatusCompleted: uuid.NewV4().String()}

	// The cases change the state of w in turn, so they run one after the
	// other.
	for _, s := range []struct {
		name   string
		status workspace.Status
		marker workspace.Marker
		want   string
	}{
		{"pending", workspace.StatusPending, workspace.MarkerNone, "workspace_busy"},
		{"cloning", workspace.StatusCloning, workspace.MarkerNone, "workspace_busy"},
		{"a reclone running", workspace.StatusReady, workspace.MarkerReclone, "workspace_busy"},
		{"an archive running", workspace.StatusReady, workspace.MarkerArchive, "workspace_busy"},
		{"a delete running", workspace.StatusReady, workspace.MarkerDelete, "workspace_busy"},
		{"failed", workspace.StatusFailed, workspace.MarkerNone, "workspace_not_ready"},
		{"archived", workspace.StatusArchived, workspace.MarkerNone, "workspace_archived"},
	} {
		t.Run(s.name, func(t *testing.T) {
			h.setState(t, "w", s.status, s.marker)
			gits := len(h.gitCommands())
			for _, rt := range jobRoutes {
				if !rt.ready {
					if a := rt.send(t, h, h.key, "w", ids, nil); a.status != http.StatusOK || !bytes.Equal(a.raw, kept[rt]) {
						t.Errorf("%s = %d %s, want 200 %s as in the ready workspace", rt, a.status, a.raw, kept[rt])
					}
					continue
				}
				for _, named := range []map[string]string{ids, unknown} {
					if a := rt.send(t, h, h.key, "w", named, nil); a.status != http.StatusConflict || a.Error.ErrorType != s.want {
						t.Errorf("%s for the job %s = %d %s, want 409 %s", rt, named[rt.names], a.status, a.raw, s.want)
					}
				}
			}
			if ran := h.gitCommands()[gits:]; len(ran) != 0 {
				t.Errorf("the requests ran git: %q", ran)
			}
			h.unchanged(t, "w", before)
		})
	}
}

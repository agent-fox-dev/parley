package carrypatch_test

import (
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

	"github.com/agent-fox-dev/parley/internal/carrypatch"
	"github.com/agent-fox-dev/parley/internal/jobs"
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

	routes := []struct{ method, suffix string }{{http.MethodGet, ""}}
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
		for _, typ := range []string{"parley.rebuild.cancel", "parley.rebuild.requeue"} {
			if got := h.rebuildEvents(t, "w2", typ); len(got) != 0 {
				t.Errorf("w2 has %d %s events, want none", len(got), typ)
			}
		}
	})
}

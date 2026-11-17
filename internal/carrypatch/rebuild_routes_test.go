package carrypatch_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

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
			if stored := h.recordOf(t, id); !reflect.DeepEqual(stored, a.patch) {
				t.Errorf("the stored job is %v, want the record answered %s", stored, a.raw)
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

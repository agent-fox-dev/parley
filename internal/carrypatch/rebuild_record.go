package carrypatch

import (
	"bytes"
	"encoding/json"

	"github.com/agent-fox-dev/parley/internal/jobs"
	"github.com/agent-fox-dev/parley/internal/server/timefmt"
)

// RebuildRecord is a rebuild job as the API shows it: every member present,
// null where it is unset. strategy, fail_mode, integration_branch and
// submitted_by are what the job was queued with; conflict is the detail of
// its last failure; the members from upstream_head_sha on are what its
// running or last attempt recorded as its result.
type RebuildRecord struct {
	ID                string          `json:"id"`
	Workspace         string          `json:"workspace_slug"`
	Status            string          `json:"status"`
	Strategy          string          `json:"strategy"`
	FailMode          string          `json:"fail_mode"`
	IntegrationBranch string          `json:"integration_branch"`
	SubmittedBy       string          `json:"submitted_by"`
	RetryCount        int             `json:"retry_count"`
	NextAttemptAt     *timefmt.Time   `json:"next_attempt_at"`
	Error             *string         `json:"error"`
	Conflict          json.RawMessage `json:"conflict"`
	CreatedAt         timefmt.Time    `json:"created_at"`
	UpdatedAt         timefmt.Time    `json:"updated_at"`
	CompletedAt       *timefmt.Time   `json:"completed_at"`
	rebuildResult
}

// rebuildResult is the result document of a rebuild job's attempt, the
// members of its record that the attempt reports. A member the document
// does not hold is null, 0, false or [] in the record.
type rebuildResult struct {
	UpstreamHeadSHA            *string         `json:"upstream_head_sha"`
	IntegrationHeadSHA         *string         `json:"integration_head_sha"`
	PreviousIntegrationHeadSHA *string         `json:"previous_integration_head_sha"`
	PatchesApplied             int             `json:"patches_applied"`
	PatchesSkipped             int             `json:"patches_skipped"`
	PatchesConflicted          int             `json:"patches_conflicted"`
	PatchesRemoved             int             `json:"patches_removed"`
	IntegrationBranchPushed    bool            `json:"integration_branch_pushed"`
	PatchResults               json.RawMessage `json:"patch_results"`
}

// RebuildRecordOf is the rebuild job j as the API shows it. A payload or
// result document that does not decode leaves the members it would give at
// their defaults, so that a record is always whole.
func RebuildRecordOf(j jobs.Job) RebuildRecord {
	var p rebuildPayload
	if j.Payload != nil {
		_ = json.Unmarshal(j.Payload, &p)
	}
	var res rebuildResult
	if j.Result != nil {
		if json.Unmarshal(j.Result, &res) != nil {
			res = rebuildResult{}
		}
	}
	if len(res.PatchResults) == 0 || bytes.Equal(res.PatchResults, []byte("null")) {
		res.PatchResults = json.RawMessage("[]")
	}
	r := RebuildRecord{
		ID: j.ID, Workspace: j.Workspace, Status: j.Status,
		Strategy: p.Strategy, FailMode: p.FailMode, IntegrationBranch: p.IntegrationBranch, SubmittedBy: p.SubmittedBy,
		RetryCount: j.RetryCount, NextAttemptAt: timeOf(j.NextAttemptAt), Conflict: j.Detail,
		CreatedAt: timefmt.Time(j.CreatedAt), UpdatedAt: timefmt.Time(j.UpdatedAt), CompletedAt: timeOf(j.CompletedAt),
		rebuildResult: res,
	}
	if j.Error != nil && *j.Error != "" {
		r.Error = j.Error
	}
	return r
}

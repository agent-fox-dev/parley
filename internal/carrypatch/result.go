package carrypatch

import "encoding/json"

// The statuses of a patch's result in a rebuild.
const (
	resultSuccess  = "success"
	resultConflict = "conflict"
	resultSkipped  = "skipped"
)

// The reasons a run skips a patch.
const (
	skippedDisabled       = "disabled"
	skippedMergedUpstream = "merged_upstream"
	skippedBranchNotFound = "branch_not_found"
	skippedNothingToApply = "nothing_to_apply"
)

// patchResult is what a run did with one patch, as the job's patch_results
// lists it: the patch as the run listed it, the tip of its branch the run
// read then (nil when the branch did not exist), and what became of it.
type patchResult struct {
	PatchID       string          `json:"patch_id"`
	BranchName    string          `json:"branch_name"`
	Position      int             `json:"position"`
	Status        string          `json:"status"`
	SkippedReason *string         `json:"skipped_reason"`
	SourceSHA     *string         `json:"source_sha"`
	NewHeadSHA    *string         `json:"new_head_sha"`
	ConflictFiles []string        `json:"conflict_files"`
	Conflict      json.RawMessage `json:"conflict"`
}

// MarshalJSON writes every member of the result, each that its status does
// not give as null: new_head_sha for a success alone, skipped_reason for a
// skip alone, conflict_files and conflict for a conflict alone, and
// conflict_files as [] when it holds no file.
func (r patchResult) MarshalJSON() ([]byte, error) {
	if r.Status != resultSuccess {
		r.NewHeadSHA = nil
	}
	if r.Status != resultSkipped {
		r.SkippedReason = nil
	}
	if r.Status != resultConflict {
		r.ConflictFiles, r.Conflict = nil, nil
	}
	if r.ConflictFiles == nil {
		r.ConflictFiles = []string{}
	}
	// The plain type has no MarshalJSON, which would call this one again.
	type plain patchResult
	return json.Marshal(plain(r))
}

// resultOf is the result of the patch p as the run listed it, before the
// run has done anything with it.
func resultOf(p runPatch) patchResult {
	return patchResult{PatchID: p.ID, BranchName: p.BranchName, Position: *p.Position, SourceSHA: p.source}
}

// skipReason is the reason the run skips the patch p without applying it,
// "" when the run applies it: a patch in status conflict is applied as an
// active one, since its branch may apply now.
func skipReason(p runPatch) string {
	switch {
	case p.Status == StatusDisabled:
		return skippedDisabled
	case p.Status == StatusMergedUpstream:
		return skippedMergedUpstream
	case p.source == nil:
		return skippedBranchNotFound
	}
	return ""
}

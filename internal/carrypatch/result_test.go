package carrypatch_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/carrypatch"
)

func TestPatchResult_JSONShapeByStatus(t *testing.T) {
	t.Parallel()
	source, head := strings.Repeat("a", 40), strings.Repeat("b", 40)
	// Every result sets every member, as a careless caller might: the JSON
	// holds only those its status gives.
	full := func(status, reason string) carrypatch.PatchResult {
		return carrypatch.PatchResult{
			PatchID: "p1", BranchName: "patch/1", Position: 1, Status: status,
			SkippedReason: new(reason), SourceSHA: new(source), NewHeadSHA: new(head),
			ConflictFiles: []string{"f.txt"}, Conflict: json.RawMessage(`{"patch_id":"p1"}`),
		}
	}
	cases := []struct {
		name   string
		result carrypatch.PatchResult
		// set are the members the status gives a value to, besides the
		// patch's own.
		set map[string]any
	}{
		{"success", full(carrypatch.ResultSuccess, ""), map[string]any{"new_head_sha": head}},
		{"conflict", full(carrypatch.ResultConflict, ""), map[string]any{
			"conflict_files": []any{"f.txt"}, "conflict": map[string]any{"patch_id": "p1"},
		}},
		{"skipped as disabled", full(carrypatch.ResultSkipped, carrypatch.SkippedDisabled),
			map[string]any{"skipped_reason": carrypatch.SkippedDisabled}},
		{"skipped as merged upstream", full(carrypatch.ResultSkipped, carrypatch.SkippedMergedUpstream),
			map[string]any{"skipped_reason": carrypatch.SkippedMergedUpstream}},
		{"skipped with nothing to apply", full(carrypatch.ResultSkipped, carrypatch.SkippedNothingToApply),
			map[string]any{"skipped_reason": carrypatch.SkippedNothingToApply}},
		{"skipped without a branch", func() carrypatch.PatchResult {
			r := full(carrypatch.ResultSkipped, carrypatch.SkippedBranchNotFound)
			r.SourceSHA = nil
			return r
		}(), map[string]any{"skipped_reason": carrypatch.SkippedBranchNotFound, "source_sha": nil}},
		{"zero value of a status", carrypatch.PatchResult{PatchID: "p1", BranchName: "patch/1", Position: 1, Status: carrypatch.ResultConflict},
			map[string]any{"source_sha": nil}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			raw, err := json.Marshal(tc.result)
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]any
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}
			want := map[string]any{
				"patch_id": "p1", "branch_name": "patch/1", "position": float64(1), "status": tc.result.Status,
				"skipped_reason": nil, "source_sha": source, "new_head_sha": nil, "conflict_files": []any{}, "conflict": nil,
			}
			for k, v := range tc.set {
				want[k] = v
			}
			if len(got) != len(want) {
				t.Errorf("the JSON %s has %d members, want the nine", raw, len(got))
			}
			for k, v := range want {
				g, ok := got[k]
				if !ok {
					t.Errorf("the JSON %s has no member %s", raw, k)
					continue
				}
				if gj, wj := mustJSON(t, g), mustJSON(t, v); gj != wj {
					t.Errorf("%s = %s, want %s", k, gj, wj)
				}
			}
		})
	}
}

// mustJSON is v as JSON text.
func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

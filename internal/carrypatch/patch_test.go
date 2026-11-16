package carrypatch_test

import (
	"encoding/json"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/carrypatch"
)

func TestObjectOf_WritesEveryMember(t *testing.T) {
	t.Parallel()
	added := time.Date(2026, 3, 4, 5, 6, 7, 890_000_000, time.FixedZone("+02:00", 2*60*60))
	fresh := carrypatch.Patch{
		ID: "6f1d7f2e-0d55-4c1c-9d0e-000000000001", Workspace: "demo", BranchName: "fix/a", Position: new(2),
		Status: carrypatch.StatusActive, AddedAt: added, UpdatedAt: added,
	}
	deleted := fresh
	deleted.Position, deleted.Status, deleted.DeletedAt = nil, carrypatch.StatusDeleted, &added
	deleted.ConflictFiles = []string{"a.go"}
	cases := []struct {
		name     string
		p        carrypatch.Patch
		replaced string
		want     string
	}{
		{"a new patch", fresh, "", `{"id":"6f1d7f2e-0d55-4c1c-9d0e-000000000001","workspace_slug":"demo","branch_name":"fix/a",` +
			`"position":2,"status":"active","conflict_files":[],"upstream_pr_url":null,"description":null,` +
			`"added_at":"2026-03-04T03:06:07Z","updated_at":"2026-03-04T03:06:07Z","deleted_at":null,` +
			`"origin_sync_state":null,"origin_sha":null,"origin_synced_at":null}`},
		{"a deleted patch with a backup ref", deleted, "0123456789abcdef0123456789abcdef01234567",
			`{"id":"6f1d7f2e-0d55-4c1c-9d0e-000000000001","workspace_slug":"demo","branch_name":"fix/a",` +
				`"position":null,"status":"deleted","conflict_files":["a.go"],"upstream_pr_url":null,"description":null,` +
				`"added_at":"2026-03-04T03:06:07Z","updated_at":"2026-03-04T03:06:07Z","deleted_at":"2026-03-04T03:06:07Z",` +
				`"origin_sync_state":null,"origin_sha":null,"origin_synced_at":null,` +
				`"replaced_sha":"0123456789abcdef0123456789abcdef01234567"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := json.Marshal(carrypatch.ObjectOf(c.p, c.replaced))
			if err != nil {
				t.Fatal(err)
			}
			var gotMembers, wantMembers map[string]json.RawMessage
			if err := json.Unmarshal(got, &gotMembers); err != nil {
				t.Fatalf("%s: %v", got, err)
			}
			if err := json.Unmarshal([]byte(c.want), &wantMembers); err != nil {
				t.Fatal(err)
			}
			if !maps.EqualFunc(gotMembers, wantMembers, func(a, b json.RawMessage) bool { return string(a) == string(b) }) {
				t.Errorf("ObjectOf = %s\nwant      %s (members %v)", got, c.want, slices.Sorted(maps.Keys(wantMembers)))
			}
		})
	}
}

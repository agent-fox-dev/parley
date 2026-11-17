package carrypatch_test

import (
	"fmt"
	"maps"
	"testing"

	"github.com/agent-fox-dev/parley/internal/carrypatch"
	"github.com/agent-fox-dev/parley/internal/gitrepo/gitrepotest"
)

func TestRebuild_PublishMovesOnlyTheIntegrationBranch(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		exists bool
	}{
		{"deploy exists and is moved", true},
		{"deploy does not exist and is created", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := startRebuildFixture(t)
			f.addPatch(t, "p1")
			f.addPatch(t, "p2")
			var previous any
			if tc.exists {
				h0 := commitFiles(t, f.copy, f.base, "H0", map[string]string{"h0.txt": "h0\n"})
				gitrepotest.Git(t, f.copy, "update-ref", "refs/heads/deploy", h0)
				previous = h0
			} else {
				// The set-up at the attempt's start creates a missing
				// integration branch: it goes after that.
				f.hooks.beforePublish(func() {
					if _, err := gitrepotest.Run(f.copy, "", "update-ref", "-d", "refs/heads/deploy"); err != nil {
						t.Errorf("delete deploy: %v", err)
					}
				})
			}
			before, err := visibleRefs(f.copy)
			if err != nil {
				t.Fatal(err)
			}

			out, id := f.attempt(t, nil)
			if out.Kind != attemptCompleted {
				t.Fatalf("the attempt = %+v, want completed", out)
			}
			res, results := f.result(t, id), f.patchResults(t, id)
			if len(results) != 2 {
				t.Fatalf("patch_results = %v, want two", results)
			}
			deploy := refIn(f.copy, "refs/heads/deploy")
			if last := results[1]["new_head_sha"]; deploy == "" || last != deploy || res["integration_head_sha"] != deploy {
				t.Errorf("deploy = %q, the last new_head_sha = %v, integration_head_sha = %v; want all three the run's head", deploy, last, res["integration_head_sha"])
			}
			if got := res["previous_integration_head_sha"]; got != previous {
				t.Errorf("previous_integration_head_sha = %v, want %v", got, previous)
			}
			after, err := visibleRefs(f.copy)
			if err != nil {
				t.Fatal(err)
			}
			delete(after, "refs/heads/deploy")
			delete(before, "refs/heads/deploy")
			if !maps.Equal(after, before) {
				t.Errorf("the refs other than deploy after the publish = %v, want them as before, %v", after, before)
			}
		})
	}
}

func TestRebuild_PublishesTheUpstreamBaseWhenNothingApplies(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		statuses []string
	}{
		{"no patch at all", nil},
		{"one disabled and one merged upstream patch", []string{carrypatch.StatusDisabled, carrypatch.StatusMergedUpstream}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := startRebuildFixture(t)
			for i, status := range tc.statuses {
				f.addPatch(t, fmt.Sprintf("p%d", i+1), withStatus(status))
			}
			base := f.moveUpstream(t, "U2", map[string]string{"u2.txt": "u2\n"})

			out, id := f.attempt(t, nil)
			if out.Kind != attemptCompleted {
				t.Fatalf("the attempt = %+v, want completed", out)
			}
			res := f.result(t, id)
			deploy := refIn(f.copy, "refs/heads/deploy")
			if deploy != base || res["integration_head_sha"] != base || res["upstream_head_sha"] != base {
				t.Errorf("deploy = %s, integration_head_sha = %v, upstream_head_sha = %v; want all three the upstream base %s",
					deploy, res["integration_head_sha"], res["upstream_head_sha"], base)
			}
		})
	}
}

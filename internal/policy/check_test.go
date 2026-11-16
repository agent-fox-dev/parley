package policy

import (
	"testing"

	"github.com/agent-fox-dev/parley/internal/change"
)

func TestApplying_IsTheOwnResultElseTheNearestOneThroughPatchEquivalence(t *testing.T) {
	t.Parallel()
	versions := []change.Version{{Number: 1}, {Number: 2, PatchEquivalentTo: new(int64(1))},
		{Number: 3, PatchEquivalentTo: new(int64(2))}, {Number: 4}, {Number: 5, PatchEquivalentTo: new(int64(4))},
		{Number: 6, PatchEquivalentTo: new(int64(3))}}
	result := func(version int64, name, status string) storedCheck {
		return storedCheck{Version: version, Name: name, Status: status}
	}
	reported := []storedCheck{result(1, "test", checkPassed), result(2, "test", checkFailed),
		result(1, "lint", checkPassed), result(4, "test", checkPending)}
	ownPending := append(reported[:len(reported):len(reported)], result(3, "test", checkPending))
	for _, tc := range []struct {
		name    string
		results []storedCheck
		version int64
		check   string
		// want is the version of the applying result, 0 for none.
		want int64
	}{
		{"test on 3 is the one on 2", reported, 3, "test", 2},
		{"test on 6 is the one on 2", reported, 6, "test", 2},
		{"lint on 6 is the one on 1", reported, 6, "lint", 1},
		{"test on 1 is its own", reported, 1, "test", 1},
		{"test on 5 is the one on 4", reported, 5, "test", 4},
		{"lint on 5 is none", reported, 5, "lint", 0},
		{"lint on 4 is none", reported, 4, "lint", 0},
		{"build on 3 is none", reported, 3, "build", 0},
		{"test on 3 with its own pending result is that one", ownPending, 3, "test", 3},
		{"test on 6 with a pending result on 3 is that one", ownPending, 6, "test", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := indexChecks(equivalenceChain(versions, tc.version), tc.results).applying(tc.check)
			if !ok {
				if tc.want != 0 {
					t.Fatalf("no applying result, want the one on %d", tc.want)
				}
				return
			}
			var want storedCheck
			for _, r := range tc.results {
				if r.Version == tc.want && r.Name == tc.check {
					want = r
				}
			}
			if tc.want == 0 || got.Version != want.Version || got.Name != want.Name || got.Status != want.Status {
				t.Errorf("applying result %+v, want %+v", got, want)
			}
		})
	}
}

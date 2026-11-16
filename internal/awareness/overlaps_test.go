package awareness_test

import (
	"fmt"
	"maps"
	"slices"
	"testing"
)

func TestPartners_LeaveOutTheChangesOfTheSameStack(t *testing.T) {
	cases := []struct {
		name string
		// dependsOn is each change's depends_on, 0 for none.
		dependsOn map[int64]int64
		// want are the partners of each change.
		want map[int64][]int64
	}{
		{
			name:      "two changes in one stack",
			dependsOn: map[int64]int64{1: 0, 2: 1},
			want:      map[int64][]int64{1: {}, 2: {}},
		},
		{
			name:      "three changes in one stack",
			dependsOn: map[int64]int64{1: 0, 2: 1, 3: 2},
			want:      map[int64][]int64{1: {}, 2: {}, 3: {}},
		},
		{
			name:      "two changes on one bottom",
			dependsOn: map[int64]int64{1: 0, 4: 1, 5: 1},
			want:      map[int64][]int64{1: {}, 4: {}, 5: {}},
		},
		{
			name:      "a change outside the stack",
			dependsOn: map[int64]int64{1: 0, 2: 1, 3: 2, 4: 1, 5: 1, 6: 0},
			want:      map[int64][]int64{1: {6}, 2: {6}, 3: {6}, 4: {6}, 5: {6}, 6: {1, 2, 3, 4, 5}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			for _, n := range slices.Sorted(maps.Keys(tc.dependsOn)) {
				ch := f.touching(n, 1, "", "api/a.go")
				if d := tc.dependsOn[n]; d != 0 {
					ch.DependsOn = &d
				}
				f.version(ch)
			}
			f.pass()
			var wantPairs []string
			for _, n := range slices.Sorted(maps.Keys(tc.want)) {
				if got := f.partners(n); !slices.Equal(got, tc.want[n]) {
					t.Errorf("partners of %d = %v, want %v", n, got, tc.want[n])
				}
				for _, p := range tc.want[n] {
					if n < p {
						wantPairs = append(wantPairs, fmt.Sprintf("%d-%d", n, p), fmt.Sprintf("%d-%d", n, p))
					}
				}
			}
			slices.Sort(wantPairs)
			if got := f.announced(); !slices.Equal(got, wantPairs) {
				t.Errorf("announced = %v, want %v (no pair within a stack)", got, wantPairs)
			}
		})
	}
}

package perf_test

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/agent-fox-dev/parley/test/perf"
)

// shuffled is the integers 1 to n in an order of a fixed seed.
func shuffled(n int) []float64 {
	s := make([]float64, n)
	for i := range s {
		s[i] = float64(i + 1)
	}
	r := rand.New(rand.NewPCG(1, uint64(n)))
	r.Shuffle(n, func(i, j int) { s[i], s[j] = s[j], s[i] })
	return s
}

func TestP95_IsTheNearestRankSampleAndLeavesTheListAlone(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		samples []float64
		want    float64
	}{
		{"one sample", []float64{7}, 7},
		{"two samples", []float64{1, 2}, 2},
		{"twenty", shuffled(20), 19},
		{"twenty-one", shuffled(21), 20},
		{"a hundred", shuffled(100), 95},
		{"fifty", shuffled(50), 48},
		{"equal samples", []float64{5, 5, 5}, 5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			in := slices.Clone(tt.samples)
			v, ok := perf.P95(tt.samples)
			if !ok || v != tt.want {
				t.Errorf("P95 = %v, %v, want %v, true", v, ok, tt.want)
			}
			if !slices.Equal(tt.samples, in) {
				t.Errorf("P95 reordered its samples: %v, was %v", tt.samples, in)
			}
		})
	}
	if v, ok := perf.P95(nil); ok {
		t.Errorf("P95 of no samples = %v, true, want no measurement", v)
	}
}

func TestVerdict_ComparesTheMeasurementWithTheTarget(t *testing.T) {
	t.Parallel()
	m := func(v float64) *float64 { return &v }
	tests := []struct {
		comparison string
		target     float64
		measured   *float64
		want       perf.Status
	}{
		{"lt", 1000, m(999.9), perf.Met},
		{"lt", 1000, m(1000), perf.Missed},
		{"le", 60, m(60), perf.Met},
		{"le", 60, m(60.1), perf.Missed},
		{"le", 0, m(0), perf.Met},
		{"le", 0, m(1), perf.Missed},
		{"ge", 60, m(60), perf.Met},
		{"ge", 60, m(59.9), perf.Missed},
		{"lt", 1000, nil, perf.Error},
		{"ge", 60, nil, perf.Error},
	}
	for _, tt := range tests {
		target := perf.Target{Comparison: tt.comparison, Value: tt.target, Unit: "ms"}
		if got := perf.Verdict(target, tt.measured); got != tt.want {
			measured := "no measurement"
			if tt.measured != nil {
				measured = fmt.Sprint(*tt.measured)
			}
			t.Errorf("Verdict(%s %v, %s) = %q, want %q", tt.comparison, tt.target, measured, got, tt.want)
		}
	}
}

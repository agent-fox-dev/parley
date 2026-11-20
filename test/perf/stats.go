package perf

import (
	"math"
	"slices"
)

// The comparisons of a target: the measured value is under, at most or at
// least the target's value.
const (
	LT = "lt"
	LE = "le"
	GE = "ge"
)

// Target is what a scenario's measurement is compared with.
type Target struct {
	Comparison string  `json:"comparison"`
	Value      float64 `json:"value"`
	Unit       string  `json:"unit"`
}

// Status is a scenario's verdict.
type Status string

// The verdicts of a scenario.
const (
	// Met is a measurement that satisfies the target.
	Met Status = "met"
	// Missed is a measurement that does not.
	Missed Status = "missed"
	// Skipped is a scenario whose variable is not set.
	Skipped Status = "skipped"
	// Error is a scenario with no measurement, or one that ended in an
	// error.
	Error Status = "error"
)

// P95 is the nearest-rank 95th percentile of samples: of the n samples
// sorted ascending, the one at position ceil(0.95·n), counting from 1. It
// reports false, no measurement, for no samples, and leaves samples in the
// order it was given them.
func P95(samples []float64) (float64, bool) {
	if len(samples) == 0 {
		return 0, false
	}
	sorted := slices.Sorted(slices.Values(samples))
	// 95·n/100 in integers, so that no rounding of 0.95 moves the rank.
	rank := (95*len(sorted) + 99) / 100
	return sorted[rank-1], true
}

// Verdict is the status of a measured value against t: met or missed by
// t's comparison, and error when there is no measurement or the comparison
// is not one of lt, le and ge.
func Verdict(t Target, measured *float64) Status {
	if measured == nil || math.IsNaN(*measured) {
		return Error
	}
	var ok bool
	switch v := *measured; t.Comparison {
	case LT:
		ok = v < t.Value
	case LE:
		ok = v <= t.Value
	case GE:
		ok = v >= t.Value
	default:
		return Error
	}
	if ok {
		return Met
	}
	return Missed
}

package sandbox_test

import (
	"testing"

	"github.com/agent-fox-dev/parley/internal/sandbox"
)

func TestReleaseRange_TakesTheLowestReleaseAndRefusesTheFirstUnsupportedOne(t *testing.T) {
	t.Parallel()
	r := sandbox.ReleaseRange{Lowest: "1.2.0", FirstUnsupported: "2.0.0"}
	for _, tc := range []struct {
		release string
		want    bool
	}{
		{"1.1.9", false},
		{"1.2.0", true},
		{"1.9.3", true},
		{"1.19.0", true},
		{"2.0.0", false},
		{"2.1.0", false},
		{"v1.4.0", true},
		{"1.4.0+build.7", true},
		{"1.2.0-rc.1", false},
		{"1.4.0-rc.1", true},
		{"nightly", false},
		{"", false},
		{"v", false},
		{"1.4", false},
		{"1.4.0.1", false},
		{"01.4.0", false},
		{"1.4.x", false},
		{"V1.4.0", false},
		{"1.4.0-", false},
	} {
		if got := r.Contains(tc.release); got != tc.want {
			t.Errorf("Contains(%q) = %v, want %v", tc.release, got, tc.want)
		}
	}
}

func TestReleaseRange_IsWrittenWithItsTwoBounds(t *testing.T) {
	t.Parallel()
	if got := (sandbox.ReleaseRange{Lowest: "1.2.0", FirstUnsupported: "2.0.0"}).String(); got != ">=1.2.0 <2.0.0" {
		t.Errorf("String() = %q, want %q", got, ">=1.2.0 <2.0.0")
	}
	if got := sandbox.SupportedReleases.String(); got != ">="+sandbox.LowestSupportedRelease+" <"+sandbox.FirstUnsupportedRelease {
		t.Errorf("SupportedReleases = %q, want the two constants", got)
	}
}

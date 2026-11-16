package awareness_test

import (
	"testing"

	"github.com/agent-fox-dev/parley/internal/awareness"
)

func TestCanOverlap_AnswersWhetherTwoGlobsCanMatchTheSameFile(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"api/**", "api/v1/*.go", true},
		{"api/**", "api/**", true},
		{"src/*.go", "src/main.go", true},
		{"src/*.go", "src/**", true},
		{"*.md", "**", true},
		{"docs/new/**", "docs/**", true},
		{"docs/**", "src/**", false},
		{"**/*.go", "src/x.py", false},
		{"src/*.go", "src/x/y.go", false},
		{"api/a.go", "api/a.go", true},
		{"api/a.go", "api/b.go", false},
	} {
		if got := awareness.CanOverlap(tc.a, tc.b); got != tc.want {
			t.Errorf("CanOverlap(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
		if got := awareness.CanOverlap(tc.b, tc.a); got != tc.want {
			t.Errorf("CanOverlap(%q, %q) = %v, want %v", tc.b, tc.a, got, tc.want)
		}
	}
}

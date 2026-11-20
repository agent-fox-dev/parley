package upgrade_test

import (
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/test/upgrade"
)

func TestCompatible_AcceptsRecordedStepsRegisteredInTheSameOrderAndNamesTheFirstThatIsNot(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name                 string
		recorded, registered []string
		want                 []string // texts of the error; nil for compatible
	}{
		{"nothing recorded or registered", nil, nil, nil},
		{"nothing recorded", nil, []string{"a"}, nil},
		{"new steps after the recorded ones", []string{"a", "b"}, []string{"a", "b", "c"}, nil},
		{"a new step between recorded ones", []string{"a", "c"}, []string{"a", "b", "c"}, nil},
		{"a removed step", []string{"a", "b"}, []string{"a"}, []string{`"b"`, "not registered"}},
		{"a renamed step", []string{"a", "b"}, []string{"a", "b2"}, []string{`"b"`, "not registered"}},
		{"steps registered in another order", []string{"a", "b"}, []string{"b", "a"}, []string{`"b"`, "out of order"}},
		{"nothing registered", []string{"a"}, nil, []string{`"a"`, "not registered"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := upgrade.Compatible(tt.recorded, tt.registered)
			if (err == nil) != (tt.want == nil) {
				t.Fatalf("Compatible(%q, %q) = %v, want an error: %v", tt.recorded, tt.registered, err, tt.want != nil)
			}
			for _, text := range tt.want {
				if !strings.Contains(err.Error(), text) {
					t.Errorf("Compatible(%q, %q) = %q, want it to contain %q", tt.recorded, tt.registered, err, text)
				}
			}
		})
	}
}

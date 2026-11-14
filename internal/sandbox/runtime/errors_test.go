package runtime_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
)

var kinds = []error{runtime.ErrNotFound, runtime.ErrInvalid, runtime.ErrUnsupported, runtime.ErrUnavailable}

func TestError_IsOfExactlyOneKindAndKeepsTheReason(t *testing.T) {
	t.Parallel()
	rows := []struct {
		name string
		kind error
		want error
	}{
		{"not found", runtime.ErrNotFound, runtime.ErrNotFound},
		{"invalid", runtime.ErrInvalid, runtime.ErrInvalid},
		{"unsupported", runtime.ErrUnsupported, runtime.ErrUnsupported},
		{"unavailable", runtime.ErrUnavailable, runtime.ErrUnavailable},
		{"no kind is unavailable", nil, runtime.ErrUnavailable},
		{"another error is unavailable", errors.New("other"), runtime.ErrUnavailable},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			err := error(runtime.NewError(row.kind, runtime.OpValidatePolicy, "bad rule"))
			for _, e := range []error{err, fmt.Errorf("checking the policy: %w", err)} {
				var held []error
				for _, k := range kinds {
					if errors.Is(e, k) {
						held = append(held, k)
					}
				}
				if len(held) != 1 || held[0] != row.want {
					t.Fatalf("%q is of kinds %v, want exactly %v", e, held, row.want)
				}
				if runtime.KindOf(e) != row.want {
					t.Fatalf("KindOf(%q) = %v, want %v", e, runtime.KindOf(e), row.want)
				}
			}
			if text := err.Error(); !strings.Contains(text, "bad rule") || !strings.Contains(text, runtime.OpValidatePolicy) || !strings.Contains(text, row.want.Error()) {
				t.Fatalf("text %q does not name the operation, the kind and the reason", text)
			}
		})
	}
}

func TestKindOf_TakesAnErrorWithoutAKindForUnavailable(t *testing.T) {
	t.Parallel()
	if k := runtime.KindOf(nil); k != nil {
		t.Fatalf("KindOf(nil) = %v, want nil", k)
	}
	if k := runtime.KindOf(errors.New("connection reset")); k != runtime.ErrUnavailable {
		t.Fatalf("KindOf(plain error) = %v, want unavailable", k)
	}
	if k := runtime.KindOf(fmt.Errorf("x: %w", runtime.ErrNotFound)); k != runtime.ErrNotFound {
		t.Fatalf("KindOf(wrapped ErrNotFound) = %v, want not found", k)
	}
}

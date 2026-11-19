package sandbox_test

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"testing"

	"github.com/agent-fox-dev/parley/internal/sandbox"
)

func TestHooks_RunInOrderAndStopAtTheFirstError(t *testing.T) {
	t.Parallel()
	type hooks struct {
		register func(*sandbox.Service, sandbox.Hook)
		run      func(*sandbox.Service, context.Context, *sql.Tx, string) error
	}
	deleteHooks := hooks{(*sandbox.Service).RegisterDeleteHook, (*sandbox.Service).RunDeleteHooks}
	purgeHooks := hooks{(*sandbox.Service).RegisterPurgeHook, (*sandbox.Service).RunPurgeHooks}
	cases := []struct {
		name        string
		list, other hooks
	}{{"delete hooks", deleteHooks, purgeHooks}, {"purge hooks", purgeHooks, deleteHooks}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			svc := sandbox.NewService()
			failed := errors.New("hook failed")
			var ran []string
			hook := func(name string, err error) sandbox.Hook {
				return func(_ context.Context, _ *sql.Tx, id string) error {
					ran = append(ran, name+":"+id)
					return err
				}
			}
			tc.list.register(svc, hook("a", nil))
			tc.list.register(svc, hook("b", failed))
			tc.list.register(svc, hook("c", nil))
			tc.other.register(svc, hook("other", nil))

			if err := tc.list.run(svc, context.Background(), nil, "sbx_1"); !errors.Is(err, failed) {
				t.Errorf("run = %v; want the error of the failing hook", err)
			}
			if want := []string{"a:sbx_1", "b:sbx_1"}; !slices.Equal(ran, want) {
				t.Errorf("ran %v; want %v", ran, want)
			}
		})
	}
}

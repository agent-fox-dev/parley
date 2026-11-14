package fork_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/agent-fox-dev/parley/internal/fork"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

func TestCheckParentState(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w := newWorld(t, nil)
	w.seedFork(t, "demo--f")
	for _, c := range []struct {
		status workspace.Status
		marker workspace.Marker
		use    fork.Use
		want   string // "" for no error
	}{
		{workspace.StatusReady, workspace.MarkerNone, fork.UseRead, ""},
		{workspace.StatusReady, workspace.MarkerNone, fork.UseWrite, ""},
		{workspace.StatusArchived, workspace.MarkerNone, fork.UseRead, ""},
		{workspace.StatusArchived, workspace.MarkerNone, fork.UseWrite, "parent_archived"},
		{workspace.StatusPending, workspace.MarkerNone, fork.UseRead, "workspace_busy"},
		{workspace.StatusPending, workspace.MarkerNone, fork.UseWrite, "workspace_busy"},
		{workspace.StatusCloning, workspace.MarkerNone, fork.UseRead, "workspace_busy"},
		{workspace.StatusCloning, workspace.MarkerNone, fork.UseWrite, "workspace_busy"},
		{workspace.StatusReady, workspace.MarkerArchive, fork.UseRead, "workspace_busy"},
		{workspace.StatusReady, workspace.MarkerArchive, fork.UseWrite, "workspace_busy"},
		{workspace.StatusFailed, workspace.MarkerNone, fork.UseRead, "workspace_not_ready"},
		{workspace.StatusFailed, workspace.MarkerNone, fork.UseWrite, "workspace_not_ready"},
	} {
		t.Run(fmt.Sprintf("%s %q use %d", c.status, c.marker, c.use), func(t *testing.T) {
			w.setParent(t, c.status, c.marker)
			err := w.forks.CheckParentState(ctx, "demo--f", c.use)
			if c.want == "" {
				if err != nil {
					t.Fatalf("CheckParentState = %v, want nil", err)
				}
				return
			}
			if e, ok := errors.AsType[*server.Error](err); !ok || e.Status() != http.StatusConflict || e.Type() != c.want {
				t.Fatalf("CheckParentState = %#v, want 409 %s", err, c.want)
			}
		})
	}
	if err := w.forks.CheckParentState(ctx, parentSlug, fork.UseWrite); err != nil {
		t.Errorf("CheckParentState(%s) = %v, want nil for a repository workspace", parentSlug, err)
	}
}

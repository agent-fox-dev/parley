package workspace_test

import (
	"cmp"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

func TestRequireReady_AnswersByMarkerFirstThenByStatus(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		status    workspace.Status
		marker    workspace.Marker
		errorType string // "" when the request passes
	}{
		{workspace.StatusPending, workspace.MarkerNone, "workspace_busy"},
		{workspace.StatusCloning, workspace.MarkerNone, "workspace_busy"},
		{workspace.StatusReady, workspace.MarkerReclone, "workspace_busy"},
		{workspace.StatusFailed, workspace.MarkerReclone, "workspace_busy"},
		{workspace.StatusReady, workspace.MarkerArchive, "workspace_busy"},
		{workspace.StatusArchived, workspace.MarkerDelete, "workspace_busy"},
		{workspace.StatusFailed, workspace.MarkerNone, "workspace_not_ready"},
		{workspace.StatusArchived, workspace.MarkerNone, "workspace_archived"},
		{workspace.StatusReady, workspace.MarkerNone, ""},
	} {
		name := string(c.status) + " marker " + cmp.Or(string(c.marker), "none")
		t.Run(name, func(t *testing.T) {
			err := workspace.RequireReady(workspace.Workspace{Slug: "acme", Status: c.status, Marker: c.marker})
			if c.errorType == "" {
				if err != nil {
					t.Errorf("RequireReady = %v, want nil", err)
				}
				return
			}
			var se *server.Error
			if !errors.As(err, &se) || se.Status() != http.StatusConflict || se.Type() != c.errorType {
				t.Errorf("RequireReady = %v, want 409 %s", err, c.errorType)
			}
		})
	}
}

func TestRequireReady_LetsEveryConcurrentRequestOnAReadyWorkspacePass(t *testing.T) {
	t.Parallel()
	ready := workspace.Workspace{Slug: "acme", Status: workspace.StatusReady}
	start := make(chan struct{})
	errs := make(chan error, 50)
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() { <-start; errs <- workspace.RequireReady(ready) })
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("RequireReady on a ready workspace = %v, want nil", err)
		}
	}
}

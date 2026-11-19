package audit

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/telemetry"
)

// HubEvents are the stored hub events the query reads, newest first and
// under the visibility rule for the caller: the event component.
type HubEvents interface {
	ReadNewest(ctx context.Context, caller event.Caller, q event.NewestQuery) ([]event.Record, error)
}

// RunEvents are the stored run events the query reads, newest first and
// confined to a set of workspaces: the telemetry component.
type RunEvents interface {
	NewestRunEvents(ctx context.Context, q telemetry.RunEventQuery) ([]telemetry.RunEventRecord, error)
}

// Readable is the access rule's readable set of a caller, which for a
// workspace-bound PAT holds only what its binding covers: the access
// service.
type Readable interface {
	Readable(ctx context.Context, c access.Caller) (access.ReadableSet, error)
}

// Forks are the workspace records of the task forks that exist.
type Forks interface {
	ForksOf(ctx context.Context, parents []string) ([]access.Record, error)
}

// forkSeparator separates a task fork's parent from its name in its slug.
const forkSeparator = "--"

// runScope is the set of workspaces whose run events sc may see: every
// workspace, deleted ones included, for the admin token; otherwise the
// caller's readable set and, unless the caller is a workspace-bound PAT,
// the deleted task forks of the repositories in that set, whose records
// stay their parent's history. An existing fork the caller cannot read
// stays out. A bound PAT sees only what its binding covers, which holds no
// deleted fork, as for hub events.
func (c *Component) runScope(ctx context.Context, sc server.Caller) (telemetry.Scope, error) {
	if sc.Admin {
		return telemetry.Scope{All: true}, nil
	}
	caller := access.CallerFrom(sc)
	set, err := c.deps.Access.Readable(ctx, caller)
	if err != nil {
		return telemetry.Scope{}, fmt.Errorf("read the caller's readable set: %w", err)
	}
	scope := telemetry.Scope{Workspaces: slices.Sorted(maps.Keys(set.Entries))}
	if caller.Workspace != "" {
		return scope, nil
	}
	for _, slug := range scope.Workspaces {
		if !strings.Contains(slug, forkSeparator) {
			scope.ForkParents = append(scope.ForkParents, slug)
		}
	}
	if len(scope.ForkParents) == 0 {
		return scope, nil
	}
	forks, err := c.deps.Forks.ForksOf(ctx, scope.ForkParents)
	if err != nil {
		return telemetry.Scope{}, fmt.Errorf("read the forks of the caller's workspaces: %w", err)
	}
	for _, f := range forks {
		if !set.Contains(f.Slug) {
			scope.Except = append(scope.Except, f.Slug)
		}
	}
	return scope, nil
}

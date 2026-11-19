package audit

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/telemetry"
)

// readFilters reads the filters of the audit query r and refuses, with 400
// invalid_request naming the parameter, a value outside its set or form:
// source, actor_type and severity outside their sets, a session_id that is
// not a UUID, a run_id that is not a run id, a since or until that is not
// RFC 3339 or a since not before until (the shared validator of the run
// reads), and a types list event.ParseTypes refuses. A filter of the one
// source given with source naming the other is refused as well, naming the
// filter: it would select nothing, which an audit must not answer quietly.
// Parameters it does not know are ignored.
func readFilters(r *http.Request) (filters, error) {
	q := telemetry.ReadQuery(r)
	var f filters
	if s := q.OneOf("source", sourceHub, sourceAgent); s != nil {
		f.source = *s
	}
	f.workspace = q.String("workspace")
	f.sessionID = q.UUID("session_id")
	f.since, f.until = q.Range()
	f.actorID, f.actorType = q.String("actor_id"), q.OneOf("actor_type", actorTypes...)
	f.resourceType, f.resourceID, f.action = q.String("resource_type"), q.String("resource_id"), q.String("action")
	f.runID, f.nodeID = q.RunID("run_id"), q.String("node_id")
	f.severity = q.OneOf("severity", telemetry.Severities()...)
	if err := q.Err(); err != nil {
		return filters{}, err
	}
	if raw := q.String("types"); raw != nil {
		types, err := event.ParseTypes(*raw)
		if err != nil {
			return filters{}, err
		}
		f.types = types
	}
	if name := firstGiven(f.hubOnly()); name != "" && f.source == sourceAgent {
		return filters{}, server.InvalidRequest(fmt.Sprintf("%s filters hub events and cannot be given with source=%s", name, sourceAgent))
	}
	if name := firstGiven(f.agentOnly()); name != "" && f.source == sourceHub {
		return filters{}, server.InvalidRequest(fmt.Sprintf("%s filters run events and cannot be given with source=%s", name, sourceHub))
	}
	return f, nil
}

// readsHub reports whether the list may hold hub events: unless source
// names the agents or a filter of run events only is given.
func (f filters) readsHub() bool {
	return f.source != sourceAgent && firstGiven(f.agentOnly()) == ""
}

// readsAgent reports whether the list may hold run events: unless source
// names the hub or a filter of hub events only is given.
func (f filters) readsAgent() bool {
	return f.source != sourceHub && firstGiven(f.hubOnly()) == ""
}

// hubQuery is the read of the hub events f selects, limit of them.
func (f filters) hubQuery(limit int) event.NewestQuery {
	return event.NewestQuery{
		Since: f.since, Until: f.until, Types: f.types, Workspace: f.workspace, SessionID: f.sessionID,
		ActorType: f.actorType, ActorID: f.actorID, ResourceType: f.resourceType, ResourceID: f.resourceID,
		Action: f.action, Limit: limit,
	}
}

// agentQuery is the read of the run events f selects, limit of them.
func (f filters) agentQuery(limit int) telemetry.RunEventQuery {
	return telemetry.RunEventQuery{
		Since: f.since, Until: f.until, Types: f.types, Workspace: f.workspace, SessionID: f.sessionID,
		RunID: f.runID, NodeID: f.nodeID, Severity: f.severity, Limit: limit,
	}
}

// checkWorkspace refuses the workspace filter slug, before anything is
// read, unless caller may filter by it: the admin token by any slug, a
// deleted workspace's or one no workspace has included; any other caller
// by a workspace the event component's rule lets it read (inside the
// binding for a workspace-bound token), or by a task fork's slug,
// <parent>--<name>, whose parent that rule lets it read, which covers a
// fork that was deleted. Every refusal is event.ErrWorkspaceNotFound, so
// that an unreadable workspace and a missing one answer the same bytes.
func (c *Component) checkWorkspace(ctx context.Context, caller event.Caller, slug *string) error {
	if slug == nil || caller.Admin {
		return nil
	}
	if *slug == "" {
		return event.ErrWorkspaceNotFound
	}
	err := c.deps.Hub.CheckWorkspace(ctx, caller, *slug)
	if !errors.Is(err, event.ErrWorkspaceNotFound) {
		return err
	}
	parent, _, fork := strings.Cut(*slug, forkSeparator)
	if !fork || parent == "" {
		return err
	}
	return c.deps.Hub.CheckWorkspace(ctx, caller, parent)
}

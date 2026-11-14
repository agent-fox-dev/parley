package gitserver

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/server"
)

// EventFetch is the type of the event that records a fetch from a workspace
// whose setting audit_fetches is true.
const EventFetch = "parley.git.fetch"

// settingAuditFetches is the workspace setting that asks for the fetch
// event.
const settingAuditFetches = "audit_fetches"

// EventSink stores a hub event in a transaction of its own: the hub's
// event log, *event.Component, is one.
type EventSink interface {
	EmitNow(ctx context.Context, in event.Input) (event.Record, error)
}

// Settings answers the effective settings of a workspace from its stored
// settings document and its default branch, every registered key with its
// value or its default: *workspace.SettingsRegistry is one.
type Settings interface {
	EffectiveSettings(stored map[string]json.RawMessage, defaultBranch *string) map[string]any
}

// auditsFetches reports whether the workspace of x has its setting
// audit_fetches at true. Without Settings, or while no feature has
// registered the key, it is false.
func (c *Component) auditsFetches(x Exchange) bool {
	if c.deps.Settings == nil {
		return false
	}
	on, _ := c.deps.Settings.EffectiveSettings(x.Workspace.Settings, x.Workspace.DefaultBranch)[settingAuditFetches].(bool)
	return on
}

// recordFetch writes the parley.git.fetch event of a served fetch that sent
// a pack, when the workspace audits its fetches: the addressed workspace,
// the caller as its actor, and whether the request asked for a shallow
// history and with which filter. The fetch is served whatever becomes of
// its event, so a sink that fails is logged and nothing more, and without a
// sink no event is written. The event outlives a client that has gone
// away, since the fetch it records was served.
func (c *Component) recordFetch(r *http.Request, x Exchange, scan *requestScanner) {
	if c.deps.Events == nil || !c.auditsFetches(x) {
		return
	}
	shallow, filter := scan.args()
	meta := map[string]any{"shallow": shallow, "filter": nil}
	if filter != nil {
		meta["filter"] = *filter
	}
	slug := x.Workspace.Slug
	in := event.Input{
		Type: EventFetch, Workspace: &slug, ActorType: credentialType(x.Principal),
		ResourceType: "workspace", ResourceID: slug, Metadata: meta,
	}
	if x.Caller.UserID != "" {
		in.ActorID = &x.Caller.UserID
	}
	ctx := context.WithoutCancel(r.Context())
	if _, err := c.deps.Events.EmitNow(ctx, in); err != nil {
		c.deps.Logger.LogAttrs(ctx, slog.LevelError, "git fetch event not written",
			slog.String("event", EventFetch),
			slog.String("request_id", server.RequestID(ctx)),
			slog.String("repository", r.PathValue("repo")),
			slog.String("error", err.Error()),
		)
	}
}

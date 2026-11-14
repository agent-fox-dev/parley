package gitserver

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/agent-fox-dev/parley/internal/access"
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
// audit_fetches at true. A task fork has no settings of its own: it has its
// parent's, read now, as the workspace object of a fork shows them, so a
// fork is audited when its parent is. Without Settings, or while no feature
// has registered the key, it is false; so is it for a fork whose parent is
// not found or cannot be read, the latter logged, since the fetch it would
// record has been served.
func (c *Component) auditsFetches(ctx context.Context, x Exchange) bool {
	if c.deps.Settings == nil {
		return false
	}
	w := x.Workspace
	if w.Kind == access.KindFork {
		if w.ParentSlug == nil || c.deps.Records == nil {
			return false
		}
		parent, found, err := c.deps.Records.Workspace(ctx, *w.ParentSlug)
		switch {
		case err != nil:
			c.deps.Logger.LogAttrs(ctx, slog.LevelError, "git fetch event not written: the fork's parent cannot be read",
				slog.String("event", EventFetch),
				slog.String("request_id", server.RequestID(ctx)),
				slog.String("workspace", w.Slug),
				slog.String("error", err.Error()),
			)
			return false
		case !found:
			return false
		}
		w = parent
	}
	on, _ := c.deps.Settings.EffectiveSettings(w.Settings, w.DefaultBranch)[settingAuditFetches].(bool)
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
	if c.deps.Events == nil || !c.auditsFetches(context.WithoutCancel(r.Context()), x) {
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

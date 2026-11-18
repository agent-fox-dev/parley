package forge

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/scrub"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// The scopes a personal access token needs for the forge routes: reading
// the forge, and writing to it, which grants reading too.
const (
	ScopeRead  = "forge:read"
	ScopeWrite = "forge:write"
)

// AccessDecider decides a caller's access to a workspace, as the access
// rule does. The forge routes ask it about a task fork's parent, which the
// pipeline's access check, run on the fork, does not decide.
type AccessDecider interface {
	Decide(ctx context.Context, c access.Caller, slug string, need access.Need) (access.Decision, error)
}

// SettingsReader gives a workspace's settings with their defaults, as the
// workspace service's settings registry does.
type SettingsReader interface {
	EffectiveSettings(stored map[string]json.RawMessage, defaultBranch *string) map[string]any
}

// Deps are what the forge component reads: the workspace records, their
// settings and secrets, the access rule it asks about a fork's parent, the
// options of the client it sends forge requests with, the hub's scrubber,
// logger and clock, and the hub's external URL.
type Deps struct {
	// Workspaces reads the workspace record of a slug.
	Workspaces func(ctx context.Context, slug string) (workspace.Workspace, bool, error)
	Settings   SettingsReader
	Secrets    workspace.SecretReader
	Access     AccessDecider
	// Client are the options the client is built with when the component
	// is bound.
	Client      ClientOptions
	Scrubber    *scrub.Scrubber
	Logger      *slog.Logger
	Now         func() time.Time
	ExternalURL string
}

// Component is the hub's forge client and the shared checks of its routes.
// The hub creates its zero value when it is composed, builds the routes
// with it then, and binds it in a start hook, before the first request.
type Component struct {
	deps   Deps
	client *Client

	mu           sync.Mutex
	lastDelivery LastDeliveryProvider
}

// Bind gives the component its dependencies and builds its client from
// d.Client.
func (c *Component) Bind(d Deps) {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Client.Now == nil {
		d.Client.Now = d.Now
	}
	c.deps = d
	c.client = NewClient(d.Client)
}

// Routes are the forge component's routes, which the hub registers. They
// may be built before the component is bound.
func (c *Component) Routes() []server.Route {
	read := func(pattern string, params func(*http.Request, *Call) error, serve func(http.ResponseWriter, *http.Request, *Call) error) server.Route {
		return c.Route(Route{Method: http.MethodGet, Pattern: pattern, Scope: ScopeRead, Need: access.NeedRead, Params: params, Serve: serve})
	}
	return []server.Route{
		c.Route(Route{Method: http.MethodGet, Pattern: "/workspaces/:slug/forge", Scope: ScopeRead, Need: access.NeedRead,
			NoTarget: true, Serve: c.serveStatus}),
		read("/workspaces/:slug/forge/repository", nil, serveRepository),
		read("/workspaces/:slug/issues", readIssueList, serveIssues),
		read("/workspaces/:slug/issues/:number", readIssueNumber, c.serveIssueThread),
		read(commentsPattern, readComments, serveComments),
	}
}

// Client is the client the component sends forge requests with, nil before
// it is bound.
func (c *Component) Client() *Client { return c.client }

// settings reads the two forge settings of w. A stored value that is not a
// string is its JSON text, which the check at use refuses.
func (c *Component) settings(w workspace.Workspace) Settings {
	effective := c.deps.Settings.EffectiveSettings(w.Settings, w.DefaultBranch)
	read := func(name string) string {
		switch v := effective[name].(type) {
		case string:
			return v
		case json.RawMessage:
			return string(v)
		}
		return ""
	}
	return Settings{APIURL: read(SettingAPIURL), UpstreamAPIURL: read(SettingUpstreamAPIURL)}
}

// LastDeliveryProvider reports the time of the newest webhook delivery
// recorded for the workspace slug, and whether there is one.
type LastDeliveryProvider func(ctx context.Context, slug string) (time.Time, bool, error)

// SetLastDeliveryProvider registers p as the source of the diagnostic
// document's webhook.last_delivery_at, replacing the one registered before;
// nil registers none. It may be called at any time.
func (c *Component) SetLastDeliveryProvider(p LastDeliveryProvider) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastDelivery = p
}

// lastDeliveryProvider is the provider registered, nil for none.
func (c *Component) lastDeliveryProvider() LastDeliveryProvider {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastDelivery
}

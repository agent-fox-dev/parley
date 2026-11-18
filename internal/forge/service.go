package forge

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/scrub"
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

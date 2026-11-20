package sandbox

import (
	"cmp"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/agent-fox-dev/parley/internal/server"
)

// ProfilesRoute declares GET /sandbox-profiles: every profile of the
// configuration, in name order, on one page. It needs the scope
// sandboxes:read of a PAT and access to no workspace, takes no query
// parameter (it ignores any), and answers from the loaded configuration
// alone, so it keeps answering while the runtime is unreachable.
func (s *Service) ProfilesRoute() server.Route {
	return s.Route(server.Route{
		Method:  http.MethodGet,
		Pattern: "/sandbox-profiles",
		Scope:   scopeRead,
		Handler: s.listProfiles,
	})
}

// profileList is the answer of GET /sandbox-profiles: a list of one page,
// whose next_cursor is always null.
type profileList struct {
	Items      []profileItem `json:"items"`
	NextCursor *string       `json:"next_cursor"`
}

// profileItem is what a caller learns of a profile. It leaves out the
// policy file's path, the env values and the two expose lists: an agent
// picks a profile by what it offers, and those may name or hold secrets.
type profileItem struct {
	Name                string          `json:"name"`
	Default             bool            `json:"default"`
	Description         string          `json:"description"`
	Image               string          `json:"image"`
	CPU                 string          `json:"cpu"`
	Memory              string          `json:"memory"`
	GPU                 int             `json:"gpu"`
	TTL                 string          `json:"ttl"`
	TTLSeconds          int64           `json:"ttl_seconds"`
	IdleTimeout         string          `json:"idle_timeout"`
	PolicySummary       []string        `json:"policy_summary"`
	Credentials         []string        `json:"credentials"`
	OptionalCredentials []string        `json:"optional_credentials"`
	ExposedSecrets      []exposedSecret `json:"exposed_secrets"`
	HubAccess           string          `json:"hub_access"`
	EnvKeys             []string        `json:"env_keys"`
	Checkpoint          bool            `json:"checkpoint"`
	AllowProposals      bool            `json:"allow_proposals"`
	// Tools reports whether the profile names a tool server; its path is
	// not shown.
	Tools bool `json:"tools"`
}

// exposedSecret is the name of a secret a profile exposes, and whether a
// sandbox starts without it.
type exposedSecret struct {
	Name     string `json:"name"`
	Optional bool   `json:"optional"`
}

func (s *Service) listProfiles(w http.ResponseWriter, _ *http.Request) error {
	profiles := s.settings.Profiles()
	items := make([]profileItem, 0, len(profiles))
	for _, p := range profiles {
		items = append(items, newProfileItem(p))
	}
	server.WriteJSON(w, http.StatusOK, profileList{Items: items})
	return nil
}

func newProfileItem(p Profile) profileItem {
	secrets := make([]exposedSecret, 0, len(p.ExposeSecrets)+len(p.OptionalExposeSecrets))
	for _, n := range p.ExposeSecrets {
		secrets = append(secrets, exposedSecret{Name: n})
	}
	for _, n := range p.OptionalExposeSecrets {
		secrets = append(secrets, exposedSecret{Name: n, Optional: true})
	}
	slices.SortFunc(secrets, func(a, b exposedSecret) int { return cmp.Compare(a.Name, b.Name) })
	hosts := slices.Clone(p.PolicyHosts)
	slices.Sort(hosts)
	return profileItem{
		Name:                p.Name,
		Default:             p.Default,
		Description:         p.Description,
		Image:               p.Image,
		CPU:                 p.CPU,
		Memory:              p.Memory,
		GPU:                 p.GPU,
		TTL:                 formatDuration(p.TTL),
		TTLSeconds:          int64(p.TTL / time.Second),
		IdleTimeout:         formatDuration(p.IdleTimeout),
		PolicySummary:       orEmpty(hosts),
		Credentials:         orEmpty(slices.Clone(p.Credentials)),
		OptionalCredentials: orEmpty(slices.Clone(p.OptionalCredentials)),
		ExposedSecrets:      secrets,
		HubAccess:           p.HubAccess,
		EnvKeys:             orEmpty(slices.Sorted(maps.Keys(p.Env))),
		Checkpoint:          p.Checkpoint,
		AllowProposals:      p.AllowProposals,
		Tools:               p.ToolServer != "",
	}
}

// orEmpty is l, or an empty list for nil, so that a list member is written
// [] and never null.
func orEmpty(l []string) []string {
	if l == nil {
		return []string{}
	}
	return l
}

// durationUnits are the units of the duration grammar (timefmt) above the
// second, largest first.
var durationUnits = []struct {
	suffix string
	length time.Duration
}{
	{"d", 24 * time.Hour},
	{"h", time.Hour},
	{"m", time.Minute},
}

// formatDuration writes d in the duration grammar the configuration is read
// with, in the largest unit that divides it: 4h, 30m, 1d; zero is 0s. The
// configuration holds whole seconds only; a fraction of a second, which it
// cannot hold, is cut off.
func formatDuration(d time.Duration) string {
	d = d.Truncate(time.Second)
	if d <= 0 {
		return "0s"
	}
	for _, u := range durationUnits {
		if d%u.length == 0 {
			return strconv.FormatInt(int64(d/u.length), 10) + u.suffix
		}
	}
	return strconv.FormatInt(int64(d/time.Second), 10) + "s"
}

// GetRoute declares GET /workspaces/:slug/sandboxes/:id: the sandbox id of
// the workspace, from its stored record, in every state, deleted included.
// With wait_for and wait it holds the request until the sandbox is in the
// state asked for or the wait has passed (see getSandbox). It needs the
// scope sandboxes:read of a PAT and read access to the workspace, and reads
// the store only, so it keeps answering while the runtime is unreachable.
func (s *Service) GetRoute() server.Route {
	return s.WorkspaceRoute(KindRead, server.Route{
		Method:  http.MethodGet,
		Pattern: "/workspaces/:slug/sandboxes/:id",
		Handler: s.getSandbox,
	})
}

// CheckpointRoute declares POST /workspaces/:slug/sandboxes/:id/checkpoint:
// it saves the working directory of the ready sandbox id as a checkpoint
// for the caller and answers with the sandbox (see serveCheckpoint). It
// needs the scope sandboxes:write of a PAT and write access to the
// workspace; the admin token acts as an owner.
func (s *Service) CheckpointRoute() server.Route {
	return s.WorkspaceRoute(KindWrite, server.Route{
		Method:  http.MethodPost,
		Pattern: "/workspaces/:slug/sandboxes/:id/checkpoint",
		Handler: s.serveCheckpoint,
	})
}

// StopRoute declares POST /workspaces/:slug/sandboxes/:id/stop: it stops
// the sandbox id for the caller, keeping its working directory, and
// answers with the sandbox in every state (see serveStop). It needs the
// scope sandboxes:write of a PAT and write access to the workspace; the
// admin token acts as an owner.
func (s *Service) StopRoute() server.Route {
	return s.WorkspaceRoute(KindWrite, server.Route{
		Method:  http.MethodPost,
		Pattern: "/workspaces/:slug/sandboxes/:id/stop",
		Handler: s.serveStop,
	})
}

// StartRoute declares POST /workspaces/:slug/sandboxes/:id/start: it
// starts the stopped, completed or failed sandbox id again for the caller
// and answers with the sandbox (see serveStart). It needs the scope
// sandboxes:write of a PAT and write access to the workspace; the admin
// token acts as an owner.
func (s *Service) StartRoute() server.Route {
	return s.WorkspaceRoute(KindWrite, server.Route{
		Method:  http.MethodPost,
		Pattern: "/workspaces/:slug/sandboxes/:id/start",
		Handler: s.serveStart,
	})
}

// DeleteRoute declares DELETE /workspaces/:slug/sandboxes/:id: it deletes
// the sandbox id for the caller, with a checkpoint first as the query
// checkpoint or the profile asks, and answers 204, also for a sandbox
// already deleted (see serveDelete). It needs the scope sandboxes:write of a
// PAT and write access to the workspace; the admin token acts as an owner.
func (s *Service) DeleteRoute() server.Route {
	return s.WorkspaceRoute(KindWrite, server.Route{
		Method:  http.MethodDelete,
		Pattern: "/workspaces/:slug/sandboxes/:id",
		Handler: s.serveDelete,
	})
}

// ExtendRoute declares PATCH /workspaces/:slug/sandboxes/:id: it sets the
// ttl, purpose and labels the body gives on the sandbox id, which is not
// deleted, and answers with the sandbox (see serveExtend). It needs the
// scope sandboxes:write of a PAT and write access to the workspace; the
// admin token acts as an owner.
func (s *Service) ExtendRoute() server.Route {
	return s.WorkspaceRoute(KindWrite, server.Route{
		Method:  http.MethodPatch,
		Pattern: "/workspaces/:slug/sandboxes/:id",
		Handler: s.serveExtend,
	})
}

// ListRoute declares GET /workspaces/:slug/sandboxes: the stored sandboxes
// of the workspace, newest first, paged, filtered by the query (see
// listWorkspaceSandboxes). It needs the scope sandboxes:read of a PAT and
// read access to the workspace, and reads the store only.
func (s *Service) ListRoute() server.Route {
	return s.WorkspaceRoute(KindRead, server.Route{
		Method:  http.MethodGet,
		Pattern: "/workspaces/:slug/sandboxes",
		Handler: s.listWorkspaceSandboxes,
	})
}

// states are the values the state filter of GET /sandboxes takes, written
// exactly so.
var states = []string{"provisioning", "ready", "stopped", "completed", "failed", "deleted"}

// msgAdminOnly refuses a caller of GET /sandboxes other than the admin
// token.
const msgAdminOnly = "only the admin token may list every sandbox of the hub"

// SandboxesRoute declares GET /sandboxes: the sandboxes of every workspace
// that the registered Source holds, newest first, paged, for the admin
// token alone. It takes no scope: every other credential, an admin user's
// API key included, is refused 403 forbidden before anything is read. It
// reads the source only, never the runtime, so it keeps answering while
// the runtime is unreachable; without a source the list is empty.
func (s *Service) SandboxesRoute() server.Route {
	return s.Route(server.Route{
		Method:  http.MethodGet,
		Pattern: "/sandboxes",
		Handler: s.listSandboxes,
	})
}

func (s *Service) listSandboxes(w http.ResponseWriter, r *http.Request) error {
	// The admin token alone, not identity's admin rule, which also admits
	// the API keys of admin users.
	if c, ok := server.CallerOf(r.Context()); !ok || !c.Admin {
		return server.Forbidden(msgAdminOnly)
	}
	page, err := server.ReadPage(r)
	if err != nil {
		return err
	}
	query := r.URL.Query()
	filter := Filter{State: query.Get("state"), User: query.Get("user")}
	if query.Has("state") && !slices.Contains(states, filter.State) {
		return server.InvalidRequest("state must be one of " + strings.Join(states, ", "))
	}
	var after *Position
	var pos Position
	if ok, err := page.After(&pos); err != nil {
		return err
	} else if ok {
		after = &pos
	}
	var items []Item
	if s.source != nil {
		if items, err = s.source.List(r.Context(), filter, after, page.Limit+1); err != nil {
			// %v and not %w: the list is stored state, so a failure of the
			// source is an internal error even when it carries a runtime
			// error's kind.
			return fmt.Errorf("list sandboxes: %v", err)
		}
	}
	return server.WritePage(w, page, items, func(i Item) any { return Position{CreatedAt: i.CreatedAt, ID: i.ID} })
}

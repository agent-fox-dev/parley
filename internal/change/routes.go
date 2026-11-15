package change

import (
	"database/sql"
	"net/http"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
)

// Routes are the change routes, for the server's router: the open, the
// list and the reads of one change, its versions, the change of a commit
// and the diff of a version.
func (c *Component) Routes() []server.Route {
	need := map[string]string{access.MarkNeed: string(access.NeedRead)}
	read := func(pattern string, h server.HandlerFunc) server.Route {
		return server.Route{Method: http.MethodGet, Pattern: pattern, Scope: "changes:read", Marks: need, Handler: h}
	}
	// The patterns are those of the contributor table of internal/access,
	// parameter names included, so that a token bound to a fork reaches
	// them on the fork's parent. The router prefers the literal by-commit
	// to :number.
	return []server.Route{
		{
			Method: http.MethodPost, Pattern: "/workspaces/:slug/changes", Scope: "changes:write", Create: true,
			Marks: need, Handler: c.openRoute,
		},
		read("/workspaces/:slug/changes", c.listChanges),
		read("/workspaces/:slug/changes/:number", c.getChange),
		read("/workspaces/:slug/changes/by-commit/:sha", c.byCommit),
		read("/workspaces/:slug/changes/:number/versions", c.listVersions),
		read("/workspaces/:slug/changes/:number/versions/:version", c.getVersion),
		read("/workspaces/:slug/changes/:number/diff", c.getDiff),
	}
}

// openBody is the body of POST /workspaces/:slug/changes.
type openBody struct {
	Source struct {
		WorkspaceSlug *string `json:"workspace_slug"`
		Branch        string  `json:"branch"`
	} `json:"source"`
	TargetBranch *string  `json:"target_branch"`
	Title        *string  `json:"title"`
	Intent       *string  `json:"intent"`
	Draft        *bool    `json:"draft"`
	DependsOn    *int64   `json:"depends_on"`
	AutoLand     *bool    `json:"auto_land"`
	Labels       []string `json:"labels"`
	SessionID    *string  `json:"session_id"`
}

// openRoute opens a change in the workspace :slug from the request's body,
// in a transaction of its own, with version 1 of kind ref: the head is on
// the source branch already. The shared checks have answered for the
// token's binding to :slug, the scope and read access to :slug; a member of
// the wrong type is 400 invalid_request; Open checks the rest, the paths of
// a path-scoped token among them. It answers 201 with the change object.
func (c *Component) openRoute(w http.ResponseWriter, r *http.Request) error {
	var body openBody
	if err := server.DecodeJSON(r, &body); err != nil {
		return err
	}
	ctx := r.Context()
	p, _ := identity.PrincipalOf(ctx)
	in := OpenInput{
		Workspace: server.Param(r, "slug"), SourceBranch: body.Source.Branch,
		TargetBranch: body.TargetBranch, Title: body.Title, Intent: body.Intent, Draft: body.Draft != nil && *body.Draft,
		DependsOn: body.DependsOn, AutoLand: body.AutoLand, Labels: body.Labels, SessionID: body.SessionID,
		Caller: CallerOf(p), Kind: KindRef, CheckPaths: true,
	}
	if body.Source.WorkspaceSlug != nil {
		in.SourceWorkspace = *body.Source.WorkspaceSlug
	}
	var ch Change
	err := c.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		ch, err = c.Open(ctx, tx, in)
		return err
	})
	if err != nil {
		return err
	}
	server.WriteJSON(w, http.StatusCreated, ch)
	return nil
}

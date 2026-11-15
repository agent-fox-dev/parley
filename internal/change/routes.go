package change

import (
	"database/sql"
	"net/http"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
)

// Routes are the change routes, for the server's router.
func (c *Component) Routes() []server.Route {
	return []server.Route{{
		Method: http.MethodPost, Pattern: "/workspaces/:slug/changes", Scope: "changes:write", Create: true,
		Marks: map[string]string{access.MarkNeed: string(access.NeedRead)}, Handler: c.openRoute,
	}}
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

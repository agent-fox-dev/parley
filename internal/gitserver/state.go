package gitserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/fork"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// Parents judges the parent of a task fork: *fork.Component is one. It
// returns nil for a fork whose parent can take the request's use, and
// otherwise a *server.Error: of type parent_archived for a write to a fork
// whose parent is archived, of status 409 for a parent that is busy or not
// ready. Any other error is the store's.
type Parents interface {
	CheckParentState(ctx context.Context, slug string, use fork.Use) error
}

// Repositories opens the bare repository of a workspace and returns its git
// directory, or an error when the place the hub keeps it at does not open as
// a repository.
type Repositories interface {
	Open(ctx context.Context, slug string) (dir string, err error)
}

// errParentArchived is the type of the refusal Parents gives a write to a
// fork whose parent is archived.
const errParentArchived = "parent_archived"

// The causes logged when a request reaches a check on a component bound
// without the collaborator the check asks.
var (
	errNoParents      = errors.New("no parent check is installed")
	errNoRepositories = errors.New("no repository opener is installed")
)

// DiskRepositories returns the Repositories that open the repository of the
// workspace slug at path(slug) with g.
func DiskRepositories(g gitrepo.Git, path func(slug string) string) Repositories {
	return diskRepositories{g, path}
}

type diskRepositories struct {
	g    gitrepo.Git
	path func(string) string
}

func (d diskRepositories) Open(ctx context.Context, slug string) (string, error) {
	r, err := d.g.Open(ctx, d.path(slug))
	if err != nil {
		return "", err
	}
	return r.Dir(), nil
}

// state refuses a request for a workspace that cannot serve it, after
// validation, so that only a caller who may make the request learns the
// workspace's state. The workspace's own state comes first: a status other
// than ready is named in the answer, and a maintenance operation running on
// a ready workspace is answered as busy, because it may be replacing the
// repository the exchange would read. A ready task fork is then judged by
// its parent: a parent that is busy or not ready refuses every request,
// since a fork's refs are kept at its parent's; an archived parent refuses
// only a push, since a fork of an archived workspace stays readable but
// takes no more work.
func (c *Component) state(w http.ResponseWriter, r *http.Request, x *Exchange) bool {
	ws := x.Workspace
	if err := workspace.RequireReady(ws); err != nil {
		status := string(ws.Status)
		if ws.Marker != workspace.MarkerNone {
			status = "busy"
		}
		refuse(w, http.StatusConflict, msgNotAvailable+status)
		return false
	}
	if ws.Kind != access.KindFork {
		return true
	}
	if c.deps.Parents == nil {
		c.fail(w, r, errNoParents)
		return false
	}
	use := fork.UseRead
	if x.Need == access.NeedWrite {
		use = fork.UseWrite
	}
	err := c.deps.Parents.CheckParentState(r.Context(), ws.Slug, use)
	var se *server.Error
	switch {
	case err == nil:
		return true
	case errors.As(err, &se) && se.Type() == errParentArchived:
		refuse(w, http.StatusConflict, msgParentArchived)
	case errors.As(err, &se) && se.Status() == http.StatusConflict:
		refuse(w, http.StatusConflict, msgParentBusy)
	case errors.As(err, &se) && se.Status() == http.StatusNotFound:
		// Removed since it was resolved: as for any slug with no workspace.
		refuse(w, http.StatusNotFound, msgNotFound)
	default:
		c.fail(w, r, fmt.Errorf("check the parent of %q: %w", ws.Slug, err))
	}
	return false
}

// open opens the workspace's repository before the service runs, so that a
// repository missing from disk is answered 500 internal error, its cause
// logged, rather than by a git process that fails halfway through the
// exchange. It keeps the git directory for the service.
func (c *Component) open(w http.ResponseWriter, r *http.Request, x *Exchange) bool {
	if c.deps.Repositories == nil {
		c.fail(w, r, errNoRepositories)
		return false
	}
	dir, err := c.deps.Repositories.Open(r.Context(), x.Workspace.Slug)
	if err != nil {
		c.fail(w, r, fmt.Errorf("open the repository of %q: %w", x.Workspace.Slug, err))
		return false
	}
	x.Repo = dir
	return true
}

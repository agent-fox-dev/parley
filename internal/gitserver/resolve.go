package gitserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// Access is the workspace access rule: *access.Service is one. It reads
// the caller's memberships and the workspace's record from the store on
// every call, so that no decision outlives the request it was made for.
type Access interface {
	Decide(ctx context.Context, c access.Caller, slug string, need access.Need) (access.Decision, error)
}

// Records reads what the resolution of a repository reads besides the
// access rule: an organization by its id, identity.ErrNotFound when there is
// none, and the workspace record by its slug.
type Records interface {
	Org(ctx context.Context, id string) (identity.Org, error)
	Workspace(ctx context.Context, slug string) (workspace.Workspace, bool, error)
}

// errNoAccessRule is the cause logged when a request reaches the resolution
// of its repository on a component bound without the access rule or the
// records.
var errNoAccessRule = errors.New("no workspace access rule is installed")

// StoreRecords returns the Records that read the store's database db.
func StoreRecords(db *sql.DB) Records { return storeRecords{db} }

type storeRecords struct{ db *sql.DB }

func (s storeRecords) Org(ctx context.Context, id string) (identity.Org, error) {
	return identity.OrgByID(ctx, s.db, id)
}

func (s storeRecords) Workspace(ctx context.Context, slug string) (workspace.Workspace, bool, error) {
	return workspace.BySlug(ctx, s.db, slug)
}

// resolve finds the workspace the request names and decides the caller's
// access to it with the need of the route. A segment without .git, a slug
// with no workspace, a workspace the caller cannot read, one whose
// organization does not exist and an org segment other than the slug of
// the workspace's organization, compared exactly, are all answered with
// the same 404, so that a caller learns nothing about what exists. Only a
// member who may read the repository at this URL is told that a push needs
// a task fork. The workspace's status is not looked at: the answer is the
// same in every status.
func (c *Component) resolve(w http.ResponseWriter, r *http.Request, x *Exchange) bool {
	if c.deps.Access == nil || c.deps.Records == nil {
		c.fail(w, r, errNoAccessRule)
		return false
	}
	if !strings.HasSuffix(r.PathValue("repo"), ".git") {
		refuse(w, http.StatusNotFound, msgNotFound)
		return false
	}
	slug := slugOf(r)
	d, err := c.deps.Access.Decide(r.Context(), access.CallerFrom(x.Caller), slug, x.Need)
	if err != nil {
		c.fail(w, r, fmt.Errorf("decide the access to %q: %w", slug, err))
		return false
	}
	if d.Outcome == access.NotFound {
		refuse(w, http.StatusNotFound, msgNotFound)
		return false
	}
	ok, err := c.orgMatches(r.Context(), d.Record.OrgID, r.PathValue("org"))
	if err != nil {
		c.fail(w, r, err)
		return false
	}
	if !ok {
		refuse(w, http.StatusNotFound, msgNotFound)
		return false
	}
	if d.Outcome == access.ReadOnly {
		refuse(w, http.StatusForbidden, msgReadOnly)
		return false
	}
	rec, found, err := c.deps.Records.Workspace(r.Context(), slug)
	if err != nil {
		c.fail(w, r, fmt.Errorf("read the workspace %q: %w", slug, err))
		return false
	}
	if !found {
		// Removed since the decision: as for any slug with no workspace.
		refuse(w, http.StatusNotFound, msgNotFound)
		return false
	}
	x.Level, x.Workspace = d.Level, rec
	return true
}

// orgMatches reports whether segment is the slug of the organization orgID,
// letter case included. A workspace whose organization no longer exists
// has no URL.
func (c *Component) orgMatches(ctx context.Context, orgID, segment string) (bool, error) {
	if orgID == "" {
		return false, nil
	}
	org, err := c.deps.Records.Org(ctx, orgID)
	if errors.Is(err, identity.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read the organization %q: %w", orgID, err)
	}
	return org.Slug == segment, nil
}

// validate refuses a ref discovery whose service, the first value of the
// query parameter, is neither of the two git services; later values are
// ignored, as git ignores them. It runs after access, so that a caller who
// cannot see the repository learns nothing from a malformed request.
func (c *Component) validate(w http.ResponseWriter, _ *http.Request, x *Exchange) bool {
	if x.Route != RouteInfoRefs || x.Service == ServiceUploadPack || x.Service == ServiceReceivePack {
		return true
	}
	refuse(w, http.StatusBadRequest, msgInvalidService)
	return false
}

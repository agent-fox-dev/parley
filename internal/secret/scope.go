package secret

import (
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
)

// MarkScope is the key of server.Route.Marks with which a secret route
// declares the scope it acts on: "user", "org" or "workspace". The scope
// check resolves the scope of a marked route for its caller.
const MarkScope = "secret.scope"

// The messages of the scope refusals. The org's not-found answer is built
// from a constant alone, so its bytes are the same for an org that does not
// exist, one the caller is not a member of and a blocked one.
const (
	msgNeedsUser   = "user-scoped endpoints require a user credential"
	msgOrgNotFound = "org not found"
)

// errNoCaller is a request at the scope check whose caller is neither the
// admin token nor a user: a wiring fault, which grants nothing.
var errNoCaller = errors.New("no authenticated caller at the secret scope check")

// target is the one set of secrets a request acts on: a scope and the id of
// its owner.
type target struct {
	scope Scope
	id    string
}

type targetKey struct{}

// ScopeCheck is the secret scope check, an access check of the pipeline,
// which the hub adds after the workspace access check. On a route marked
// MarkScope it resolves the scope for the caller with the resolver of that
// scope and attaches it, where the route's handler reads it; a refusal is
// answered as the resolver says. Every secret route goes through it, so the
// routes of one scope give the same answers. It runs before the idempotency
// stage, so a replayed create is authorised too (ADR 04). Routes without the
// mark it lets through.
func (c *Component) ScopeCheck(r *http.Request) error {
	m, ok := server.MatchedRoute(r)
	if !ok {
		return nil
	}
	mark, ok := m.Route.Marks[MarkScope]
	if !ok {
		return nil
	}
	var t target
	var err error
	switch Scope(mark) {
	case ScopeUser:
		t, err = resolveUser(r)
	case ScopeOrg:
		t, err = c.resolveOrg(r)
	case ScopeWorkspace:
		t, err = resolveWorkspace(r)
	default:
		return fmt.Errorf("route %s %s declares the unknown secret scope %q", m.Method, m.Pattern, mark)
	}
	if err != nil {
		return err
	}
	server.Attach(r, targetKey{}, t)
	return nil
}

// targetOf returns the scope the scope check attached to r.
func targetOf(r *http.Request) (target, error) {
	v, _ := server.Attached(r, targetKey{})
	t, ok := v.(target)
	if !ok {
		return target{}, errors.New("a secret route was reached without a resolved scope")
	}
	return t, nil
}

// resolveUser gives a user the scope of their own secrets. The admin token
// has no user and is refused 403 forbidden.
func resolveUser(r *http.Request) (target, error) {
	sc, ok := server.CallerOf(r.Context())
	switch {
	case ok && sc.Admin:
		return target{}, server.Forbidden(msgNeedsUser)
	case !ok || sc.UserID == "":
		return target{}, errNoCaller
	}
	return target{scope: ScopeUser, id: sc.UserID}, nil
}

// resolveOrg gives the scope of the org the path's slug names, by its slug
// or its id, to the admin token for any org, a blocked one included, and to
// a user who is a member of it while it is active. Every other caller, and
// every caller for an org that does not exist, gets one 404 not_found, so
// that nobody learns of an org they cannot use.
func (c *Component) resolveOrg(r *http.Request) (target, error) {
	sc, ok := server.CallerOf(r.Context())
	if !ok || !sc.Admin && sc.UserID == "" {
		return target{}, errNoCaller
	}
	if c.deps.Store == nil {
		return target{}, errNotBound
	}
	ctx, db := r.Context(), c.deps.Store.Main()
	o, err := identity.OrgBySelector(ctx, db, server.Param(r, "slug"))
	if errors.Is(err, identity.ErrNotFound) {
		return target{}, server.NotFound(msgOrgNotFound)
	}
	if err != nil {
		return target{}, fmt.Errorf("resolve the org of a secret route: %w", err)
	}
	if !sc.Admin {
		// The active orgs of the user: a blocked org's membership grants
		// nothing.
		orgs, err := identity.ActiveOrgIDs(ctx, db, sc.UserID)
		if err != nil {
			return target{}, fmt.Errorf("resolve the org of a secret route: %w", err)
		}
		if !slices.Contains(orgs, o.ID) {
			return target{}, server.NotFound(msgOrgNotFound)
		}
	}
	return target{scope: ScopeOrg, id: o.ID}, nil
}

// resolveWorkspace gives the scope of the workspace the path's slug names.
// The workspace access check, which runs before the scope check on a route
// that needs owner, has decided already: it answers a member 403 and
// everyone else without access 404 workspace not found. The workspace's
// status plays no part.
func resolveWorkspace(r *http.Request) (target, error) {
	if _, ok := access.DecisionOf(r); !ok {
		return target{}, errors.New("a workspace secret route was reached without a workspace access decision")
	}
	return target{scope: ScopeWorkspace, id: server.Param(r, "slug")}, nil
}

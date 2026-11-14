package access

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
)

// MarkSubsetRule is the key of server.Route.Marks with which a route whose
// body is a token request (the members of POST /user/tokens) asks for the
// subset rule to be applied by SubsetCheck, before the workspace is looked
// up. Its value is not read.
const MarkSubsetRule = "access.subset_rule"

// The messages of the bound-token routes' own refusals. msgNeedsUser is the
// one message the admin token gets wherever a user is needed.
const (
	msgNeedsUser = "this route needs a user"
	msgArchived  = "workspace is archived"
)

// errNoStore is a bound-token route served by a service bound without a
// store: a fault of the wiring.
var errNoStore = errors.New("the access service has no store to mint in")

// SubsetCheck is an access check that holds a PAT caller to the subset rule
// (identity.CheckSubset) on a route marked MarkSubsetRule: a PAT cannot ask
// for a permission it does not hold exactly, or for paths its own paths do
// not cover, and is answered 403 insufficient_scope naming the first one.
// It is installed before the workspace access check, so the refusal is the
// same for an existing workspace and an unknown slug. A body that does not
// decode, or whose paths are not valid, passes: the route answers it 400
// after the access check. Every other caller and route passes.
func (s *Service) SubsetCheck(r *http.Request) error {
	m, ok := server.MatchedRoute(r)
	if !ok {
		return nil
	}
	if _, marked := m.Route.Marks[MarkSubsetRule]; !marked {
		return nil
	}
	caller, ok := identity.PrincipalOf(r.Context())
	if !ok || caller.Type != identity.TypePAT {
		return nil
	}
	req, err := identity.DecodeMintRequest(r)
	if err != nil {
		return nil
	}
	paths, err := identity.ValidatePaths(req.Paths)
	if err != nil {
		return nil
	}
	return identity.CheckSubset(caller, req.Permissions, paths)
}

// TokenRoutes are the routes of the tokens bound to a workspace:
// POST /workspaces/:slug/tokens, which mints one. It needs the scope
// tokens:manage and the level owner on the workspace, is a create route
// whose token member a replay leaves out, and holds a PAT caller to the
// subset rule.
func (s *Service) TokenRoutes() []server.Route {
	return []server.Route{
		{Method: http.MethodPost, Pattern: workspaceRoute + "/tokens", Create: true, ShowOnce: []string{"token"},
			Scope: "tokens:manage", Marks: map[string]string{MarkNeed: string(NeedOwner), MarkSubsetRule: ""},
			Handler: s.createBoundToken},
	}
}

// createBoundToken answers POST /workspaces/:slug/tokens, after the
// pipeline's checks have granted the caller owner on the workspace: it
// mints a PAT of the caller's user bound to the workspace from the body of
// POST /user/tokens, and answers 201 with the Token object and the token's
// value. The admin token is answered 403 before the body is read, since a
// bound token needs a user as its owner; it is answered here and not by
// the authenticator, so that the access check's 404 comes first. Then the
// body is validated (400), and only a valid one on an archived workspace is
// answered 409. Token and event are written in one transaction, so a
// failure of either, or of the commit, leaves neither and is answered 500.
func (s *Service) createBoundToken(w http.ResponseWriter, r *http.Request) error {
	caller, ok := identity.PrincipalOf(r.Context())
	if !ok {
		return errors.New("a token route was reached without a caller")
	}
	if caller.User == nil {
		return server.Forbidden(msgNeedsUser)
	}
	d, ok := DecisionOf(r)
	if !ok {
		return errors.New("a token route was reached without an access decision")
	}
	req, err := identity.DecodeMintRequest(r)
	if err != nil {
		return err
	}
	if s.deps.Store == nil {
		return errNoStore
	}
	in := MintInput{Creator: caller, Workspace: d.Record.Slug, Name: req.Name, Permissions: req.Permissions,
		Paths: req.Paths, Expires: req.Expires}
	archived := func() error {
		if d.Record.Status == StatusArchived {
			return server.WorkspaceArchived(msgArchived)
		}
		return nil
	}
	var created createdToken
	err = s.deps.Store.Tx(r.Context(), func(tx *sql.Tx) error {
		var err error
		created.PAT, created.value, err = s.mintBound(r.Context(), tx, in, archived)
		return err
	})
	if err != nil {
		return err
	}
	server.WriteJSON(w, http.StatusCreated, created)
	return nil
}

// createdToken is the answer to the request that minted a token: the Token
// object and, as its member token, the token's value, which no other
// answer carries.
type createdToken struct {
	identity.PAT
	value string
}

func (c createdToken) MarshalJSON() ([]byte, error) {
	obj, err := json.Marshal(c.PAT)
	if err != nil {
		return nil, err
	}
	value, err := json.Marshal(c.value)
	if err != nil {
		return nil, err
	}
	// The Token object is a JSON object; token is appended as its last
	// member, so the object keeps the order of POST /user/tokens' answer.
	out := append(obj[:len(obj)-1:len(obj)-1], `,"token":`...)
	return append(append(out, value...), '}'), nil
}

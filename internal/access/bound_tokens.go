package access

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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

// TokenRoutes are the routes of the tokens bound to a workspace, both
// needing the level owner on the workspace and closed to every bound token
// (MarkRefusesBound), so that a bound token can neither mint another nor see
// its siblings: POST /workspaces/:slug/tokens, which mints one, needs the
// scope tokens:manage, is a create route whose token member a replay leaves
// out, and holds a PAT caller to the subset rule; GET
// /workspaces/:slug/tokens, which lists them, needs the scope tokens:read.
func (s *Service) TokenRoutes() []server.Route {
	return []server.Route{
		{Method: http.MethodPost, Pattern: workspaceRoute + "/tokens", Create: true, ShowOnce: []string{"token"},
			Scope: "tokens:manage", Marks: map[string]string{MarkNeed: string(NeedOwner), MarkSubsetRule: "", MarkRefusesBound: ""},
			Handler: s.createBoundToken},
		{Method: http.MethodGet, Pattern: workspaceRoute + "/tokens", Scope: "tokens:read",
			Marks:   map[string]string{MarkNeed: string(NeedOwner), MarkRefusesBound: ""},
			Handler: s.listBoundTokens},
	}
}

// listBoundTokens answers GET /workspaces/:slug/tokens, after the
// pipeline's checks have granted the caller owner on the workspace (the
// admin token included): a page of every PAT bound to the workspace, live,
// revoked or expired, as Token objects, which never carry a value. An
// archived workspace is listed like any other, so its owner can still audit
// what reaches it.
func (s *Service) listBoundTokens(w http.ResponseWriter, r *http.Request) error {
	page, err := server.ReadPage(r)
	if err != nil {
		return err
	}
	var pos boundPosition
	hasAfter, err := page.After(&pos)
	if err != nil {
		return err
	}
	var after *boundPosition
	if hasAfter {
		after = &pos
	}
	d, ok := DecisionOf(r)
	if !ok {
		return errors.New("a token route was reached without an access decision")
	}
	if s.deps.Store == nil {
		return errNoStore
	}
	items, err := boundTokens(r.Context(), s.deps.Store.Main(), d.Record.Slug, after, page.Limit+1)
	if err != nil {
		return err
	}
	return server.WritePage(w, page, items, positionOf)
}

// boundPosition is the position of a token in the list of the tokens bound
// to a workspace: oldest first, created_at ascending, then token_id
// ascending, since created_at has whole seconds and tokens minted in the
// same second must keep one order across pages.
type boundPosition struct {
	CreatedAt int64  `json:"c"`
	TokenID   string `json:"id"`
}

func positionOf(p identity.PAT) any {
	return boundPosition{CreatedAt: p.CreatedAt.UnixNano(), TokenID: p.TokenID}
}

// boundTokens returns up to limit PATs bound to the workspace slug, in any
// state, in the order of boundPosition and after the position after when it
// is not nil. It reads the ids of the page and then each PAT with
// identity.PATByID, so that a PAT is read by the one function that knows
// its record.
func boundTokens(ctx context.Context, db *sql.DB, slug string, after *boundPosition, limit int) ([]identity.PAT, error) {
	query := `SELECT token_id FROM pats WHERE workspace = ?`
	args := []any{slug}
	if after != nil {
		query += ` AND (created_at > ? OR created_at = ? AND token_id > ?)`
		args = append(args, after.CreatedAt, after.CreatedAt, after.TokenID)
	}
	query += ` ORDER BY created_at, token_id LIMIT ?`
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("list the tokens bound to %s: %w", slug, err)
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, query, append(args, limit)...)
	if err != nil {
		return nil, fmt.Errorf("list the tokens bound to %s: %w", slug, err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("list the tokens bound to %s: %w", slug, err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the tokens bound to %s: %w", slug, err)
	}
	pats := make([]identity.PAT, 0, len(ids))
	for _, id := range ids {
		p, err := identity.PATByID(ctx, tx, id)
		if err != nil {
			return nil, fmt.Errorf("list the tokens bound to %s: %w", slug, err)
		}
		pats = append(pats, p)
	}
	return pats, nil
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

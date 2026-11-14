package secret

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/server/timefmt"
)

// The PAT scopes of the secret routes: reading a list needs secrets:read,
// which secrets:write implies; a change needs secrets:write.
const (
	scopeRead  = "secrets:read"
	scopeWrite = "secrets:write"
)

// The event types of the secret routes. An event names the secret by its
// stored key and never carries its value.
const (
	eventCreate = "parley.secret.create"
	eventUpdate = "parley.secret.update"
	eventDelete = "parley.secret.delete"
)

// errLimit is how the routes answer ErrLimitReached.
var errLimit = server.NewError("secret_limit_reached", http.StatusTooManyRequests, ErrLimitReached.Error())

// collections are the collection path of each secret scope and the marks
// its routes carry: the scope the scope check resolves, and on a workspace
// the need owner, so that only its owner and the admin token reach its
// secrets.
var collections = []struct {
	pattern string
	marks   map[string]string
}{
	{"/user/secrets", map[string]string{MarkScope: string(ScopeUser)}},
	{"/orgs/:slug/secrets", map[string]string{MarkScope: string(ScopeOrg)}},
	{"/workspaces/:slug/secrets", map[string]string{MarkScope: string(ScopeWorkspace), access.MarkNeed: string(access.NeedOwner)}},
}

// Routes returns the secret routes of the REST API: a list and a create on
// each collection, an update and a delete on each of its secrets. Each
// carries its scope mark, so that ScopeCheck resolves its scope before the
// handler runs, and the PAT scope it needs. A create is a create route, so
// that a retried request with its Idempotency-Key creates nothing twice.
func (c *Component) Routes() []server.Route {
	var routes []server.Route
	for _, col := range collections {
		one := col.pattern + "/:key"
		routes = append(routes,
			server.Route{Method: http.MethodGet, Pattern: col.pattern, Scope: scopeRead, Marks: col.marks, Handler: c.list},
			server.Route{Method: http.MethodPost, Pattern: col.pattern, Scope: scopeWrite, Marks: col.marks, Handler: c.create, Create: true},
			server.Route{Method: http.MethodPatch, Pattern: one, Scope: scopeWrite, Marks: col.marks, Handler: c.update},
			server.Route{Method: http.MethodDelete, Pattern: one, Scope: scopeWrite, Marks: col.marks, Handler: c.remove},
		)
	}
	return routes
}

// item is a secret as a list answers it: its key and its times, never its
// value nor its owner. position, which is not answered, is the stored
// item's Position, from which a list's cursor continues.
type item struct {
	Key       string       `json:"key"`
	CreatedAt timefmt.Time `json:"created_at"`
	UpdatedAt timefmt.Time `json:"updated_at"`
	position  string
}

func itemOf(it Item) item {
	return item{Key: it.Key, CreatedAt: timefmt.Time(it.CreatedAt), UpdatedAt: timefmt.Time(it.UpdatedAt), position: it.Position()}
}

// itemsOf are the stored items as an answer gives them, in their order.
func itemsOf(stored []Item) []item {
	items := make([]item, len(stored))
	for i, it := range stored {
		items[i] = itemOf(it)
	}
	return items
}

// list answers GET on a secrets collection with a page of the scope's
// secrets, ordered by key with A to Z lowered. It reads no value, so it
// lists every secret without a key and those no key opens.
func (c *Component) list(w http.ResponseWriter, r *http.Request) error {
	t, err := targetOf(r)
	if err != nil {
		return err
	}
	page, err := server.ReadPage(r)
	if err != nil {
		return err
	}
	var after string
	if _, err := page.After(&after); err != nil {
		return err
	}
	stored, err := c.ListPage(r.Context(), t.scope, t.id, after, page.Limit+1)
	if err != nil {
		return err
	}
	return server.WritePage(w, page, itemsOf(stored), func(it item) any { return it.position })
}

// create answers POST on a secrets collection: it stores every entry of the
// body or none, each with an event, in one transaction, and answers the
// entries in request order. A body is checked before the key, so a bad
// request is answered 400 also on a hub that cannot store; the limit is
// checked before a conflict.
func (c *Component) create(w http.ResponseWriter, r *http.Request) error {
	t, err := targetOf(r)
	if err != nil {
		return err
	}
	entries, err := decodeEntries(r)
	if err != nil {
		return err
	}
	if !c.Available() {
		return UnavailableError
	}
	ctx := r.Context()
	in, err := eventInput(ctx, t)
	if err != nil {
		return err
	}
	var stored []Item
	err = c.change(ctx, func(tx *sql.Tx) error {
		var err error
		if stored, err = c.CreateEntries(ctx, tx, t.scope, t.id, entries); err != nil {
			return err
		}
		for _, it := range stored {
			if err := c.emit(ctx, tx, t, in, eventCreate, it.Key); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return routeError(err)
	}
	server.WriteJSON(w, http.StatusCreated, struct {
		Items []item `json:"items"`
	}{itemsOf(stored)})
	return nil
}

// update answers PATCH on a secret: it seals the body's value under the
// current key in place of the old one, which it never opens, so a value no
// configured key opens can still be replaced. The key keeps the casing it
// was first stored with and its creation time. The body is checked before
// the key, and the key before the secret is looked up, so a hub that cannot
// store answers 503 for a key the scope does not hold too. The path's key is
// not checked against the key rules: one no secret can have is not found.
func (c *Component) update(w http.ResponseWriter, r *http.Request) error {
	t, err := targetOf(r)
	if err != nil {
		return err
	}
	name := server.Param(r, "key")
	value, err := decodeValue(r, name)
	if err != nil {
		return err
	}
	if !c.Available() {
		return UnavailableError
	}
	ctx := r.Context()
	in, err := eventInput(ctx, t)
	if err != nil {
		return err
	}
	var it Item
	err = c.change(ctx, func(tx *sql.Tx) error {
		var err error
		if it, err = c.UpdateEntry(ctx, tx, t.scope, t.id, name, value); err != nil {
			return err
		}
		return c.emit(ctx, tx, t, in, eventUpdate, it.Key)
	})
	if err != nil {
		return routeError(err)
	}
	server.WriteJSON(w, http.StatusOK, itemOf(it))
	return nil
}

// remove answers DELETE on a secret with 204 and no body. It never opens
// the value, so it works on a hub without a key and on a value no
// configured key opens.
func (c *Component) remove(w http.ResponseWriter, r *http.Request) error {
	t, err := targetOf(r)
	if err != nil {
		return err
	}
	ctx := r.Context()
	in, err := eventInput(ctx, t)
	if err != nil {
		return err
	}
	err = c.change(ctx, func(tx *sql.Tx) error {
		it, err := c.DeleteEntry(ctx, tx, t.scope, t.id, server.Param(r, "key"))
		if err != nil {
			return err
		}
		return c.emit(ctx, tx, t, in, eventDelete, it.Key)
	})
	if err != nil {
		return routeError(err)
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// decodeValue reads the body of an update request, {"value": string}, and
// answers 400 invalid_request, naming key, when value is absent, null, not
// a string or over the size limit. Other members are ignored.
func decodeValue(r *http.Request, key string) (string, error) {
	var body map[string]json.RawMessage
	if err := server.DecodeJSON(r, &body); err != nil {
		return "", err
	}
	var value string
	if !isString(body["value"]) || json.Unmarshal(body["value"], &value) != nil {
		return "", server.InvalidRequest(fmt.Sprintf("the value of key %q must be a string", key))
	}
	if err := CheckValue(key, value); err != nil {
		return "", server.InvalidRequest(err.Error())
	}
	return value, nil
}

// change runs fn in a write transaction of the store, which commits the
// secrets and their events together or neither.
func (c *Component) change(ctx context.Context, fn func(*sql.Tx) error) error {
	if c.deps.Store == nil || c.deps.Events == nil {
		return errNotBound
	}
	return c.deps.Store.Tx(ctx, fn)
}

// eventInput is the event of a change to t's secrets by the request's
// caller, without its type and its secret: the actor is the admin token,
// with no actor id, or the credential's type with its user's id.
func eventInput(ctx context.Context, t target) (event.Input, error) {
	in := event.Input{ResourceType: "secret"}
	if t.scope == ScopeWorkspace {
		slug := t.id
		in.Workspace = &slug
	}
	p, ok := identity.PrincipalOf(ctx)
	switch {
	case ok && p.Type == identity.TypeAdmin:
		in.ActorType = event.ActorAdminToken
	case ok && p.User != nil:
		id := p.User.ID
		in.ActorType, in.ActorID = p.Type, &id
	default:
		return event.Input{}, errNoCaller
	}
	return in, nil
}

// emit writes the event of type typ about the secret key of t, inside tx,
// from the base in. Its metadata names the scope and the key, never a value.
func (c *Component) emit(ctx context.Context, tx *sql.Tx, t target, in event.Input, typ, key string) error {
	in.Type, in.ResourceID = typ, key
	in.Metadata = map[string]string{"scope": string(t.scope), "scope_id": t.id, "key": key}
	if _, err := c.deps.Events.Emit(ctx, tx, in); err != nil {
		return fmt.Errorf("write the %s event: %w", typ, err)
	}
	return nil
}

// routeError is how a route answers an error of the component: its refusals
// with their status and message, anything else as an internal error.
func routeError(err error) error {
	var ke *keyError
	switch {
	case errors.As(err, &ke) && errors.Is(err, ErrInvalid):
		return server.InvalidRequest(ke.msg)
	case errors.As(err, &ke) && errors.Is(err, ErrConflict):
		return server.Conflict(ke.msg)
	case errors.As(err, &ke) && errors.Is(err, ErrNotFound):
		return server.NotFound(ke.msg)
	case errors.Is(err, ErrUnavailable):
		return UnavailableError
	case errors.Is(err, ErrLimitReached):
		return errLimit
	}
	return err
}

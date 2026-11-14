package access

import (
	"net/http"

	"github.com/agent-fox-dev/parley/internal/server"
)

// Relation is how the workspace a request names stands to the workspace a
// PAT is bound to. The empty Relation is none: the binding does not reach
// the workspace.
type Relation string

// The relations. RelationBound is the bound workspace itself;
// RelationCreatedFork a task fork of it that the token created, which the
// binding covers in full; RelationParentOfBoundFork the parent of the fork a
// token is bound to, on which the token reaches only the contributor routes.
const (
	RelationBound             Relation = "bound"
	RelationCreatedFork       Relation = "created_fork"
	RelationParentOfBoundFork Relation = "parent_of_bound_fork"
)

// Binding is what the binding guard found for a bound PAT's request, which
// the route's handler reads to confine what it does.
type Binding struct {
	// TokenID is the bound PAT's token_id.
	TokenID string
	// Workspace is the slug the PAT is bound to.
	Workspace string
	// Relation is how the workspace the request names stands to Workspace;
	// none on a route that names no workspace.
	Relation Relation
}

// bindingKey is the key a Binding is attached to a request under.
type bindingKey struct{}

// BindingOf returns the binding the guard attached to the request r. It
// reports false for every caller but a bound PAT. The guard is a check and
// cannot change the request's context, so it attaches the binding instead
// (ADR 04).
func BindingOf(r *http.Request) (Binding, bool) {
	v, ok := server.Attached(r, bindingKey{})
	if !ok {
		return Binding{}, false
	}
	b, ok := v.(Binding)
	return b, ok
}

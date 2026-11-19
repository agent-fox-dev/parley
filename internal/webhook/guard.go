package webhook

import (
	"net/http"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/google/uuid"
)

// The scopes a PAT needs on a webhook route: scopeRead on one that reads,
// scopeWrite, which grants scopeRead too, on one that changes something.
const (
	scopeRead  = "workspaces:read"
	scopeWrite = "workspaces:write"
)

// The refusals of the guard.
const (
	errorTypeNotOnFork       = "not_supported_on_fork"
	errorTypeWebhookNotFound = "webhook_not_found"
	// msgWebhookNotFound is the not-found answer's message, built from a
	// constant alone, so that its bytes are the same for an id of another
	// workspace's subscription and for one that names none.
	msgWebhookNotFound = "webhook not found"
)

// request is what the guard hands a webhook handler: the access decision,
// whose record is the addressed workspace, and on a route with :id the
// subscription it names in that workspace.
type request struct {
	decision     access.Decision
	subscription *Subscription
}

// slug is the addressed workspace's slug.
func (q request) slug() string { return q.decision.Record.Slug }

// handler serves a webhook route once the guard has let it through.
type handler func(w http.ResponseWriter, r *http.Request, q request) error

// route declares a webhook route of the component. The pipeline runs the
// binding guard, which refuses every workspace-bound token, then the scope
// check, then the access check with need owner; the handler the route gets
// then answers, in this order, 400 not_supported_on_fork on a task fork,
// 400 invalid_request for an :id or a :delivery_id that is not a UUID and
// 404 webhook_not_found for an :id that names no subscription of the
// addressed workspace, before it runs h.
func (c *Component) route(method, pattern, scope string, h handler) server.Route {
	return server.Route{
		Method:  method,
		Pattern: pattern,
		Scope:   scope,
		Marks:   map[string]string{access.MarkNeed: string(access.NeedOwner), access.MarkRefusesBound: ""},
		Handler: c.guard(h),
	}
}

// guard wraps h with the route conditions that follow the pipeline's
// checks (see route).
func (c *Component) guard(h handler) server.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		d, ok := access.DecisionOf(r)
		if !ok {
			return server.Internal("the webhook route found no access decision")
		}
		if d.Record.Kind == access.KindFork {
			return server.NewError(errorTypeNotOnFork, http.StatusBadRequest,
				"a task fork has no event webhooks; subscribe on its parent "+d.Record.ParentSlug+" with include_forks")
		}
		m, _ := server.MatchedRoute(r)
		for _, name := range []string{"id", "delivery_id"} {
			if v, ok := m.Params[name]; ok && !isUUID(v) {
				return server.InvalidRequest(name + " must be a UUID")
			}
		}
		q := request{decision: d}
		if id, ok := m.Params["id"]; ok {
			sub, found, err := subscriptionIn(r.Context(), c.deps.Store.Main(), d.Record.Slug, id)
			if err != nil {
				return err
			}
			if !found {
				return server.NewError(errorTypeWebhookNotFound, http.StatusNotFound, msgWebhookNotFound)
			}
			q.subscription = &sub
		}
		return h(w, r, q)
	}
}

// isUUID reports whether s is a UUID in its 36-character form, the one form
// the hub writes its ids in.
func isUUID(s string) bool {
	_, err := uuid.Parse(s)
	return len(s) == 36 && err == nil
}

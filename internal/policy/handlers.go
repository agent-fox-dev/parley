package policy

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/server"
)

// Routes are the policy routes, for the server's router. Their patterns are
// those of the contributor table of internal/access, so that a token bound
// to a task fork reaches them on the fork's parent; the scope, the binding
// and the access decision come from the shared pipeline before a handler
// runs.
func (c *Component) Routes() []server.Route {
	return []server.Route{{
		Method: http.MethodGet, Pattern: "/workspaces/:slug/policy", Scope: "changes:read",
		Marks: map[string]string{access.MarkNeed: string(access.NeedRead)}, Handler: c.getPolicy,
	}}
}

// getPolicy answers the current revision of the workspace's policy, or the
// revision the revision parameter names, with its number as the ETag. A
// task fork has no policy of its own: it answers revision 0 whatever its
// parent's policy is. Every status, archived included, answers the read.
func (c *Component) getPolicy(w http.ResponseWriter, r *http.Request) error {
	var n *int64
	if q := r.URL.Query(); q.Has("revision") {
		v, ok := nonNegative(q.Get("revision"))
		if !ok {
			return server.InvalidRequest("revision must be a non-negative integer")
		}
		n = &v
	}
	d, _ := access.DecisionOf(r)
	rev, found, err := c.readRevision(r.Context(), server.Param(r, "slug"), d.Record.Kind == access.KindFork, n)
	if err != nil {
		return err
	}
	if !found {
		return server.NotFound("no such policy revision")
	}
	writeRevision(w, rev)
	return nil
}

// readRevision reads the current revision of the workspace slug for n nil,
// else revision n; a fork has revision 0 only.
func (c *Component) readRevision(ctx context.Context, slug string, fork bool, n *int64) (Revision, bool, error) {
	switch {
	case fork:
		return revisionZero, n == nil || *n == 0, nil
	case n == nil:
		rev, err := CurrentRevision(ctx, c.deps.Store.Main(), slug)
		return rev, err == nil, err
	default:
		return RevisionAt(ctx, c.deps.Store.Main(), slug, *n)
	}
}

// writeRevision answers 200 with rev and its number, quoted, as the ETag,
// which a replacement names in If-Match.
func writeRevision(w http.ResponseWriter, rev Revision) {
	w.Header().Set("ETag", strconv.Quote(strconv.FormatInt(rev.Number, 10)))
	server.WriteJSON(w, http.StatusOK, rev)
}

// nonNegative is s as a non-negative integer in decimal digits, and false
// for anything else, such as "", -1, +1 or 1.5.
func nonNegative(s string) (int64, bool) {
	if s == "" || strings.Trim(s, "0123456789") != "" {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil
}

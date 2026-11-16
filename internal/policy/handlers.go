package policy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// maxRequestBytes is the body limit of the routes that take a document. It
// is above the largest document the format allows, so that a document too
// long is refused by the validator, which names it, whatever body limit
// the operator set for the other routes.
const maxRequestBytes = 1 << 20

// eventUpdate is the type of the event a replacement of a policy records.
const eventUpdate = "parley.policy.update"

// Routes are the policy routes, for the server's router. The patterns of
// the reads are those of the contributor table of internal/access, so that
// a token bound to a task fork reaches them on the fork's parent, where the
// table does not let it replace the policy; the scope, the binding and the
// access decision come from the shared pipeline before a handler runs.
func (c *Component) Routes() []server.Route {
	return []server.Route{{
		Method: http.MethodGet, Pattern: "/workspaces/:slug/policy", Scope: "changes:read",
		Marks: map[string]string{access.MarkNeed: string(access.NeedRead)}, Handler: c.getPolicy,
	}, {
		Method: http.MethodPut, Pattern: "/workspaces/:slug/policy", Scope: "workspaces:write",
		Marks: map[string]string{access.MarkNeed: string(access.NeedOwner)}, Handler: c.putPolicy,
		MaxBodySize: maxRequestBytes,
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

// putPolicy replaces the workspace's policy with the document of the body
// and answers the new revision as GET does. The refusals come in this
// order: a task fork, which has no policy of its own, before the body is
// read; then a body that is not one JSON object or an If-Match that is not
// a revision; then a document that breaks the format; then, in the write's
// transaction, an archived workspace, which takes no writes, and an
// If-Match that is not the stored revision.
func (c *Component) putPolicy(w http.ResponseWriter, r *http.Request) error {
	ctx, slug := r.Context(), server.Param(r, "slug")
	if d, _ := access.DecisionOf(r); d.Record.Kind == access.KindFork {
		return server.NewError("not_supported_on_fork", http.StatusBadRequest,
			fmt.Sprintf("workspace %q is a task fork, which has no policy of its own; set the policy of its parent", slug))
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return server.InvalidRequest("request body could not be read")
	}
	ifMatch, err := parseIfMatch(r.Header.Values("If-Match"))
	if err != nil {
		return err
	}
	// A body too long for a document is the validator's to refuse, as a
	// document, whatever it holds.
	if len(raw) <= maxDocumentBytes {
		if _, ok := decodeObject(raw); !ok {
			return server.InvalidRequest("request body must be one JSON object")
		}
	}
	doc, err := Validate(ctx, raw, c.deps.Users)
	if inv := (*InvalidPolicyError)(nil); errors.As(err, &inv) {
		return inv.APIError()
	}
	if err != nil {
		return err
	}
	p, _ := identity.PrincipalOf(ctx)
	by := writerOf(p)
	var rev Revision
	err = c.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		ws, found, err := c.deps.Workspaces.Workspace(ctx, tx, slug)
		switch {
		case err != nil:
			return err
		case !found:
			return server.NotFound("workspace not found")
		case ws.Status == string(workspace.StatusArchived):
			return server.WorkspaceArchived(fmt.Sprintf("workspace %q is archived", slug))
		}
		rev, err = Replace(ctx, tx, slug, *doc, ifMatch, by, c.deps.Now())
		if errors.Is(err, ErrRevisionMismatch) {
			return server.NewError("revision_mismatch", http.StatusPreconditionFailed,
				"the policy has been replaced since the revision named in If-Match; read it again")
		}
		if err != nil {
			return err
		}
		if _, err := c.deps.Events.Emit(ctx, tx, event.Input{
			Type: eventUpdate, Workspace: &slug, ActorType: by.ActorType, ActorID: by.ActorID,
			ResourceType: "policy", ResourceID: slug,
			Metadata: map[string]any{"revision": rev.Number, "rules": len(doc.Rules)},
		}); err != nil {
			return err
		}
		return c.afterReplace(ctx, tx, slug)
	})
	if err != nil {
		return err
	}
	writeRevision(w, rev)
	return nil
}

// afterReplace runs in the transaction of a replacement of the workspace
// slug's policy, after the new revision is stored and before it commits;
// an error from it rolls the replacement back. The landings of the
// workspace's changes are not evaluated against the new revision yet.
func (c *Component) afterReplace(context.Context, *sql.Tx, string) error {
	return nil
}

// parseIfMatch is the revision the If-Match values name, bare or in double
// quotes, nil without the header. Any other value, a weak tag or a list
// included, is refused.
func parseIfMatch(values []string) (*int64, error) {
	if len(values) == 0 {
		return nil, nil
	}
	refused := server.InvalidRequest("If-Match must be a policy revision, a non-negative integer, bare or in double quotes")
	if len(values) > 1 {
		return nil, refused
	}
	v := strings.TrimSpace(values[0])
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		v = v[1 : len(v)-1]
	}
	n, ok := nonNegative(v)
	if !ok {
		return nil, refused
	}
	return &n, nil
}

// writerOf is who a principal writes a revision as: the admin token, with
// no user, or the credential's type with its user.
func writerOf(p identity.Principal) Writer {
	if p.User == nil {
		return Writer{ActorType: event.ActorAdminToken}
	}
	return Writer{ActorType: p.Type, ActorID: new(p.User.ID), Username: new(p.User.Username)}
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

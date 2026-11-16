package policy

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"unicode/utf8"
	"uuid"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// maxCommentChars is the longest comment a verdict takes, in Unicode code
// points.
const maxCommentChars = 16000

// The types of the events a verdict records; the action is the verdict.
const (
	eventApprove = "parley.change.approve"
	eventReject  = "parley.change.reject"
)

// approvalRoutes are the routes of the verdicts. Recording one needs the
// scope changes:approve, which no other scope implies, and read access: a
// reviewer need not be able to write the workspace. Withdrawing one needs
// the same, and the handler then decides whose verdict the caller may
// withdraw. Neither write pattern is in the contributor table of
// internal/access, so that a token bound to a task fork cannot certify, on
// the fork's parent, the work it pushed, nor take back the verdicts given
// on it.
func (c *Component) approvalRoutes() []server.Route {
	read := map[string]string{access.MarkNeed: string(access.NeedRead)}
	return []server.Route{{
		Method: http.MethodPost, Pattern: "/workspaces/:slug/changes/:number/approvals", Scope: "changes:approve",
		Marks: read, Handler: c.postApproval, Create: true,
	}, {
		Method: http.MethodGet, Pattern: "/workspaces/:slug/changes/:number/approvals", Scope: "changes:read",
		Marks: read, Handler: c.getApprovals,
	}, {
		Method: http.MethodDelete, Pattern: "/workspaces/:slug/changes/:number/approvals/:id", Scope: "changes:approve",
		Marks: read, Handler: c.deleteApproval,
	}}
}

// verdictRequest is the body of a verdict: the verdict, its comment, and
// the version it is on, 0 for the change's current version.
type verdictRequest struct {
	verdict, comment string
	version          int64
}

// postApproval records the caller's verdict on a version of the change and
// answers the approval, 201 for the user's first verdict on that version
// and 200 for one that replaces it. The admin token is refused before
// anything else, since a verdict needs a user; then come, in this order, a
// change the workspace does not have, a body that is not a valid verdict,
// an archived workspace, a closed change and a version the change does not
// have. The verdict, the landing it re-evaluates and its event are written
// in one store transaction, so that a failure of any of them writes none.
func (c *Component) postApproval(w http.ResponseWriter, r *http.Request) error {
	ctx, slug := r.Context(), server.Param(r, "slug")
	p, _ := identity.PrincipalOf(ctx)
	if p.User == nil {
		return server.Forbidden("a verdict is given by a user; the admin token cannot give one")
	}
	found, err := c.existingChange(ctx, r)
	if err != nil {
		return err
	}
	number := found.Number
	var body map[string]json.RawMessage
	if err := server.DecodeJSON(r, &body); err != nil {
		return err
	}
	req, err := parseVerdict(body)
	if err != nil {
		return err
	}
	caller := change.CallerOf(p)
	var (
		a        approval
		replaced bool
	)
	err = c.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		ch, err := c.writableChange(ctx, tx, slug, number)
		if err != nil {
			return err
		}
		v := storedVerdict{
			ID: uuid.NewV4().String(), Workspace: slug, Number: number, Version: req.version,
			UserID: caller.UserID, Username: caller.Username, Credential: caller.Credential,
			Verdict: req.verdict, Comment: req.comment, CreatedAt: c.deps.Now(),
		}
		if v.Version == 0 {
			v.Version = ch.Version
		}
		if v.Version > ch.Version {
			return server.NotFound(fmt.Sprintf("%s has no version %d", ch.Ref, v.Version))
		}
		if replaced, err = replaceVerdict(ctx, tx, v); err != nil {
			return err
		}
		res, _, err := c.ReevaluateIn(ctx, tx, slug, number)
		if err != nil {
			return err
		}
		k, err := c.newCounter(ctx, tx, ch.WorkspaceSlug, ch.Number, ch.Version, res.Requirements)
		if err != nil {
			return err
		}
		countsFor, err := k.countsFor(ctx, v)
		if err != nil {
			return err
		}
		a = approvalOf(v, countsFor)
		typ := eventApprove
		if v.Verdict == verdictReject {
			typ = eventReject
		}
		_, err = c.deps.Events.Emit(ctx, tx, event.Input{
			Type: typ, Workspace: &slug, ActorType: p.Type, ActorID: new(p.User.ID),
			ResourceType: "change", ResourceID: strconv.FormatInt(number, 10),
			Metadata: map[string]any{"version": v.Version, "approval_id": v.ID, "counts_for": a.CountsFor,
				"number": number, "ref": ch.Ref},
		})
		return err
	})
	if err != nil {
		return err
	}
	status := http.StatusCreated
	if replaced {
		status = http.StatusOK
	}
	server.WriteJSON(w, status, a)
	return nil
}

// getApprovals answers the verdicts of the change, of every version or of
// the version the query names, newest first, in pages of the list
// envelope. After the page come, in this order, a change the workspace
// does not have, a version that is not a positive integer and a version
// the change does not have. Every status of the workspace answers the
// read.
func (c *Component) getApprovals(w http.ResponseWriter, r *http.Request) error {
	page, err := server.ReadPage(r)
	if err != nil {
		return err
	}
	ctx := r.Context()
	ch, err := c.existingChange(ctx, r)
	if err != nil {
		return err
	}
	var f verdictFilter
	if q := r.URL.Query(); q.Has("version") {
		v, ok := positive(q.Get("version"))
		if !ok {
			return server.InvalidRequest("version must be a positive integer")
		}
		if v > ch.Version {
			return server.NotFound(fmt.Sprintf("%s has no version %d", ch.Ref, v))
		}
		f.Version = v
	}
	var after verdictPosition
	more, err := page.After(&after)
	if err != nil {
		return err
	}
	if more {
		f.After = &after
	}
	f.Limit = page.Limit + 1
	items, err := c.listApprovals(ctx, c.deps.Store.Main(), ch, f)
	if err != nil {
		return err
	}
	return server.WritePage(w, page, items, positionOf)
}

// deleteApproval withdraws the verdict :id of the change and answers 204.
// The refusals come in this order: a change the workspace does not have,
// an archived workspace, a closed change, an id that names no verdict of
// the change, and a caller who is neither the verdict's user, with any
// credential, nor the workspace's owner nor the admin token. The removal
// and the landing it re-evaluates are written in one store transaction;
// a withdrawal writes no event of its own.
func (c *Component) deleteApproval(w http.ResponseWriter, r *http.Request) error {
	ctx, slug, id := r.Context(), server.Param(r, "slug"), server.Param(r, "id")
	p, _ := identity.PrincipalOf(ctx)
	d, _ := access.DecisionOf(r)
	found, err := c.existingChange(ctx, r)
	if err != nil {
		return err
	}
	err = c.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		ch, err := c.writableChange(ctx, tx, slug, found.Number)
		if err != nil {
			return err
		}
		v, ok, err := readVerdict(ctx, tx, slug, ch.Number, id)
		switch {
		case err != nil:
			return err
		case !ok:
			return server.NotFound(fmt.Sprintf("%s has no verdict %q", ch.Ref, id))
		case d.Level != access.LevelAdmin && d.Level != access.LevelOwner && (p.User == nil || p.User.ID != v.UserID):
			return server.Forbidden("only the verdict's user, the workspace's owner or the admin token may withdraw it")
		}
		if err := deleteVerdict(ctx, tx, v.ID); err != nil {
			return err
		}
		_, _, err = c.ReevaluateIn(ctx, tx, slug, ch.Number)
		return err
	})
	if err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// existingChange is the change the request's :number names in the
// workspace :slug, read from the store, refused alike with the constant
// change_not_found for a :number that is not a positive integer and for a
// number the workspace does not have.
func (c *Component) existingChange(ctx context.Context, r *http.Request) (change.Change, error) {
	number, ok := positive(server.Param(r, "number"))
	if !ok {
		return change.Change{}, change.ErrChangeNotFound
	}
	ch, err := c.deps.Changes.ReadChange(ctx, c.deps.Store.Main(), server.Param(r, "slug"), number)
	if errors.Is(err, change.ErrChangeNotFound) {
		return change.Change{}, change.ErrChangeNotFound
	}
	return ch, err
}

// writableChange reads, in tx, the change number of the workspace slug,
// which takes evidence only while the workspace is not archived, checked
// first, and the change is a draft or open.
func (c *Component) writableChange(ctx context.Context, tx *sql.Tx, slug string, number int64) (change.Change, error) {
	ws, found, err := c.deps.Workspaces.Workspace(ctx, tx, slug)
	switch {
	case err != nil:
		return change.Change{}, err
	case !found:
		return change.Change{}, change.ErrChangeNotFound
	case ws.Status == string(workspace.StatusArchived):
		return change.Change{}, server.WorkspaceArchived(fmt.Sprintf("workspace %q is archived", slug))
	}
	ch, err := c.deps.Changes.ReadChange(ctx, tx, slug, number)
	if errors.Is(err, change.ErrChangeNotFound) {
		return change.Change{}, change.ErrChangeNotFound
	}
	if err != nil {
		return change.Change{}, err
	}
	if ch.State != change.StateDraft && ch.State != change.StateOpen {
		return change.Change{}, server.NewError("change_closed", http.StatusConflict,
			fmt.Sprintf("%s is %s and takes no more evidence", ch.Ref, ch.State))
	}
	return ch, nil
}

// parseVerdict reads the members of a verdict's body, refusing with 400
// invalid_request a member other than verdict, comment and version, a
// verdict other than approve and reject, a comment that is not a string of
// at most maxCommentChars code points, and a version that is not a
// positive integer.
func parseVerdict(body map[string]json.RawMessage) (verdictRequest, error) {
	var req verdictRequest
	for name := range body {
		if name != "verdict" && name != "comment" && name != "version" {
			return req, server.InvalidRequest(fmt.Sprintf("unknown member %q; a verdict has verdict, comment and version", name))
		}
	}
	if json.Unmarshal(body["verdict"], &req.verdict) != nil || (req.verdict != verdictApprove && req.verdict != verdictReject) {
		return req, server.InvalidRequest("verdict must be approve or reject")
	}
	if raw, given := body["comment"]; given {
		var comment *string
		if json.Unmarshal(raw, &comment) != nil || comment == nil || utf8.RuneCountInString(*comment) > maxCommentChars {
			return req, server.InvalidRequest(fmt.Sprintf("comment must be a string of at most %d characters", maxCommentChars))
		}
		req.comment = *comment
	}
	if raw, given := body["version"]; given {
		if json.Unmarshal(raw, &req.version) != nil || req.version <= 0 {
			return req, server.InvalidRequest("version must be a positive integer")
		}
	}
	return req, nil
}

// positive is s as a positive integer in decimal digits, and false for
// anything else, as the change routes read :number.
func positive(s string) (int64, bool) {
	n, ok := nonNegative(s)
	return n, ok && n > 0
}

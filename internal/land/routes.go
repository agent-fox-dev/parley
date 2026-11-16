package land

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"uuid"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
)

// Routes are the land routes, for the server's router: the land of a
// change, the revert of a landed change, the attempts of a change, the
// attempts of a workspace, one attempt by id, and the current conflict of
// a change and its files.
func (c *Component) Routes() []server.Route {
	need := map[string]string{access.MarkNeed: string(access.NeedRead)}
	read := func(pattern string, h server.HandlerFunc) server.Route {
		return server.Route{Method: http.MethodGet, Pattern: pattern, Scope: "changes:read", Marks: need, Handler: h}
	}
	// The patterns of the land, of a change's attempts, of one attempt and
	// of a change's conflict and its files are those of the contributor
	// table of internal/access, parameter names included, so that a token
	// bound to a fork reaches them on the fork's parent. The land needs only read
	// access: who may land is the land's own rule (authorize). The revert
	// is not in that table, so the binding guard refuses it to a token
	// bound to a fork, and needs only read access too.
	return []server.Route{
		{
			Method: http.MethodPost, Pattern: "/workspaces/:slug/changes/:number/land", Scope: "changes:write", Create: true,
			Marks: need, Handler: c.landRoute,
		},
		{
			Method: http.MethodPost, Pattern: "/workspaces/:slug/changes/:number/revert", Scope: "changes:write", Create: true,
			Marks: need, Handler: c.revertRoute,
		},
		read("/workspaces/:slug/changes/:number/lands", c.changeLands),
		read("/workspaces/:slug/lands", c.workspaceLands),
		read("/workspaces/:slug/lands/:id", c.getLand),
		read("/workspaces/:slug/changes/:number/conflict", c.conflictRoute),
		read("/workspaces/:slug/changes/:number/conflict/file", c.conflictFileRoute),
	}
}

// errLandNotFound is the answer for an id that names no attempt of the
// workspace.
func errLandNotFound(workspace, id string) error {
	return server.NewError("land_not_found", http.StatusNotFound, fmt.Sprintf("%s has no land attempt %q", workspace, id))
}

// positive is s as a positive integer in decimal digits, and false for
// anything else, such as 0, -1, +1 or 1.5.
func positive(s string) (int64, bool) {
	if s == "" || strings.Trim(s, "0123456789") != "" {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil && n > 0
}

// isUUID reports whether s is a UUID in its 36-character form, the one
// form an attempt's id is written in.
func isUUID(s string) bool {
	_, err := uuid.Parse(s)
	return len(s) == 36 && err == nil
}

// secondsOf is created_at, stored in nanoseconds, at the resolution the
// attempt object shows it, so that the lists' order is the order of the
// values the caller sees, and ties among them go by id.
const secondsOf = `(a.created_at / 1000000000)`

// listPosition is an attempt's place in the lists' order.
type listPosition struct {
	Seconds int64  `json:"t"`
	ID      string `json:"id"`
}

// changeLands answers the attempts that cover the change :number of the
// workspace :slug, newest first, in pages of the list envelope. A :number
// that is not a positive integer, or that the workspace has no change of,
// is 404 change_not_found; one whose source is not the fork a token is
// bound to, calling on the fork's parent, is 403 token_bound.
func (c *Component) changeLands(w http.ResponseWriter, r *http.Request) error {
	page, err := server.ReadPage(r)
	if err != nil {
		return err
	}
	slug := server.Param(r, "slug")
	number, ok := positive(server.Param(r, "number"))
	if !ok {
		return change.ErrChangeNotFound
	}
	ch, err := c.deps.Changes.ReadChange(r.Context(), c.deps.Store.Main(), slug, number)
	if err != nil {
		return err
	}
	if err := confine(r, ch); err != nil {
		return err
	}
	return c.writeList(w, r, page, []string{`a.workspace_slug = ?`,
		`EXISTS (SELECT 1 FROM land_attempt_changes c WHERE c.land_id = a.id AND c.number = ?)`}, slug, number)
}

// workspaceLands answers the attempts whose target is the workspace :slug,
// newest first, in pages of the list envelope, narrowed by every filter
// the query gives: status, repeatable, any of the values; target_branch;
// since, an RFC 3339 timestamp the attempt was created at or after. An
// unknown status or a since that does not parse is 400 invalid_request.
func (c *Component) workspaceLands(w http.ResponseWriter, r *http.Request) error {
	page, err := server.ReadPage(r)
	if err != nil {
		return err
	}
	query := r.URL.Query()
	where, args := []string{`a.workspace_slug = ?`}, []any{server.Param(r, "slug")}
	if wanted := query["status"]; len(wanted) > 0 {
		for _, s := range wanted {
			if !slices.Contains(statuses, s) {
				return server.InvalidRequest("status must be one of " + strings.Join(statuses, ", "))
			}
			args = append(args, s)
		}
		where = append(where, `a.status IN (`+strings.TrimSuffix(strings.Repeat("?, ", len(wanted)), ", ")+`)`)
	}
	if query.Has("target_branch") {
		where, args = append(where, `a.target_branch = ?`), append(args, query.Get("target_branch"))
	}
	if query.Has("since") {
		since, err := time.Parse(time.RFC3339, query.Get("since"))
		if err != nil {
			return server.InvalidRequest("since must be an RFC 3339 timestamp")
		}
		where, args = append(where, `a.created_at >= ?`), append(args, since.UnixNano())
	}
	return c.writeList(w, r, page, where, args...)
}

// writeList answers the page of the attempts that meet every condition of
// where, with args, ordered by created_at, as shown, descending, then by
// id descending.
func (c *Component) writeList(w http.ResponseWriter, r *http.Request, page server.Page, where []string, args ...any) error {
	var after listPosition
	more, err := page.After(&after)
	if err != nil {
		return err
	}
	if more {
		where = append(where, `(`+secondsOf+` < ? OR (`+secondsOf+` = ? AND a.id < ?))`)
		args = append(args, after.Seconds, after.Seconds, after.ID)
	}
	attempts, err := readAttempts(r.Context(), c.deps.Store.Main(), `SELECT `+attemptColumns+` FROM land_attempts a WHERE `+
		strings.Join(where, ` AND `)+` ORDER BY `+secondsOf+` DESC, a.id DESC LIMIT ?`, append(args, page.Limit+1)...)
	if err != nil {
		return err
	}
	return server.WritePage(w, page, attempts, func(a Attempt) any { return listPosition{a.CreatedAt.Unix(), a.ID} })
}

// getLand answers the attempt :id of the workspace :slug: an id that is no
// UUID, unknown, or of an attempt of another workspace is 404
// land_not_found; an attempt none of whose changes comes from the fork a
// token is bound to, calling on the fork's parent, is 403 token_bound.
func (c *Component) getLand(w http.ResponseWriter, r *http.Request) error {
	slug, id := server.Param(r, "slug"), server.Param(r, "id")
	if !isUUID(id) {
		return errLandNotFound(slug, id)
	}
	a, found, err := readAttempt(r.Context(), c.deps.Store.Main(), slug, id)
	if err != nil {
		return err
	}
	if !found {
		return errLandNotFound(slug, id)
	}
	if err := confineAttempt(r, a); err != nil {
		return err
	}
	server.WriteJSON(w, http.StatusOK, a)
	return nil
}

// The bounds of the wait query of a land request, in seconds, and the wait
// when it names none.
const (
	maxWait     = 60
	defaultWait = 30 * time.Second
)

// landRequest is what a land request carries: its body's members, nil or
// false where absent, and the wait of its query.
type landRequest struct {
	expectedVersion *int64
	stack           bool
	override        *Override
	sessionID       *string
	wait            time.Duration
}

// parseLand reads the land request r: a body that is none or a JSON
// object, whose expected_version is an integer of at least 1, stack a
// boolean, override an object whose reason is a string that is not blank,
// and session_id a string, and a wait query that is a whole number of
// seconds from 0 to 60. Members it does not know are ignored; anything
// else is 400 invalid_request.
func parseLand(r *http.Request) (landRequest, error) {
	req := landRequest{wait: defaultWait}
	if q := r.URL.Query(); q.Has("wait") {
		s := q.Get("wait")
		n, err := strconv.Atoi(s)
		if s == "" || strings.Trim(s, "0123456789") != "" || err != nil || n > maxWait {
			return req, server.InvalidRequest(fmt.Sprintf("wait must be a whole number of seconds from 0 to %d", maxWait))
		}
		req.wait = time.Duration(n) * time.Second
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return req, server.InvalidRequest("request body could not be read")
	}
	body := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(raw)) > 0 {
		r.Body = io.NopCloser(bytes.NewReader(raw))
		if err := server.DecodeJSON(r, &body); err != nil {
			return req, err
		}
	}
	for name, raw := range body {
		// A null is no value of any member's type.
		null := bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
		switch name {
		case "expected_version":
			var v int64
			if null || json.Unmarshal(raw, &v) != nil || v < 1 {
				return req, server.InvalidRequest("expected_version: must be an integer of at least 1")
			}
			req.expectedVersion = &v
		case "stack":
			if null || json.Unmarshal(raw, &req.stack) != nil {
				return req, server.InvalidRequest("stack: must be a boolean")
			}
		case "override":
			var o struct {
				Reason *string `json:"reason"`
			}
			if null || bytes.TrimSpace(raw)[0] != '{' || json.Unmarshal(raw, &o) != nil || o.Reason == nil ||
				strings.TrimSpace(*o.Reason) == "" {
				return req, server.InvalidRequest("override: must be an object whose reason is a string that is not blank")
			}
			req.override = &Override{Reason: *o.Reason}
		case "session_id":
			var s string
			if null || json.Unmarshal(raw, &s) != nil {
				return req, server.InvalidRequest("session_id: must be a string")
			}
			req.sessionID = &s
		}
	}
	return req, nil
}

// landRoute lands the change :number of the workspace :slug, with the
// changes below it for stack, and answers the land attempt. In this order:
// the change (404 change_not_found), the body and the wait (400
// invalid_request), the confinement of a token bound to a fork, calling on
// the fork's parent, to the changes of the fork (403 token_bound), who may
// land (authorize, 403 forbidden), the workspace's status (409, such as
// workspace_archived), the session (400 unknown_session) and the request
// guards (askRequestGuards); then, in one store transaction, the land's
// preconditions (admit, 409), the check that the changes it covers are
// still those judged before, each at the version judged (409
// version_mismatch), and the creation of the running attempt, which is
// handed to the pool, and the request waits for it (answer).
func (c *Component) landRoute(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	slug := server.Param(r, "slug")
	number, ok := positive(server.Param(r, "number"))
	if !ok {
		return change.ErrChangeNotFound
	}
	ch, err := c.deps.Changes.ReadChange(ctx, nil, slug, number)
	if err != nil {
		return err
	}
	req, err := parseLand(r)
	if err != nil {
		return err
	}
	chain, err := c.collect(ctx, nil, ch, req.stack)
	if err != nil {
		return err
	}
	if err := confine(r, chain...); err != nil {
		return err
	}
	ws, err := c.workspace(ctx, slug)
	if err != nil {
		return err
	}
	p, _ := identity.PrincipalOf(ctx)
	caller := change.CallerOf(p)
	if err := c.authorize(ctx, caller, chain, ws); err != nil {
		return err
	}
	if ws.Ready != nil {
		return ws.Ready
	}
	in := StartInput{
		Workspace: slug, Number: number, Stack: req.stack, ExpectedVersion: req.expectedVersion, Trigger: TriggerManual,
		RequestedBy: actorOf(caller), Caller: caller, SessionID: req.sessionID, Override: req.override,
	}
	session, err := c.session(ctx, in)
	if err != nil {
		return err
	}
	in.judged = covered(chain)
	if err := c.askRequestGuards(ctx, RequestInput{Caller: caller, Workspace: slug, Changes: in.judged,
		Override: req.override}); err != nil {
		return err
	}
	a, err := c.create(ctx, ws, in, session)
	if err != nil {
		return err
	}
	return c.answer(ctx, w, a, req.wait)
}

// askRequestGuards asks each request guard about the land request in, in
// the order they were registered, and returns the first refusal as the
// guard gives it; the guards after it are not asked.
func (c *Component) askRequestGuards(ctx context.Context, in RequestInput) error {
	for _, g := range c.registrations().requestGuards {
		if err := g.Check(ctx, in); err != nil {
			return err
		}
	}
	return nil
}

// actorOf is the actor caller is, as an attempt's requested_by names it.
func actorOf(caller change.Caller) change.Actor {
	a := change.Actor{ActorType: caller.Credential.Type}
	if caller.UserID != "" {
		a.ActorID = new(caller.UserID)
	}
	if caller.Username != "" {
		a.Username = new(caller.Username)
	}
	return a
}

// answer waits up to wait for the attempt a to end, and answers it as it
// then is: 202 while it runs on, 200 when it landed, and for an attempt
// that ended otherwise the error of its status with the attempt as its
// details (final). The attempt runs on the pool whatever becomes of the
// request: a caller that goes away ends only the wait.
func (c *Component) answer(ctx context.Context, w http.ResponseWriter, a Attempt, wait time.Duration) error {
	if wait > 0 {
		select {
		case <-c.ended(a.ID):
		case <-c.deps.After(wait):
		case <-ctx.Done():
		}
	}
	got, found, err := readAttempt(context.WithoutCancel(ctx), c.deps.Store.Main(), a.Workspace, a.ID)
	if err != nil {
		return err
	}
	if !found {
		return errLandNotFound(a.Workspace, a.ID)
	}
	switch got.Status {
	case StatusRunning:
		server.WriteJSON(w, http.StatusAccepted, got)
	case StatusLanded:
		server.WriteJSON(w, http.StatusOK, got)
	default:
		return final(got)
	}
	return nil
}

// final is the answer for the attempt a that ended without landing: 409
// land_conflict for a conflict, 409 policy_blocked for a block and 503
// land_failed for a failure, each with a as its details.
func final(a Attempt) error {
	var e *server.Error
	switch {
	case a.Status == StatusConflict && a.Conflict != nil:
		e = server.NewError("land_conflict", http.StatusConflict, fmt.Sprintf(
			"land attempt %s: commit %s does not apply on top of %s", a.ID, a.Conflict.CommitSHA, a.Conflict.TargetSHA))
	case a.Status == StatusBlocked && a.Blocked != nil:
		e = server.NewError("policy_blocked", http.StatusConflict, fmt.Sprintf(
			"land attempt %s: the landing of %s is %s", a.ID, change.Ref(a.Workspace, a.Blocked.Change), a.Blocked.Status))
	default:
		reason := a.Status
		if a.Error != nil {
			reason = *a.Error
		}
		e = server.NewError("land_failed", http.StatusServiceUnavailable, fmt.Sprintf("land attempt %s failed: %s", a.ID, reason))
	}
	return e.WithDetails(a)
}

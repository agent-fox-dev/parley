package session

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/server"
)

// msgSessionNotFound is the message of every refusal to name a session the
// caller may not read. The answer is built from this constant alone, so
// its bytes are the same for a session the caller may not read, one whose
// workspace a bound token does not cover and an id that names no session.
const msgSessionNotFound = "session not found"

// readRoute serves GET /sessions/:id: the session object, to a caller who
// may read the session.
func (c *Component) readRoute(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	sc, ok := server.CallerOf(ctx)
	if !ok {
		return errNoPrincipal
	}
	s, err := c.readableSession(ctx, access.CallerFrom(sc), server.Param(r, "id"))
	if err != nil {
		return err
	}
	o, err := object(ctx, c.deps.Store.Main(), s)
	if err != nil {
		return err
	}
	server.WriteJSON(w, http.StatusOK, o)
	return nil
}

// readableSession reads the session a route's path names by pathID, for
// the caller: 400 invalid_request when pathID is not a UUID, and 404
// not_found, with one message, when it names no session or one the caller
// may not read (see canRead). The id is looked up in lower case, so it is
// accepted in any letter case.
func (c *Component) readableSession(ctx context.Context, caller access.Caller, pathID string) (row, error) {
	if !server.IsUUID(pathID) {
		return row{}, server.InvalidRequest("the session id must be a UUID in the 8-4-4-4-12 form")
	}
	s, found, err := readRow(ctx, c.deps.Store.Main(), strings.ToLower(pathID))
	if err != nil {
		return row{}, err
	}
	if !found {
		return row{}, server.NotFound(msgSessionNotFound)
	}
	readable, err := c.canRead(ctx, caller, s)
	if err != nil {
		return row{}, err
	}
	if !readable {
		return row{}, server.NotFound(msgSessionNotFound)
	}
	return s, nil
}

// canRead is the session visibility rule: whether caller may read the
// session s. A bound token reads only a session whose workspace its binding
// covers, its own sessions included. Otherwise the admin token reads every
// session; a user reads the sessions they created, with any credential; and
// a caller with a level on the session's workspace, or on the parent of the
// task fork the session is on, reads it. The parent is the one stored with
// the session, so that its readers keep reading it after the fork is gone.
// Levels do not depend on a workspace's status, and a slug with no record
// grants nothing by itself. A failed read is an error, never an answer.
func (c *Component) canRead(ctx context.Context, caller access.Caller, s row) (bool, error) {
	if c.deps.Coverage != nil {
		bound, covered, err := c.deps.Coverage.Covers(ctx, caller, s.WorkspaceSlug)
		if err != nil {
			return false, fmt.Errorf("ask the coverage of workspace %s: %w", s.WorkspaceSlug, err)
		}
		if bound && !covered {
			return false, nil
		}
	}
	if caller.Admin || caller.UserID != "" && caller.UserID == s.UserID {
		return true, nil
	}
	for _, slug := range []string{s.WorkspaceSlug, s.ParentSlug} {
		if slug == "" {
			continue
		}
		d, err := c.deps.Access.Decide(ctx, caller, slug, access.NeedRead)
		if err != nil {
			return false, fmt.Errorf("decide read access to workspace %s: %w", slug, err)
		}
		if d.Outcome == access.Granted {
			return true, nil
		}
	}
	return false, nil
}

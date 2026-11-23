package session

import (
	"context"
	"fmt"
	"strings"

	"github.com/agent-fox-dev/parley/internal/server"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/identity"
)

// Check is the session-linking rule: whether caller may attribute an
// operation on the workspace slug to the session sessionID. It accepts the
// reference, returning the session id in lower case, when the session
// exists, was created by the caller's user or the caller is the admin
// token, and the session is linked to the workspace: its workspace is the
// slug, the slug is the parent stored with a session on a task fork (so
// that a session keeps linking to the parent after its fork is gone), or
// the slug is a task fork of the session's workspace. A sibling fork is
// neither, so it does not link.
//
// Every refusal is ErrUnknownSession itself, for an id that is not a UUID
// or names no session, a session of another creator and an unrelated
// workspace alike, so that the answer does not tell them apart. A failed
// read of the store or of a workspace record is another error, never an
// acceptance.
//
// The session's status does not matter, a bound token's coverage is not
// asked (the caller's access to the operation's workspace is the calling
// package's to decide), and Check writes nothing.
func (c *Component) Check(ctx context.Context, caller identity.Principal, sessionID, slug string) (string, error) {
	if !server.IsUUID(sessionID) {
		return "", ErrUnknownSession
	}
	id := strings.ToLower(sessionID)
	s, found, err := readRow(ctx, c.deps.Store.Main(), id)
	if err != nil {
		return "", fmt.Errorf("check session %s: %w", id, err)
	}
	if !found || !createdBy(caller, s) {
		return "", ErrUnknownSession
	}
	if slug == s.WorkspaceSlug || s.ParentSlug != "" && slug == s.ParentSlug {
		return id, nil
	}
	w, found, err := c.deps.Workspaces.Workspace(ctx, slug)
	if err != nil {
		return "", fmt.Errorf("check session %s: read workspace %s: %w", id, slug, err)
	}
	if found && w.Kind == access.KindFork && w.ParentSlug == s.WorkspaceSlug {
		return id, nil
	}
	return "", ErrUnknownSession
}

// createdBy reports whether caller may name the session s as its creator:
// the admin token names every session, a user only the sessions the user
// created, with any credential.
func createdBy(caller identity.Principal, s row) bool {
	if caller.Type == identity.TypeAdmin {
		return true
	}
	return caller.User != nil && caller.User.ID != "" && caller.User.ID == s.UserID
}

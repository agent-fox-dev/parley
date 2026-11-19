package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/agent-fox-dev/parley/internal/event"
)

// eventForceClosed is the event a forced close writes for each session it
// ends.
const eventForceClosed = "parley.session.force_closed"

// closeReasons are the reasons a forced close accepts: the workspace's
// archive, its delete and, for a task fork, its expiry.
var closeReasons = map[string]bool{"workspace archived": true, "workspace deleted": true, "fork expired": true}

// CloseActive is the forced close: in the caller's transaction tx it ends
// every active session whose workspace is exactly workspaceSlug (a fork's
// sessions are closed by a call for the fork) as terminated, ended at the
// time at in UTC whole seconds, with reason as its error message and no
// duration, and writes one parley.session.force_closed event for each in
// the same transaction. It returns the number of sessions it closed.
//
// A reason other than workspace archived, workspace deleted and fork
// expired, or an empty slug, is refused with an error before the store is
// touched. The end is the conditional one a complete makes, so of a
// complete and a forced close racing on one session exactly one takes
// effect, and a session already ended, a second call included, is left as
// it is and gets no event. A failed event write is returned as the error:
// the caller rolls back, and with it every session this call ended.
// CloseActive never begins, commits or rolls back tx.
func (c *Component) CloseActive(ctx context.Context, tx *sql.Tx, workspaceSlug, reason string, at time.Time) (int, error) {
	if !closeReasons[reason] {
		return 0, fmt.Errorf("forced close: unknown reason %q", reason)
	}
	if workspaceSlug == "" {
		return 0, errors.New("forced close: no workspace slug given")
	}
	if c.deps.Events == nil {
		return 0, errors.New("forced close: no event log to record the close in")
	}
	active, err := activeSessions(ctx, tx, workspaceSlug)
	if err != nil {
		return 0, err
	}
	e := ending{Status: StatusTerminated, EndedAt: at.UTC().Truncate(time.Second), ErrorMessage: &reason}
	closed := 0
	for _, s := range active {
		changed, err := endActive(ctx, tx, s.id, e)
		if err != nil {
			return 0, err
		}
		if !changed {
			continue
		}
		id := s.id
		// The stored parent is passed so that the event of a session on a
		// fork needs no workspace record: within a removal it may be gone.
		if _, err := c.deps.Events.Emit(ctx, tx, event.Input{
			Type: eventForceClosed, Workspace: &workspaceSlug, ActorType: event.ActorSystem,
			ResourceType: "session", ResourceID: id, SessionID: &id,
			Metadata: map[string]string{"reason": reason}, Parent: s.parent,
		}); err != nil {
			return 0, fmt.Errorf("close session %s: %w", id, err)
		}
		closed++
	}
	return closed, nil
}

// activeSession is a session a forced close ends: its id and the parent
// slug stored with it ("" for a session on a repository).
type activeSession struct{ id, parent string }

// activeSessions reads the active sessions of exactly the workspace slug
// through tx, oldest first.
func activeSessions(ctx context.Context, tx *sql.Tx, slug string) ([]activeSession, error) {
	var out []activeSession
	err := eachRow(ctx, tx, func(scan func(...any) error) error {
		var s activeSession
		var parent sql.NullString
		if err := scan(&s.id, &parent); err != nil {
			return err
		}
		s.parent = parent.String
		out = append(out, s)
		return nil
	}, `SELECT id, parent_slug FROM sessions WHERE workspace_slug = ? AND status = 'active' ORDER BY started_at, id`, slug)
	if err != nil {
		return nil, fmt.Errorf("read the active sessions of workspace %q: %w", slug, err)
	}
	return out, nil
}

package scm

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
)

// ErrUnknownSession is the answer of a SessionLinker's check that refuses a
// session id: the write answers it 400 unknown_session.
var ErrUnknownSession = errors.New("unknown session")

// SessionLinker is the session-linking rule as the commit and rebase
// routes ask it, the check the session feature hands to every surface that
// takes a session_id. The writes only check: a write becomes part of a
// session through the version of a change the ref writer's stage records
// with the session the write passes it, so the link is recorded there and
// never here.
type SessionLinker interface {
	// Check judges the session id the caller names for a write to the
	// workspace slug. It returns the id in lower case when it accepts it,
	// an error matching ErrUnknownSession when it refuses it, and any
	// other error when it cannot tell.
	Check(ctx context.Context, caller identity.Principal, sessionID, slug string) (string, error)
}

// RegisterSessionLinker makes l the session-linking rule the writes ask,
// replacing the one registered before; nil removes it. The session feature
// registers it while the hub is composed, on this instance rather than in
// a registry of the package, so that every hub has its own
// (docs/adr/08-register-the-push-collaborators-on-the-ref-writer.md).
// Without one no session exists, and every session_id is refused.
func (c *Component) RegisterSessionLinker(l SessionLinker) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sessions = l
}

// checkSession judges the session_id id, nil for none, that the write wr
// names, and returns the id as the rule gives it back, in lower case: 400
// unknown_session when the rule refuses it or no rule is registered, since
// an id nothing can verify is never accepted, and an internal failure when
// the rule cannot tell.
func (c *Component) checkSession(ctx context.Context, wr *write, id *string) (*string, error) {
	if id == nil {
		return nil, nil
	}
	c.mu.RLock()
	l := c.sessions
	c.mu.RUnlock()
	unknown := server.NewError("unknown_session", http.StatusBadRequest, fmt.Sprintf("session_id: no session %q", *id))
	if l == nil {
		return nil, unknown
	}
	got, err := l.Check(ctx, wr.principal, *id, wr.ws.Slug)
	switch {
	case errors.Is(err, ErrUnknownSession):
		return nil, unknown
	case err != nil:
		return nil, fmt.Errorf("check session %q: %w", *id, err)
	}
	return &got, nil
}

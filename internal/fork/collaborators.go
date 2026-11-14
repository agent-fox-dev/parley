package fork

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/agent-fox-dev/parley/internal/identity"
)

// ErrUnknownSession is the answer of a SessionLinker's check that refuses a
// session id: the create answers it 400 unknown_session.
var ErrUnknownSession = errors.New("unknown session")

// SessionLinker is the session-linking rule as the fork create asks it. The
// session feature registers it on the component; without one no session
// exists, and every session_id is refused.
type SessionLinker interface {
	// Check judges the session id the caller names for a fork of the
	// workspace parent. It returns the id in lower case when it accepts
	// it, an error matching ErrUnknownSession when it refuses it, and any
	// other error when it cannot tell.
	Check(ctx context.Context, caller identity.Principal, sessionID, parent string) (string, error)
	// Record links the fork forkSlug of the workspace parent to the
	// session sessionID in tx, the fork's creating transaction, so that the
	// link is stored with the fork or not at all.
	Record(ctx context.Context, tx *sql.Tx, sessionID, parent, forkSlug string) error
}

// SandboxRequest is what the fork create hands the sandbox component: the
// caller, the new fork, the body's sandbox member as received, and the
// fork's session and purpose, which the sandbox takes when the member names
// none.
type SandboxRequest struct {
	Caller  identity.Principal
	Fork    Fork
	Sandbox json.RawMessage
	// SessionID and Purpose are the fork's, nil when it has none.
	SessionID *string
	Purpose   *string
}

// SandboxCreator is the sandbox component as the fork create asks it. The
// sandbox feature registers it on the component; without one a create that
// asks for a sandbox is refused 404 sandboxes_disabled.
type SandboxCreator interface {
	// CreateSandbox creates in tx, the fork's creating transaction, the
	// sandbox req asks for on the fork and returns its sandbox object, a
	// JSON object, or the API error the create answers unchanged, on which
	// the fork is not created either.
	CreateSandbox(ctx context.Context, tx *sql.Tx, req SandboxRequest) (json.RawMessage, error)
}

// RegisterSessionLinker makes l the session-linking rule of the create; nil
// removes it. It is called while the hub is composed, before it serves.
func (c *Component) RegisterSessionLinker(l SessionLinker) { c.linker = l }

// RegisterSandbox makes s the sandbox component of the create; nil removes
// it. It is called while the hub is composed, before it serves.
func (c *Component) RegisterSandbox(s SandboxCreator) { c.sandbox = s }

// SandboxLifecycle is the sandbox component as a fork's removal and its
// early expiry ask it. The sandbox feature registers it on the component;
// without one a fork has no sandboxes.
type SandboxLifecycle interface {
	// DeleteSandboxes deletes every sandbox of the workspace slug, giving
	// reason, and saves a checkpoint of each first when checkpoint is set.
	// An error leaves the fork in place.
	DeleteSandboxes(ctx context.Context, slug, reason string, checkpoint bool) error
	// HasLiveSandbox reports whether the workspace slug has a sandbox in
	// state provisioning or ready.
	HasLiveSandbox(ctx context.Context, slug string) (bool, error)
}

// RegisterSandboxes makes s the sandbox lifecycle of a fork's removal; nil
// removes it. It is called while the hub is composed, before it serves.
func (c *Component) RegisterSandboxes(s SandboxLifecycle) { c.sandboxes = s }

// SourceChange is a change as the early expiry of a fork reads it: the
// fork's branch it proposes, and its state, such as open, landed or
// abandoned.
type SourceChange struct {
	Branch string
	State  string
}

// ChangeReader is the change component as the early expiry of a fork asks
// it. The change feature registers it on the component; without one a fork
// has no changes, and no fork is removed before its expires_at.
type ChangeReader interface {
	// ChangesFrom returns every change whose source is the fork forkSlug,
	// in any state.
	ChangesFrom(ctx context.Context, forkSlug string) ([]SourceChange, error)
}

// RegisterChangeReader makes r the change reader of the early expiry; nil
// removes it. It is called while the hub is composed, before it serves.
func (c *Component) RegisterChangeReader(r ChangeReader) { c.changes = r }

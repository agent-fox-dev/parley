package policy

import (
	"log/slog"
	"time"

	"github.com/agent-fox-dev/parley/internal/store"
)

// Deps are what the component works with.
type Deps struct {
	// Store is the hub's store, which holds the policy revisions.
	Store *store.Store
	// Workspaces reads the workspace records.
	Workspaces WorkspaceReader
	// Users answers whether the users a document names exist.
	Users Users
	// Events takes the policy events in the caller's transaction.
	Events EventWriter
	// Now is the hub's clock; nil is time.Now.
	Now func() time.Time
	// Logger is where the component logs; nil discards.
	Logger *slog.Logger
}

// Component is the policy of the hub's workspaces: it keeps their
// revisions and serves the policy routes. Its zero value is composed into
// the hub, which registers its routes and collaborators, and Bind gives it
// its dependencies before the hub serves. It embeds the evidence slot, so
// that the source of approvals and check results is registered on the
// instance (RegisterEvidence).
type Component struct {
	EvidenceSlot
	deps Deps
}

// Bind gives the component its dependencies. It is called once, before the
// hub serves.
func (c *Component) Bind(d Deps) {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	c.deps = d
}

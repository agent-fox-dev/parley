package secret

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/agent-fox-dev/parley/internal/access"
)

// Origin names the step of a workspace's chain a value was found in.
type Origin string

// The origins of the steps of a chain, highest priority first.
const (
	// OriginWorkspace is the workspace's own scope.
	OriginWorkspace Origin = "workspace"
	// OriginParent is the workspace scope of a task fork's parent.
	OriginParent Origin = "parent"
	// OriginOrg is the scope of the workspace's organization.
	OriginOrg Origin = "org"
	// OriginUser is the scope of the workspace's owner.
	OriginUser Origin = "user"
)

// Step is one scope of a workspace's chain: where it stands in the chain,
// and the scope and the id of its owner that a lookup reads.
type Step struct {
	Origin  Origin
	Scope   Scope
	ScopeID string
}

// ErrWorkspaceNotFound is returned by Chain for a slug that names no
// workspace, so that a caller tells it from a store that cannot be read.
var ErrWorkspaceNotFound = errors.New("workspace not found")

// errChainNotBound is returned by Chain before the component is bound to
// the readers of the workspace records, the memberships and the users.
var errChainNotBound = errors.New("the secret component is not bound to the workspace records, memberships and users")

// Chain returns the scopes the workspace slug looks variables and secrets
// up in, highest priority first: the workspace's own scope; for a task fork
// whose parent has a record, the parent's workspace scope; the scope of the
// workspace's organization, a fork's being its parent's, while the
// workspace's owner is a member of it and it is active; and the scope of
// the owner, while the owner exists. The chain depends on the workspace
// alone, never on who asks, so that every caller is answered the same. A
// record that is gone ends its step, not the chain: leaving a scope out
// never hands a caller a value it should not get. Every call reads the
// records, the memberships and the user again; nothing is kept between
// calls. A slug with no record is ErrWorkspaceNotFound; a read that fails is
// another error.
func (c *Component) Chain(ctx context.Context, slug string) ([]Step, error) {
	d := c.deps
	if d.Records == nil || d.Memberships == nil || d.UserExists == nil {
		return nil, errChainNotBound
	}
	rec, found, err := d.Records.Record(ctx, slug)
	if err != nil {
		return nil, fmt.Errorf("read the chain of %s: %w", slug, err)
	}
	if !found {
		return nil, ErrWorkspaceNotFound
	}
	steps := []Step{{Origin: OriginWorkspace, Scope: ScopeWorkspace, ScopeID: rec.Slug}}
	// A fork's org is its parent's; the fork's own record carries a copy of
	// it, which stands when the parent's record is gone.
	orgID := rec.OrgID
	if rec.Kind == access.KindFork {
		parent, found, err := d.Records.Record(ctx, rec.ParentSlug)
		if err != nil {
			return nil, fmt.Errorf("read the chain of %s: %w", slug, err)
		}
		if found {
			steps = append(steps, Step{Origin: OriginParent, Scope: ScopeWorkspace, ScopeID: parent.Slug})
			orgID = parent.OrgID
		}
	}
	orgs, err := d.Memberships.ActiveOrgIDs(ctx, rec.OwnerID)
	if err != nil {
		return nil, fmt.Errorf("read the chain of %s: %w", slug, err)
	}
	if slices.Contains(orgs, orgID) {
		steps = append(steps, Step{Origin: OriginOrg, Scope: ScopeOrg, ScopeID: orgID})
	}
	exists, err := d.UserExists(ctx, rec.OwnerID)
	if err != nil {
		return nil, fmt.Errorf("read the chain of %s: %w", slug, err)
	}
	if exists {
		steps = append(steps, Step{Origin: OriginUser, Scope: ScopeUser, ScopeID: rec.OwnerID})
	}
	return steps, nil
}

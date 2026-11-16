package land

import (
	"context"

	"github.com/agent-fox-dev/parley/internal/change"
)

// CollidedChange is a landed change as a conflict report names it in its
// landed_changes: its number, its title, the commit its landing made the
// head of its target and the session it was written in, if any.
type CollidedChange struct {
	Number    int64   `json:"number"`
	Title     string  `json:"title"`
	LandedSHA string  `json:"landed_sha"`
	SessionID *string `json:"session_id"`
}

// LandedLookup finds the landed change a commit belongs to.
type LandedLookup interface {
	// LandedChange is the landed change of the workspace that the commit
	// sha, given in full, belongs to; found is false for a commit no landed
	// change brought, as a direct push.
	LandedChange(ctx context.Context, workspace, sha string) (ch CollidedChange, found bool, err error)
}

// landedChanges is the LandedLookup over the change component: the
// landed-commit mapping names the change that landed a commit, and the
// change record its title, session and landed commit. A commit that is the
// head of a version but was never landed is no landed change's.
type landedChanges struct{ changes *change.Component }

func (l landedChanges) LandedChange(ctx context.Context, workspace, sha string) (CollidedChange, bool, error) {
	cc, found, err := l.changes.ChangeOfCommit(ctx, nil, workspace, sha)
	if err != nil || !found || !cc.Landed {
		return CollidedChange{}, false, err
	}
	ch, err := l.changes.ReadChange(ctx, nil, cc.WorkspaceSlug, cc.Number)
	if err != nil {
		return CollidedChange{}, false, err
	}
	out := CollidedChange{Number: ch.Number, Title: ch.Title, SessionID: ch.SessionID}
	if ch.Landed != nil {
		out.LandedSHA = ch.Landed.SHA
	}
	return out, true, nil
}

// landed is the lookup the attribution reads landed changes through:
// Deps.Landed, else the change component's landed-commit mapping; nil when
// the component has neither.
func (c *Component) landed() LandedLookup {
	switch {
	case c.deps.Landed != nil:
		return c.deps.Landed
	case c.deps.Changes != nil:
		return landedChanges{c.deps.Changes}
	}
	return nil
}

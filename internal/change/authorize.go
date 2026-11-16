package change

import (
	"context"
	"fmt"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/server"
)

// AccessRule is the workspace access rule, which the access feature keeps;
// the hub's *access.Service is one.
type AccessRule interface {
	// Level is the caller's level on the workspace slug, found false for
	// a slug with no record.
	Level(ctx context.Context, c access.Caller, slug string) (access.Level, access.Record, bool, error)
	// CanWrite reports whether the user may write the workspace slug.
	CanWrite(ctx context.Context, userID, slug string) (bool, error)
	// RelationOf is how the workspace slug stands to the binding of the
	// bound PAT c.
	RelationOf(ctx context.Context, c access.Caller, slug string) (access.Relation, error)
}

// Authorize judges whether caller may open a change in the workspace target
// from the source workspace source ("" for target), and answers nil or the
// refusal, in this order:
//
//   - a PAT bound to a task fork, on the fork's parent, opens only from that
//     fork (403 token_bound); any other bound PAT only from a workspace its
//     binding covers, the others being invisible to it (400
//     source_not_related);
//   - the caller reads target (404 not_found, the answer of a workspace that
//     does not exist);
//   - the caller writes source: one they read but cannot write is 403
//     forbidden, one that does not exist or that they cannot see 400
//     source_not_related.
//
// The admin token reads and writes every workspace, and the hub (a caller
// without a credential) opens changes of its own. The scope a credential
// needs is the check of the surface it comes in by, such as changes:write
// on the REST route; whether source is target or a fork of it is Open's.
// Open asks the same between its body checks and its workspace checks.
func (c *Component) Authorize(ctx context.Context, caller Caller, target, source string) error {
	if err := c.authorizeBinding(ctx, caller, target, source); err != nil {
		return err
	}
	return c.authorizeAccess(ctx, caller, target, source, false)
}

// accessCaller is the caller as the access rule takes it.
func accessCaller(caller Caller) access.Caller {
	ac := access.Caller{Admin: caller.Credential.Type == CredentialAdminToken, UserID: caller.UserID, Workspace: caller.Bound}
	if caller.Bound != "" {
		ac.TokenID = caller.Credential.ID
	}
	return ac
}

// authorizeBinding confines a bound PAT's open to the sources its binding
// reaches.
func (c *Component) authorizeBinding(ctx context.Context, caller Caller, target, source string) error {
	if caller.Bound == "" {
		return nil
	}
	if source == "" {
		source = target
	}
	ac := accessCaller(caller)
	rel, err := c.deps.Access.RelationOf(ctx, ac, target)
	if err != nil {
		return err
	}
	switch rel {
	case access.RelationParentOfBoundFork:
		if source != caller.Bound {
			return server.TokenBound(fmt.Sprintf("source.workspace_slug: a token bound to %s opens changes in %s only from %s", caller.Bound, target, caller.Bound))
		}
		return nil
	case access.RelationBound, access.RelationCreatedFork:
		if source == target {
			return nil
		}
		srel, err := c.deps.Access.RelationOf(ctx, ac, source)
		if err != nil {
			return err
		}
		if srel != access.RelationBound && srel != access.RelationCreatedFork {
			return sourceNotRelated(source, target)
		}
		return nil
	}
	return server.NotFound(fmt.Sprintf("no workspace %q", target))
}

// authorizeAccess asks the access rule whether the caller reads target and,
// unless granted says the operation opening the change decided that the
// caller may open it from source, writes source.
func (c *Component) authorizeAccess(ctx context.Context, caller Caller, target, source string, granted bool) error {
	if caller.Credential.Type == CredentialAdminToken || caller.Credential.Type == "" {
		return nil
	}
	if source == "" {
		source = target
	}
	user := access.Caller{UserID: caller.UserID}
	level, _, _, err := c.deps.Access.Level(ctx, user, target)
	if err != nil {
		return err
	}
	if level == access.LevelNone {
		return server.NotFound(fmt.Sprintf("no workspace %q", target))
	}
	if granted {
		return nil
	}
	writes, err := c.deps.Access.CanWrite(ctx, caller.UserID, source)
	if err != nil || writes {
		return err
	}
	if source != target {
		if level, _, _, err = c.deps.Access.Level(ctx, user, source); err != nil {
			return err
		}
	}
	if level == access.LevelNone {
		return sourceNotRelated(source, target)
	}
	return server.Forbidden(fmt.Sprintf("source.workspace_slug: you can read %s but not write it", source))
}

package land

import (
	"context"
	"fmt"
	"net/http"
	"slices"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/server"
)

// authorize admits caller to land the change ch on the workspace ws: the
// admin token, a user who writes the workspace, which is its owner, and,
// while the workspace's land.by_author is on, the change's author with
// any credential of theirs, so that an org member lands the change they
// opened from their fork into a workspace they only read. Every other
// caller, who reads the workspace since the route's shared checks passed,
// is answered 403 forbidden.
func (c *Component) authorize(ctx context.Context, caller change.Caller, ch change.Change, ws Workspace) error {
	if caller.Credential.Type == change.CredentialAdminToken {
		return nil
	}
	if caller.UserID != "" {
		writes, err := c.deps.Access.CanWrite(ctx, caller.UserID, ws.Slug)
		if err != nil || writes {
			return err
		}
		if ws.ByAuthor && ch.Author.ActorID != nil && *ch.Author.ActorID == caller.UserID {
			return nil
		}
	}
	return server.Forbidden(fmt.Sprintf("%s may be landed only by a writer of %s, the admin token or, while land.by_author is on, its author",
		ch.Ref, ws.Slug))
}

// confinedTo is the fork a request's token is bound to when it calls on
// the fork's parent, where it lands and reads only the changes whose source
// is the fork; false for every other caller.
func confinedTo(r *http.Request) (string, bool) {
	b, ok := access.BindingOf(r)
	if !ok || b.Relation != access.RelationParentOfBoundFork {
		return "", false
	}
	return b.Workspace, true
}

// confine answers 403 token_bound to a token bound to a fork, calling on
// the fork's parent, for changes of which one has another source: a land
// or the attempts of a change it may not land. Unlike the change routes,
// which answer such a change 404, the land routes tell the token why it
// is refused; a number or id that names nothing keeps its 404.
func confine(r *http.Request, changes ...change.Change) error {
	fork, ok := confinedTo(r)
	if !ok {
		return nil
	}
	for _, ch := range changes {
		if ch.Source.WorkspaceSlug != fork {
			return errBound(fork, ch.WorkspaceSlug)
		}
	}
	return nil
}

// confineAttempt answers 403 token_bound to a token bound to a fork,
// calling on the fork's parent, for an attempt none of whose changes comes
// from the fork.
func confineAttempt(r *http.Request, a Attempt) error {
	fork, ok := confinedTo(r)
	if !ok || slices.ContainsFunc(a.Covered, func(c CoveredChange) bool { return c.SourceWorkspace == fork }) {
		return nil
	}
	return errBound(fork, a.Workspace)
}

// errBound is the refusal of a token bound to fork on its parent.
func errBound(fork, parent string) error {
	return server.TokenBound(fmt.Sprintf("a token bound to %s lands and reads in %s only the changes from %s", fork, parent, fork))
}

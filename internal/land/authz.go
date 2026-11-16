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

// authorize admits caller to land chain, the changes a land covers,
// bottom first, the named change last, on the workspace ws: the admin
// token, a user who writes the workspace, which is its owner, and, while
// the workspace's land.by_author is on, the author of every change of
// chain with any credential of theirs, so that an org member lands the
// changes they opened from their fork into a workspace they only read,
// and no change of another author with them. Every other caller, who
// reads the workspace since the route's shared checks passed, is answered
// 403 forbidden.
func (c *Component) authorize(ctx context.Context, caller change.Caller, chain []change.Change, ws Workspace) error {
	may, err := mayLand(ctx, c.deps.Access, caller, chain, ws)
	if err != nil || may {
		return err
	}
	ch := chain[len(chain)-1]
	if len(chain) > 1 {
		return server.Forbidden(fmt.Sprintf("the stack of %s may be landed only by a writer of %s, the admin token or, "+
			"while land.by_author is on, the author of each of its changes", ch.Ref, ws.Slug))
	}
	return server.Forbidden(fmt.Sprintf("%s may be landed only by a writer of %s, the admin token or, while land.by_author is on, its author",
		ch.Ref, ws.Slug))
}

// mayLand reports whether caller may land chain on the workspace ws by the
// rule authorize gives, asking rule who writes the workspace. The
// automatic lander asks it about the setter of a flag.
func mayLand(ctx context.Context, rule AccessRule, caller change.Caller, chain []change.Change, ws Workspace) (bool, error) {
	if caller.Credential.Type == change.CredentialAdminToken {
		return true, nil
	}
	if caller.UserID == "" {
		return false, nil
	}
	writes, err := rule.CanWrite(ctx, caller.UserID, ws.Slug)
	if err != nil || writes {
		return writes, err
	}
	return ws.ByAuthor && !slices.ContainsFunc(chain, func(cov change.Change) bool {
		return cov.Author.ActorID == nil || *cov.Author.ActorID != caller.UserID
	}), nil
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

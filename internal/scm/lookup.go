package scm

import (
	"context"

	"github.com/agent-fox-dev/parley/internal/workspace"
)

// ProtectedLookup says which branches of a workspace are protected. The hub
// answers with the push rules' protected-branch test over the workspace's
// effective protected_branches, so that the reads show what a push meets;
// this package matches no branch glob itself.
type ProtectedLookup interface {
	// ProtectedBranches is the test of the workspace w: whether a branch,
	// named without refs/heads/, is protected.
	ProtectedBranches(ctx context.Context, w workspace.Workspace) (func(branch string) bool, error)
}

// Change names a change: the workspace it belongs to and its number.
type Change struct {
	WorkspaceSlug string `json:"workspace_slug"`
	Number        int    `json:"number"`
}

// ChangeLookup names the change a branch or a commit of a workspace
// belongs to; nil with a nil error is none.
type ChangeLookup interface {
	// OpenChangeOfBranch is the open or draft change whose source is the
	// branch, named without refs/heads/, of the workspace slug.
	OpenChangeOfBranch(ctx context.Context, slug, branch string) (*Change, error)
	// ChangeOfCommit is the change a version of which holds the commit
	// sha of the workspace slug.
	ChangeOfCommit(ctx context.Context, slug, sha string) (*Change, error)
}

// SessionLookup names the agent session that introduced a commit of a
// workspace; nil with a nil error is none.
type SessionLookup interface {
	SessionOf(ctx context.Context, slug, sha string) (*string, error)
}

// defaultBranchOnly protects a workspace's default branch alone.
type defaultBranchOnly struct{}

func (defaultBranchOnly) ProtectedBranches(_ context.Context, w workspace.Workspace) (func(string) bool, error) {
	return func(branch string) bool { return w.DefaultBranch != nil && branch == *w.DefaultBranch }, nil
}

// noChanges names no change: no package records changes yet.
type noChanges struct{}

func (noChanges) OpenChangeOfBranch(context.Context, string, string) (*Change, error) {
	return nil, nil
}

func (noChanges) ChangeOfCommit(context.Context, string, string) (*Change, error) { return nil, nil }

// noSessions names no session.
type noSessions struct{}

func (noSessions) SessionOf(context.Context, string, string) (*string, error) { return nil, nil }

package pushrule

import (
	"context"
	"fmt"
	"strings"

	"github.com/agent-fox-dev/parley/internal/access"
)

// The namespaces the rules tell apart.
const (
	hubOwnedPrefix = "refs/parley/"
	branchPrefix   = "refs/heads/"
)

// The messages of the rules; %s stands for the branch, the ref name without
// refs/heads/.
const (
	msgHubOwnedRef    = "refs/parley/ is maintained by the hub and cannot be pushed to"
	msgDefaultBranch  = "%s is the default branch and cannot be deleted"
	msgTrackingBranch = "%s tracks the parent workspace and is read-only in this fork; push a new branch"
	msgInternal       = "internal error; try again"
)

// refusal is a rule's answer that refuses an update.
type refusal struct {
	message, errorType, path string
}

// internal is the answer for an update a rule could not be evaluated for.
var internal = &refusal{message: msgInternal, errorType: ErrorTypeInternal}

// decision is the refused decision about u.
func (r *refusal) decision(u Update) Decision {
	return Decision{Update: u, Message: r.message, ErrorType: r.errorType, Path: r.path}
}

// rule refuses u, or returns nil to leave it to the rules after it.
type rule func(ctx context.Context, ev *evaluation, u Update) *refusal

// order is the rules each update is evaluated by, in order: hub-owned
// refs, protected branches, default branch deletion, tracking branches,
// carry-patch push control. The last rule, path-scoped tokens, follows them
// for the request as a whole (pathScoped), since a commit may come with any
// of its updates.
var order = [...]rule{
	hubOwnedRef,
	protectedBranch,
	defaultBranchDeletion,
	trackingBranch,
	carryPatch,
}

// hubOwnedRef refuses every write under refs/parley/, whoever the caller:
// the hub keeps changes, replaced patches and sandboxes there.
func hubOwnedRef(_ context.Context, _ *evaluation, u Update) *refusal {
	if !strings.HasPrefix(u.Ref, hubOwnedPrefix) {
		return nil
	}
	return &refusal{message: msgHubOwnedRef, errorType: ErrorTypeHubOwnedRef}
}

// defaultBranchDeletion refuses the delete of the default branch, which
// every clone checks out, even where no protected_branches glob covers it.
func defaultBranchDeletion(_ context.Context, ev *evaluation, u Update) *refusal {
	branch, ok := branchOf(u.Ref)
	if !ok || u.After != ZeroID || branch != ev.req.Workspace.DefaultBranch {
		return nil
	}
	return &refusal{message: fmt.Sprintf(msgDefaultBranch, branch), errorType: ErrorTypeDefaultBranch}
}

// trackingBranch refuses every write to a branch a fork tracks from its
// parent, which always equals the parent's branch.
func trackingBranch(ctx context.Context, ev *evaluation, u Update) *refusal {
	branch, ok := branchOf(u.Ref)
	if !ok || ev.req.Workspace.Kind != access.KindFork || ev.deps.Tracking == nil {
		return nil
	}
	tracked, err := ev.deps.Tracking.IsTracking(ctx, ev.req.Workspace.Slug, branch)
	switch {
	case err != nil:
		return internal
	case !tracked:
		return nil
	}
	return &refusal{message: fmt.Sprintf(msgTrackingBranch, branch), errorType: ErrorTypeTrackingBranch}
}

// branchOf is the branch ref names, without refs/heads/; ok is false for a
// ref outside refs/heads/, which the branch rules leave alone.
func branchOf(ref string) (branch string, ok bool) {
	branch, ok = strings.CutPrefix(ref, branchPrefix)
	return branch, ok && branch != ""
}

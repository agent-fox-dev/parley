package pushrule

import (
	"context"
	"fmt"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// The messages of the protected-branch rule; %s stands for the branch.
const (
	msgProtectedRewrite = "%s is protected and cannot be force-pushed or deleted"
	msgProtectedChange  = "%s is protected; push another branch and open a change (git push -o parley.change)"
)

// Protected reports whether the protected_branches globs protect branch, a
// branch name without refs/heads/: whether at least one glob matches all of
// it. An empty list protects nothing. Globs are matched by the workspace
// package's one branch-glob matcher, so that settings validation, the push
// rules, the source control API and landing agree on what a glob protects.
func Protected(branch string, globs []string) bool {
	for _, g := range globs {
		if workspace.MatchBranchGlob(g, branch) {
			return true
		}
	}
	return false
}

// defaultSettings are a workspace's push settings when the engine has no
// settings reader: the defaults of the settings themselves, which protect
// the default branch and allow direct pushes.
func defaultSettings(ws Workspace) Settings {
	s := Settings{ProtectedBranches: []string{}, OwnerPush: true}
	if ws.DefaultBranch != "" {
		s.ProtectedBranches = append(s.ProtectedBranches, ws.DefaultBranch)
	}
	return s
}

// protectedBranch holds a protected branch to what may move it outside a
// land or a sync: nobody rewrites or deletes it, nobody creates it by push,
// and only the owner exemption fast-forwards it, as a direct push.
func protectedBranch(ctx context.Context, ev *evaluation, u Update) *refusal {
	branch, ok := branchOf(u.Ref)
	if !ok {
		return nil
	}
	s, err := ev.pushSettings(ctx)
	if err != nil {
		return internal
	}
	if !Protected(branch, s.ProtectedBranches) {
		return nil
	}
	rewrite := &refusal{message: fmt.Sprintf(msgProtectedRewrite, branch), errorType: ErrorTypeProtectedBranch}
	viaChange := &refusal{message: fmt.Sprintf(msgProtectedChange, branch), errorType: ErrorTypeProtectedBranch}
	switch {
	case u.After == ZeroID:
		return rewrite
	case u.Before == ZeroID:
		return viaChange
	}
	forced, err := ev.forced(ctx, u)
	switch {
	case err != nil:
		return internal
	case forced:
		return rewrite
	case !s.OwnerPush || !exempt(ev.req.Caller):
		return viaChange
	}
	ev.cur.direct = true
	return nil
}

// exempt reports whether caller may fast-forward a protected branch
// directly while protect.owner_push allows it: the workspace's owner or the
// admin token, with a credential that is not confined to a workspace or to
// paths, since such a token is meant to reach the branch through a change.
func exempt(caller Caller) bool {
	if caller.Level != access.LevelOwner && caller.Level != access.LevelAdmin {
		return false
	}
	switch caller.CredentialType {
	case CredentialAPIKey, CredentialAdminToken:
		return true
	case CredentialPAT:
		return !caller.Bound && caller.Paths == nil
	}
	return false
}

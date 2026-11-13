package identity

import (
	"fmt"
	"slices"

	"github.com/agent-fox-dev/parley/internal/server"
)

// CheckSubset holds a PAT caller to the subset rule when it asks for a
// token with the permissions and the paths given: a PAT can never create a
// token beyond itself, or a leaked token could mint a stronger one and its
// scope limits would mean nothing. Every permission must be one the caller
// holds itself, compared exactly, without the implication the scope check
// applies (a PAT with git:write cannot mint one with git:read). A caller
// with path limits must ask for paths, nil being none asked for, and each
// requested glob must be covered by one of the caller's globs; a caller
// without path limits may ask for any paths or none.
//
// A refusal is 403 insufficient_scope naming the first permission, in
// request order, the caller does not hold, and only when every permission
// is held, the first glob none of its globs covers, or that paths is
// required. Other callers (API keys, the admin token) are not held to the
// rule, and nil is returned for them. The values are expected to be
// validated already: an invalid request is answered as invalid, not here.
func CheckSubset(caller Principal, permissions, paths []string) error {
	if err := checkPermissionSubset(caller, permissions); err != nil {
		return err
	}
	return checkPathSubset(caller, paths,
		"paths is required: this token is limited to paths, so the token it creates must be too")
}

// CheckWidenSubset holds a PAT caller to the subset rule when it adds the
// scopes added to an existing token whose paths are targetPaths, nil being
// none: the change cannot touch the token's paths, so the token it leaves
// is within the caller only if the caller holds every added scope exactly
// and its globs cover the token's paths. Otherwise a PAT limited to paths
// could give a scope it holds to a token without that limit, which would
// then use the scope anywhere. A change that adds nothing, such as one that
// removes scopes, never widens a token and is never refused.
//
// A refusal is 403 insufficient_scope naming the first added scope, in
// request order, the caller does not hold, and only when every one is
// held, the first of the token's globs none of the caller's globs covers,
// or that the token has no path limit. Other callers are not held to it.
func CheckWidenSubset(caller Principal, added, targetPaths []string) error {
	if len(added) == 0 {
		return nil
	}
	if err := checkPermissionSubset(caller, added); err != nil {
		return err
	}
	return checkPathSubset(caller, targetPaths,
		"paths: this token is limited to paths, so it cannot add scopes to a token that is not")
}

// checkPermissionSubset refuses the first permission, in request order,
// that a PAT caller does not hold exactly.
func checkPermissionSubset(caller Principal, permissions []string) error {
	if caller.Type != TypePAT {
		return nil
	}
	for _, p := range permissions {
		if !slices.Contains(caller.Permissions, p) {
			return server.InsufficientScope(fmt.Sprintf("permissions: this token does not hold %q", p))
		}
	}
	return nil
}

// checkPathSubset refuses, for a PAT caller with path limits, paths that
// are nil, with the message unlimited, or the first glob of paths that none
// of the caller's globs covers.
func checkPathSubset(caller Principal, paths []string, unlimited string) error {
	if caller.Type != TypePAT || caller.Paths == nil {
		return nil
	}
	if paths == nil {
		return server.InsufficientScope(unlimited)
	}
	if g, ok := FirstUncovered(caller.Paths, paths); ok {
		return server.InsufficientScope(fmt.Sprintf("paths: %q is not covered by a glob of this token", g))
	}
	return nil
}

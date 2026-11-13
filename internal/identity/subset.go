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
	if err := CheckPermissionSubset(caller, permissions); err != nil {
		return err
	}
	if caller.Type != TypePAT || caller.Paths == nil {
		return nil
	}
	if paths == nil {
		return server.InsufficientScope("paths is required: this token is limited to paths, so the token it creates must be too")
	}
	if g, ok := FirstUncovered(caller.Paths, paths); ok {
		return server.InsufficientScope(fmt.Sprintf("paths: %q is not covered by a glob of this token", g))
	}
	return nil
}

// CheckPermissionSubset is the permission half of CheckSubset, for a
// request that asks for permissions and no paths, such as adding scopes to
// a token, whose paths do not change: the first permission, in request
// order, that a PAT caller does not hold exactly is refused with 403
// insufficient_scope. Other callers are not held to it.
func CheckPermissionSubset(caller Principal, permissions []string) error {
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

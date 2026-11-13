package identity

import (
	"net/http"
	"slices"
	"strings"

	"github.com/agent-fox-dev/parley/internal/server"
)

// registeredScopes are the scopes a PAT can carry, in the order the API
// documents them. The list is fixed: a route of any package declares one of
// these, and a token is minted with some of them.
var registeredScopes = []string{
	"users:read", "users:write",
	"orgs:read",
	"keys:read", "keys:manage",
	"tokens:read", "tokens:write", "tokens:manage",
	"workspaces:read", "workspaces:write",
	"git:read", "git:write",
	"changes:read", "changes:write", "changes:approve",
	"checks:write",
	"patches:read", "patches:write",
	"rebuilds:read", "rebuilds:write",
	"secrets:read", "secrets:write",
	"vars:read", "vars:write",
	"forge:read", "forge:write",
	"sandboxes:read", "sandboxes:write",
	"audit:read", "audit:write",
	"sessions:read", "sessions:write",
}

// impliedBy maps a scope to the other scopes that satisfy a check for it:
// R:write satisfies R:read wherever both are registered, keys:manage
// satisfies keys:read, and tokens:manage satisfies tokens:write and
// tokens:read. Nothing else implies anything, so changes:approve and
// checks:write are granted only by name: approving a change or reporting a
// check is never a side effect of holding a broader write scope.
var impliedBy = func() map[string][]string {
	m := map[string][]string{
		"keys:read":    {"keys:manage"},
		"tokens:read":  {"tokens:manage"},
		"tokens:write": {"tokens:manage"},
	}
	for _, s := range registeredScopes {
		if area, ok := strings.CutSuffix(s, ":read"); ok && slices.Contains(registeredScopes, area+":write") {
			m[s] = append(m[s], area+":write")
		}
	}
	return m
}()

// IsRegisteredScope reports whether s is a registered scope. The names are
// case-sensitive and taken as they are, without trimming.
func IsRegisteredScope(s string) bool { return slices.Contains(registeredScopes, s) }

// RegisteredScopes returns every registered scope in the documented order.
// The list is a copy the caller may change.
func RegisteredScopes() []string { return slices.Clone(registeredScopes) }

// Satisfies reports whether a token holding the scopes held passes a check
// for needed: it holds needed itself or a scope that implies it.
func Satisfies(held []string, needed string) bool {
	if slices.Contains(held, needed) {
		return true
	}
	for _, s := range impliedBy[needed] {
		if slices.Contains(held, s) {
			return true
		}
	}
	return false
}

// ScopeCheck is the pipeline's scope check. It holds a PAT to the one scope
// its route declared at registration (server.Route.Scope) and answers 403
// insufficient_scope, naming that scope, when the token's permissions do not
// satisfy it. The admin token and API keys carry no scopes and pass, as does
// any caller on a route that declares none. It runs before the access checks
// and the handler, so a token without the scope learns nothing about the
// resource it addressed, not even whether it exists.
func ScopeCheck(r *http.Request) error {
	m, ok := server.MatchedRoute(r)
	if !ok || m.Route.Scope == "" {
		return nil
	}
	p, ok := PrincipalOf(r.Context())
	if !ok || p.Type != TypePAT || Satisfies(p.Permissions, m.Route.Scope) {
		return nil
	}
	return server.InsufficientScope("this route needs the scope " + m.Route.Scope)
}

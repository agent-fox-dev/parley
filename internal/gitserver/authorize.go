package gitserver

import (
	"context"
	"net/http"
	"strings"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
)

// The scopes a PAT needs on git: git:write for a push, git:read for
// everything else. The scope registry lets git:write satisfy git:read.
const (
	scopeRead  = "git:read"
	scopeWrite = "git:write"
)

// Guard is the binding guard on git: *access.Service is one. It passes
// every caller but a bound PAT, and a bound PAT on what its binding covers,
// and reports its own refusals.
type Guard interface {
	GuardGit(ctx context.Context, c server.Caller, slug, method, pattern string) (access.GitDecision, error)
}

// needOf is what a request on route for service needs, decided from the
// route alone, before anything is looked up: a push, and the ref discovery
// of one, needs write; everything else, an unknown service included, needs
// read.
func needOf(route Route, service string) access.Need {
	if route == RouteReceivePack || (route == RouteInfoRefs && service == ServiceReceivePack) {
		return access.NeedWrite
	}
	return access.NeedRead
}

// slugOf is the slug a request names as the caller typed it: the
// repository segment without its .git suffix. A segment without the suffix
// is taken as it is, and resolve answers it as a repository that does not
// exist.
func slugOf(r *http.Request) string {
	return strings.TrimSuffix(r.PathValue("repo"), ".git")
}

// bound reports whether p is a PAT bound to a workspace.
func bound(p identity.Principal) bool {
	return p.Type == identity.TypePAT && p.Workspace != nil
}

// guard asks the binding guard about a bound PAT's request, before the
// scope check, so that a bound PAT outside its binding is answered as for a
// repository that does not exist, whatever its scopes. It keeps the
// binding of a pass for the push. Without a guard every bound PAT is
// refused, since nothing could say what its binding covers.
func (c *Component) guard(w http.ResponseWriter, r *http.Request, x *Exchange) bool {
	if !bound(x.Principal) {
		return true
	}
	if c.deps.Guard == nil {
		refuse(w, http.StatusNotFound, msgNotFound)
		return false
	}
	d, err := c.deps.Guard.GuardGit(r.Context(), x.Caller, slugOf(r), r.Method, r.Pattern)
	if err != nil {
		c.fail(w, r, err)
		return false
	}
	if !d.Pass {
		refuse(w, http.StatusNotFound, msgNotFound)
		return false
	}
	x.Binding = d.Binding
	return true
}

// scope refuses a PAT that lacks the scope the request needs, before the
// repository is resolved, so that the answer is the same whether or not the
// repository exists. An API key and the admin token carry no scopes and
// skip it.
func (c *Component) scope(w http.ResponseWriter, _ *http.Request, x *Exchange) bool {
	if x.Principal.Type != identity.TypePAT {
		return true
	}
	needed := scopeRead
	if x.Need == access.NeedWrite {
		needed = scopeWrite
	}
	if identity.Satisfies(x.Principal.Permissions, needed) {
		return true
	}
	refuse(w, http.StatusForbidden, msgLacksScope+needed)
	return false
}

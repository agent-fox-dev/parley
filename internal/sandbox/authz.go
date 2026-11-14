package sandbox

import (
	"errors"
	"maps"
	"net/http"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime"
	"github.com/agent-fox-dev/parley/internal/scrub"
	"github.com/agent-fox-dev/parley/internal/server"
)

// The error types of the sandbox feature, outside the server's catalogue.
const (
	// errorTypeDisabled answers every sandbox route of a hub without a
	// [sandbox] section, with 404.
	errorTypeDisabled = "sandboxes_disabled"
	// errorTypeRuntime answers a request the runtime failed, with 502.
	errorTypeRuntime = "sandbox_runtime_error"
)

// The messages of the feature's own refusals.
const (
	msgDisabled  = "sandboxes are not enabled on this hub"
	msgNeedsUser = "the admin token cannot create a sandbox"
)

// The scopes a PAT needs on a workspace sandbox route of each kind.
const (
	scopeRead  = "sandboxes:read"
	scopeWrite = "sandboxes:write"
)

// Kind is what a workspace sandbox route declares it does to the
// workspace's sandboxes, and so which scope a PAT needs and which access to
// the workspace the caller needs.
//
// The routes of the sandbox packages take these kinds: KindRead for
// reading, that is getting a sandbox, listing a workspace's sandboxes,
// reading its logs, downloading a file and listing its policy proposals;
// KindWrite for everything else, creating, exec, attach, uploading,
// stopping, starting, deleting, checkpoints and approving or rejecting a
// proposal included.
type Kind string

const (
	// KindRead needs the scope sandboxes:read of a PAT and read access to
	// the workspace.
	KindRead Kind = "read"
	// KindWrite needs the scope sandboxes:write of a PAT and write access
	// to the workspace.
	KindWrite Kind = "write"
)

// RouteOption adds a rule to a workspace sandbox route.
type RouteOption func(*routeOptions)

type routeOptions struct{ needsUser bool }

// NeedsUser declares a route that acts for a user, such as creating a
// sandbox, whose hub credential is minted for that user: the admin token
// is answered 403 forbidden, after workspace access is decided and before
// the handler reads the body.
func NeedsUser() RouteOption { return func(o *routeOptions) { o.needsUser = true } }

// disabled is the answer of every sandbox route while sandboxes are off.
func disabled() *server.Error {
	return server.NewError(errorTypeDisabled, http.StatusNotFound, msgDisabled)
}

// RuntimeError is the answer to a request the runtime failed: 502
// sandbox_runtime_error with message. The message goes to the caller as it
// is, so the caller scrubs it first.
func RuntimeError(message string) *server.Error {
	return server.NewError(errorTypeRuntime, http.StatusBadGateway, message)
}

// SetScrubber gives the service the hub's scrubber, which knows the
// gateway's credentials, for the text of the runtime-error answer. Without
// one the answer is still cleared of URL user information.
func (s *Service) SetScrubber(sc *scrub.Scrubber) { s.scrubber = sc }

// Route declares a sandbox route of the whole hub, one that names no
// workspace, such as the profile list: it gets the feature check and the
// runtime-error answer, and keeps the scope and marks rt gives.
//
// Routes are declared while the hub composes, before the configuration is
// loaded, so the feature check reads whether sandboxes are on when a request
// comes.
func (s *Service) Route(rt server.Route) server.Route {
	rt.Feature = s.checkFeature
	rt.Handler = s.answerRuntimeErrors(rt.Handler)
	return rt
}

// WorkspaceRoute declares a sandbox route of a workspace, whose pattern
// names it with :slug, as one of kind: on top of what Route adds, it sets
// the scope a PAT needs and the workspace need of kind, replacing those rt
// gives. The pipeline then runs, in order, the feature check, the binding
// guard, the scope check and workspace access before the handler; the admin
// token passes as an owner of every workspace unless opts say the route
// needs a user.
func (s *Service) WorkspaceRoute(kind Kind, rt server.Route, opts ...RouteOption) server.Route {
	var o routeOptions
	for _, opt := range opts {
		opt(&o)
	}
	scope, need := scopeRead, access.NeedRead
	if kind == KindWrite {
		scope, need = scopeWrite, access.NeedWrite
	}
	rt.Scope = scope
	// A copy, so that the caller's map is not changed.
	rt.Marks = maps.Clone(rt.Marks)
	if rt.Marks == nil {
		rt.Marks = make(map[string]string, 1)
	}
	rt.Marks[access.MarkNeed] = string(need)
	if o.needsUser {
		rt.Handler = refuseAdmin(rt.Handler)
	}
	return s.Route(rt)
}

// checkFeature is the feature check of every sandbox route.
func (s *Service) checkFeature(*http.Request) error {
	if !s.Enabled() {
		return disabled()
	}
	return nil
}

// refuseAdmin answers the admin token before next runs. As the handler, it
// runs after every check of the pipeline, workspace access included.
func refuseAdmin(next server.HandlerFunc) server.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		if c, ok := server.CallerOf(r.Context()); ok && c.Admin {
			return server.Forbidden(msgNeedsUser)
		}
		return next(w, r)
	}
}

// answerRuntimeErrors turns a runtime error of kind unavailable or
// unsupported that next returns into the runtime-error answer, with the
// error's text scrubbed. Every other error is left to the pipeline, which
// answers one that is not a *server.Error with 500 internal.
func (s *Service) answerRuntimeErrors(next server.HandlerFunc) server.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		err := next(w, r)
		var answer *server.Error
		if err == nil || errors.As(err, &answer) {
			return err
		}
		if errors.Is(err, runtime.ErrUnavailable) || errors.Is(err, runtime.ErrUnsupported) {
			return RuntimeError(s.scrub(err.Error()))
		}
		return err
	}
}

// scrub clears text of the secrets the hub's scrubber knows and of URL
// user information.
func (s *Service) scrub(text string) string {
	if s.scrubber == nil {
		return scrub.New().Scrub(text)
	}
	return s.scrubber.Scrub(text)
}

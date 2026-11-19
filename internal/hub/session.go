package hub

import (
	"context"
	"slices"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/session"
	"github.com/agent-fox-dev/parley/internal/store"
)

// sessionHook is the name of the start hook that binds the session
// component.
const sessionHook = "sessions"

// composeSessions adds the agent sessions: the schema of the sessions,
// their usage records and their links, the session routes, and the
// component, bound in a start hook to the store, the hub's clock, the access
// rule, which decides a caller's access to a session's workspace, the
// minimal workspace records, read from the store, and the coverage of a
// bound token, answered by the access rule's covered set.
//
// It is composed last, so that the components it registers with all exist.
//
// The component exists from the composition on (Sessions); tests change its
// dependencies with AdjustSessions.
func composeSessions(c *Composition) {
	comp := &session.Component{}
	c.sessions = comp
	c.AddSchemaStep(session.SchemaStep)
	for _, r := range comp.Routes() {
		c.AddRoute(r)
	}
	c.AddStartHook(sessionHook, func(_ context.Context, rt *Runtime) error {
		d := session.Deps{
			Store:      rt.Store,
			Now:        rt.Now,
			Logger:     rt.Logger,
			Access:     c.access,
			Workspaces: sessionWorkspaces{rt.Store},
			Coverage:   sessionCoverage{c.access},
		}
		for _, adjust := range c.sessionAdjusters {
			adjust(&d)
		}
		comp.Bind(d)
		return nil
	})
}

// sessionWorkspaces is the session component's workspace reader: the
// minimal workspace record of the access rule, read from the store.
type sessionWorkspaces struct{ store *store.Store }

func (r sessionWorkspaces) Workspace(ctx context.Context, slug string) (session.Workspace, bool, error) {
	rec, found, err := access.RecordBySlug(ctx, r.store.Main(), slug)
	if err != nil || !found {
		return session.Workspace{}, false, err
	}
	return session.Workspace{Slug: rec.Slug, Kind: rec.Kind, ParentSlug: rec.ParentSlug, Status: rec.Status}, true, nil
}

// sessionCoverage is the session component's coverage question: a caller
// is bound when its PAT is bound to a workspace, and the binding covers the
// workspaces of the access rule's covered set.
type sessionCoverage struct{ access *access.Service }

func (s sessionCoverage) Covers(ctx context.Context, c access.Caller, slug string) (bool, bool, error) {
	if c.Workspace == "" {
		return false, false, nil
	}
	covered, err := s.access.CoveredSet(ctx, c)
	if err != nil {
		return true, false, err
	}
	return true, slices.Contains(covered, slug), nil
}

// Sessions returns the hub's session component, which serves once its
// start hook has run.
func (c *Composition) Sessions() *session.Component { return c.sessions }

// AdjustSessions adds fn to the functions that change the session
// component's dependencies after the start hook has built them and before
// it binds them, in the order they were added.
func (c *Composition) AdjustSessions(fn func(*session.Deps)) {
	c.sessionAdjusters = append(c.sessionAdjusters, fn)
}

package hub

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/awareness"
	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/fork"
	"github.com/agent-fox-dev/parley/internal/gitserver"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/sandbox"
	"github.com/agent-fox-dev/parley/internal/scm"
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
// bound token, answered by the access rule's covered set. It registers the
// session-linking rule (Check) and the session record (Record) with every
// part that takes a session id: the fork create, the push options stage,
// the source control API, the change feature, the landing, the awareness
// feature and the sandbox create (registerSessionLinkers).
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
	registerSessionLinkers(c, comp)
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

// registerSessionLinkers registers the session-linking rule and the session
// record of comp with every consumer of them, each through an adapter
// written to the interface that consumer declares: the shapes differ (the
// caller's type, a check alone or a check and a record, the key of the
// link), so no adapter is shared. An adapter calls comp when it is asked,
// so it serves once the session start hook has bound comp.
func registerSessionLinkers(c *Composition, comp *session.Component) {
	c.forks.RegisterSessionLinker(forkSessions{comp})
	c.pushOptions.RegisterSessionLinker(pushSessions{comp})
	c.scm.RegisterSessionLinker(scmSessions{comp})
	c.changes.RegisterSessionLinker(changeSessions{comp})
	c.land.RegisterSessionLinker(landSessions{comp})
	c.awareness.RegisterSessions(awarenessSessions{comp})
	c.sandbox.RegisterSessionLinker(sandboxSessions{comp})
}

// checkSession asks the session-linking rule of sessions whether caller may
// name sessionID for an operation on the workspace slug, and answers its
// refusal as refusal, the consumer's own unknown-session error, and any
// other error as a failure that does not match refusal.
func checkSession(ctx context.Context, sessions *session.Component, caller identity.Principal, sessionID, slug string, refusal error) (string, error) {
	id, err := sessions.Check(ctx, caller, sessionID, slug)
	switch {
	case errors.Is(err, session.ErrUnknownSession):
		return "", refusal
	case err != nil:
		return "", fmt.Errorf("the session-linking rule cannot tell: %w", err)
	}
	return id, nil
}

// principalOf is the caller as the session-linking rule takes it: the admin
// token when admin is true, else a credential of the user userID ("" for
// none, which names no session).
func principalOf(admin bool, userID string) identity.Principal {
	if admin {
		return identity.Principal{Type: identity.TypeAdmin}
	}
	p := identity.Principal{}
	if userID != "" {
		p.User = &identity.User{ID: userID}
	}
	return p
}

// forkSessions is the fork create's session linker: the fork is linked to
// the session as a fork link of the parent, keyed by the fork's slug.
type forkSessions struct{ sessions *session.Component }

func (l forkSessions) Check(ctx context.Context, caller identity.Principal, sessionID, parent string) (string, error) {
	return checkSession(ctx, l.sessions, caller, sessionID, parent, fork.ErrUnknownSession)
}

func (l forkSessions) Record(ctx context.Context, tx *sql.Tx, sessionID, parent, forkSlug string) error {
	return l.sessions.Record(ctx, tx, session.ForkLink(sessionID, parent, forkSlug))
}

// pushSessions is a push's session linker, which only checks: a push
// becomes part of a session through the change version recorded for it.
type pushSessions struct{ sessions *session.Component }

func (l pushSessions) Check(ctx context.Context, caller gitserver.PushCaller, sessionID, workspace string) (string, error) {
	p := principalOf(caller.CredentialType == gitserver.CredentialAdminToken, caller.UserID)
	return checkSession(ctx, l.sessions, p, sessionID, workspace, gitserver.ErrUnknownSession)
}

// scmSessions is the source control API's session linker, which only
// checks: a write becomes part of a session through the change version
// recorded for it.
type scmSessions struct{ sessions *session.Component }

func (l scmSessions) Check(ctx context.Context, caller identity.Principal, sessionID, slug string) (string, error) {
	return checkSession(ctx, l.sessions, caller, sessionID, slug, scm.ErrUnknownSession)
}

// changeSessions is the change feature's session linker: a version is
// linked to the session as a version link of its change.
type changeSessions struct{ sessions *session.Component }

func (l changeSessions) Check(ctx context.Context, caller change.Caller, sessionID, workspace string) (string, error) {
	p := principalOf(caller.Credential.Type == change.CredentialAdminToken, caller.UserID)
	return checkSession(ctx, l.sessions, p, sessionID, workspace, change.ErrUnknownSession)
}

func (l changeSessions) Record(ctx context.Context, tx *sql.Tx, sessionID, workspace string, number, version int64) error {
	return l.sessions.Record(ctx, tx, session.VersionLink(sessionID, workspace, number, version))
}

// landSessions is the landing's session linker: a land attempt is linked
// to the session as a land link. Its refusal is the change feature's, as
// the landing declares it.
type landSessions struct{ sessions *session.Component }

func (l landSessions) Check(ctx context.Context, caller change.Caller, sessionID, workspace string) (string, error) {
	p := principalOf(caller.Credential.Type == change.CredentialAdminToken, caller.UserID)
	return checkSession(ctx, l.sessions, p, sessionID, workspace, change.ErrUnknownSession)
}

func (l landSessions) Record(ctx context.Context, tx *sql.Tx, sessionID, workspace, landID string) error {
	return l.sessions.Record(ctx, tx, session.LandLink(sessionID, workspace, landID))
}

// awarenessSessions is the awareness feature's session interface: a claim
// is linked to the session as a claim link.
type awarenessSessions struct{ sessions *session.Component }

func (l awarenessSessions) Check(ctx context.Context, caller awareness.Actor, sessionID, workspace string) (string, error) {
	userID := ""
	if caller.ActorID != nil {
		userID = *caller.ActorID
	}
	p := principalOf(caller.ActorType == event.ActorAdminToken, userID)
	return checkSession(ctx, l.sessions, p, sessionID, workspace, awareness.ErrUnknownSession)
}

func (l awarenessSessions) Record(ctx context.Context, tx *sql.Tx, sessionID, workspace, claim string) error {
	return l.sessions.Record(ctx, tx, session.ClaimLink(sessionID, workspace, claim))
}

// sandboxSessions is the sandbox create's session linker: a sandbox is
// linked to the session as a sandbox link of its workspace.
type sandboxSessions struct{ sessions *session.Component }

func (l sandboxSessions) Check(ctx context.Context, caller identity.Principal, sessionID, workspace string) (string, error) {
	return checkSession(ctx, l.sessions, caller, sessionID, workspace, sandbox.ErrUnknownSession)
}

func (l sandboxSessions) Record(ctx context.Context, tx *sql.Tx, sessionID, workspace, sandboxID string) error {
	return l.sessions.Record(ctx, tx, session.SandboxLink(sessionID, workspace, sandboxID))
}

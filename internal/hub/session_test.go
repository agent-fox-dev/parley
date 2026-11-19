package hub_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/awareness"
	"github.com/agent-fox-dev/parley/internal/change"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/fork"
	"github.com/agent-fox-dev/parley/internal/gitserver"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/sandbox"
	"github.com/agent-fox-dev/parley/internal/scm"
	"github.com/agent-fox-dev/parley/internal/session"
)

// unreadableWorkspaces is a workspace reader that cannot read the record of
// the workspace unreadable and reads every other through next.
type unreadableWorkspaces struct{ next session.WorkspaceReader }

const unreadable = "unreadable"

func (r unreadableWorkspaces) Workspace(ctx context.Context, slug string) (session.Workspace, bool, error) {
	if slug == unreadable {
		return session.Workspace{}, false, errors.New("the workspace record cannot be read")
	}
	return r.next.Workspace(ctx, slug)
}

// sessionConsumer is one consumer of the session-linking rule: its check
// asked by alice, by the admin token or by bob (a user with no access), its
// refusal, and, for a consumer that records, its record and the count of
// the session object the link adds to.
type sessionConsumer struct {
	name    string
	check   func(ctx context.Context, who, sessionID, slug string) (string, error)
	refusal error
	record  func(ctx context.Context, tx *sql.Tx, sessionID, slug string) error
	count   string
}

func TestSessionLinkers_AnswerEachConsumerInItsOwnTerms(t *testing.T) {
	var linkers hub.SessionLinkers
	h := hubtest.StartWith(t, hubtest.Config{Compose: func(c *hub.Composition) {
		linkers = hub.SessionLinkersOf(c)
		c.AdjustSessions(func(d *session.Deps) { d.Workspaces = unreadableWorkspaces{d.Workspaces} })
	}})
	ctx := context.Background()
	alice, bob := h.CreateUser(t, "alice"), h.CreateUser(t, "bob")
	aliceKey, acme := h.IssueKey(t, alice), h.CreateOrg(t, "acme")
	h.AddMember(t, acme, alice)
	h.AddWorkspace(t, access.Record{Slug: "demo", OwnerID: alice, OrgID: acme, Kind: access.KindRepository, Status: access.StatusReady})
	users := map[string]string{"alice": alice, "bob": bob}

	principal := func(who string) identity.Principal {
		if who == "admin" {
			return identity.Principal{Type: identity.TypeAdmin, CredentialID: "admin"}
		}
		return identity.Principal{Type: identity.TypeAPIKey, CredentialID: "key", User: &identity.User{ID: users[who]}}
	}
	changeCaller := func(who string) change.Caller {
		if who == "admin" {
			return change.Caller{Credential: change.Credential{Type: change.CredentialAdminToken, ID: "admin"}}
		}
		return change.Caller{UserID: users[who], Credential: change.Credential{Type: identity.TypeAPIKey, ID: "key"}}
	}
	consumers := []sessionConsumer{
		{
			name: "fork",
			check: func(ctx context.Context, who, id, slug string) (string, error) {
				return linkers.Fork.Check(ctx, principal(who), id, slug)
			},
			refusal: fork.ErrUnknownSession,
			record: func(ctx context.Context, tx *sql.Tx, id, slug string) error {
				return linkers.Fork.Record(ctx, tx, id, slug, slug+"--t1")
			},
			count: "forks",
		},
		{
			name: "push",
			check: func(ctx context.Context, who, id, slug string) (string, error) {
				caller := gitserver.PushCaller{CredentialType: gitserver.CredentialAPIKey, CredentialID: "key", UserID: users[who]}
				if who == "admin" {
					caller = gitserver.PushCaller{CredentialType: gitserver.CredentialAdminToken, CredentialID: "admin"}
				}
				return linkers.Push.Check(ctx, caller, id, slug)
			},
			refusal: gitserver.ErrUnknownSession,
		},
		{
			name: "scm",
			check: func(ctx context.Context, who, id, slug string) (string, error) {
				return linkers.SCM.Check(ctx, principal(who), id, slug)
			},
			refusal: scm.ErrUnknownSession,
		},
		{
			name: "change",
			check: func(ctx context.Context, who, id, slug string) (string, error) {
				return linkers.Change.Check(ctx, changeCaller(who), id, slug)
			},
			refusal: change.ErrUnknownSession,
			record: func(ctx context.Context, tx *sql.Tx, id, slug string) error {
				return linkers.Change.Record(ctx, tx, id, slug, 7, 1)
			},
			count: "versions",
		},
		{
			name: "land",
			check: func(ctx context.Context, who, id, slug string) (string, error) {
				return linkers.Land.Check(ctx, changeCaller(who), id, slug)
			},
			refusal: change.ErrUnknownSession,
			record: func(ctx context.Context, tx *sql.Tx, id, slug string) error {
				return linkers.Land.Record(ctx, tx, id, slug, "land-1")
			},
			count: "lands",
		},
		{
			name: "awareness",
			check: func(ctx context.Context, who, id, slug string) (string, error) {
				actor := awareness.Actor{ActorType: event.ActorAPIKey, ActorID: new(users[who])}
				if who == "admin" {
					actor = awareness.Actor{ActorType: event.ActorAdminToken}
				}
				return linkers.Awareness.Check(ctx, actor, id, slug)
			},
			refusal: awareness.ErrUnknownSession,
			record: func(ctx context.Context, tx *sql.Tx, id, slug string) error {
				return linkers.Awareness.Record(ctx, tx, id, slug, "claim-1")
			},
			count: "claims",
		},
		{
			name: "sandbox",
			check: func(ctx context.Context, who, id, slug string) (string, error) {
				return linkers.Sandbox.Check(ctx, principal(who), id, slug)
			},
			refusal: sandbox.ErrUnknownSession,
			record: func(ctx context.Context, tx *sql.Tx, id, slug string) error {
				return linkers.Sandbox.Record(ctx, tx, id, slug, "sandbox-1")
			},
			count: "sandboxes",
		},
	}

	for _, c := range consumers {
		t.Run(c.name, func(t *testing.T) {
			status, raw := h.Call(t, http.MethodPost, "/api/v1/sessions", aliceKey, map[string]any{"workspace_slug": "demo"})
			if status != http.StatusCreated {
				t.Fatalf("POST /sessions = %d %s, want 201", status, raw)
			}
			var opened struct{ ID string }
			if err := json.Unmarshal(raw, &opened); err != nil {
				t.Fatal(err)
			}
			id := opened.ID

			for _, who := range []string{"alice", "admin"} {
				if got, err := c.check(ctx, who, strings.ToUpper(id), "demo"); err != nil || got != id {
					t.Errorf("check by %s = %q, %v; want %q, nil", who, got, err, id)
				}
			}
			for _, refused := range []struct{ name, who, id string }{
				{"bob", "bob", id},
				{"an id that names no session", "alice", "0badc0de-0000-4000-8000-000000000000"},
			} {
				if _, err := c.check(ctx, refused.who, refused.id, "demo"); !errors.Is(err, c.refusal) {
					t.Errorf("check of %s = %v, want the consumer's unknown-session error", refused.name, err)
				}
			}
			if _, err := c.check(ctx, "alice", id, unreadable); err == nil || errors.Is(err, c.refusal) {
				t.Errorf("check with an unreadable workspace record = %v, want a failure that is not the refusal", err)
			}

			if c.record == nil {
				return
			}
			if err := h.Store.Tx(ctx, func(tx *sql.Tx) error { return c.record(ctx, tx, id, "demo") }); err != nil {
				t.Fatalf("record = %v, want nil", err)
			}
			status, raw = h.Call(t, http.MethodGet, "/api/v1/sessions/"+id, h.Token, nil)
			if status != http.StatusOK {
				t.Fatalf("GET /sessions/%s = %d %s, want 200", id, status, raw)
			}
			var s struct{ Counts map[string]int }
			if err := json.Unmarshal(raw, &s); err != nil {
				t.Fatal(err)
			}
			if len(s.Counts) != 8 {
				t.Fatalf("counts = %v, want the eight counts", s.Counts)
			}
			for kind, n := range s.Counts {
				want := 0
				if kind == c.count || c.count == "versions" && kind == "changes" {
					want = 1
				}
				if n != want {
					t.Errorf("counts.%s = %d after the record, want %d", kind, n, want)
				}
			}
		})
	}
}

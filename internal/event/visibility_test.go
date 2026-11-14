package event

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
)

// callerNames are the callers the tables decide for, in the order of their
// want lists: the admin token and the users of the world.
var callerNames = []string{"admin", "dev", "mem", "out"}

// caller is the caller named name: "admin" is the admin token.
func (w *world) caller(name string) Caller {
	if name == "admin" {
		return Caller{Admin: true, CredentialID: "admin"}
	}
	return w.user(name)
}

// stored emits in and returns its record as the log serves it, with the
// fork parent the store kept.
func (w *world) stored(in Input) Record {
	w.t.Helper()
	seq := w.emit(in).Seq
	recs := w.readAll(seq - 1)
	if len(recs) == 0 || recs[0].Seq != seq {
		w.t.Fatalf("the log does not serve the event %d just emitted", seq)
	}
	return recs[0]
}

// visibleTo decides rec for each caller of names and fails the test on an
// error.
func (w *world) visibleTo(rec Record, names ...string) []bool {
	w.t.Helper()
	out := make([]bool, len(names))
	for i, name := range names {
		ok, err := w.events.Visible(context.Background(), w.caller(name), rec)
		if err != nil {
			w.t.Fatalf("Visible(%s, %s on %v): %v", name, rec.Type, rec.Workspace, err)
		}
		out[i] = ok
	}
	return out
}

func TestVisible_WorkspaceEvents(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	for _, tc := range []struct {
		typ, workspace string
		names          []string
		want           []bool
	}{
		{"parley.change.open", "api", callerNames, []bool{true, true, true, false}},
		// gone names no workspace: a deleted repository.
		{"parley.change.open", "gone", callerNames, []bool{true, false, false, false}},
		{"parley.secret.create", "api", callerNames, []bool{true, true, false, false}},
		{"parley.token.bind", "api", callerNames, []bool{true, true, false, false}},
		{"parley.webhook.create", "api", callerNames, []bool{true, true, false, false}},
		{"parley.forge.webhook.set", "api", callerNames, []bool{true, true, true, false}},
		// mem owns the fork, dev its parent; mem2 is only a member of both.
		{"parley.token.bind", "api-f1", []string{"admin", "mem", "dev", "mem2", "out"}, []bool{true, true, true, false, false}},
	} {
		rec := w.stored(on(tc.typ, tc.workspace))
		if got := w.visibleTo(rec, tc.names...); !slices.Equal(got, tc.want) {
			t.Errorf("%s on %s: visible to %v = %v, want %v", tc.typ, tc.workspace, tc.names, got, tc.want)
		}
	}
}

func TestVisible_ForkEventsFollowTheParent(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	want := []bool{true, true, true, false}
	check := func(when string, rec Record) {
		t.Helper()
		if got := w.visibleTo(rec, callerNames...); !slices.Equal(got, want) {
			t.Errorf("%s: visible to %v = %v, want %v", when, callerNames, got, want)
		}
	}

	rec := w.stored(on("parley.workspace.fork", "api-f1"))
	check("fork event", rec)
	w.deleteWorkspace("api-f1")
	check("fork event after the fork is deleted", rec)

	w.addWorkspace(access.Record{Slug: "api-f2", OwnerID: w.users["mem"], OrgID: w.acme, Kind: access.KindFork, ParentSlug: "api"})
	w.deleteWorkspace("api-f2")
	in := on("parley.fork.expire", "api-f2")
	in.Parent = "api"
	check("event of a fork deleted before the emit, parent passed", w.stored(in))
}

func TestVisible_EventsWithoutWorkspace(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	userEvent := scoped("parley.secret.create", "user", w.users["dev"])
	orgEvent := scoped("parley.variable.update", "org", w.acme)
	for _, tc := range []struct {
		name  string
		in    Input
		setup func()
		names []string
		want  []bool
	}{
		{"user scope", userEvent, nil, []string{"dev", "mem", "admin"}, []bool{true, false, true}},
		{"org scope", orgEvent, nil, []string{"dev", "mem", "out", "admin"}, []bool{true, true, false, true}},
		{"org scope after the org is blocked", orgEvent, func() { w.blockOrg(w.acme) }, []string{"mem", "admin"}, []bool{false, true}},
		{"no scope", scoped("parley.user.create", "", ""), nil, callerNames, []bool{true, false, false, false}},
		{"unknown scope", scoped("parley.user.create", "hub", w.users["dev"]), nil, callerNames, []bool{true, false, false, false}},
	} {
		if tc.setup != nil {
			tc.setup()
		}
		rec := w.stored(tc.in)
		if got := w.visibleTo(rec, tc.names...); !slices.Equal(got, tc.want) {
			t.Errorf("%s: visible to %v = %v, want %v", tc.name, tc.names, got, tc.want)
		}
	}
}

// coverage is a covered-set stub: the slugs each binding covers.
type coverage map[string][]string

func (c coverage) Covers(_ context.Context, caller Caller, slug string) (bool, error) {
	return slices.Contains(c[caller.Workspace], slug), nil
}

// changeSources is a change-lookup stub: the source workspace of each
// change, by workspace and number.
type changeSources map[string]map[int64]string

func (c changeSources) ChangeSource(_ context.Context, workspace string, number int64) (string, error) {
	return c[workspace][number], nil
}

func TestVisible_BoundTokens(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	change := func(typ, slug, number string) Input {
		in := on(typ, slug)
		in.ResourceID = number
		return in
	}
	secret := scoped("parley.secret.create", "user", w.users["dev"])
	push := on("parley.git.push", "api")
	push.ResourceType, push.ResourceID = "workspace", "api"

	devAPI := Caller{UserID: w.users["dev"], CredentialID: "tok-dev", Workspace: "api"}
	memFork := Caller{UserID: w.users["mem"], CredentialID: "tok-mem", Workspace: "api-f1"}
	cases := []struct {
		name   string
		caller Caller
		rec    Record
		// want is the answer with both stubs wired; parentChange marks the
		// answer that the change lookup alone grants.
		want, parentChange bool
	}{
		{"bound to api, event on api", devAPI, w.stored(on("parley.change.open", "api")), true, false},
		{"bound to api, event on web", devAPI, w.stored(on("parley.change.open", "web")), false, false},
		{"bound to api, user-scoped event of its user", devAPI, w.stored(secret), false, false},
		{"bound to the fork, event on the fork", memFork, w.stored(on("parley.change.open", "api-f1")), true, false},
		{"bound to the fork, the parent's change from the fork", memFork, w.stored(change("parley.change.version", "api", "42")), true, true},
		{"bound to the fork, the parent's change from another source", memFork, w.stored(change("parley.change.version", "api", "43")), false, false},
		{"bound to the fork, the parent's push", memFork, w.stored(push), false, false},
	}
	sources := changeSources{"api": {42: "api-f1", 43: "api-f9"}}
	for _, wiring := range []struct {
		name     string
		coverage Coverage
		sources  ChangeSources
	}{
		{"both wired", coverage{"api": {"api"}, "api-f1": {"api-f1"}}, sources},
		{"no change lookup", coverage{"api": {"api"}, "api-f1": {"api-f1"}}, nil},
		{"no covered set", nil, sources},
	} {
		w.events.SetCoverage(wiring.coverage)
		w.events.SetChangeSources(wiring.sources)
		for _, tc := range cases {
			want := tc.want && wiring.coverage != nil && (wiring.sources != nil || !tc.parentChange)
			got, err := w.events.Visible(context.Background(), tc.caller, tc.rec)
			if err != nil || got != want {
				t.Errorf("%s, %s: Visible = %v, %v, want %v, nil", wiring.name, tc.name, got, err, want)
			}
		}
	}
}

func TestVisibleToOwnerOf(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	secret := scoped("parley.secret.create", "user", w.users["dev"])
	expire := w.stored(on("parley.fork.expire", "api-f1"))
	w.deleteWorkspace("api-f1")
	for _, tc := range []struct {
		name string
		rec  Record
		want bool
	}{
		{"an event of the workspace", w.stored(on("parley.change.land", "api")), true},
		{"an owner-only event of the workspace", w.stored(on("parley.token.bind", "api")), true},
		{"an event of a deleted fork of the workspace", expire, true},
		{"an event of another workspace", w.stored(on("parley.change.land", "web")), false},
		{"a user-scoped event", w.stored(secret), false},
	} {
		got, err := w.events.VisibleToOwnerOf(context.Background(), "api", tc.rec)
		if err != nil || got != tc.want {
			t.Errorf("%s: VisibleToOwnerOf(api) = %v, %v, want %v, nil", tc.name, got, err, tc.want)
		}
	}
}

func TestVisible_FailureIsNeverAGrant(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	recs := []Record{w.stored(on("parley.change.open", "api")), w.stored(scoped("parley.variable.update", "org", w.acme))}
	mem := w.key("mem")
	s := w.openStream(t, mem, "", "")
	s.heartbeat(2 * time.Second)

	w.failMemberships.Store(true)
	for _, rec := range recs {
		ok, err := w.events.Visible(context.Background(), w.user("mem"), rec)
		if ok || !errors.Is(err, errMembershipRead) {
			t.Errorf("Visible(mem, %s) with the memberships unreadable = %v, %v, want false and the read error", rec.Type, ok, err)
		}
	}

	w.ask(t, "/events/log", mem, "after=0", "").refused(t, "GET /events/log with the memberships unreadable", http.StatusInternalServerError, "internal")
	w.emitMany(1)
	s.ends(2 * time.Second)
}

func TestCallerOf_ReadsTheAuthenticatedCredential(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	var got Caller
	w.srv.Register(server.Route{Method: http.MethodGet, Pattern: "/test/caller", Handler: func(_ http.ResponseWriter, r *http.Request) error {
		got = CallerOf(r)
		return nil
	}})
	ctx := context.Background()
	var key, keyID string
	err := w.store.Tx(ctx, func(tx *sql.Tx) error {
		k, rec, err := identity.IssueAPIKey(ctx, tx, w.users["dev"], 0, clockStart)
		key, keyID = k, rec.KeyID
		return err
	})
	if err != nil {
		t.Fatalf("issue an API key: %v", err)
	}
	pat, patID := w.pat("mem", "api-f1", "audit:read")
	for _, tc := range []struct {
		name, credential string
		want             Caller
	}{
		{"admin token", w.admin, Caller{Admin: true, CredentialID: "admin"}},
		{"API key", key, Caller{UserID: w.users["dev"], CredentialID: keyID}},
		{"bound PAT", pat, Caller{UserID: w.users["mem"], CredentialID: patID, Workspace: "api-f1"}},
	} {
		got = Caller{}
		if a := w.call(http.MethodGet, "/test/caller", tc.credential, nil); a.status != http.StatusNoContent && a.status != http.StatusOK {
			t.Fatalf("%s: GET /test/caller = %d %s", tc.name, a.status, a.body)
		}
		if got != tc.want {
			t.Errorf("%s: CallerOf = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

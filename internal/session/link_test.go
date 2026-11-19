package session_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/session"
)

// record records links in one committed transaction through the hub's
// session component, advancing the clock a second after each link so that
// every link is recorded at a time of its own.
func (w *world) record(t *testing.T, links ...session.Link) {
	t.Helper()
	ctx := context.Background()
	err := w.Store.Tx(ctx, func(tx *sql.Tx) error {
		for _, l := range links {
			if err := w.sessions.Record(ctx, tx, l); err != nil {
				return fmt.Errorf("record %+v: %w", l, err)
			}
			w.clock.Advance(time.Second)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// object is the session object of the session id as the admin token reads
// it.
func (w *world) object(t *testing.T, id string) map[string]any {
	t.Helper()
	status, raw := w.get(t, w.Token, id)
	if status != http.StatusOK {
		t.Fatalf("GET /sessions/%s = %d %s, want 200", id, status, raw)
	}
	return decode(t, raw)
}

// newSession opens a session of alice on demo and returns its id.
func (w *world) newSession(t *testing.T) string {
	t.Helper()
	return w.open(t, w.aliceKey, map[string]any{"workspace_slug": demo})["id"].(string)
}

// counts is the counts member of a session object as JSON decodes it: n
// for the counts it names, 0 for the others.
func counts(n map[string]float64) map[string]any {
	c := map[string]any{}
	for _, k := range []string{"forks", "changes", "sandboxes", "versions", "pushes", "commits", "claims", "lands"} {
		c[k] = n[k]
	}
	return c
}

// wantCounts fails the test unless the session id counts want.
func (w *world) wantCounts(t *testing.T, id string, want map[string]any) {
	t.Helper()
	if got := w.object(t, id)["counts"]; !reflect.DeepEqual(got, want) {
		t.Errorf("counts = %v, want %v", got, want)
	}
}

// sha is a commit id for push and commit links.
const sha = "0123456789abcdef0123456789abcdef01234567"

func TestRecord_StoresALinkExactlyWhenTheCallersTransactionCommits(t *testing.T) {
	w := newWorld(t)
	errRollback := errors.New("the caller rolls back")
	cases := []struct {
		kind   string
		link   func(id string) session.Link
		counts map[string]float64
	}{
		{"fork", func(id string) session.Link { return session.ForkLink(id, demo, "demo--a") }, map[string]float64{"forks": 1}},
		{"sandbox", func(id string) session.Link { return session.SandboxLink(id, demo, "sb1") }, map[string]float64{"sandboxes": 1}},
		{"version", func(id string) session.Link { return session.VersionLink(id, demo, 7, 1) }, map[string]float64{"versions": 1, "changes": 1}},
		{"push", func(id string) session.Link { return session.PushLink(id, demo, "refs/heads/main", sha) }, map[string]float64{"pushes": 1}},
		{"commit", func(id string) session.Link { return session.CommitLink(id, demo, sha) }, map[string]float64{"commits": 1}},
		{"claim", func(id string) session.Link { return session.ClaimLink(id, demo, "c1") }, map[string]float64{"claims": 1}},
		{"land", func(id string) session.Link { return session.LandLink(id, demo, "l1") }, map[string]float64{"lands": 1}},
	}
	ctx := context.Background()
	for _, c := range cases {
		t.Run(c.kind, func(t *testing.T) {
			id := w.newSession(t)
			err := w.Store.Tx(ctx, func(tx *sql.Tx) error {
				if err := w.sessions.Record(ctx, tx, c.link(id)); err != nil {
					t.Errorf("Record = %v, want nil", err)
				}
				return errRollback
			})
			if !errors.Is(err, errRollback) {
				t.Fatalf("Tx = %v, want the caller's rollback", err)
			}
			w.wantCounts(t, id, counts(nil))
			w.record(t, c.link(id))
			w.wantCounts(t, id, counts(c.counts))

			ended := w.newSession(t)
			if status, raw := w.complete(t, w.aliceKey, ended, map[string]any{"status": "completed"}); status != http.StatusOK {
				t.Fatalf("complete = %d %s, want 200", status, raw)
			}
			w.wantCounts(t, ended, counts(nil))
			w.record(t, c.link(ended))
			w.wantCounts(t, ended, counts(c.counts))
		})
	}
}

func TestObject_ListsForksSandboxesAndChangesInTheOrderTheyWereLinked(t *testing.T) {
	w := newWorld(t)
	w.AddWorkspace(t, access.Record{Slug: "demo--a", OwnerID: w.alice, OrgID: w.acme, Kind: access.KindFork, ParentSlug: demo, Status: access.StatusReady})
	id := w.newSession(t)
	w.record(t,
		session.ForkLink(id, demo, "demo--b"),
		session.ForkLink(id, demo, "demo--a"),
		session.SandboxLink(id, "demo--a", "sb2"),
		session.SandboxLink(id, "demo--a", "sb1"),
		session.VersionLink(id, demo, 7, 1),
		session.VersionLink(id, demo, 3, 1),
		session.VersionLink(id, demo, 7, 2),
	)
	want := map[string]any{
		"forks": []any{"demo--b", "demo--a"},
		"sandboxes": []any{
			map[string]any{"workspace": "demo--a", "id": "sb2"},
			map[string]any{"workspace": "demo--a", "id": "sb1"},
		},
		"changes": []any{
			map[string]any{"workspace": demo, "number": 7.0},
			map[string]any{"workspace": demo, "number": 3.0},
		},
	}
	wantLists := func(t *testing.T) {
		t.Helper()
		s := w.object(t, id)
		for member, list := range want {
			if !reflect.DeepEqual(s[member], list) {
				t.Errorf("%s = %v, want %v", member, s[member], list)
			}
		}
	}
	wantLists(t)
	w.removeWorkspace(t, "demo--a")
	w.removeWorkspace(t, demo)
	wantLists(t)
}

func TestObject_CountsTheLinksOfEachKind(t *testing.T) {
	w := newWorld(t)
	id := w.newSession(t)
	w.record(t,
		session.ForkLink(id, demo, "demo--a"),
		session.ForkLink(id, demo, "demo--b"),
		session.SandboxLink(id, demo, "sb1"),
		session.VersionLink(id, demo, 7, 1),
		session.VersionLink(id, demo, 7, 2),
		session.VersionLink(id, demo, 3, 1),
		session.PushLink(id, demo, "refs/heads/a", sha),
		session.PushLink(id, demo, "refs/heads/b", sha),
		session.PushLink(id, demo, "refs/heads/a", "1123456789abcdef0123456789abcdef01234567"),
		session.PushLink(id, fork1, "refs/heads/a", sha),
		session.CommitLink(id, demo, sha),
		session.CommitLink(id, demo, "1123456789abcdef0123456789abcdef01234567"),
		session.ClaimLink(id, demo, "c1"),
		session.LandLink(id, demo, "l1"),
	)
	w.wantCounts(t, id, counts(map[string]float64{
		"forks": 2, "changes": 2, "sandboxes": 1, "versions": 3, "pushes": 4, "commits": 2, "claims": 1, "lands": 1,
	}))
}

func TestObject_AgreesWithItsCountsWhileLinksArrive(t *testing.T) {
	w := newWorld(t)
	id := w.newSession(t)
	const links = 200
	done := make(chan error, 1)
	go func() {
		ctx := context.Background()
		for i := range links {
			err := w.Store.Tx(ctx, func(tx *sql.Tx) error {
				if err := w.sessions.Record(ctx, tx, session.ForkLink(id, demo, fmt.Sprintf("demo--c%d", i))); err != nil {
					return err
				}
				return w.sessions.Record(ctx, tx, session.VersionLink(id, demo, int64(i+1), 1))
			})
			if err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	// The object is read until the links are all recorded, or until it
	// disagrees with its counts once; the writer is waited for either way.
	agrees := true
	for agrees {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
			return
		default:
		}
		o := w.object(t, id)
		c := o["counts"].(map[string]any)
		if forks := len(o["forks"].([]any)); c["forks"] != float64(forks) {
			t.Errorf("counts.forks = %v with %d forks listed", c["forks"], forks)
			agrees = false
		}
		if changes := len(o["changes"].([]any)); c["changes"] != float64(changes) {
			t.Errorf("counts.changes = %v with %d changes listed", c["changes"], changes)
			agrees = false
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestObject_ListsTheFirst500EntriesAndCountsThemAll(t *testing.T) {
	w := newWorld(t)
	// The keys fall as the links are recorded, so that a list in the order
	// of its keys is not the list in the order of its links.
	cases := []struct {
		kind, list, count string
		link              func(id string, i int) session.Link
		entry             func(i int) any
	}{
		{
			"fork", "forks", "forks",
			func(id string, i int) session.Link {
				return session.ForkLink(id, demo, fmt.Sprintf("demo--f%04d", 1000-i))
			},
			func(i int) any { return fmt.Sprintf("demo--f%04d", 1000-i) },
		},
		{
			"sandbox", "sandboxes", "sandboxes",
			func(id string, i int) session.Link {
				return session.SandboxLink(id, demo, fmt.Sprintf("sb%04d", 1000-i))
			},
			func(i int) any { return map[string]any{"workspace": demo, "id": fmt.Sprintf("sb%04d", 1000-i)} },
		},
		{
			"version of a change of its own", "changes", "changes",
			func(id string, i int) session.Link { return session.VersionLink(id, demo, int64(1000-i), 1) },
			func(i int) any { return map[string]any{"workspace": demo, "number": float64(1000 - i)} },
		},
	}
	for _, c := range cases {
		for _, n := range []int{500, 501} {
			t.Run(fmt.Sprintf("%d links of kind %s", n, c.kind), func(t *testing.T) {
				id := w.newSession(t)
				links := make([]session.Link, n)
				for i := range links {
					links[i] = c.link(id, i)
				}
				w.record(t, links...)
				s := w.object(t, id)
				list, _ := s[c.list].([]any)
				if len(list) != 500 {
					t.Fatalf("len(%s) = %d, want 500", c.list, len(list))
				}
				for _, i := range []int{0, 499} {
					if !reflect.DeepEqual(list[i], c.entry(i)) {
						t.Errorf("%s[%d] = %v, want %v", c.list, i, list[i], c.entry(i))
					}
				}
				for _, e := range list {
					if n > 500 && reflect.DeepEqual(e, c.entry(500)) {
						t.Errorf("%s holds %v, the 501st link", c.list, e)
					}
				}
				if got := s["counts"].(map[string]any)[c.count]; got != float64(n) {
					t.Errorf("counts.%s = %v, want %d", c.count, got, n)
				}
			})
		}
	}
}

func TestRecord_KeepsARepeatedLinkWithItsOriginalTime(t *testing.T) {
	w := newWorld(t)
	id := w.newSession(t)
	recordedAt := w.clock.stamp()
	v := session.VersionLink(id, demo, 7, 1)
	w.record(t, v)
	w.record(t, v)
	w.record(t, v, v)
	w.wantCounts(t, id, counts(map[string]float64{"versions": 1, "changes": 1}))
	var at string
	err := w.Store.Main().QueryRowContext(context.Background(),
		`SELECT created_at FROM session_links WHERE session_id = ? AND kind = 'version'`, id).Scan(&at)
	if err != nil {
		t.Fatal(err)
	}
	if at != recordedAt {
		t.Errorf("the version link's time = %s, want %s, the time it was first recorded", at, recordedAt)
	}

	w.record(t, session.ForkLink(id, demo, "demo--a"))
	w.record(t, session.ForkLink(id, demo, "demo--b"))
	w.record(t, session.ForkLink(id, demo, "demo--a"))
	if got, want := w.object(t, id)["forks"], []any{"demo--a", "demo--b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("forks = %v, want %v", got, want)
	}
}

func TestRecord_RefusesAMalformedLinkAndLeavesTheTransactionUsable(t *testing.T) {
	w := newWorld(t)
	cases := []struct {
		name    string
		link    func(id string) session.Link
		unknown bool
	}{
		{"a UUID that names no session", func(string) session.Link { return session.ForkLink(unknownID, demo, "demo--a") }, true},
		{"a session id that is not a UUID", func(string) session.Link { return session.ForkLink("abc", demo, "demo--a") }, true},
		{"the kind merge", func(id string) session.Link {
			return session.Link{SessionID: id, Kind: "merge", Workspace: demo, Key: "m1"}
		}, false},
		{"an empty workspace", func(id string) session.Link { return session.ForkLink(id, "", "demo--a") }, false},
		{"an empty key", func(id string) session.Link { return session.ForkLink(id, demo, "") }, false},
		{"a version of change 0", func(id string) session.Link { return session.VersionLink(id, demo, 0, 1) }, false},
		{"version 0 of a change", func(id string) session.Link { return session.VersionLink(id, demo, 7, 0) }, false},
		{"a version whose key is not its change and version", func(id string) session.Link {
			return session.Link{SessionID: id, Kind: session.KindVersion, Workspace: demo, Key: "x", Number: 7, Version: 1}
		}, false},
		{"a push without its ref", func(id string) session.Link { return session.PushLink(id, demo, "", sha) }, false},
		{"a push without its new commit", func(id string) session.Link { return session.PushLink(id, demo, "refs/heads/a", "") }, false},
	}
	ctx := context.Background()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			id := w.newSession(t)
			before := w.count(t, "session_links")
			err := w.Store.Tx(ctx, func(tx *sql.Tx) error {
				err := w.sessions.Record(ctx, tx, c.link(id))
				switch {
				case err == nil:
					t.Errorf("Record = nil, want an error")
				case c.unknown && !errors.Is(err, session.ErrUnknownSession):
					t.Errorf("Record = %v, want ErrUnknownSession", err)
				case !c.unknown && errors.Is(err, session.ErrUnknownSession):
					t.Errorf("Record = %v, want a validation error, not ErrUnknownSession", err)
				}
				return w.sessions.Record(ctx, tx, session.ClaimLink(id, demo, "c1"))
			})
			if err != nil {
				t.Fatalf("a valid link after the refusal in the same transaction: %v", err)
			}
			if after := w.count(t, "session_links"); after != before+1 {
				t.Errorf("links = %d after the transaction, want %d", after, before+1)
			}
		})
	}
}

// apiKey is the principal of an API key of the user userID.
func apiKey(userID string) identity.Principal {
	return identity.Principal{Type: identity.TypeAPIKey, CredentialID: "key-" + userID, User: &identity.User{ID: userID}}
}

// adminToken is the principal of the admin token.
var adminToken = identity.Principal{Type: identity.TypeAdmin, CredentialID: "admin"}

// linkedSessions are the sessions the linking check is asked about: x of
// alice on demo, f of alice on its fork demo--t1, a of the admin token on
// demo and d of dave on other.
type linkedSessions struct{ x, f, a, d string }

func (w *world) linkedSessions(t *testing.T) linkedSessions {
	t.Helper()
	open := func(credential, slug string) string {
		return w.open(t, credential, map[string]any{"workspace_slug": slug})["id"].(string)
	}
	return linkedSessions{x: open(w.aliceKey, demo), f: open(w.aliceKey, fork1), a: open(w.Token, demo), d: open(w.daveKey, other)}
}

// linkCheck is one question to the linking check: may caller name the
// session id for an operation on the workspace slug.
type linkCheck struct {
	name   string
	caller identity.Principal
	id     string
	slug   string
}

// acceptedChecks are the questions the linking check accepts, about the
// sessions s and a session of alice on demo ended in each status.
func (w *world) acceptedChecks(t *testing.T, s linkedSessions) []linkCheck {
	t.Helper()
	alice := apiKey(w.alice)
	alicePAT := alice
	alicePAT.Type = identity.TypePAT
	boundElsewhere := alicePAT
	boundElsewhere.Workspace = new(other)
	checks := []linkCheck{
		{"alice, her session on demo, for demo", alice, s.x, demo},
		{"alice, her session on demo, for its fork", alice, s.x, fork1},
		{"alice, her session on a fork, for its parent", alice, s.f, demo},
		{"alice, her session on a fork, for the fork", alice, s.f, fork1},
		{"alice with a PAT", alicePAT, s.x, demo},
		{"alice with a PAT bound to another workspace", boundElsewhere, s.x, demo},
		{"the admin token, alice's session", adminToken, s.x, demo},
		{"the admin token, its session on demo, for its fork", adminToken, s.a, fork1},
		{"alice, her session's id in upper case", alice, strings.ToUpper(s.x), demo},
	}
	for _, status := range []string{"completed", "failed", "timeout", "terminated"} {
		id := w.newSession(t)
		if status == "terminated" {
			w.terminate(t, id)
		} else if code, raw := w.complete(t, w.aliceKey, id, map[string]any{"status": status}); code != http.StatusOK {
			t.Fatalf("complete %s = %d %s, want 200", status, code, raw)
		}
		checks = append(checks, linkCheck{"alice, her session " + status, alice, id, demo})
	}
	return checks
}

// refusedChecks are the questions the linking check refuses, about the
// sessions s.
func (w *world) refusedChecks(s linkedSessions) []linkCheck {
	alice := apiKey(w.alice)
	return []linkCheck{
		{"an id that is not a UUID", alice, "abc", demo},
		{"a UUID that names no session", alice, freshID(), demo},
		{"bob, alice's session", apiKey(w.bob), s.x, demo},
		{"a user's credential without its user", identity.Principal{Type: identity.TypeAPIKey}, s.x, demo},
		{"alice, the admin token's session", alice, s.a, demo},
		{"alice, dave's session", alice, s.d, demo},
		{"alice, her session on demo, for an unrelated workspace", alice, s.x, other},
		{"alice, her session on a fork, for a sibling fork", alice, s.f, fork2},
		{"dave, his session on other, for demo", apiKey(w.dave), s.d, demo},
	}
}

func TestCheck_AcceptsTheCreatorAndTheAdminTokenOnALinkedWorkspace(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	for _, c := range w.acceptedChecks(t, w.linkedSessions(t)) {
		t.Run(c.name, func(t *testing.T) {
			id, err := w.sessions.Check(ctx, c.caller, c.id, c.slug)
			if err != nil || id != strings.ToLower(c.id) {
				t.Errorf("Check(%s, %s) = %q, %v; want %q, nil", c.id, c.slug, id, err, strings.ToLower(c.id))
			}
		})
	}
}

func TestCheck_RefusesEveryOtherReferenceWithOneAnswer(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	texts := map[string][]string{}
	for _, c := range w.refusedChecks(w.linkedSessions(t)) {
		t.Run(c.name, func(t *testing.T) {
			id, err := w.sessions.Check(ctx, c.caller, c.id, c.slug)
			if !errors.Is(err, session.ErrUnknownSession) || id != "" {
				t.Fatalf("Check(%s, %s) = %q, %v; want \"\", ErrUnknownSession", c.id, c.slug, id, err)
			}
			texts[err.Error()] = append(texts[err.Error()], c.name)
		})
	}
	if len(texts) != 1 {
		t.Errorf("the refusals have %d texts, want one: %v", len(texts), texts)
	}
}

func TestCheck_KeepsAcceptingASessionOfARemovedForkForItsParent(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	f := w.open(t, w.aliceKey, map[string]any{"workspace_slug": fork1})["id"].(string)
	w.removeWorkspace(t, fork1)
	for _, caller := range []identity.Principal{apiKey(w.alice), adminToken} {
		if id, err := w.sessions.Check(ctx, caller, f, demo); err != nil || id != f {
			t.Errorf("Check(%s, %s, demo) = %q, %v; want %q, nil", caller.Type, f, id, err, f)
		}
	}
	for _, c := range []linkCheck{
		{"alice for an unrelated workspace", apiKey(w.alice), f, other},
		{"alice for a sibling fork", apiKey(w.alice), f, fork2},
		{"bob for the parent", apiKey(w.bob), f, demo},
	} {
		if _, err := w.sessions.Check(ctx, c.caller, c.id, c.slug); !errors.Is(err, session.ErrUnknownSession) {
			t.Errorf("%s: Check = %v, want ErrUnknownSession", c.name, err)
		}
	}
}

// failingWorkspaces is a workspace reader that cannot read the record of
// the workspace broken and reads every other through next.
type failingWorkspaces struct {
	next   session.WorkspaceReader
	broken string
}

func (r failingWorkspaces) Workspace(ctx context.Context, slug string) (session.Workspace, bool, error) {
	if slug == r.broken {
		return session.Workspace{}, false, errors.New("the workspace record cannot be read")
	}
	return r.next.Workspace(ctx, slug)
}

func TestCheck_ReportsAFailedReadAsAFailure(t *testing.T) {
	w := newWorld(t, func(d *session.Deps) { d.Workspaces = failingWorkspaces{next: d.Workspaces, broken: fork1} })
	ctx := context.Background()
	x := w.newSession(t)
	wantFailure := func(t *testing.T, what string, id string, err error) {
		t.Helper()
		if err == nil || errors.Is(err, session.ErrUnknownSession) || id != "" {
			t.Errorf("%s: Check = %q, %v; want \"\" and an error that is not ErrUnknownSession", what, id, err)
		}
	}
	id, err := w.sessions.Check(ctx, apiKey(w.alice), x, fork1)
	wantFailure(t, "the fork's record cannot be read", id, err)

	if err := w.Store.Close(); err != nil {
		t.Fatal(err)
	}
	id, err = w.sessions.Check(ctx, apiKey(w.alice), x, demo)
	wantFailure(t, "the store is closed", id, err)
}

func TestCheck_WritesNothing(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	s := w.linkedSessions(t)
	checks := append(w.acceptedChecks(t, s), w.refusedChecks(s)...)
	tables := []string{"sessions", "session_usage", "session_links"}
	before := map[string]int{}
	for _, table := range tables {
		before[table] = w.count(t, table)
	}
	x := w.object(t, s.x)
	for _, c := range checks {
		_, _ = w.sessions.Check(ctx, c.caller, c.id, c.slug)
	}
	for _, table := range tables {
		if n := w.count(t, table); n != before[table] {
			t.Errorf("%s: %d rows after the checks, want %d", table, n, before[table])
		}
	}
	if after := w.object(t, s.x); !reflect.DeepEqual(after, x) {
		t.Errorf("session x after the checks = %v, want %v", after, x)
	}
	w.wantCounts(t, s.x, counts(nil))
}

package session_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
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

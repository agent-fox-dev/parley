package access_test

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/access"
)

const jsonContentType = "application/json; charset=utf-8"

// assertRefused fails the test unless a is the error envelope with status,
// errorType and message, and the probe handler did not run.
func assertRefused(t *testing.T, p *probe, before int64, what string, a answer, status int, errorType, message string) {
	t.Helper()
	e := decode(t, a)
	if a.status != status || a.contentType != jsonContentType || e.Error.Code != status ||
		e.Error.ErrorType != errorType || message != "" && e.Error.Message != message {
		t.Errorf("%s = %d %q %s, want %d %s %q", what, a.status, a.contentType, a.body, status, errorType, message)
	}
	if n := p.calls.Load(); n != before {
		t.Errorf("%s ran the handler", what)
	}
}

func TestCheck_GrantsByLevelAndNeedAndRefusesWithoutRunningTheHandler(t *testing.T) {
	t.Parallel()
	st := openStore(t, t.TempDir())
	seed(t, st, repo("acme", "u-owner", "o1", access.StatusReady), repo("old", "u-owner", "o1", access.StatusArchived))
	p := newProbe(newService(access.StoreRecords(st.Main()), memberships{of: map[string][]string{"u-member": {"o1"}}, active: []string{"o1"}}))

	cases := []struct {
		callers []string
		slugs   []string
		routes  string
		status  int
		level   access.Level // on a grant
	}{
		{[]string{"admin"}, []string{"acme", "old"}, "rwo", 200, access.LevelAdmin},
		{[]string{"u-owner"}, []string{"acme", "old"}, "rwo", 200, access.LevelOwner},
		{[]string{"u-member"}, []string{"acme", "old"}, "r", 200, access.LevelMember},
		{[]string{"u-member"}, []string{"acme", "old"}, "wo", 403, ""},
		{[]string{"u-stranger"}, []string{"acme", "old"}, "rwo", 404, ""},
		{[]string{"admin", "u-owner", "u-member", "u-stranger"}, []string{"nope"}, "rwo", 404, ""},
	}
	for _, c := range cases {
		for _, caller := range c.callers {
			for _, slug := range c.slugs {
				for _, route := range c.routes {
					path := "/workspaces/" + slug + "/" + string(route)
					what := caller + " on " + path
					before := p.calls.Load()
					a := p.get(caller, path)
					switch c.status {
					case 200:
						e := decode(t, a)
						if a.status != 200 || e.Level != c.level || e.Record.Slug != slug || p.calls.Load() != before+1 {
							t.Errorf("%s = %d %s, want 200 with level %q and the record of %s, handler run once", what, a.status, a.body, c.level, slug)
						}
					case 403:
						assertRefused(t, p, before, what, a, 403, "forbidden", "org members have read-only access to this workspace")
					default:
						assertRefused(t, p, before, what, a, 404, "not_found", "workspace not found")
					}
				}
			}
		}
	}
}

func TestCheck_AnswersAFailedReadAsInternalWithoutItsTextOrTheRecord(t *testing.T) {
	t.Parallel()
	st := openStore(t, t.TempDir())
	seed(t, st,
		repo("acme", "u-owner", "o1", access.StatusReady),
		repo("theirs", "u-member", "o1", access.StatusReady),
		fork("acme--f", "u-member", "o1", "acme"),
	)
	m := memberships{of: map[string][]string{"u-member": {"o1"}}, active: []string{"o1"}}
	records := failingRecords{RecordReader: access.StoreRecords(st.Main()), fail: []string{"acme"}}
	badRecords := newProbe(newService(records, m))
	badMemberships := newProbe(newService(access.StoreRecords(st.Main()), failingMemberships))

	// Each row puts the failing read on the caller's path: the admin token
	// reads only the workspace's record, and an owner is decided before any
	// membership is read, so the owner's membership row is on a workspace
	// of someone else.
	cases := []struct {
		failing string
		p       *probe
		callers []string
		slug    string
	}{
		{"the record read", badRecords, []string{"admin", "u-owner", "u-member", "u-stranger"}, "acme"},
		{"the parent's record read", badRecords, []string{"u-owner", "u-member", "u-stranger"}, "acme--f"},
		{"the membership read", badMemberships, []string{"u-member", "u-stranger"}, "acme"},
		{"the membership read", badMemberships, []string{"u-owner"}, "theirs"},
	}
	for _, c := range cases {
		for _, caller := range c.callers {
			for _, route := range "rwo" {
				what := c.failing + " failing, " + caller + " on " + c.slug + "/" + string(route)
				before := c.p.calls.Load()
				a := c.p.get(caller, "/workspaces/"+c.slug+"/"+string(route))
				assertRefused(t, c.p, before, what, a, 500, "internal", "")
				if bytes.Contains(a.body, []byte("/data/parley.db")) || bytes.Contains(a.body, []byte("acme")) || bytes.Contains(a.body, []byte("theirs")) {
					t.Errorf("%s: body %s shows the failure or the workspace", what, a.body)
				}
			}
		}
	}
}

func TestCheck_AnswersAMissingCallerOrAMisdeclaredRouteAsInternal(t *testing.T) {
	t.Parallel()
	st := openStore(t, t.TempDir())
	seed(t, st, repo("acme", "u-owner", "o1", access.StatusReady))
	p := newProbe(newService(access.StoreRecords(st.Main()), memberships{}))
	p.route("/workspaces/:slug/admin-need", "admin")
	p.route("/things", "read")

	for _, c := range []struct {
		name, caller, path string
	}{
		{"no caller from authentication", "", "/workspaces/acme/r"},
		{"a need outside read, write and owner", "u-owner", "/workspaces/acme/admin-need"},
		{"a need on a route without a slug", "u-owner", "/things"},
	} {
		before := p.calls.Load()
		assertRefused(t, p, before, c.name, p.get(c.caller, c.path), 500, "internal", "internal error")
	}
}

func TestCheck_AnswersAnInvisibleWorkspaceByteForByteAsOneThatDoesNotExist(t *testing.T) {
	t.Parallel()
	st := openStore(t, t.TempDir())
	stored := []string{"acme"}
	seed(t, st, repo("acme", "u-owner", "o1", access.StatusReady))
	for _, status := range []string{access.StatusPending, access.StatusCloning, access.StatusReady, access.StatusFailed, access.StatusArchived} {
		seed(t, st, repo("ws-"+status, "u-owner", "o1", status))
		stored = append(stored, "ws-"+status)
	}
	seed(t, st, fork("ws-ready--f", "u-left", "o1", "ws-ready"))
	stored = append(stored, "ws-ready--f")
	// u-left owns the fork and is a member of nothing; u-admin-role stands
	// for a user with the role admin, which the rule's caller does not carry.
	p := newProbe(newService(access.StoreRecords(st.Main()), memberships{}))

	baseline := map[rune]answer{}
	for _, route := range "rwo" {
		baseline[route] = p.get("u-stranger", "/workspaces/does-not-exist/"+string(route))
		if a := baseline[route]; a.status != 404 || !bytes.Equal(a.body, baseline['r'].body) {
			t.Fatalf("baseline of /%c = %d %s, want 404 and the body of /r %s", route, a.status, a.body, baseline['r'].body)
		}
	}
	same := func(caller, slug string) {
		for _, route := range "rwo" {
			if a := p.get(caller, "/workspaces/"+slug+"/"+string(route)); a.status != 404 || !bytes.Equal(a.body, baseline[route].body) {
				t.Errorf("%s on %s/%c = %d %s, want 404 %s", caller, slug, route, a.status, a.body, baseline[route].body)
			}
		}
	}
	for _, caller := range []string{"u-stranger", "u-left", "u-admin-role"} {
		for _, slug := range stored {
			same(caller, slug)
		}
	}
	// No shape check runs before the lookup: a string that is no legal slug,
	// or differs from one only in case, is a workspace that does not exist,
	// to its owner too.
	for _, caller := range []string{"u-stranger", "u-owner"} {
		for _, slug := range []string{"ACME", "a--b--c", strings.Repeat("a", 300), "%2e%2e"} {
			same(caller, slug)
		}
	}
}

func TestCheck_GrantsAMemberAReadRouteWithAnyMethodAndGivesTheHandlerTheRecordAsStored(t *testing.T) {
	t.Parallel()
	st := openStore(t, t.TempDir())
	stored := map[string]access.Record{
		"acme":    repo("acme", "u-owner", "o1", access.StatusReady),
		"acme--m": fork("acme--m", "u-member", "o1", "acme"),
	}
	seed(t, st, stored["acme"], stored["acme--m"])
	p := newProbe(newService(access.StoreRecords(st.Main()), memberships{of: map[string][]string{"u-member": {"o1"}}, active: []string{"o1"}}))
	methods := []string{http.MethodPost, http.MethodPatch, http.MethodPut, http.MethodDelete}
	for _, method := range methods {
		p.routeFor(method, "/workspaces/:slug/contrib", "read")
	}

	// A member's own contributions change state on routes that need read:
	// the method decides nothing, and the handler reads the facts its own
	// condition needs from the decision.
	for slug, want := range map[string]access.Level{"acme": access.LevelMember, "acme--m": access.LevelOwner} {
		for _, method := range methods {
			before := p.calls.Load()
			a := p.call(method, "u-member", "/workspaces/"+slug+"/contrib")
			e := decode(t, a)
			if a.status != 200 || e.Level != want || e.Record != stored[slug] || p.calls.Load() != before+1 {
				t.Errorf("u-member %s %s = %d %s, want 200 with level %q and the record %+v, handler run once", method, slug, a.status, a.body, want, stored[slug])
			}
		}
	}
}

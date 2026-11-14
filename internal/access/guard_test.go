package access_test

import (
	"bytes"
	"context"
	"database/sql"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
)

// probePattern is the probe route that needs read and the scope git:read.
const probePattern = "/workspaces/:slug/probe"

// route is a route's method, its pattern and a path that matches it.
type route struct{ method, pattern, path string }

// contributorRoutes are the routes of a workspace, relative to
// /workspaces/:slug, that a token bound to a fork of it reaches.
var contributorRoutes = []route{
	{http.MethodPost, "/changes", "/changes"},
	{http.MethodGet, "/changes", "/changes"},
	{http.MethodGet, "/changes/:number", "/changes/7"},
	{http.MethodGet, "/changes/by-commit/:sha", "/changes/by-commit/a1b2c3"},
	{http.MethodPatch, "/changes/:number", "/changes/7"},
	{http.MethodPost, "/changes/:number/abandon", "/changes/7/abandon"},
	{http.MethodPost, "/changes/:number/restore", "/changes/7/restore"},
	{http.MethodPost, "/changes/:number/restack", "/changes/7/restack"},
	{http.MethodGet, "/changes/:number/versions", "/changes/7/versions"},
	{http.MethodGet, "/changes/:number/versions/:version", "/changes/7/versions/2"},
	{http.MethodGet, "/changes/:number/diff", "/changes/7/diff"},
	{http.MethodGet, "/changes/:number/stack", "/changes/7/stack"},
	{http.MethodPost, "/changes/:number/land", "/changes/7/land"},
	{http.MethodGet, "/changes/:number/lands", "/changes/7/lands"},
	{http.MethodGet, "/lands/:id", "/lands/l1"},
	{http.MethodGet, "/changes/:number/conflict", "/changes/7/conflict"},
	{http.MethodGet, "/changes/:number/conflict/file", "/changes/7/conflict/file"},
	{http.MethodGet, "/changes/:number/approvals", "/changes/7/approvals"},
	{http.MethodGet, "/changes/:number/checks", "/changes/7/checks"},
	{http.MethodGet, "/policy", "/policy"},
	{http.MethodPost, "/policy/evaluate", "/policy/evaluate"},
	{http.MethodGet, "/activity", "/activity"},
	{http.MethodGet, "/changes/:number/overlaps", "/changes/7/overlaps"},
	{http.MethodGet, "/claims", "/claims"},
	{http.MethodGet, "/claims/:id", "/claims/c1"},
}

// parentOnlyRoutes are routes of a workspace, relative to /workspaces/:slug,
// that a token bound to a fork of it does not reach.
var parentOnlyRoutes = []route{
	{http.MethodGet, "/lands", "/lands"},
	{http.MethodPut, "/policy", "/policy"},
	{http.MethodPost, "/changes/:number/approvals", "/changes/7/approvals"},
	{http.MethodDelete, "/changes/:number/approvals/:id", "/changes/7/approvals/a1"},
	{http.MethodPut, "/changes/:number/checks/:name", "/changes/7/checks/ci"},
	{http.MethodPost, "/changes/:number/revert", "/changes/7/revert"},
	{http.MethodPost, "/claims", "/claims"},
	{http.MethodPatch, "/claims/:id", "/claims/c1"},
	{http.MethodDelete, "/claims/:id", "/claims/c1"},
	{http.MethodPost, "/tokens", "/tokens"},
	{http.MethodGet, "/tokens", "/tokens"},
	{http.MethodPost, "/forks", "/forks"},
}

// allowlist is every route that names no workspace and that a bound PAT
// reaches.
var allowlist = []route{
	{http.MethodGet, "/workspaces", "/workspaces"},
	{http.MethodGet, "/events", "/events"},
	{http.MethodGet, "/events/log", "/events/log"},
	{http.MethodGet, "/audit", "/audit"},
	{http.MethodGet, "/sandbox-profiles", "/sandbox-profiles"},
	{http.MethodPost, "/sessions", "/sessions"},
	{http.MethodGet, "/sessions", "/sessions"},
	{http.MethodGet, "/sessions/:id", "/sessions/s1"},
	{http.MethodPost, "/sessions/:id/complete", "/sessions/s1/complete"},
	{http.MethodPost, "/sessions/:id/usage", "/sessions/s1/usage"},
	{http.MethodGet, "/sessions/:id/usage", "/sessions/s1/usage"},
}

// cred is a stored PAT: its value, its token_id, the workspace it is bound
// to ("" for none) and its user.
type cred struct{ value, id, bound, user string }

// guardWorld is a guard hub with identity's routes and probe routes for
// every route of the tables above, the probe route and a write probe route
// /workspaces/:slug/probe-write, POST /workspaces and GET /sandboxes. Alice
// owns w1, the archived w2 and w5 in her personal org, and w7 in org7, of
// which dave is a member; bob owns w6. The forks of w1 are f1, created by
// alice's token "w1", f2, created by "w1 other", f3 and f9, created by no
// token, and f4, created by bob's token; g1 is a fork of w5 created by "w1".
// Its PATs are in tok, by name.
type guardWorld struct {
	*guardHub
	alice, org string
	tok        map[string]cred
}

func newGuardWorld(t *testing.T) *guardWorld {
	t.Helper()
	w := &guardWorld{guardHub: newGuardHub(t), tok: map[string]cred{}}
	w.routes()
	alice, org := w.user("alice")
	bob, bobOrg := w.user("bob")
	carol, _ := w.user("carol")
	dave, _ := w.user("dave")
	w.alice, w.org, w.orgs[dave] = alice, org, []string{"org7"}
	seed(t, w.st, repo("w1", alice, org, access.StatusReady), repo("w2", alice, org, access.StatusArchived),
		repo("w5", alice, org, access.StatusReady), repo("w6", bob, bobOrg, access.StatusReady),
		repo("w7", alice, "org7", access.StatusReady))
	read, past := []string{"git:read"}, time.Now().Add(-time.Hour)
	add := func(name, user, bound string, perms []string, expires *time.Time) {
		v, id := w.storePAT(identity.PATSpec{UserID: user, Permissions: perms, Workspace: bound, ExpiresAt: expires})
		w.tok[name] = cred{v, id, bound, user}
	}
	add("w1", alice, "w1", read, nil)
	add("w1 other", alice, "w1", read, nil)
	add("w1 no scope", alice, "w1", []string{"changes:read"}, nil)
	add("w1 every scope", alice, "w1", identity.RegisteredScopes(), nil)
	add("w2", alice, "w2", read, nil)
	add("gone", alice, "gone", read, nil)
	add("unbound", alice, "", read, nil)
	add("unbound no scope", alice, "", []string{"changes:read"}, nil)
	add("bob", bob, "", read, nil)
	add("carol w7", carol, "w7", read, nil)
	add("carol", carol, "", read, nil)
	add("dave w7", dave, "w7", read, nil)
	add("dave", dave, "", read, nil)
	add("revoked", alice, "w1", read, nil)
	add("expired", alice, "w1", []string{"users:read"}, &past)
	if err := w.st.Tx(context.Background(), func(tx *sql.Tx) error {
		return identity.RevokeTokenByID(context.Background(), tx, w.tok["revoked"].id, time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	v := w.tok["w1"].value
	w.tok["wrong secret"] = cred{value: v[:len(v)-1] + map[bool]string{true: "b", false: "a"}[strings.HasSuffix(v, "a")]}
	seed(t, w.st,
		createdBy(fork("f1", alice, org, "w1"), w.tok["w1"].id),
		createdBy(fork("f2", alice, org, "w1"), w.tok["w1 other"].id),
		fork("f3", alice, org, "w1"),
		createdBy(fork("f4", bob, org, "w1"), w.tok["bob"].id),
		fork("f9", alice, org, "w1"),
		createdBy(fork("g1", alice, org, "w5"), w.tok["w1"].id))
	add("f1", alice, "f1", read, nil)
	return w
}

// routes registers the world's routes on its server.
func (w *guardWorld) routes() {
	w.identityRoutes()
	w.probe(http.MethodGet, probePattern, access.NeedRead, "git:read")
	w.probe(http.MethodGet, "/workspaces/:slug/probe-write", access.NeedWrite, "git:read")
	for _, r := range slices.Concat(contributorRoutes, parentOnlyRoutes) {
		w.probe(r.method, "/workspaces/:slug"+r.pattern, access.NeedRead, "git:read")
	}
	for _, r := range allowlist {
		w.probe(r.method, r.pattern, "", "")
	}
	w.probe(http.MethodPost, "/workspaces", "", "workspaces:write")
	w.probe(http.MethodGet, "/sandboxes", "", "sandboxes:read")
}

// guardCase is one request of the matrix and what it is answered.
type guardCase struct {
	token, method, path string
	status              int
	errorType           string   // of a refusal
	found               *binding // what the handler finds on a 200; nil for none
	covered             []string // the covered set the handler reads, checked when not nil
	refusal             string   // the pattern of the route a refusal of the guard reports; "" for none
	same                string   // the token answered the same, byte for byte, when not ""
}

// on is the binding of the workspace ws with the relation rel.
func on(ws string, rel access.Relation) *binding { return &binding{ws, rel} }

// outside is the request of token on the probe route of slug, which the
// guard answers 404 as a workspace outside the binding.
func outside(token, slug string) guardCase {
	return guardCase{token: token, method: http.MethodGet, path: "/workspaces/" + slug + "/probe",
		status: http.StatusNotFound, errorType: "not_found", refusal: probePattern}
}

// check sends each request of cases and checks its answer, that the handler
// ran on a 200 only, and the refusal the guard reported. Every 404 of the
// guard is the same bytes.
func (w *guardWorld) check(t *testing.T, cases []guardCase) {
	t.Helper()
	var notFound [][]byte
	for _, c := range cases {
		tok := w.tok[c.token]
		what := c.token + ": " + c.method + " " + c.path
		reported, calls := len(w.refusals.all()), w.calls.Load()
		a := w.do(c.method, c.path, tok.value)
		e := decode(t, a)
		if a.status != c.status || e.Error.ErrorType != c.errorType {
			t.Errorf("%s = %d %s, want %d %s", what, a.status, a.body, c.status, c.errorType)
			continue
		}
		if ran := w.calls.Load() - calls; ran != map[bool]int64{true: 1}[c.status == http.StatusOK] {
			t.Errorf("%s ran the handler %d times", what, ran)
		}
		if c.status == http.StatusOK && !reflect.DeepEqual(e.Binding, c.found) {
			t.Errorf("%s: the handler found %+v, want %+v", what, e.Binding, c.found)
		}
		if c.covered != nil && !slices.Equal(e.Covered, c.covered) {
			t.Errorf("%s: covered set %v, want %v", what, e.Covered, c.covered)
		}
		if c.status == http.StatusMethodNotAllowed && a.allow == "" {
			t.Errorf("%s: 405 without an Allow header", what)
		}
		if c.same != "" {
			if b := w.do(c.method, c.path, w.tok[c.same].value); b.status != a.status || !bytes.Equal(b.body, a.body) {
				t.Errorf("%s = %d %s, but for %s %d %s", what, a.status, a.body, c.same, b.status, b.body)
			}
		}
		got := w.refusals.all()[reported:]
		if c.refusal == "" {
			if len(got) != 0 {
				t.Errorf("%s: refusals %+v, want none", what, got)
			}
			continue
		}
		var requested *string
		if rest, ok := strings.CutPrefix(c.path, "/workspaces/"); ok {
			requested = new(strings.Split(rest, "/")[0])
		}
		want := access.Refusal{TokenID: tok.id, UserID: tok.user, BoundWorkspace: tok.bound, RequestedWorkspace: requested,
			Method: c.method, Route: "/api/v1" + c.refusal, Status: c.status, Surface: access.SurfaceAPI}
		if len(got) != 1 || !reflect.DeepEqual(got[0], want) {
			t.Errorf("%s: refusals %+v, want one %+v", what, got, want)
		}
		if c.status == http.StatusNotFound {
			notFound = append(notFound, a.body)
		}
	}
	for _, b := range notFound {
		if !bytes.Equal(b, notFound[0]) {
			t.Errorf("a 404 of the guard is %s, another %s, want the same bytes", b, notFound[0])
		}
	}
}

func TestGuard_PassesWhatTheBindingCoversWithTheBindingForTheHandler(t *testing.T) {
	w := newGuardWorld(t)
	pass := func(token, slug string, found *binding) guardCase {
		return guardCase{token: token, method: http.MethodGet, path: "/workspaces/" + slug + "/probe", status: http.StatusOK, found: found}
	}
	w.check(t, []guardCase{
		pass("w1", "w1", on("w1", access.RelationBound)),
		pass("w2", "w2", on("w2", access.RelationBound)),
		pass("w1", "f1", on("w1", access.RelationCreatedFork)),
		pass("f1", "f1", on("f1", access.RelationBound)),
		pass("unbound", "w1", nil),
		pass("unbound", "w2", nil),
		pass("unbound", "f1", nil),
		{token: "w1", method: http.MethodGet, path: "/workspaces/w1/changes", status: http.StatusOK, found: on("w1", access.RelationBound)},
		outside("w1", "f5"),
	})
	// A fork the token creates after an earlier answer is covered at once.
	seed(t, w.st, createdBy(fork("f5", w.alice, w.org, "w1"), w.tok["w1"].id))
	w.check(t, []guardCase{pass("w1", "f5", on("w1", access.RelationCreatedFork))})
}

func TestGuard_AnswersEveryWorkspaceOutsideTheBindingNotFoundBeforeScopeAndAccess(t *testing.T) {
	w := newGuardWorld(t)
	cases := []guardCase{
		outside("w1", "f2"), // created by another token of the same user
		outside("w1", "f3"), // created with no token
		outside("w1", "f4"), // created by another user's token
		outside("f1", "f9"), // a sibling of the bound fork
		outside("w1", "g1"), // created by the token, but a fork of another workspace
		outside("f1", "w5"), // a fork token on another workspace
	}
	for _, slug := range []string{"w5", "w6", "nosuch", "f9"} {
		cases = append(cases, outside("w1", slug), outside("w1 no scope", slug))
	}
	w.check(t, cases)
}

func TestGuard_LeavesScopeAndAccessToDecideAsForAnUnboundToken(t *testing.T) {
	w := newGuardWorld(t)
	w.check(t, []guardCase{
		// A user no longer in the workspace's org.
		{token: "carol w7", method: http.MethodGet, path: "/workspaces/w7/probe", status: http.StatusNotFound, errorType: "not_found", same: "carol"},
		// A token without the route's scope.
		{token: "w1 no scope", method: http.MethodGet, path: "/workspaces/w1/probe", status: http.StatusForbidden, errorType: "insufficient_scope", same: "unbound no scope"},
		// A member on a route that needs write.
		{token: "dave w7", method: http.MethodGet, path: "/workspaces/w7/probe-write", status: http.StatusForbidden, errorType: "forbidden", same: "dave"},
		// A bound slug with no workspace.
		{token: "gone", method: http.MethodGet, path: "/workspaces/gone/probe", status: http.StatusNotFound, errorType: "not_found", same: "unbound"},
	})
}

func TestGuard_LetsAForkTokenOntoTheContributorRoutesOfTheParentOnly(t *testing.T) {
	w := newGuardWorld(t)
	cases := []guardCase{{token: "f1", method: http.MethodGet, path: "/workspaces/w1/probe", status: http.StatusForbidden,
		errorType: "token_bound", refusal: probePattern}}
	for _, r := range contributorRoutes {
		cases = append(cases, guardCase{token: "f1", method: r.method, path: "/workspaces/w1" + r.path,
			status: http.StatusOK, found: on("f1", access.RelationParentOfBoundFork)})
	}
	for _, r := range parentOnlyRoutes {
		cases = append(cases, guardCase{token: "f1", method: r.method, path: "/workspaces/w1" + r.path,
			status: http.StatusForbidden, errorType: "token_bound", refusal: "/workspaces/:slug" + r.pattern})
	}
	w.check(t, cases)
}

func TestGuard_PassesTheAllowlistAndRefusesEveryOtherRouteThatNamesNoWorkspaceBeforeTheScopeCheck(t *testing.T) {
	w := newGuardWorld(t)
	var cases []guardCase
	for _, r := range allowlist {
		cases = append(cases, guardCase{token: "w1", method: r.method, path: r.path, status: http.StatusOK,
			found: on("w1", ""), covered: []string{"f1", "w1"}})
	}
	for _, token := range []string{"w1 every scope", "w1"} {
		id := w.tok[token].id
		for _, r := range []route{
			{http.MethodGet, "/user", "/user"},
			{http.MethodPatch, "/user", "/user"},
			{http.MethodGet, "/user/orgs", "/user/orgs"},
			{http.MethodGet, "/user/keys", "/user/keys"},
			{http.MethodGet, "/user/tokens", "/user/tokens"},
			{http.MethodPost, "/user/tokens", "/user/tokens"},
			{http.MethodGet, "/user/tokens/:token_id", "/user/tokens/" + id},
			{http.MethodDelete, "/user/tokens/:token_id", "/user/tokens/" + id},
			{http.MethodPut, "/user/tokens/:token_id/permissions", "/user/tokens/" + id + "/permissions"},
			{http.MethodGet, "/orgs", "/orgs"},
			{http.MethodGet, "/users", "/users"},
			{http.MethodPost, "/workspaces", "/workspaces"},
			{http.MethodGet, "/sandboxes", "/sandboxes"},
		} {
			cases = append(cases, guardCase{token: token, method: r.method, path: r.path, status: http.StatusForbidden,
				errorType: "token_bound", refusal: r.pattern})
		}
	}
	w.check(t, cases)
	for _, token := range []string{"w1 every scope", "w1"} {
		if pat, err := identity.PATByID(context.Background(), w.st.Main(), w.tok[token].id); err != nil || pat.RevokedAt != nil {
			t.Errorf("%s after the requests = %+v, %v, want it live", token, pat, err)
		}
	}
}

func TestGuard_DoesNotRunOnARequestAnEarlierCheckAnswers(t *testing.T) {
	w := newGuardWorld(t)
	w.check(t, []guardCase{
		{token: "w1", method: http.MethodGet, path: "/nosuch", status: http.StatusNotFound, errorType: "not_found"},
		{token: "w1", method: http.MethodDelete, path: "/workspaces/w5/probe", status: http.StatusMethodNotAllowed, errorType: "method_not_allowed"},
		{token: "wrong secret", method: http.MethodGet, path: "/workspaces/w5/probe", status: http.StatusUnauthorized, errorType: "unauthenticated"},
		{token: "revoked", method: http.MethodGet, path: "/workspaces/w5/probe", status: http.StatusUnauthorized, errorType: "unauthenticated"},
		{token: "expired", method: http.MethodGet, path: "/user", status: http.StatusUnauthorized, errorType: "unauthenticated"},
	})
}

func TestGuard_LeavesEveryOtherCredentialAsWithoutTheGuard(t *testing.T) {
	w := newGuardWorld(t)
	without := &guardWorld{guardHub: w.unguarded()}
	without.routes()
	paths := []string{"/workspaces/w1/probe", "/workspaces/w6/probe", "/workspaces/nosuch/probe", "/user"}
	for _, c := range []struct {
		name, credential string
		paths            []string
	}{
		{"an API key", w.key(w.alice), paths},
		{"the admin token", w.admin(), paths[:3]},
		{"an unbound PAT", w.tok["unbound"].value, paths},
	} {
		for _, path := range c.paths {
			with, wo := w.do(http.MethodGet, path, c.credential), without.do(http.MethodGet, path, c.credential)
			if with.status != wo.status || !bytes.Equal(with.body, wo.body) {
				t.Errorf("%s on %s = %d %s with the guard, %d %s without", c.name, path, with.status, with.body, wo.status, wo.body)
			}
			if with.status == http.StatusOK && decode(t, with).Binding != nil {
				t.Errorf("%s on %s: the handler found a binding, %s", c.name, path, with.body)
			}
		}
	}
	if got := w.refusals.all(); len(got) != 0 {
		t.Errorf("refusals = %+v, want none", got)
	}
}

func TestGuardGit_PassesWhatTheBindingCoversAndAnswersEverythingElseNotFound(t *testing.T) {
	w := newGuardWorld(t)
	alice, idA, idF := w.alice, w.tok["w1"].id, w.tok["f1"].id
	unbound := server.Caller{CredentialID: "t_unbound", UserID: alice, Detail: &identity.Principal{
		Type: identity.TypePAT, CredentialID: "t_unbound", User: &identity.User{ID: alice}}}
	const pattern = "/git/:org/:slug.git/git-upload-pack"

	for _, c := range []struct {
		name   string
		caller server.Caller
		slug   string
		pass   bool
		want   *access.Binding
	}{
		{"the bound workspace", boundPAT(alice, idA, "w1"), "w1", true, &access.Binding{TokenID: idA, Workspace: "w1", Relation: access.RelationBound}},
		{"a fork the token created", boundPAT(alice, idA, "w1"), "f1", true, &access.Binding{TokenID: idA, Workspace: "w1", Relation: access.RelationCreatedFork}},
		{"another workspace", boundPAT(alice, idA, "w1"), "w5", false, nil},
		{"a slug with no workspace", boundPAT(alice, idA, "w1"), "nosuch", false, nil},
		{"the parent of the bound fork", boundPAT(alice, idF, "f1"), "w1", false, nil},
		{"an API key", server.Caller{CredentialID: "k1", UserID: alice}, "w5", true, nil},
		{"the admin token", server.Caller{CredentialID: "admin", Admin: true}, "w5", true, nil},
		{"an unbound PAT", unbound, "w5", true, nil},
	} {
		reported := len(w.refusals.all())
		d, err := w.svc.GuardGit(context.Background(), c.caller, c.slug, http.MethodPost, pattern)
		got := w.refusals.all()[reported:]
		want := access.GitDecision{Pass: c.pass, Binding: c.want}
		var refusals []access.Refusal
		if !c.pass {
			want.Status = http.StatusNotFound
			ac := access.CallerFrom(c.caller)
			refusals = []access.Refusal{{TokenID: ac.TokenID, UserID: alice, BoundWorkspace: ac.Workspace, RequestedWorkspace: &c.slug,
				Method: http.MethodPost, Route: pattern, Status: http.StatusNotFound, Surface: access.SurfaceGit}}
		}
		if err != nil || !reflect.DeepEqual(d, want) || len(got) != len(refusals) || len(got) > 0 && !reflect.DeepEqual(got, refusals) {
			t.Errorf("%s: GuardGit = %+v, %v, refusals %+v; want %+v, refusals %+v", c.name, d, err, got, want, refusals)
		}
	}
}

package access_test

import (
	"bytes"
	"net/http"
	"slices"
	"testing"

	"github.com/agent-fox-dev/parley/internal/access"
)

// probePattern is the probe route every guard test registers: it needs read
// and the scope git:read.
const probePattern = "/workspaces/:slug/probe"

func TestGuard_PassesWhatTheBindingCoversWithTheBindingForTheHandler(t *testing.T) {
	h := newGuardHub(t)
	h.probe(http.MethodGet, probePattern, access.NeedRead, "git:read")
	alice, org := h.user("alice")
	seed(t, h.st, repo("w1", alice, org, access.StatusReady), repo("w2", alice, org, access.StatusArchived))
	tokW1, idW1 := h.pat(alice, "w1", "git:read")
	tokW2, _ := h.pat(alice, "w2", "git:read")
	seed(t, h.st, createdBy(fork("f1", alice, org, "w1"), idW1))
	tokF1, _ := h.pat(alice, "f1", "git:read")
	unbound, _ := h.pat(alice, "", "git:read")

	for _, tc := range []struct {
		name, token, slug string
		want              binding
	}{
		{"the bound workspace", tokW1, "w1", binding{"w1", access.RelationBound}},
		{"the bound workspace archived", tokW2, "w2", binding{"w2", access.RelationBound}},
		{"a fork the token created", tokW1, "f1", binding{"w1", access.RelationCreatedFork}},
		{"the bound fork", tokF1, "f1", binding{"f1", access.RelationBound}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := h.do(http.MethodGet, "/workspaces/"+tc.slug+"/probe", tc.token)
			u := h.do(http.MethodGet, "/workspaces/"+tc.slug+"/probe", unbound)
			if b.status != http.StatusOK || u.status != http.StatusOK {
				t.Fatalf("bound %d %s, unbound %d %s, want 200 for both", b.status, b.body, u.status, u.body)
			}
			if got := decode(t, b).Binding; got == nil || *got != tc.want {
				t.Errorf("bound PAT: the handler found %+v, want %+v", got, tc.want)
			}
			if got := decode(t, u).Binding; got != nil {
				t.Errorf("unbound PAT: the handler found %+v, want none", got)
			}
		})
	}
	if got := h.refusals.all(); len(got) != 0 {
		t.Errorf("refusals = %+v, want none", got)
	}
}

func TestGuard_AnswersEveryWorkspaceOutsideTheBindingNotFoundBeforeScopeAndAccess(t *testing.T) {
	h := newGuardHub(t)
	h.probe(http.MethodGet, probePattern, access.NeedRead, "git:read")
	alice, org := h.user("alice")
	bob, bobOrg := h.user("bob")
	seed(t, h.st, repo("w1", alice, org, access.StatusReady), repo("w5", alice, org, access.StatusReady),
		repo("w6", bob, bobOrg, access.StatusReady))
	tokA, idA := h.pat(alice, "w1", "git:read")
	tokNoScope, _ := h.pat(alice, "w1", "changes:read")
	_, idB := h.pat(alice, "w1", "git:read")
	_, idBob := h.pat(bob, "", "git:read")
	seed(t, h.st,
		createdBy(fork("f1", alice, org, "w1"), idA),
		createdBy(fork("f2", alice, org, "w1"), idB),
		fork("f3", alice, org, "w1"),
		createdBy(fork("f4", bob, org, "w1"), idBob),
		fork("f9", alice, org, "w1"))
	tokF, idF := h.pat(alice, "f1", "git:read")

	type row struct{ token, tokenID, bound, slug string }
	rows := []row{
		{tokA, idA, "w1", "f2"}, // created by another token of the same user
		{tokA, idA, "w1", "f3"}, // created with no token
		{tokA, idA, "w1", "f4"}, // created by another user's token
		{tokF, idF, "f1", "f9"}, // a sibling of the bound fork
	}
	for _, slug := range []string{"w5", "w6", "nosuch", "f9"} {
		rows = append(rows, row{tokA, idA, "w1", slug}, row{tokNoScope, "", "w1", slug})
	}
	var bodies [][]byte
	for _, r := range rows {
		a := h.do(http.MethodGet, "/workspaces/"+r.slug+"/probe", r.token)
		e := decode(t, a)
		if a.status != http.StatusNotFound || e.Error.ErrorType != "not_found" || e.Error.Message != "workspace not found" {
			t.Errorf("token bound to %s on %s = %d %s, want 404 not_found workspace not found", r.bound, r.slug, a.status, a.body)
		}
		bodies = append(bodies, a.body)
	}
	for i, b := range bodies {
		if !bytes.Equal(b, bodies[0]) {
			t.Errorf("body of row %d = %s, want the same as row 0, %s", i, b, bodies[0])
		}
	}
	if n := h.calls.Load(); n != 0 {
		t.Errorf("the probe ran %d times, want 0", n)
	}
	got := h.refusals.all()
	if len(got) != len(rows) {
		t.Fatalf("%d refusals reported, want one per request, %d", len(got), len(rows))
	}
	for i, f := range got {
		r := rows[i]
		if f.RequestedWorkspace == nil || *f.RequestedWorkspace != r.slug || f.BoundWorkspace != r.bound ||
			f.Status != http.StatusNotFound || f.Method != http.MethodGet || f.Route != "/api/v1"+probePattern ||
			f.Surface != "api" || f.UserID != alice || r.tokenID != "" && f.TokenID != r.tokenID {
			t.Errorf("refusal %d = %+v, want the request of the token bound to %s on %s", i, f, r.bound, r.slug)
		}
	}
}

func TestGuard_LeavesScopeAndAccessToDecideAsForAnUnboundToken(t *testing.T) {
	h := newGuardHub(t)
	h.probe(http.MethodGet, probePattern, access.NeedRead, "git:read")
	h.probe(http.MethodGet, "/workspaces/:slug/probe-write", access.NeedWrite, "git:read")
	alice, org := h.user("alice")
	carol, _ := h.user("carol")
	seed(t, h.st, repo("w1", alice, org, access.StatusReady),
		repo("w7", alice, "org7", access.StatusReady))

	for _, tc := range []struct {
		name, user, slug, route string
		perms                   []string
		member                  bool
		status                  int
	}{
		{"a user no longer in the workspace's org", carol, "w7", "probe", []string{"git:read"}, false, http.StatusNotFound},
		{"a token without the route's scope", alice, "w1", "probe", []string{"changes:read"}, false, http.StatusForbidden},
		{"a member on a route that needs write", carol, "w7", "probe-write", []string{"git:read"}, true, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h.orgs[carol] = nil
			if tc.member {
				h.orgs[carol] = []string{"org7"}
			}
			bound, _ := h.pat(tc.user, tc.slug, tc.perms...)
			unbound, _ := h.pat(tc.user, "", tc.perms...)
			path := "/workspaces/" + tc.slug + "/" + tc.route
			b, u := h.do(http.MethodGet, path, bound), h.do(http.MethodGet, path, unbound)
			if b.status != tc.status || u.status != b.status || !bytes.Equal(b.body, u.body) {
				t.Errorf("bound %d %s, unbound %d %s, want %d and the same body", b.status, b.body, u.status, u.body, tc.status)
			}
		})
	}
	if got := h.refusals.all(); len(got) != 0 {
		t.Errorf("refusals = %+v, want none", got)
	}
}

// parentWithFork is a guard hub where alice owns p1 and its fork f1, with
// tokF, alice's PAT [git:read] bound to f1.
func parentWithFork(t *testing.T) (h *guardHub, alice, org, tokF string) {
	h = newGuardHub(t)
	alice, org = h.user("alice")
	seed(t, h.st, repo("p1", alice, org, access.StatusReady), fork("f1", alice, org, "p1"))
	tokF, _ = h.pat(alice, "f1", "git:read")
	return h, alice, org, tokF
}

func TestGuard_LetsAForkTokenOntoTheContributorRoutesOfTheParent(t *testing.T) {
	h, _, _, tokF := parentWithFork(t)
	rows := []struct{ method, pattern, path string }{
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
	table := access.ContributorRoutes()
	if len(rows) != 25 || len(table) != 25 {
		t.Fatalf("%d rows and %d contributor routes, want 25 of each", len(rows), len(table))
	}
	want := binding{"f1", access.RelationParentOfBoundFork}
	for _, r := range rows {
		if !slices.Contains(table, access.ContributorRoute{Method: r.method, Pattern: r.pattern}) {
			t.Errorf("%s %s is not in the contributor table", r.method, r.pattern)
		}
		// One server per route: the router cannot hold /changes/by-commit/:sha
		// together with the routes under /changes/:number.
		s := h.fresh()
		s.probe(r.method, "/workspaces/:slug"+r.pattern, access.NeedRead, "git:read")
		a := s.do(r.method, "/workspaces/p1"+r.path, tokF)
		if a.status != http.StatusOK {
			t.Errorf("%s p1%s = %d %s, want 200", r.method, r.path, a.status, a.body)
			continue
		}
		if got := decode(t, a).Binding; got == nil || *got != want {
			t.Errorf("%s p1%s: the handler found %+v, want %+v", r.method, r.path, got, want)
		}
	}
	if got := h.refusals.all(); len(got) != 0 {
		t.Errorf("refusals = %+v, want none", got)
	}
}

func TestGuard_RefusesAForkTokenEveryOtherRouteOfTheParent(t *testing.T) {
	h, alice, _, tokF := parentWithFork(t)
	routes := []struct{ method, pattern, path string }{
		{http.MethodGet, "/probe", "/probe"},
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
	for _, r := range routes {
		h.probe(r.method, "/workspaces/:slug"+r.pattern, access.NeedRead, "git:read")
	}
	for _, r := range routes {
		a := h.do(r.method, "/workspaces/p1"+r.path, tokF)
		if e := decode(t, a); a.status != http.StatusForbidden || e.Error.ErrorType != "token_bound" {
			t.Errorf("%s p1%s = %d %s, want 403 token_bound", r.method, r.path, a.status, a.body)
		}
	}
	if n := h.calls.Load(); n != 0 {
		t.Errorf("the probes ran %d times, want 0", n)
	}
	got := h.refusals.all()
	if len(got) != len(routes) {
		t.Fatalf("%d refusals reported, want one per request, %d", len(got), len(routes))
	}
	for i, f := range got {
		r := routes[i]
		if f.Status != http.StatusForbidden || f.Method != r.method || f.Route != "/api/v1/workspaces/:slug"+r.pattern ||
			f.RequestedWorkspace == nil || *f.RequestedWorkspace != "p1" || f.BoundWorkspace != "f1" || f.UserID != alice {
			t.Errorf("refusal %d = %+v, want the 403 of %s p1%s", i, f, r.method, r.path)
		}
	}
}

func TestGuard_OpensTheContributorRoutesOnlyOnTheBoundForksOwnParent(t *testing.T) {
	h, alice, org, tokF := parentWithFork(t)
	h.probe(http.MethodGet, "/workspaces/:slug/changes", access.NeedRead, "git:read")
	seed(t, h.st, repo("p2", alice, org, access.StatusReady), fork("f9", alice, org, "p1"))
	tokP, _ := h.pat(alice, "p1", "git:read")

	for _, tc := range []struct {
		name, token, slug string
		status            int
	}{
		{"a fork token on another workspace", tokF, "p2", http.StatusNotFound},
		{"a repository token on another workspace", tokP, "p2", http.StatusNotFound},
		{"a fork token on a sibling fork", tokF, "f9", http.StatusNotFound},
		{"a repository token on its own workspace", tokP, "p1", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := h.do(http.MethodGet, "/workspaces/"+tc.slug+"/changes", tc.token)
			e := decode(t, a)
			switch {
			case a.status != tc.status:
				t.Errorf("GET %s/changes = %d %s, want %d", tc.slug, a.status, a.body, tc.status)
			case tc.status == http.StatusNotFound && e.Error.Message != "workspace not found":
				t.Errorf("message = %q, want workspace not found", e.Error.Message)
			case tc.status == http.StatusOK && (e.Binding == nil || *e.Binding != binding{"p1", access.RelationBound}):
				t.Errorf("the handler found %+v, want p1 bound", e.Binding)
			}
		})
	}
}

package secret_test

import (
	"bytes"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/secret"
)

func TestVarRoutes_HoldAPATToTheVarScopes(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	pat := func(perm string) string {
		return w.IssuePAT(t, identity.PATSpec{UserID: w.ownerID, Permissions: []string{perm}})
	}
	// ROUTE_KEY is held in every scope while PATs without the scope of a
	// route call it, so that a change they were let make would show.
	for _, pl := range w.varPlaces() {
		w.putVars(t, w.ownerKey, pl.path, "ROUTE_KEY", "held")
	}
	before := w.allVars(t)
	for _, perm := range []string{"secrets:write", "vars:read"} {
		cred := pat(perm)
		for _, r := range varRoutes {
			if perm == r.need {
				if a := w.do(t, cred, r.method, r.at("acme"), r.body); a.status != r.ok {
					t.Errorf("%v with %s = %d %s, want %d", r, perm, a.status, a.body, r.ok)
				}
				continue
			}
			names := []string{"acme", "no-such"}
			if r.scope == secret.ScopeUser {
				names = names[:1]
			}
			var first []byte
			for i, name := range names {
				a := w.do(t, cred, r.method, r.at(name), r.body)
				if typ, _ := errorOf(t, a); a.status != http.StatusForbidden || typ != "insufficient_scope" || i > 0 && !bytes.Equal(a.body, first) {
					t.Errorf("%v on %q with %s = %d %s, want 403 insufficient_scope, the body on acme %s", r, name, perm, a.status, a.body, first)
				}
				if i == 0 {
					first = a.body
				}
			}
		}
	}
	if got := w.allVars(t); !slices.Equal(got, before) {
		t.Errorf("after the refusals the store holds %v, want %v", got, before)
	}
	for _, pl := range w.varPlaces() {
		if a := w.do(t, w.ownerKey, http.MethodDelete, pl.path+"/ROUTE_KEY", nil); a.status != http.StatusNoContent {
			t.Fatalf("remove the held ROUTE_KEY of %s = %d %s, want 204", pl.path, a.status, a.body)
		}
	}
	// Each credential served calls every route in order, which creates,
	// changes and removes ROUTE_KEY in each scope it reaches.
	served := []struct {
		name, cred string
		user       bool
	}{
		{"a PAT with vars:write alone", pat("vars:write"), true},
		{"an API key", w.ownerKey, true},
		{"the admin token", w.Token, false},
	}
	for _, s := range served {
		for _, r := range varRoutes {
			if r.scope == secret.ScopeUser && !s.user {
				continue
			}
			if a := w.do(t, s.cred, r.method, r.at("acme"), r.body); a.status != r.ok {
				t.Errorf("%v as %s = %d %s, want %d", r, s.name, a.status, a.body, r.ok)
			}
		}
	}
}

func TestVarRoutes_RefuseEveryCallerOutsideTheirScope(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.putVars(t, w.ownerKey, "/orgs/acme/vars", "ROUTE_KEY", "held")
	w.putVars(t, w.ownerKey, "/workspaces/acme/vars", "A", "held", "ROUTE_KEY", "held")
	before := w.allVars(t)
	const orgNotFound, noWorkspace = "org not found", "workspace not found"
	// The refusals asked of every route of a scope, in order; same: the
	// body is the one of the refusal before, byte for byte.
	refusals := map[secret.Scope][]struct {
		name, cred, at string
		status         int
		typ, msg       string
		same           bool
	}{
		secret.ScopeUser: {{"the admin token", w.Token, "", http.StatusForbidden, "forbidden", "user-scoped endpoints require a user credential", false}},
		secret.ScopeOrg: {
			{"a stranger, unknown org", w.outsiderKey, "no-such", http.StatusNotFound, "not_found", orgNotFound, false},
			{"a stranger", w.outsiderKey, "acme", http.StatusNotFound, "not_found", orgNotFound, true},
			{"a member of another org, unknown org", w.ownerKey, "no-such", http.StatusNotFound, "not_found", orgNotFound, true},
			{"a member of a blocked org", w.memberKey, "frozen", http.StatusNotFound, "not_found", orgNotFound, true},
		},
		secret.ScopeWorkspace: {
			{"a stranger, unknown slug", w.outsiderKey, "no-such", http.StatusNotFound, "not_found", noWorkspace, false},
			{"a stranger", w.outsiderKey, "acme", http.StatusNotFound, "not_found", noWorkspace, true},
			{"the owner, unknown slug", w.ownerKey, "no-such", http.StatusNotFound, "not_found", noWorkspace, true},
		},
	}
	for _, r := range varRoutes {
		var last []byte
		for _, c := range refusals[r.scope] {
			a := w.do(t, c.cred, r.method, r.at(c.at), r.body)
			if typ, msg := errorOf(t, a); a.status != c.status || typ != c.typ || msg != c.msg || c.same && !bytes.Equal(a.body, last) {
				t.Errorf("%v, %s = %d %s; want %d %s %q, the same body as %s", r, c.name, a.status, a.body, c.status, c.typ, c.msg, last)
			}
			last = a.body
		}
	}
	got := w.allVars(t)
	if !slices.Equal(got, before) {
		t.Errorf("after the refusals the store holds %v, want %v", got, before)
	}
	for _, v := range got {
		if strings.HasPrefix(v, string(secret.ScopeUser)+"/") {
			t.Errorf("the store holds the user variable %s", v)
		}
	}
}

func TestVarRoutes_ActOnTheScopeTheCallerNames(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	t.Run("a user's own variables", func(t *testing.T) {
		w.putVars(t, w.ownerKey, "/user/vars", "A", "1")
		if got := keysOf(w.listVars(t, w.ownerKey, "/user/vars")); !slices.Equal(got, []string{"A"}) {
			t.Errorf("the owner's list = %v, want A", got)
		}
		for _, other := range []struct{ name, cred string }{{"member", w.memberKey}, {"member2", w.member2Key}, {"outsider", w.outsiderKey}} {
			if got := w.listVars(t, other.cred, "/user/vars"); len(got) != 0 {
				t.Errorf("%s's list = %v, want nothing", other.name, got)
			}
		}
		a := w.do(t, w.memberKey, http.MethodPatch, "/user/vars/A", changed)
		if typ, msg := errorOf(t, a); a.status != http.StatusNotFound || typ != "not_found" || msg != `key "A" not found` {
			t.Errorf("member's PATCH of A = %d %s, want 404 not_found key \"A\" not found", a.status, a.body)
		}
		if got := w.storedVars(t, secret.ScopeUser, w.ownerID); !slices.Equal(got, []string{"A=1"}) {
			t.Errorf("the owner's user scope holds %v, want A=1", got)
		}
	})
	t.Run("an org's variables by its slug or its id", func(t *testing.T) {
		w.putVars(t, w.memberKey, "/orgs/acme/vars", "A", "1")
		bySlug := w.listVars(t, w.ownerKey, "/orgs/acme/vars")
		if got := keysOf(bySlug); !slices.Equal(got, []string{"A"}) {
			t.Errorf("the org's list by slug = %v, want A", got)
		}
		if byID := w.listVars(t, w.ownerKey, "/orgs/"+w.orgID+"/vars"); !slices.Equal(byID, bySlug) {
			t.Errorf("the org's list by id = %v, want %v", byID, bySlug)
		}
		if got := w.storedVars(t, secret.ScopeOrg, w.orgID); !slices.Equal(got, []string{"A=1"}) {
			t.Errorf("the org scope of acme holds %v, want A=1", got)
		}
		if v := varOf(t, w.do(t, w.Token, http.MethodPatch, "/orgs/acme/vars/A", changed), http.StatusOK); v.Value != "changed" {
			t.Errorf("the admin token's PATCH of A answered %v, want the value changed", v)
		}
		if a := w.do(t, w.Token, http.MethodDelete, "/orgs/"+w.orgID+"/vars/A", nil); a.status != http.StatusNoContent {
			t.Errorf("the admin token's DELETE of A = %d %s, want 204", a.status, a.body)
		}
		if got := w.storedVars(t, secret.ScopeOrg, w.orgID); len(got) != 0 {
			t.Errorf("after the delete the org scope of acme holds %v, want nothing", got)
		}
		w.putVars(t, w.Token, "/orgs/frozen/vars", "B", "2")
		if got := keysOf(w.listVars(t, w.Token, "/orgs/frozen/vars")); !slices.Equal(got, []string{"B"}) {
			t.Errorf("the admin token's list of the blocked org = %v, want B", got)
		}
	})
}

func TestWorkspaceVarRoutes_ServeAlikeWhateverTheWorkspacesState(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	const col, readOnly = "/workspaces/acme/vars", "org members have read-only access to this workspace"
	w.putVars(t, w.ownerKey, col, "A", "held")
	states := []struct{ name, status, marker string }{
		{"pending", access.StatusPending, ""},
		{"cloning", access.StatusCloning, ""},
		{"ready", access.StatusReady, ""},
		{"failed", access.StatusFailed, ""},
		{"archived", access.StatusArchived, ""},
		{"a reclone running", access.StatusReady, "reclone"},
		{"an archive running", access.StatusReady, "archive"},
		{"a delete running", access.StatusReady, "delete"},
	}
	for _, s := range states {
		t.Run(s.name, func(t *testing.T) {
			if _, err := w.Store.Main().Exec(`UPDATE workspaces SET status = ?, maintenance = ? WHERE slug = 'acme'`, s.status, s.marker); err != nil {
				t.Fatal(err)
			}
			for _, c := range []struct{ name, cred string }{{"the owner", w.ownerKey}, {"the admin token", w.Token}} {
				steps := []struct {
					method, path string
					body         any
					status       int
				}{
					{http.MethodPost, col, entries(map[string]any{"key": "K", "value": "1"}), http.StatusCreated},
					{http.MethodGet, col, nil, http.StatusOK},
					{http.MethodPatch, col + "/K", changed, http.StatusOK},
					{http.MethodGet, col + "/resolved", nil, http.StatusOK},
					{http.MethodDelete, col + "/k", nil, http.StatusNoContent},
				}
				for _, st := range steps {
					if a := w.do(t, c.cred, st.method, st.path, st.body); a.status != st.status {
						t.Errorf("%s %s as %s = %d %s, want %d", st.method, st.path, c.name, a.status, a.body, st.status)
					}
				}
			}
			before := w.storedVars(t, secret.ScopeWorkspace, "acme")
			refused := []struct {
				name, method, path string
				body               any
			}{
				{"a create", http.MethodPost, col, keyed("NEW")},
				{"a create with an invalid key", http.MethodPost, col, keyed("1A")},
				{"a create whose body is not JSON", http.MethodPost, col, "{"},
				{"an update", http.MethodPatch, col + "/A", changed},
				{"an update with a null value", http.MethodPatch, col + "/A", map[string]any{"value": nil}},
				{"a delete", http.MethodDelete, col + "/A", nil},
			}
			for _, r := range refused {
				a := w.do(t, w.memberKey, r.method, r.path, r.body)
				if typ, msg := errorOf(t, a); a.status != http.StatusForbidden || typ != "forbidden" || msg != readOnly {
					t.Errorf("%s by a member = %d %s, want 403 forbidden %q", r.name, a.status, a.body, readOnly)
				}
			}
			if got := w.storedVars(t, secret.ScopeWorkspace, "acme"); !slices.Equal(got, before) || !slices.Equal(got, []string{"A=held"}) {
				t.Errorf("after the member's writes the workspace holds %v, want A=held", got)
			}
			if got := keysOf(w.listVars(t, w.memberKey, col)); !slices.Equal(got, []string{"A"}) {
				t.Errorf("the member's list = %v, want A", got)
			}
		})
	}
}

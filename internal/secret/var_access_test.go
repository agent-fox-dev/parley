package secret_test

import (
	"bytes"
	"fmt"
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
		w.create(t, w.ownerKey, pl.path, "ROUTE_KEY=held")
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
			// A user path has no name, so its second call is its first.
			first := w.do(t, cred, r.method, r.at("acme"), r.body)
			wantError(t, fmt.Sprintf("%v on acme with %s", r, perm), first, http.StatusForbidden, "insufficient_scope", "")
			if a := w.do(t, cred, r.method, r.at("no-such"), r.body); a.status != first.status || !bytes.Equal(a.body, first.body) {
				t.Errorf("%v on no-such with %s = %d %s, want the body on acme %s", r, perm, a.status, a.body, first.body)
			}
		}
	}
	if got := w.allVars(t); !slices.Equal(got, before) {
		t.Errorf("after the refusals the store holds %v, want %v", got, before)
	}
	for _, pl := range w.varPlaces() {
		w.must(t, w.ownerKey, http.MethodDelete, pl.path+"/ROUTE_KEY", nil, http.StatusNoContent)
	}
	// Each credential served calls every route in order, which creates,
	// changes and removes ROUTE_KEY in each scope it reaches.
	for _, s := range []struct {
		name, cred string
		user       bool
	}{
		{"a PAT with vars:write alone", pat("vars:write"), true},
		{"an API key", w.ownerKey, true},
		{"the admin token", w.Token, false},
	} {
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
	w.create(t, w.ownerKey, "/orgs/acme/vars", "ROUTE_KEY=held")
	w.create(t, w.ownerKey, "/workspaces/acme/vars", "A=held", "ROUTE_KEY=held")
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
			wantError(t, fmt.Sprintf("%v, %s", r, c.name), a, c.status, c.typ, c.msg)
			if c.same && !bytes.Equal(a.body, last) {
				t.Errorf("%v, %s = %s; want the same body as %s", r, c.name, a.body, last)
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
		w.create(t, w.ownerKey, "/user/vars", "A=1")
		for _, c := range []struct {
			name, cred string
			want       []string
		}{{"owner", w.ownerKey, []string{"A"}}, {"member", w.memberKey, nil}, {"member2", w.member2Key, nil}, {"outsider", w.outsiderKey, nil}} {
			if got := keysOf(w.list(t, c.cred, "/user/vars")); !slices.Equal(got, c.want) {
				t.Errorf("%s's list = %v, want %v", c.name, got, c.want)
			}
		}
		wantError(t, "member's PATCH of A", w.do(t, w.memberKey, http.MethodPatch, "/user/vars/A", changed), http.StatusNotFound, "not_found", `key "A" not found`)
		if got := w.storedVars(t, secret.ScopeUser, w.ownerID); !slices.Equal(got, []string{"A=1"}) {
			t.Errorf("the owner's user scope holds %v, want A=1", got)
		}
	})
	t.Run("an org's variables by its slug or its id", func(t *testing.T) {
		w.create(t, w.memberKey, "/orgs/acme/vars", "A=1")
		bySlug := w.list(t, w.ownerKey, "/orgs/acme/vars")
		if byID := w.list(t, w.ownerKey, "/orgs/"+w.orgID+"/vars"); !slices.Equal(keysOf(bySlug), []string{"A"}) || !slices.Equal(byID, bySlug) {
			t.Errorf("the org's list by slug = %v, by id %v; want A in both", bySlug, byID)
		}
		if got := w.storedVars(t, secret.ScopeOrg, w.orgID); !slices.Equal(got, []string{"A=1"}) {
			t.Errorf("the org scope of acme holds %v, want A=1", got)
		}
		if v := varOf(t, w.do(t, w.Token, http.MethodPatch, "/orgs/acme/vars/A", changed), http.StatusOK); v.Value != "changed" {
			t.Errorf("the admin token's PATCH of A answered %v, want the value changed", v)
		}
		w.must(t, w.Token, http.MethodDelete, "/orgs/"+w.orgID+"/vars/A", nil, http.StatusNoContent)
		if got := w.storedVars(t, secret.ScopeOrg, w.orgID); len(got) != 0 {
			t.Errorf("after the delete the org scope of acme holds %v, want nothing", got)
		}
		w.create(t, w.Token, "/orgs/frozen/vars", "B=2")
		if got := keysOf(w.list(t, w.Token, "/orgs/frozen/vars")); !slices.Equal(got, []string{"B"}) {
			t.Errorf("the admin token's list of the blocked org = %v, want B", got)
		}
	})
}

func TestWorkspaceVarRoutes_ServeAlikeWhateverTheWorkspacesState(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	const col, readOnly = "/workspaces/acme/vars", "org members have read-only access to this workspace"
	w.create(t, w.ownerKey, col, "A=held")
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
			w.exec(t, `UPDATE workspaces SET status = ?, maintenance = ? WHERE slug = 'acme'`, s.status, s.marker)
			for _, cred := range []string{w.ownerKey, w.Token} {
				w.create(t, cred, col, "K=1")
				w.list(t, cred, col)
				w.must(t, cred, http.MethodPatch, col+"/K", changed, http.StatusOK)
				w.resolve(t, cred, "acme")
				w.must(t, cred, http.MethodDelete, col+"/k", nil, http.StatusNoContent)
			}
			for _, r := range []struct {
				name, method, path string
				body               any
			}{
				{"a create", http.MethodPost, col, keyed("NEW")},
				{"a create with an invalid key", http.MethodPost, col, keyed("1A")},
				{"a create whose body is not JSON", http.MethodPost, col, "{"},
				{"an update", http.MethodPatch, col + "/A", changed},
				{"an update with a null value", http.MethodPatch, col + "/A", map[string]any{"value": nil}},
				{"a delete", http.MethodDelete, col + "/A", nil},
			} {
				wantError(t, r.name+" by a member", w.do(t, w.memberKey, r.method, r.path, r.body), http.StatusForbidden, "forbidden", readOnly)
			}
			if got := w.storedVars(t, secret.ScopeWorkspace, "acme"); !slices.Equal(got, []string{"A=held"}) {
				t.Errorf("after the member's writes the workspace holds %v, want A=held", got)
			}
			if got := keysOf(w.list(t, w.memberKey, col)); !slices.Equal(got, []string{"A"}) {
				t.Errorf("the member's list = %v, want A", got)
			}
		})
	}
}

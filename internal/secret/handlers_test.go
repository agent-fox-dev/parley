package secret_test

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/secret"
)

func TestUserSecrets_AreTheCallersOwn(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.seed(t, secret.ScopeUser, w.ownerID, "K")
	if got := pageOf(t, w.do(t, w.outsiderKey, http.MethodGet, "/user/secrets", nil)).keys(t); len(got) != 0 {
		t.Errorf("another user's list = %v, want []", got)
	}
	w.seed(t, secret.ScopeUser, w.outsiderID, "K")
	for _, key := range []string{w.ownerKey, w.outsiderKey} {
		if got := pageOf(t, w.do(t, key, http.MethodGet, "/user/secrets", nil)).keys(t); !slices.Equal(got, []string{"K"}) {
			t.Errorf("a user's list = %v, want [K]", got)
		}
	}
}

func TestUserSecretRoutes_RefuseTheAdminToken(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	for _, r := range routesOf(secret.ScopeUser) {
		a := w.do(t, w.Token, r.method, r.at(""), r.body)
		typ, msg := errorOf(t, a)
		if a.status != http.StatusForbidden || typ != "forbidden" || msg != "user-scoped endpoints require a user credential" {
			t.Errorf("%v as the admin token = %d %s, want 403 forbidden with the user credential message", r, a.status, a.body)
		}
	}
}

func TestOrgSecrets_AreSharedByTheOrgsMembersAndTheAdminToken(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.seed(t, secret.ScopeOrg, w.orgID, "K")
	var personal string
	if err := w.Store.Main().QueryRowContext(context.Background(),
		`SELECT id FROM orgs WHERE name = 'member'`).Scan(&personal); err != nil {
		t.Fatalf("read the personal org of member: %v", err)
	}
	cases := []struct {
		name, cred, path string
		want             []string
	}{
		{"another member by slug", w.member2Key, "/orgs/acme/secrets", []string{"K"}},
		{"the admin token by id", w.Token, "/orgs/" + w.orgID + "/secrets", []string{"K"}},
		{"a member by id", w.memberKey, "/orgs/" + w.orgID + "/secrets", []string{"K"}},
		{"a member in their personal org", w.memberKey, "/orgs/" + personal + "/secrets", []string{}},
	}
	for _, tc := range cases {
		if got := pageOf(t, w.do(t, tc.cred, http.MethodGet, tc.path, nil)).keys(t); !slices.Equal(got, tc.want) {
			t.Errorf("%s: list = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestOrgSecretRoutes_AnswerOneNotFoundOutsideTheOrg(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	for _, r := range routesOf(secret.ScopeOrg) {
		cases := []struct{ name, cred, org string }{
			{"unknown org", w.memberKey, "no-such"},
			{"not a member", w.outsiderKey, "acme"},
			{"blocked org, a member", w.memberKey, "frozen"},
		}
		var first []byte
		for _, tc := range cases {
			a := w.do(t, tc.cred, r.method, r.at(tc.org), r.body)
			if typ, _ := errorOf(t, a); a.status != http.StatusNotFound || typ != "not_found" {
				t.Errorf("%v, %s = %d %s, want 404 not_found", r, tc.name, a.status, a.body)
			}
			if first == nil {
				first = a.body
			} else if !bytes.Equal(a.body, first) {
				t.Errorf("%v, %s: body %s differs from %s", r, tc.name, a.body, first)
			}
		}
	}
	if a := w.do(t, w.Token, http.MethodGet, "/orgs/frozen/secrets", nil); a.status != http.StatusOK {
		t.Errorf("the admin token's list of the blocked org = %d %s, want 200", a.status, a.body)
	}
}

func TestWorkspaceSecrets_BelongToExactlyOneWorkspace(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.seed(t, secret.ScopeWorkspace, "acme", "P")
	w.seed(t, secret.ScopeWorkspace, "acme-fork", "F")
	for _, cred := range []string{w.ownerKey, w.Token} {
		for slug, want := range map[string]string{"acme": "P", "acme-fork": "F"} {
			if got := pageOf(t, w.do(t, cred, http.MethodGet, "/workspaces/"+slug+"/secrets", nil)).keys(t); !slices.Equal(got, []string{want}) {
				t.Errorf("list of %s = %v, want [%s]", slug, got, want)
			}
		}
	}
}

func TestWorkspaceSecretRoutes_RefuseAMember(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.seed(t, secret.ScopeWorkspace, "acme", "P")
	for _, r := range routesOf(secret.ScopeWorkspace) {
		for name, cred := range map[string]string{"an org member": w.member2Key, "the fork's creator": w.memberKey} {
			a := w.do(t, cred, r.method, r.at("acme"), r.body)
			typ, msg := errorOf(t, a)
			if a.status != http.StatusForbidden || typ != "forbidden" || msg != "org members have read-only access to this workspace" {
				t.Errorf("%v as %s = %d %s, want 403 forbidden read-only", r, name, a.status, a.body)
			}
		}
	}
	if n := w.count(t, secret.ScopeWorkspace, "acme"); n != 1 {
		t.Errorf("acme holds %d secrets, want 1", n)
	}
}

func TestWorkspaceSecretRoutes_AnswerOneNotFoundToAStranger(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	for _, r := range routesOf(secret.ScopeWorkspace) {
		a := w.do(t, w.outsiderKey, r.method, r.at("acme"), r.body)
		b := w.do(t, w.outsiderKey, r.method, r.at("no-such"), r.body)
		_, msg := errorOf(t, a)
		if a.status != http.StatusNotFound || b.status != http.StatusNotFound || !bytes.Equal(a.body, b.body) || msg != "workspace not found" {
			t.Errorf("%v: acme = %d %s, no-such = %d %s, want the same 404 workspace not found", r, a.status, a.body, b.status, b.body)
		}
	}
}

func TestSecretRoutes_HoldAPATToTheSecretScopes(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	pat := func(perms ...string) string {
		return w.IssuePAT(t, identity.PATSpec{UserID: w.ownerID, Permissions: perms})
	}
	vars, read, write := pat("vars:read"), pat("secrets:read"), pat("secrets:write")
	cases := []struct {
		r          route
		org, token string
		want       int
	}{
		{routes[0], "", vars, http.StatusForbidden},
		{routes[1], "acme", vars, http.StatusForbidden},
		{routes[1], "no-such", vars, http.StatusForbidden},
		{routes[2], "acme", vars, http.StatusForbidden},
		{routes[2], "no-such", vars, http.StatusForbidden},
		{routes[0], "", read, http.StatusOK},
		{routes[1], "acme", read, http.StatusOK},
		{routes[2], "acme", read, http.StatusOK},
		{routes[0], "", write, http.StatusOK},
		{routes[1], "acme", write, http.StatusOK},
		{routes[2], "acme", write, http.StatusOK},
	}
	for _, tc := range cases {
		a := w.do(t, tc.token, tc.r.method, tc.r.at(tc.org), tc.r.body)
		if a.status != tc.want {
			t.Errorf("%v on %q = %d %s, want %d", tc.r, tc.org, a.status, a.body, tc.want)
		} else if a.status == http.StatusForbidden {
			if typ, _ := errorOf(t, a); typ != "insufficient_scope" {
				t.Errorf("%v on %q = %s, want insufficient_scope", tc.r, tc.org, a.body)
			}
		}
	}
}

func TestListSecrets_PagesTheKeysInCaseFoldedOrder(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	stored := []string{"b", "A", "c", "_x", "B2", "a1"}
	want := []string{"_x", "A", "a1", "b", "B2", "c"}
	scopes := []struct {
		scope    secret.Scope
		id, name string
	}{
		{secret.ScopeUser, w.ownerID, ""},
		{secret.ScopeOrg, w.orgID, "acme"},
		{secret.ScopeWorkspace, "acme", "acme"},
	}
	for _, s := range scopes {
		path := routesOf(s.scope)[0].at(s.name)
		empty := w.do(t, w.ownerKey, http.MethodGet, path, nil)
		if !bytes.Contains(empty.body, []byte(`"items":[]`)) {
			t.Errorf("%s: an empty list = %s, want \"items\":[]", path, empty.body)
		}
		w.seed(t, s.scope, s.id, stored...)

		all := w.do(t, w.ownerKey, http.MethodGet, path, nil)
		p := pageOf(t, all)
		if got := p.keys(t); !slices.Equal(got, want) || p.NextCursor != nil {
			t.Errorf("%s: list = %v, next_cursor %v; want %v and null", path, got, p.NextCursor, want)
		}
		for _, it := range p.Items {
			members := slices.Sorted(maps.Keys(it))
			if !slices.Equal(members, []string{"created_at", "key", "updated_at"}) {
				t.Errorf("%s: item members = %v, want exactly key, created_at, updated_at", path, members)
			}
			var created string
			if json.Unmarshal(it["created_at"], &created) != nil {
				t.Errorf("%s: created_at %s is not a string", path, it["created_at"])
			} else if _, err := time.Parse(time.RFC3339, created); err != nil {
				t.Errorf("%s: created_at %q: %v", path, created, err)
			}
		}
		for _, k := range stored {
			if strings.Contains(string(all.body), valueOf(k)) {
				t.Errorf("%s: the list holds the value of %s", path, k)
			}
		}

		first := pageOf(t, w.do(t, w.ownerKey, http.MethodGet, path+"?limit=4", nil))
		if len(first.Items) != 4 || first.NextCursor == nil {
			t.Fatalf("%s: page 1 has %d items and cursor %v, want 4 and a cursor", path, len(first.Items), first.NextCursor)
		}
		second := pageOf(t, w.do(t, w.ownerKey, http.MethodGet, path+"?limit=4&cursor="+url.QueryEscape(*first.NextCursor), nil))
		if got := append(first.keys(t), second.keys(t)...); len(second.Items) != 2 || second.NextCursor != nil || !slices.Equal(got, want) {
			t.Errorf("%s: pages = %v, page 2 cursor %v; want %v and null", path, got, second.NextCursor, want)
		}
		for _, limit := range []string{"0", "201"} {
			a := w.do(t, w.ownerKey, http.MethodGet, path+"?limit="+limit, nil)
			if typ, _ := errorOf(t, a); a.status != http.StatusBadRequest || typ != "invalid_request" {
				t.Errorf("%s?limit=%s = %d %s, want 400 invalid_request", path, limit, a.status, a.body)
			}
		}
	}
}

func TestListSecrets_ListsEveryEntryWithoutAKeyThatOpensThem(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.seed(t, secret.ScopeWorkspace, "acme", "ONE", "TWO")
	_, other := newKey(t)
	for name, env := range map[string]map[string]string{
		"no key":      nil,
		"another key": {secret.EnvCurrentKey: other},
	} {
		w.RestartEnv(t, env)
		if got := pageOf(t, w.do(t, w.ownerKey, http.MethodGet, "/workspaces/acme/secrets", nil)).keys(t); !slices.Equal(got, []string{"ONE", "TWO"}) {
			t.Errorf("%s: list = %v, want [ONE TWO]", name, got)
		}
	}
}

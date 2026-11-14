package secret_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/event"
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
	for _, perm := range []string{"vars:read", "secrets:read", "secrets:write"} {
		pat := w.IssuePAT(t, identity.PATSpec{UserID: w.ownerID, Permissions: []string{perm}})
		for _, r := range routes {
			granted := perm == r.need || perm == "secrets:write"
			targets := []string{"acme"}
			if r.scope != secret.ScopeUser && !granted {
				targets = append(targets, "no-such")
			}
			for _, name := range targets {
				a := w.do(t, pat, r.method, r.at(name), r.body)
				typ := ""
				if !granted {
					typ, _ = errorOf(t, a)
				}
				if granted && a.status != r.ok || !granted && (a.status != http.StatusForbidden || typ != "insufficient_scope") {
					t.Errorf("%v on %q with %s = %d %s, want %d or 403 insufficient_scope", r, name, perm, a.status, a.body, r.ok)
				}
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

func TestCreateSecrets_StoresEveryEntryInRequestOrder(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	at := `"` + w.now.Truncate(time.Second).Format(time.RFC3339) + `"`
	long := strings.Repeat("k", 255)
	cases := []struct {
		name    string
		entries []map[string]any
	}{
		{"one entry", []map[string]any{{"key": "ONE", "value": "v1"}}},
		{"three entries", []map[string]any{{"key": "b", "value": "vb"}, {"key": "A", "value": "va"}, {"key": "c", "value": "vc"}}},
		{"no value", []map[string]any{{"key": "ABSENT"}}},
		{"empty value", []map[string]any{{"key": "EMPTY", "value": ""}}},
		{"underscore key", []map[string]any{{"key": "_", "value": "u"}}},
		{"longest key", []map[string]any{{"key": long, "value": "l"}}},
		{"largest value", []map[string]any{{"key": "BIG", "value": strings.Repeat("x", secret.MaxValueSize)}}},
		{"mixed case", []map[string]any{{"key": "Git_Pat", "value": "g"}}},
	}
	for _, tc := range cases {
		var body []any
		var want []string
		for _, e := range tc.entries {
			body, want = append(body, e), append(want, e["key"].(string))
		}
		a := w.do(t, w.ownerKey, http.MethodPost, "/user/secrets", entries(body...))
		p := itemsOf(t, a, http.StatusCreated)
		var top map[string]json.RawMessage
		_ = json.Unmarshal(a.body, &top)
		if got := p.keys(t); !slices.Equal(got, want) || len(top) != 1 {
			t.Errorf("%s: answer %s, want only items with keys %v", tc.name, a.body, want)
		}
		for _, it := range p.Items {
			if members := slices.Sorted(maps.Keys(it)); !slices.Equal(members, []string{"created_at", "key", "updated_at"}) {
				t.Errorf("%s: item members = %v, want exactly key, created_at, updated_at", tc.name, members)
			}
			if string(it["created_at"]) != at || string(it["updated_at"]) != at {
				t.Errorf("%s: item %s %s, want created_at and updated_at %s", tc.name, it["created_at"], it["updated_at"], at)
			}
		}
		for _, e := range tc.entries {
			wantValue, _ := e["value"].(string)
			v, found, err := w.secrets.Get(context.Background(), secret.ScopeUser, w.ownerID, e["key"].(string))
			if !found || err != nil || v != wantValue {
				t.Errorf("%s: Get(%.20s) = %.20q, %v, %v; want %.20q", tc.name, e["key"], v, found, err, wantValue)
			}
		}
	}
}

func TestCreateSecrets_RejectsAnInvalidBodyWhole(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	_, seq := w.eventsAfter(t, 0)
	long := strings.Repeat("k", 256)
	cases := []struct {
		name string
		body any
		key  string // the key the message names; "" for none
	}{
		{"no entries", map[string]any{}, ""},
		{"empty entries", entries(), ""},
		{"entries not an array", map[string]any{"entries": "x"}, ""},
		{"entry not an object", entries("x"), ""},
		{"entry without key", entries(map[string]any{"value": "v"}), ""},
		{"key not a string", entries(map[string]any{"key": 5}), ""},
		{"empty key", entries(map[string]any{"key": ""}), `""`},
		{"key too long", entries(map[string]any{"key": long}), long},
		{"key begins with a digit", entries(map[string]any{"key": "1A"}), "1A"},
		{"key with a hyphen", entries(map[string]any{"key": "A-B"}), "A-B"},
		{"key with a space", entries(map[string]any{"key": "A B"}), "A B"},
		{"key with a non-ASCII letter", entries(map[string]any{"key": "Ké"}), "Ké"},
		{"value a number", entries(map[string]any{"key": "NUM", "value": 5}), "NUM"},
		{"value null", entries(map[string]any{"key": "NUL", "value": nil}), "NUL"},
		{"value too large", entries(map[string]any{"key": "HUGE", "value": strings.Repeat("x", secret.MaxValueSize+1)}), "HUGE"},
		{"keys equal ignoring case", entries(map[string]any{"key": "Token"}, map[string]any{"key": "TOKEN"}), "TOKEN"},
		{"a valid entry, then an invalid one", entries(map[string]any{"key": "GOOD"}, map[string]any{"key": "2B"}), "2B"},
	}
	for _, tc := range cases {
		a := w.do(t, w.ownerKey, http.MethodPost, "/workspaces/acme/secrets", tc.body)
		typ, msg := errorOf(t, a)
		if a.status != http.StatusBadRequest || typ != "invalid_request" || !strings.Contains(msg, tc.key) {
			t.Errorf("%s: %d %.200s, want 400 invalid_request naming %.20q", tc.name, a.status, a.body, tc.key)
		}
	}
	if n := w.count(t, secret.ScopeWorkspace, "acme"); n != 0 {
		t.Errorf("acme holds %d secrets, want 0", n)
	}
	if recs, _ := w.eventsAfter(t, seq); len(recs) != 0 {
		t.Errorf("the refusals wrote %d events, want 0", len(recs))
	}
}

func TestCreateSecrets_RefusesToStoreWithoutACurrentKey(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	_, previous := newKey(t)
	for name, env := range map[string]map[string]string{
		"no key":            nil,
		"only previous key": {secret.EnvPreviousKey: previous},
	} {
		w.RestartEnv(t, env)
		a := w.do(t, w.ownerKey, http.MethodPost, "/user/secrets", entries(map[string]any{"key": "K", "value": "v"}))
		if typ, _ := errorOf(t, a); a.status != http.StatusServiceUnavailable || typ != "secrets_unavailable" {
			t.Errorf("%s: %d %s, want 503 secrets_unavailable", name, a.status, a.body)
		}
		if a := w.do(t, w.ownerKey, http.MethodPost, "/user/secrets", entries()); a.status != http.StatusBadRequest {
			t.Errorf("%s: an empty create = %d %s, want 400", name, a.status, a.body)
		}
		if n := w.count(t, secret.ScopeUser, w.ownerID); n != 0 {
			t.Errorf("%s: the scope holds %d secrets, want 0", name, n)
		}
	}
}

func TestCreateSecrets_KeepsAScopeToAHundredEntries(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	cases := []struct {
		name          string
		stored, added int
		existing      bool // the request's one entry is a stored key
		want          int
	}{
		{"100 stored, 1 new", 100, 1, false, http.StatusTooManyRequests},
		{"99 stored, 2 new", 99, 2, false, http.StatusTooManyRequests},
		{"0 stored, 101 new", 0, 101, false, http.StatusTooManyRequests},
		{"100 stored, 1 that exists", 100, 1, true, http.StatusTooManyRequests},
		{"99 stored, 1 new", 99, 1, false, http.StatusCreated},
		{"0 stored, 100 new", 0, 100, false, http.StatusCreated},
	}
	numbered := func(prefix string, n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("%s%d", prefix, i)
		}
		return out
	}
	for i, tc := range cases {
		id := w.CreateUser(t, fmt.Sprintf("limited%d", i))
		if tc.stored > 0 {
			w.seed(t, secret.ScopeUser, id, numbered("S", tc.stored)...)
		}
		keys := numbered("N", tc.added)
		if tc.existing {
			keys = []string{"s0"}
		}
		var body []any
		for _, k := range keys {
			body = append(body, map[string]any{"key": k})
		}
		a := w.do(t, w.IssueKey(t, id), http.MethodPost, "/user/secrets", entries(body...))
		typ, msg := errorOf(t, a)
		wantCount := tc.stored
		if tc.want == http.StatusCreated {
			wantCount += tc.added
		} else if typ != "secret_limit_reached" || msg != "maximum of 100 entries per scope exceeded" {
			t.Errorf("%s: %s, want secret_limit_reached with the limit message", tc.name, a.body)
		}
		if a.status != tc.want || w.count(t, secret.ScopeUser, id) != wantCount {
			t.Errorf("%s: %d and %d stored, want %d and %d", tc.name, a.status, w.count(t, secret.ScopeUser, id), tc.want, wantCount)
		}
	}
	w.seed(t, secret.ScopeUser, w.ownerID, numbered("S", secret.MaxEntries)...)
	if a := w.do(t, w.ownerKey, http.MethodPost, "/orgs/acme/secrets", entries(map[string]any{"key": "ORG"})); a.status != http.StatusCreated {
		t.Errorf("an org create by a user whose scope is full = %d %s, want 201", a.status, a.body)
	}
}

func TestCreateSecrets_RefusesAKeyTheScopeHolds(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.seed(t, secret.ScopeWorkspace, "acme", "Token", "Other")
	before := w.do(t, w.ownerKey, http.MethodGet, "/workspaces/acme/secrets", nil)
	cases := []struct {
		keys []string
		msg  string
	}{
		{[]string{"NEW", "TOKEN"}, `key "TOKEN" already exists`},
		{[]string{"token"}, `key "token" already exists`},
		{[]string{"other", "TOKEN"}, `key "other" already exists`},
	}
	for _, tc := range cases {
		var body []any
		for _, k := range tc.keys {
			body = append(body, map[string]any{"key": k, "value": "new"})
		}
		a := w.do(t, w.ownerKey, http.MethodPost, "/workspaces/acme/secrets", entries(body...))
		if typ, msg := errorOf(t, a); a.status != http.StatusConflict || typ != "conflict" || msg != tc.msg {
			t.Errorf("%v: %d %s, want 409 conflict %q", tc.keys, a.status, a.body, tc.msg)
		}
	}
	after := w.do(t, w.ownerKey, http.MethodGet, "/workspaces/acme/secrets", nil)
	if v, _, _ := w.secrets.Get(context.Background(), secret.ScopeWorkspace, "acme", "Token"); v != valueOf("Token") || !bytes.Equal(after.body, before.body) {
		t.Errorf("after the refusals: Token = %q and list %s; want %q and %s", v, after.body, valueOf("Token"), before.body)
	}
}

func TestCreateSecrets_WritesAnEventPerEntry(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	pat := w.IssuePAT(t, identity.PATSpec{UserID: w.ownerID, Permissions: []string{"secrets:write"}})
	cases := []struct {
		r                 route
		name, id, cred    string
		actorType         string
		actorID, wantSlug *string
	}{
		{routes[3], "", w.ownerID, w.ownerKey, event.ActorAPIKey, &w.ownerID, nil},
		{routes[4], "acme", w.orgID, w.Token, event.ActorAdminToken, nil, nil},
		{routes[5], "acme", "acme", pat, event.ActorPAT, &w.ownerID, ptr("acme")},
	}
	for _, tc := range cases {
		_, seq := w.eventsAfter(t, 0)
		a := w.do(t, tc.cred, http.MethodPost, tc.r.at(tc.name), entries(map[string]any{"key": "Z_one", "value": "1"}, map[string]any{"key": "A_two"}))
		if a.status != http.StatusCreated {
			t.Fatalf("%v: %d %s, want 201", tc.r, a.status, a.body)
		}
		recs, _ := w.eventsAfter(t, seq)
		if len(recs) != 2 {
			t.Fatalf("%v: %d new events, want 2", tc.r, len(recs))
		}
		for i, key := range []string{"Z_one", "A_two"} {
			ev := recs[i]
			wantMeta, _ := json.Marshal(map[string]string{"scope": string(tc.r.scope), "scope_id": tc.id, "key": key})
			var meta map[string]string
			_ = json.Unmarshal(ev.Metadata, &meta)
			gotMeta, _ := json.Marshal(meta)
			if ev.Type != "parley.secret.create" || ev.ResourceType != "secret" || ev.Action != "create" || ev.ResourceID != key ||
				!bytes.Equal(gotMeta, wantMeta) || !equalPtr(ev.Workspace, tc.wantSlug) ||
				ev.ActorType != tc.actorType || !equalPtr(ev.ActorID, tc.actorID) {
				t.Errorf("%v: event %d = %+v, metadata %s; want the create of %s by %s", tc.r, i, ev, ev.Metadata, key, tc.actorType)
			}
		}
	}
}

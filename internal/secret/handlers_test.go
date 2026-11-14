package secret_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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
	at := `"` + w.clock().Truncate(time.Second).Format(time.RFC3339) + `"`
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

func TestUpdateSecret_ReplacesTheValueInPlace(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	values := []string{"new", "", strings.Repeat("x", secret.MaxValueSize)}
	for i, pl := range w.places() {
		created := w.clock().Truncate(time.Second)
		w.seed(t, pl.scope, pl.id, "Git_Pat")
		now := w.advance(90 * time.Second).Truncate(time.Second)
		a := w.do(t, w.ownerKey, http.MethodPatch, pl.path+"/git_pat", map[string]any{"value": values[i]})
		var got map[string]any
		_ = json.Unmarshal(a.body, &got)
		want := map[string]any{"key": "Git_Pat", "created_at": created.Format(time.RFC3339), "updated_at": now.Format(time.RFC3339)}
		if a.status != http.StatusOK || !maps.Equal(got, want) {
			t.Errorf("%s: %d %s, want 200 %v", pl.path, a.status, a.body, want)
		}
		if v, found, err := w.secrets.Get(context.Background(), pl.scope, pl.id, "Git_Pat"); !found || err != nil || v != values[i] {
			t.Errorf("%s: Get = %.20q, %v, %v; want %.20q", pl.path, v, found, err, values[i])
		}
	}
}

func TestUpdateSecret_RejectsABodyWithoutAStringValue(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.seed(t, secret.ScopeWorkspace, "acme", "K")
	before := w.do(t, w.ownerKey, http.MethodGet, "/workspaces/acme/secrets", nil)
	w.advance(90 * time.Second)
	bodies := map[string]any{
		"no value":  map[string]any{},
		"null":      map[string]any{"value": nil},
		"a number":  map[string]any{"value": 7},
		"too large": map[string]any{"value": strings.Repeat("x", secret.MaxValueSize+1)},
	}
	for name, body := range bodies {
		a := w.do(t, w.ownerKey, http.MethodPatch, "/workspaces/acme/secrets/K", body)
		if typ, _ := errorOf(t, a); a.status != http.StatusBadRequest || typ != "invalid_request" {
			t.Errorf("%s: %d %s, want 400 invalid_request", name, a.status, a.body)
		}
	}
	after := w.do(t, w.ownerKey, http.MethodGet, "/workspaces/acme/secrets", nil)
	if v, _, _ := w.secrets.Get(context.Background(), secret.ScopeWorkspace, "acme", "K"); v != valueOf("K") || !bytes.Equal(after.body, before.body) {
		t.Errorf("after the refusals: K = %q and list %s; want %q and %s", v, after.body, valueOf("K"), before.body)
	}
}

func TestUpdateSecret_RefusesWithoutACurrentKey(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.seed(t, secret.ScopeWorkspace, "acme", "A")
	sealed := func() (b []byte) {
		if err := w.Store.Main().QueryRowContext(context.Background(), `SELECT sealed FROM secrets WHERE key = 'A'`).Scan(&b); err != nil {
			t.Fatalf("read the sealed value of A: %v", err)
		}
		return b
	}
	stored := sealed()
	w.RestartEnv(t, nil)
	for _, key := range []string{"A", "UNKNOWN"} {
		a := w.do(t, w.ownerKey, http.MethodPatch, "/workspaces/acme/secrets/"+key, map[string]any{"value": "x"})
		if typ, _ := errorOf(t, a); a.status != http.StatusServiceUnavailable || typ != "secrets_unavailable" {
			t.Errorf("%s: %d %s, want 503 secrets_unavailable", key, a.status, a.body)
		}
	}
	if !bytes.Equal(sealed(), stored) {
		t.Error("the refused update changed the sealed value of A")
	}
}

func TestUpdateSecret_OverwritesAValueNoKeyOpens(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.seed(t, secret.ScopeWorkspace, "acme", "K")
	_, other := newKey(t)
	w.RestartEnv(t, map[string]string{secret.EnvCurrentKey: other})
	ctx := context.Background()
	if _, _, err := w.secrets.Get(ctx, secret.ScopeWorkspace, "acme", "K"); !errors.Is(err, secret.ErrUnreadable) {
		t.Fatalf("Get under another key = %v, want ErrUnreadable", err)
	}
	if a := w.do(t, w.ownerKey, http.MethodPatch, "/workspaces/acme/secrets/K", map[string]any{"value": "again"}); a.status != http.StatusOK {
		t.Errorf("update = %d %s, want 200", a.status, a.body)
	}
	if v, _, err := w.secrets.Get(ctx, secret.ScopeWorkspace, "acme", "K"); v != "again" || err != nil {
		t.Errorf("Get after the update = %q, %v; want again", v, err)
	}
}

func TestChangeSecret_AnswersNotFoundForAKeyTheScopeDoesNotHold(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.seed(t, secret.ScopeUser, w.memberID, "MINE")
	w.seed(t, secret.ScopeWorkspace, "acme", "PARENTS")
	for _, method := range []string{http.MethodPatch, http.MethodDelete} {
		var body any
		if method == http.MethodPatch {
			body = changed
		}
		for _, k := range []string{"MISSING", "not-a-key", "MINE", "PARENTS"} {
			a := w.do(t, w.memberKey, method, "/workspaces/acme-fork/secrets/"+k, body)
			typ, msg := errorOf(t, a)
			if want := `key "` + k + `" not found`; a.status != http.StatusNotFound || typ != "not_found" || msg != want {
				t.Errorf("%s %s on the fork = %d %s, want 404 not_found %q", method, k, a.status, a.body, want)
			}
		}
	}
	if w.count(t, secret.ScopeUser, w.memberID) != 1 || w.count(t, secret.ScopeWorkspace, "acme") != 1 {
		t.Error("a refused change removed a secret of another scope")
	}
}

func TestDeleteSecret_RemovesItWithOrWithoutAKeyThatOpensIt(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.seed(t, secret.ScopeWorkspace, "acme", "Kept", "No_Key", "Other_Key")
	_, other := newKey(t)
	cases := []struct {
		name, key string
		restart   bool
		env       map[string]string
	}{
		{"key configured", "kept", false, nil},
		{"no key configured", "NO_KEY", true, nil},
		{"sealed under a key no longer configured", "other_key", true, map[string]string{secret.EnvCurrentKey: other}},
	}
	for i, tc := range cases {
		if tc.restart {
			w.RestartEnv(t, tc.env)
		}
		a := w.do(t, w.ownerKey, http.MethodDelete, "/workspaces/acme/secrets/"+tc.key, nil)
		if a.status != http.StatusNoContent || len(a.body) != 0 {
			t.Errorf("%s: %d %q, want 204 and no body", tc.name, a.status, a.body)
		}
		if got := pageOf(t, w.do(t, w.ownerKey, http.MethodGet, "/workspaces/acme/secrets", nil)).keys(t); len(got) != 2-i {
			t.Errorf("%s: list = %v after the delete", tc.name, got)
		}
		if i == 0 {
			if a := w.do(t, w.ownerKey, http.MethodPost, "/workspaces/acme/secrets", entries(map[string]any{"key": "Kept"})); a.status != http.StatusCreated {
				t.Errorf("%s: create again = %d %s, want 201", tc.name, a.status, a.body)
			}
			w.do(t, w.ownerKey, http.MethodDelete, "/workspaces/acme/secrets/Kept", nil)
		}
	}
}

func TestChangeSecret_WritesAnEventNamingTheStoredKey(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.seed(t, secret.ScopeWorkspace, "acme", "Git_Pat")
	steps := []struct {
		method, key, action string
		body                any
	}{
		{http.MethodPatch, "GIT_PAT", "update", changed},
		{http.MethodDelete, "git_pat", "delete", nil},
	}
	want := map[string]string{"scope": "workspace", "scope_id": "acme", "key": "Git_Pat"}
	for _, s := range steps {
		_, seq := w.eventsAfter(t, 0)
		if a := w.do(t, w.ownerKey, s.method, "/workspaces/acme/secrets/"+s.key, s.body); a.status >= 300 {
			t.Fatalf("%s %s = %d %s", s.method, s.key, a.status, a.body)
		}
		recs, _ := w.eventsAfter(t, seq)
		if len(recs) != 1 {
			t.Fatalf("%s: %d new events, want 1", s.method, len(recs))
		}
		ev := recs[0]
		var meta map[string]string
		_ = json.Unmarshal(ev.Metadata, &meta)
		if ev.Type != "parley.secret."+s.action || ev.Action != s.action || ev.ResourceType != "secret" || ev.ResourceID != "Git_Pat" ||
			!maps.Equal(meta, want) || !equalPtr(ev.Workspace, ptr("acme")) ||
			ev.ActorType != event.ActorAPIKey || !equalPtr(ev.ActorID, &w.ownerID) {
			t.Errorf("%s: event = %+v, metadata %s; want the %s of Git_Pat in acme by owner", s.method, ev, ev.Metadata, s.action)
		}
	}
}

func TestSecretRefusals_WriteNoEvent(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.seed(t, secret.ScopeWorkspace, "acme", "TAKEN")
	full := make([]string, secret.MaxEntries)
	for i := range full {
		full[i] = fmt.Sprintf("S%d", i)
	}
	w.seed(t, secret.ScopeUser, w.member2ID, full...)
	reader := w.IssuePAT(t, identity.PATSpec{UserID: w.ownerID, Permissions: []string{"secrets:read"}})
	one := entries(map[string]any{"key": "NEW", "value": "v"})
	cases := []struct {
		name, cred, method, path string
		body                     any
		noKey                    bool // asked after a restart without a key
		want                     int
	}{
		{"create with an invalid body", w.ownerKey, http.MethodPost, "/user/secrets", entries(), false, http.StatusBadRequest},
		{"create with one new key and one the scope holds", w.ownerKey, http.MethodPost, "/workspaces/acme/secrets",
			entries(map[string]any{"key": "NEW"}, map[string]any{"key": "TAKEN"}), false, http.StatusConflict},
		{"create into a full scope", w.member2Key, http.MethodPost, "/user/secrets", one, false, http.StatusTooManyRequests},
		{"update with an invalid body", w.ownerKey, http.MethodPatch, "/workspaces/acme/secrets/TAKEN", map[string]any{"value": 5}, false, http.StatusBadRequest},
		{"update of a key the scope does not hold", w.ownerKey, http.MethodPatch, "/workspaces/acme/secrets/MISSING", changed, false, http.StatusNotFound},
		{"delete of a key the scope does not hold", w.ownerKey, http.MethodDelete, "/workspaces/acme/secrets/MISSING", nil, false, http.StatusNotFound},
		{"create by an org member on the workspace", w.memberKey, http.MethodPost, "/workspaces/acme/secrets", one, false, http.StatusForbidden},
		{"create by an outsider on the org", w.outsiderKey, http.MethodPost, "/orgs/acme/secrets", one, false, http.StatusNotFound},
		{"create with a PAT without secrets:write", reader, http.MethodPost, "/user/secrets", one, false, http.StatusForbidden},
		{"create without a key", w.ownerKey, http.MethodPost, "/user/secrets", one, true, http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		if tc.noKey && w.secrets.Available() {
			w.RestartEnv(t, nil)
		}
		_, seq := w.eventsAfter(t, 0)
		a := w.do(t, tc.cred, tc.method, tc.path, tc.body)
		if recs, _ := w.eventsAfter(t, seq); a.status != tc.want || len(recs) != 0 {
			t.Errorf("%s: %d %s and %d new events, want %d and none", tc.name, a.status, a.body, len(recs), tc.want)
		}
	}
}

func TestChangeSecret_RollsBackWhenItsEventCannotBeWritten(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.seed(t, secret.ScopeWorkspace, "acme", "KEPT")
	ctx := context.Background()
	err := w.Store.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `CREATE TRIGGER refuse_events BEFORE INSERT ON hub_events BEGIN SELECT RAISE(ABORT, 'event refused'); END`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	rows := func() (s string) {
		if err := w.Store.Main().QueryRowContext(ctx, `SELECT coalesce(group_concat(r, ','), '') FROM
(SELECT key || ':' || hex(sealed) || ':' || updated_at AS r FROM secrets ORDER BY id)`).Scan(&s); err != nil {
			t.Fatalf("read the secret rows: %v", err)
		}
		return s
	}
	before := rows()
	for _, r := range []route{
		{method: http.MethodPost, path: "/workspaces/acme/secrets", body: entries(map[string]any{"key": "NEW", "value": "v"})},
		{method: http.MethodPatch, path: "/workspaces/acme/secrets/KEPT", body: changed},
		{method: http.MethodDelete, path: "/workspaces/acme/secrets/KEPT"},
	} {
		w.advance(time.Second)
		a := w.do(t, w.ownerKey, r.method, r.path, r.body)
		if typ, _ := errorOf(t, a); a.status != http.StatusInternalServerError || typ != "internal" || bytes.Contains(a.body, []byte("refused")) {
			t.Errorf("%v = %d %s, want 500 internal without the cause", r, a.status, a.body)
		}
		if got := rows(); got != before {
			t.Errorf("%v changed the rows to %s, want %s", r, got, before)
		}
	}
}

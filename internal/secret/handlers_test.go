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

func TestSecretLists_HoldTheSecretsOfTheirOwnScopeOnly(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	var personal string
	if err := w.Store.Main().QueryRowContext(context.Background(),
		`SELECT id FROM orgs WHERE name = 'member'`).Scan(&personal); err != nil {
		t.Fatalf("read the personal org of member: %v", err)
	}
	k := entries(map[string]any{"key": "K", "value": "a"})
	byID := "/orgs/" + w.orgID + "/secrets"
	// The steps run in order; a step without keys is a create, answered 201.
	steps := []struct {
		name, cred, method, path string
		body                     any
		keys                     []string
	}{
		{"user A creates K", w.ownerKey, http.MethodPost, "/user/secrets", k, nil},
		{"user B lists", w.outsiderKey, http.MethodGet, "/user/secrets", nil, []string{}},
		{"user B creates K", w.outsiderKey, http.MethodPost, "/user/secrets", k, nil},
		{"user A lists", w.ownerKey, http.MethodGet, "/user/secrets", nil, []string{"K"}},
		{"a member creates K in the org", w.memberKey, http.MethodPost, "/orgs/acme/secrets", k, nil},
		{"another member lists by slug", w.member2Key, http.MethodGet, "/orgs/acme/secrets", nil, []string{"K"}},
		{"the admin token lists by id", w.Token, http.MethodGet, byID, nil, []string{"K"}},
		{"the member lists by id", w.memberKey, http.MethodGet, byID, nil, []string{"K"}},
		{"the member lists their personal org", w.memberKey, http.MethodGet, "/orgs/" + personal + "/secrets", nil, []string{}},
		{"the owner creates P in the workspace", w.ownerKey, http.MethodPost, "/workspaces/acme/secrets", keyed("P"), nil},
		{"the owner creates F in its fork", w.ownerKey, http.MethodPost, "/workspaces/acme-fork/secrets", keyed("F"), nil},
		{"the owner lists the workspace", w.ownerKey, http.MethodGet, "/workspaces/acme/secrets", nil, []string{"P"}},
		{"the owner lists the fork", w.ownerKey, http.MethodGet, "/workspaces/acme-fork/secrets", nil, []string{"F"}},
		{"the admin token lists the workspace", w.Token, http.MethodGet, "/workspaces/acme/secrets", nil, []string{"P"}},
		{"the admin token lists the fork", w.Token, http.MethodGet, "/workspaces/acme-fork/secrets", nil, []string{"F"}},
	}
	for _, s := range steps {
		a := w.do(t, s.cred, s.method, s.path, s.body)
		if s.keys == nil {
			if a.status != http.StatusCreated {
				t.Fatalf("%s: %d %s, want 201", s.name, a.status, a.body)
			}
		} else if got := pageOf(t, a).keys(t); !slices.Equal(got, s.keys) {
			t.Errorf("%s: list = %v, want %v", s.name, got, s.keys)
		}
	}
}

func TestSecretRoutes_RefuseEveryCallerOutsideTheirScope(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.seed(t, secret.ScopeWorkspace, "acme", "P")
	const readOnly, noWorkspace = "org members have read-only access to this workspace", "workspace not found"
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
			{"unknown org", w.memberKey, "no-such", http.StatusNotFound, "not_found", "org not found", false},
			{"not a member", w.outsiderKey, "acme", http.StatusNotFound, "not_found", "org not found", true},
			{"blocked org, a member", w.memberKey, "frozen", http.StatusNotFound, "not_found", "org not found", true},
		},
		secret.ScopeWorkspace: {
			{"an org member", w.member2Key, "acme", http.StatusForbidden, "forbidden", readOnly, false},
			{"the fork's creator", w.memberKey, "acme", http.StatusForbidden, "forbidden", readOnly, false},
			{"a stranger", w.outsiderKey, "acme", http.StatusNotFound, "not_found", noWorkspace, false},
			{"a stranger, unknown slug", w.outsiderKey, "no-such", http.StatusNotFound, "not_found", noWorkspace, true},
		},
	}
	for _, r := range routes {
		var last []byte
		for _, c := range refusals[r.scope] {
			a := w.do(t, c.cred, r.method, r.at(c.at), r.body)
			if typ, msg := errorOf(t, a); a.status != c.status || typ != c.typ || msg != c.msg || c.same && !bytes.Equal(a.body, last) {
				t.Errorf("%v, %s = %d %s; want %d %s %q, the same body as %s", r, c.name, a.status, a.body, c.status, c.typ, c.msg, last)
			}
			last = a.body
		}
	}
	if a := w.do(t, w.Token, http.MethodGet, "/orgs/frozen/secrets", nil); a.status != http.StatusOK {
		t.Errorf("the admin token's list of the blocked org = %d %s, want 200", a.status, a.body)
	}
	if n := w.count(t, secret.ScopeWorkspace, "acme"); n != 1 {
		t.Errorf("acme holds %d secrets, want 1", n)
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
	for _, pl := range w.places() {
		path := pl.path
		empty := w.do(t, w.ownerKey, http.MethodGet, path, nil)
		if !bytes.Contains(empty.body, []byte(`"items":[]`)) {
			t.Errorf("%s: an empty list = %s, want \"items\":[]", path, empty.body)
		}
		w.seed(t, pl.scope, pl.id, stored...)

		all := w.do(t, w.ownerKey, http.MethodGet, path, nil)
		p := pageOf(t, all)
		if got := p.keys(t); !slices.Equal(got, want) || p.NextCursor != nil {
			t.Errorf("%s: list = %v, next_cursor %v; want %v and null", path, got, p.NextCursor, want)
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
		p := pageOf(t, a, http.StatusCreated)
		var top map[string]json.RawMessage
		if err := json.Unmarshal(a.body, &top); err != nil {
			t.Fatalf("%s: decode %s: %v", tc.name, a.body, err)
		}
		if got := p.keys(t); !slices.Equal(got, want) || len(top) != 1 {
			t.Errorf("%s: answer %s, want only items with keys %v", tc.name, a.body, want)
		}
		for _, it := range p.Items {
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
	for i, tc := range cases {
		id := w.CreateUser(t, fmt.Sprintf("limited%d", i))
		if tc.stored > 0 {
			w.seed(t, secret.ScopeUser, id, numbered("S", tc.stored)...)
		}
		keys := numbered("N", tc.added)
		if tc.existing {
			keys = []string{"s0"}
		}
		a := w.do(t, w.IssueKey(t, id), http.MethodPost, "/user/secrets", keyed(keys...))
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
	if a := w.do(t, w.ownerKey, http.MethodPost, "/orgs/acme/secrets", keyed("ORG")); a.status != http.StatusCreated {
		t.Errorf("an org create by a user whose scope is full = %d %s, want 201", a.status, a.body)
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
		if err := json.Unmarshal(a.body, &got); err != nil {
			t.Fatalf("%s: decode %s: %v", pl.path, a.body, err)
		}
		want := map[string]any{"key": "Git_Pat", "created_at": created.Format(time.RFC3339), "updated_at": now.Format(time.RFC3339)}
		if a.status != http.StatusOK || !maps.Equal(got, want) {
			t.Errorf("%s: %d %s, want 200 %v", pl.path, a.status, a.body, want)
		}
		if v, found, err := w.secrets.Get(context.Background(), pl.scope, pl.id, "Git_Pat"); !found || err != nil || v != values[i] {
			t.Errorf("%s: Get = %.20q, %v, %v; want %.20q", pl.path, v, found, err, values[i])
		}
	}
}

func TestSecretRoutes_ListUpdateAndDeleteWhatNoKeyOpens(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.seed(t, secret.ScopeWorkspace, "acme", "Kept", "No_Key", "Other_Key", "Z_Replaced")
	_, other := newKey(t)
	// Each case lists every secret before and after its delete, also those
	// no configured key opens.
	cases := []struct {
		name, key    string
		restart      bool
		env          map[string]string
		listed, left []string
	}{
		{"key configured", "kept", false, nil, []string{"Kept", "No_Key", "Other_Key", "Z_Replaced"}, []string{"No_Key", "Other_Key", "Z_Replaced"}},
		{"no key configured", "NO_KEY", true, nil, []string{"No_Key", "Other_Key", "Z_Replaced"}, []string{"Other_Key", "Z_Replaced"}},
		{"sealed under a key no longer configured", "other_key", true, map[string]string{secret.EnvCurrentKey: other}, []string{"Other_Key", "Z_Replaced"}, []string{"Z_Replaced"}},
	}
	list := func() []string {
		return pageOf(t, w.do(t, w.ownerKey, http.MethodGet, "/workspaces/acme/secrets", nil)).keys(t)
	}
	for i, tc := range cases {
		if tc.restart {
			w.RestartEnv(t, tc.env)
		}
		if got := list(); !slices.Equal(got, tc.listed) {
			t.Errorf("%s: list = %v before the delete, want %v", tc.name, got, tc.listed)
		}
		a := w.do(t, w.ownerKey, http.MethodDelete, "/workspaces/acme/secrets/"+tc.key, nil)
		if a.status != http.StatusNoContent || len(a.body) != 0 {
			t.Errorf("%s: %d %q, want 204 and no body", tc.name, a.status, a.body)
		}
		if got := list(); !slices.Equal(got, tc.left) {
			t.Errorf("%s: list = %v after the delete, want %v", tc.name, got, tc.left)
		}
		if i == 0 {
			if a := w.do(t, w.ownerKey, http.MethodPost, "/workspaces/acme/secrets", keyed("Kept")); a.status != http.StatusCreated {
				t.Errorf("%s: create again = %d %s, want 201", tc.name, a.status, a.body)
			}
			w.do(t, w.ownerKey, http.MethodDelete, "/workspaces/acme/secrets/Kept", nil)
		}
	}
	// Under the other key, a value no configured key opens is still replaced.
	ctx := context.Background()
	if _, _, err := w.secrets.Get(ctx, secret.ScopeWorkspace, "acme", "Z_Replaced"); !errors.Is(err, secret.ErrUnreadable) {
		t.Fatalf("Get under another key = %v, want ErrUnreadable", err)
	}
	if a := w.do(t, w.ownerKey, http.MethodPatch, "/workspaces/acme/secrets/Z_Replaced", map[string]any{"value": "again"}); a.status != http.StatusOK {
		t.Errorf("update = %d %s, want 200", a.status, a.body)
	}
	if v, _, err := w.secrets.Get(ctx, secret.ScopeWorkspace, "acme", "Z_Replaced"); v != "again" || err != nil {
		t.Errorf("Get after the update = %q, %v; want again", v, err)
	}
}

func TestSecretChanges_WriteAnEventNamingTheStoredKey(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	pat := w.IssuePAT(t, identity.PATSpec{UserID: w.ownerID, Permissions: []string{"secrets:write"}})
	two := entries(map[string]any{"key": "Z_one", "value": "1"}, map[string]any{"key": "A_two"})
	created, acme := []string{"Z_one", "A_two"}, ptr("acme")
	// Each step is a change and the events it writes, one per key, in order.
	steps := []struct {
		cred, method, path     string
		body                   any
		action, scope, scopeID string
		keys                   []string
		actorType              string
		actorID, slug          *string
	}{
		{w.ownerKey, http.MethodPost, "/user/secrets", two, "create", "user", w.ownerID, created, event.ActorAPIKey, &w.ownerID, nil},
		{w.Token, http.MethodPost, "/orgs/acme/secrets", two, "create", "org", w.orgID, created, event.ActorAdminToken, nil, nil},
		{pat, http.MethodPost, "/workspaces/acme/secrets", two, "create", "workspace", "acme", created, event.ActorPAT, &w.ownerID, acme},
		{w.ownerKey, http.MethodPatch, "/workspaces/acme/secrets/A_TWO", changed, "update", "workspace", "acme", []string{"A_two"}, event.ActorAPIKey, &w.ownerID, acme},
		{w.ownerKey, http.MethodDelete, "/workspaces/acme/secrets/a_two", nil, "delete", "workspace", "acme", []string{"A_two"}, event.ActorAPIKey, &w.ownerID, acme},
	}
	for _, s := range steps {
		_, seq := w.eventsAfter(t, 0)
		if a := w.do(t, s.cred, s.method, s.path, s.body); a.status >= 300 {
			t.Fatalf("%s %s = %d %s", s.method, s.path, a.status, a.body)
		}
		recs, _ := w.eventsAfter(t, seq)
		if len(recs) != len(s.keys) {
			t.Fatalf("%s %s: %d new events, want %d", s.method, s.path, len(recs), len(s.keys))
		}
		for i, key := range s.keys {
			ev, meta := recs[i], map[string]string{"scope": s.scope, "scope_id": s.scopeID, "key": key}
			if ev.Type != "parley.secret."+s.action || ev.Action != s.action || ev.ResourceType != "secret" || ev.ResourceID != key ||
				!maps.Equal(metadataOf(t, ev), meta) || !equalPtr(ev.Workspace, s.slug) || ev.ActorType != s.actorType || !equalPtr(ev.ActorID, s.actorID) {
				t.Errorf("%s %s: event %d = %+v, metadata %s; want the %s of %s by %s", s.method, s.path, i, ev, ev.Metadata, s.action, key, s.actorType)
			}
		}
	}
}

func TestSecretRefusals_ChangeNothingAndWriteNoEvent(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	const col = "/workspaces/acme/secrets"
	w.seed(t, secret.ScopeWorkspace, "acme", "Token", "Other")
	w.seed(t, secret.ScopeUser, w.memberID, "MINE")
	w.seed(t, secret.ScopeUser, w.member2ID, numbered("S", secret.MaxEntries)...)
	reader := w.IssuePAT(t, identity.PATSpec{UserID: w.ownerID, Permissions: []string{"secrets:read"}})
	one := entries(map[string]any{"key": "NEW", "value": "v"})
	key := func(k string, v ...any) map[string]any {
		if len(v) == 0 {
			return map[string]any{"key": k}
		}
		return map[string]any{"key": k, "value": v[0]}
	}
	long := strings.Repeat("k", 256)
	// A refusal of a create body in the workspace, or of an update body for
	// Token; msg is what its message holds.
	type refusal struct {
		name, cred, method, path string
		body                     any
		status                   int
		typ, msg                 string
	}
	badCreate := func(name string, body any, msg string) refusal {
		return refusal{name, w.ownerKey, http.MethodPost, col, body, http.StatusBadRequest, "invalid_request", msg}
	}
	badUpdate := func(name string, body any) refusal {
		return refusal{name, w.ownerKey, http.MethodPatch, col + "/Token", body, http.StatusBadRequest, "invalid_request", "Token"}
	}
	// The message of a 404 or a 409 is exactly msg.
	cases := []refusal{
		badCreate("no entries", map[string]any{}, ""),
		badCreate("empty entries", entries(), ""),
		badCreate("entries not an array", map[string]any{"entries": "x"}, ""),
		badCreate("entry not an object", entries("x"), ""),
		badCreate("entry without key", entries(map[string]any{"value": "v"}), ""),
		badCreate("key not a string", entries(map[string]any{"key": 5}), ""),
		badCreate("empty key", entries(key("")), `""`),
		badCreate("key too long", entries(key(long)), long),
		badCreate("key begins with a digit", entries(key("1A")), "1A"),
		badCreate("key with a hyphen", entries(key("A-B")), "A-B"),
		badCreate("key with a space", entries(key("A B")), "A B"),
		badCreate("key with a non-ASCII letter", entries(key("Ké")), "Ké"),
		badCreate("value a number", entries(key("NUM", 5)), "NUM"),
		badCreate("value null", entries(key("NUL", nil)), "NUL"),
		badCreate("value too large", entries(key("HUGE", strings.Repeat("x", secret.MaxValueSize+1))), "HUGE"),
		badCreate("keys equal ignoring case", entries(key("Token"), key("TOKEN")), "TOKEN"),
		badCreate("a valid entry, then an invalid one", entries(key("GOOD"), key("2B")), "2B"),
		{"create with one new key and one the scope holds", w.ownerKey, http.MethodPost, col, entries(key("NEW", "new"), key("TOKEN", "new")), http.StatusConflict, "conflict", `key "TOKEN" already exists`},
		{"create of a held key in another casing", w.ownerKey, http.MethodPost, col, entries(key("token", "new")), http.StatusConflict, "conflict", `key "token" already exists`},
		{"create of two held keys", w.ownerKey, http.MethodPost, col, entries(key("other", "new"), key("TOKEN", "new")), http.StatusConflict, "conflict", `key "other" already exists`},
		badUpdate("update without a value", map[string]any{}),
		badUpdate("update with a null value", map[string]any{"value": nil}),
		badUpdate("update with a number", map[string]any{"value": 7}),
		badUpdate("update with a value too large", map[string]any{"value": strings.Repeat("x", secret.MaxValueSize+1)}),
		{"create into a full scope", w.member2Key, http.MethodPost, "/user/secrets", one, http.StatusTooManyRequests, "secret_limit_reached", ""},
		{"create by an org member on the workspace", w.memberKey, http.MethodPost, col, one, http.StatusForbidden, "forbidden", ""},
		{"create by an outsider on the org", w.outsiderKey, http.MethodPost, "/orgs/acme/secrets", one, http.StatusNotFound, "not_found", "org not found"},
		{"create with a PAT without secrets:write", reader, http.MethodPost, "/user/secrets", one, http.StatusForbidden, "insufficient_scope", ""},
	}
	// On the fork, neither a key it does not hold, nor its creator's, nor
	// its parent's is found.
	for method, body := range map[string]any{http.MethodPatch: changed, http.MethodDelete: nil} {
		for _, k := range []string{"MISSING", "not-a-key", "MINE", "Token"} {
			cases = append(cases, refusal{method + " " + k + " on the fork", w.memberKey, method, "/workspaces/acme-fork/secrets/" + k,
				body, http.StatusNotFound, "not_found", `key "` + k + `" not found`})
		}
		cases = append(cases, refusal{method + " of a key the workspace does not hold", w.ownerKey, method, col + "/MISSING",
			body, http.StatusNotFound, "not_found", `key "MISSING" not found`})
	}
	refuse := func(when string, c refusal) {
		t.Helper()
		_, seq := w.eventsAfter(t, 0)
		a := w.do(t, c.cred, c.method, c.path, c.body)
		typ, msg := errorOf(t, a)
		exact := c.status == http.StatusNotFound || c.status == http.StatusConflict
		if recs, _ := w.eventsAfter(t, seq); a.status != c.status || typ != c.typ || !strings.Contains(msg, c.msg) || exact && msg != c.msg || len(recs) != 0 {
			t.Errorf("%s%s: %d %.200s and %d new events; want %d %s with %.20q and none", when, c.name, a.status, a.body, len(recs), c.status, c.typ, c.msg)
		}
	}
	sealed := func() (b []byte) {
		if err := w.Store.Main().QueryRowContext(context.Background(), `SELECT sealed FROM secrets WHERE key = 'Token'`).Scan(&b); err != nil {
			t.Fatalf("read the sealed value of Token: %v", err)
		}
		return b
	}
	before, stored := w.do(t, w.ownerKey, http.MethodGet, col, nil), sealed()
	w.advance(90 * time.Second)
	for _, c := range cases {
		refuse("", c)
	}
	after := w.do(t, w.ownerKey, http.MethodGet, col, nil)
	if v, _, err := w.secrets.Get(context.Background(), secret.ScopeWorkspace, "acme", "Token"); err != nil || v != valueOf("Token") || !bytes.Equal(after.body, before.body) {
		t.Errorf("after the refusals: Token = %q, %v and the list %s; want %q and %s", v, err, after.body, valueOf("Token"), before.body)
	}
	if w.count(t, secret.ScopeUser, w.memberID) != 1 || w.count(t, secret.ScopeUser, w.member2ID) != secret.MaxEntries {
		t.Error("a refusal changed the secrets of a user scope")
	}
	// Without a current key a body is still checked first, and nothing is
	// stored, whether or not the scope holds the key.
	_, previous := newKey(t)
	for name, env := range map[string]map[string]string{"no key": nil, "only previous key": {secret.EnvPreviousKey: previous}} {
		w.RestartEnv(t, env)
		for _, c := range []refusal{
			{"create", w.ownerKey, http.MethodPost, "/user/secrets", one, http.StatusServiceUnavailable, "secrets_unavailable", ""},
			{"update", w.ownerKey, http.MethodPatch, col + "/Token", changed, http.StatusServiceUnavailable, "secrets_unavailable", ""},
			{"update of a key the scope does not hold", w.ownerKey, http.MethodPatch, col + "/UNKNOWN", changed, http.StatusServiceUnavailable, "secrets_unavailable", ""},
			badCreate("an empty create", entries(), ""),
		} {
			refuse(name+": ", c)
		}
		if n := w.count(t, secret.ScopeUser, w.ownerID); n != 0 || !bytes.Equal(sealed(), stored) {
			t.Errorf("%s: the user scope holds %d secrets, want 0, or the sealed value of Token changed", name, n)
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

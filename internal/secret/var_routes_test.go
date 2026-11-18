package secret_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/secret"
)

func TestCreateVars_StoresEveryEntryInRequestOrder(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	at := stamp(w.clock())
	long, big := strings.Repeat("k", secret.MaxKeyLen), strings.Repeat("x", secret.MaxValueSize)
	want := []variable{{"Zed", "1", "", at, at}, {"alpha", "", "", at, at}, {long, big, "", at, at}}
	for _, pl := range w.varPlaces() {
		got, _ := varsOf(t, w.do(t, w.ownerKey, http.MethodPost, pl.path, varEntries("Zed=1", "alpha", long+"="+big)), http.StatusCreated, false)
		if !slices.Equal(got, want) {
			t.Errorf("%s: created %.40q, want Zed=1, alpha= and the long key with the largest value, each at %s", pl.path, got, at)
		}
		if got := w.storedVars(t, pl.scope, pl.id); !slices.Equal(got, []string{"alpha=", long + "=" + big, "Zed=1"}) {
			t.Errorf("%s: the scope holds %.40q, want alpha=, the long key and Zed=1", pl.path, got)
		}
		if got := w.list(t, w.ownerKey, pl.path); !slices.Equal(got, []variable{want[1], want[2], want[0]}) {
			t.Errorf("%s: the list holds %.40q, want alpha, the long key and Zed as created", pl.path, got)
		}
	}
}

func TestCreateVars_RefusesABadRequestWhole(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	for _, pl := range w.varPlaces() {
		w.create(t, w.ownerKey, pl.path, "Token=old")
	}
	w.create(t, w.member2Key, "/user/vars", numbered("V", secret.MaxEntries)...)
	good := map[string]any{"key": "GOOD", "value": "v"}
	huge := strings.Repeat("x", secret.MaxValueSize+1)
	long := strings.Repeat("k", secret.MaxKeyLen+1)
	key := func(k any, v ...any) map[string]any {
		if len(v) == 0 {
			return map[string]any{"key": k}
		}
		return map[string]any{"key": k, "value": v[0]}
	}
	// A refusal: has is what its message holds, hasNot what it must not
	// hold, and msg, when set, the message exactly.
	type refusal struct {
		name        string
		body        any
		status      int
		typ         string
		has, hasNot string
		msg         string
	}
	bad := func(name string, body any, has, hasNot string) refusal {
		return refusal{name, body, http.StatusBadRequest, "invalid_request", has, hasNot, ""}
	}
	// Sent to every place owner reaches.
	everywhere := []refusal{
		bad("no entries", map[string]any{}, "", ""),
		bad("empty entries", entries(), "", ""),
		bad("entries not an array", map[string]any{"entries": "x"}, "", ""),
		bad("an entry not an object", entries(good, 1), "", ""),
		bad("an entry without a key", entries(good, map[string]any{"value": "v"}), "", ""),
		bad("a key not a string", entries(good, key(7)), "", ""),
		bad("an empty key", entries(good, key("")), `""`, ""),
		bad("a key of 256 letters", entries(good, key(long)), long, ""),
		bad("a key beginning with a digit", entries(good, key("1A")), "1A", ""),
		bad("a key with a hyphen", entries(good, key("A-B")), "A-B", ""),
		bad("a key with a non-ASCII letter", entries(good, key("É")), "É", ""),
		bad("a value not a string", entries(good, key("NUM", 5)), "NUM", ""),
		bad("a value one byte too large", entries(good, key("HUGE", huge)), "HUGE", ""),
		bad("a value of two-byte characters too large", entries(good, key("WIDE", strings.Repeat("é", secret.MaxValueSize/2+1))), "WIDE", ""),
		bad("two keys equal ignoring case", entries(good, key("Foo"), key("FOO")), "FOO", ""),
		{"a key the scope holds in another casing", varEntries("NEW=1", "TOKEN=2"), http.StatusConflict, "conflict", "", "", `key "TOKEN" already exists`},
	}
	// The faults of one request on member2's full scope: the first in the
	// order of the checks is answered.
	firstFault := []refusal{
		bad("an invalid key, then a value too large", entries(key("1A"), key("HUGE", huge)), "1A", "HUGE"),
		bad("a value that is not a string, then an invalid key", entries(key("GOOD", 5), key("1A")), "1A", "GOOD"),
		bad("a value too large, then a key that is not a string", entries(key("HUGE", huge), key(7)), "entries[1]", "HUGE"),
		bad("a value too large, then two keys equal ignoring case", entries(key("HUGE", huge), key("Dup"), key("DUP")), "HUGE", "DUP"),
		bad("two keys equal ignoring case in a full scope", entries(key("Dup"), key("DUP")), "DUP", ""),
		{"a key the full scope holds", varEntries("v0"), http.StatusTooManyRequests, "secret_limit_reached", "", "", "maximum of 100 entries per scope exceeded"},
	}
	refuse := func(cred, path string, c refusal) {
		t.Helper()
		a := w.do(t, cred, http.MethodPost, path, c.body)
		typ, msg := errorOf(t, a)
		if a.status != c.status || typ != c.typ || !strings.Contains(msg, c.has) ||
			c.hasNot != "" && strings.Contains(msg, c.hasNot) || c.msg != "" && msg != c.msg {
			t.Errorf("%s, %s: %d %s %.120q; want %d %s naming %.20q, not %q, %q", path, c.name, a.status, typ, msg, c.status, c.typ, c.has, c.hasNot, c.msg)
		}
	}
	for _, pl := range w.varPlaces() {
		for _, c := range everywhere {
			refuse(w.ownerKey, pl.path, c)
		}
		if got := w.storedVars(t, pl.scope, pl.id); !slices.Equal(got, []string{"Token=old"}) {
			t.Errorf("%s: after the refusals the scope holds %.80v, want only Token=old", pl.path, got)
		}
	}
	for _, c := range firstFault {
		refuse(w.member2Key, "/user/vars", c)
	}
	if n := len(w.storedVars(t, secret.ScopeUser, w.member2ID)); n != secret.MaxEntries {
		t.Errorf("after the refusals the full scope holds %d variables, want %d", n, secret.MaxEntries)
	}
}

func TestCreateVars_KeepsAScopeToAHundredEntries(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	const col, limit = "/workspaces/acme/vars", "maximum of 100 entries per scope exceeded"
	w.create(t, w.ownerKey, col, numbered("W", 98)...)
	w.create(t, w.ownerKey, "/user/vars", numbered("U", secret.MaxEntries)...)
	w.seed(t, secret.ScopeWorkspace, "acme", numbered("S", secret.MaxEntries)...)
	// The workspace acme holds 98 variables and a hundred secrets.
	w.create(t, w.ownerKey, col, "N1", "N2")
	for _, keys := range [][]string{{"N3"}, {"w0"}} {
		wantError(t, fmt.Sprint("create ", keys, " in a full scope"), w.do(t, w.ownerKey, http.MethodPost, col, keyed(keys...)),
			http.StatusTooManyRequests, "secret_limit_reached", limit)
	}
	if n := len(w.storedVars(t, secret.ScopeWorkspace, "acme")); n != secret.MaxEntries {
		t.Errorf("the scope holds %d variables, want %d", n, secret.MaxEntries)
	}
	wantError(t, "101 entries in an empty scope", w.do(t, w.memberKey, http.MethodPost, "/user/vars", keyed(numbered("N", secret.MaxEntries+1)...)),
		http.StatusTooManyRequests, "secret_limit_reached", limit)
	// owner's own scope is full.
	w.create(t, w.ownerKey, "/orgs/acme/vars", "ORG")
}

func TestCreateVars_ReplaysARetriedCreate(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	header := http.Header{"Idempotency-Key": {"k1"}}
	status, first := w.CallWith(t, http.MethodPost, "/api/v1/user/vars", w.ownerKey, varEntries("K=v"), header)
	if status != http.StatusCreated {
		t.Fatalf("create = %d %s, want 201", status, first)
	}
	w.advance(time.Minute)
	status, again := w.CallWith(t, http.MethodPost, "/api/v1/user/vars", w.ownerKey, varEntries("K=v"), header)
	if status != http.StatusOK || !bytes.Equal(again, first) {
		t.Errorf("the retried create = %d %s, want 200 %s", status, again, first)
	}
	if got := w.storedVars(t, secret.ScopeUser, w.ownerID); !slices.Equal(got, []string{"K=v"}) {
		t.Errorf("the scope holds %v, want only K=v", got)
	}
}

func TestVarRoutes_ServeWithoutAnEncryptionKey(t *testing.T) {
	t.Parallel()
	w := newKeylessWorld(t)
	w.must(t, w.ownerKey, http.MethodPost, "/user/secrets", keyed("S"), http.StatusServiceUnavailable)
	for _, pl := range w.varPlaces() {
		created, _ := varsOf(t, w.do(t, w.ownerKey, http.MethodPost, pl.path, varEntries("K=readable")), http.StatusCreated, false)
		listed := w.list(t, w.ownerKey, pl.path)
		stored := w.storedVars(t, pl.scope, pl.id)
		if !slices.Equal(created, listed) || len(listed) != 1 || listed[0].Value != "readable" || !slices.Equal(stored, []string{"K=readable"}) {
			t.Errorf("%s: created %v, listed %v, stored %v; want K=readable", pl.path, created, listed, stored)
		}
	}
}

func TestListVars_PagesAScopesOwnVariablesInCaseFoldedOrder(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	at := stamp(w.clock())
	const col = "/workspaces/acme/vars"
	w.create(t, w.ownerKey, col, "b=vb", "A=vA", "c=vc", "B2=vB2")
	want := []variable{{"A", "vA", "", at, at}, {"b", "vb", "", at, at}, {"B2", "vB2", "", at, at}, {"c", "vc", "", at, at}}
	// The owner and a member, who reads the workspace and may not change
	// it, are answered the same list; a page of 2 needs two pages.
	for _, r := range []struct {
		cred  string
		limit int
		pages int
	}{{w.ownerKey, 0, 1}, {w.memberKey, 0, 1}, {w.ownerKey, 2, 2}} {
		if p := w.pages(t, r.cred, col, r.limit); len(p) != r.pages || !slices.Equal(slices.Concat(p...), want) {
			t.Errorf("list with limit %d = %v, want %v in %d pages", r.limit, p, want, r.pages)
		}
	}
	w.create(t, w.memberKey, "/workspaces/acme-fork/vars", "Own")
	if got := keysOf(w.list(t, w.memberKey, "/workspaces/acme-fork/vars")); !slices.Equal(got, []string{"Own"}) {
		t.Errorf("the fork's list = %v, want only its own Own, none of its parent's", got)
	}
	if got := w.list(t, w.ownerKey, "/orgs/acme/vars"); len(got) != 0 {
		t.Errorf("an empty scope's list = %v, want none", got)
	}
	many := numbered("K", 51)
	w.create(t, w.ownerKey, "/user/vars", many...)
	p := w.pages(t, w.ownerKey, "/user/vars", 0)
	if len(p) != 2 || len(p[0]) != 50 || !slices.Equal(keysOf(slices.Concat(p...)), slices.Sorted(slices.Values(many))) {
		t.Errorf("the pages of 51 = %v, want 50 then 1, the keys in order once each", p)
	}
}

func TestUpdateVar_ReplacesTheValueAndKeepsTheKey(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	for _, pl := range w.varPlaces() {
		created := stamp(w.clock())
		w.create(t, w.ownerKey, pl.path, "ApiUrl=a")
		for _, v := range []string{"b", "", strings.Repeat("x", secret.MaxValueSize)} {
			want := variable{"ApiUrl", v, "", created, stamp(w.advance(time.Minute))}
			got := varOf(t, w.do(t, w.ownerKey, http.MethodPatch, pl.path+"/APIURL", map[string]any{"value": v}), http.StatusOK)
			if listed := w.list(t, w.ownerKey, pl.path); got != want || !slices.Equal(listed, []variable{want}) {
				t.Errorf("%s: PATCH APIURL to %.40q = %.40q, list %.40q; want %.40q alone", pl.path, v, got, listed, want)
			}
		}
	}
}

func TestUpdateVar_RefusesABadValueWhetherOrNotTheKeyExists(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	bodies := []map[string]any{{}, {"value": nil}, {"value": 3}, {"value": strings.Repeat("x", secret.MaxValueSize+1)}}
	for _, pl := range w.varPlaces() {
		w.create(t, w.ownerKey, pl.path, "Kept")
		before := w.list(t, w.ownerKey, pl.path)
		w.advance(time.Minute)
		for _, key := range []string{"kept", "Missing"} {
			for _, b := range bodies {
				wantError(t, fmt.Sprintf("PATCH %s/%s with %.40v", pl.path, key, b), w.do(t, w.ownerKey, http.MethodPatch, pl.path+"/"+key, b),
					http.StatusBadRequest, "invalid_request", "")
			}
		}
		if after := w.list(t, w.ownerKey, pl.path); !slices.Equal(after, before) {
			t.Errorf("%s: after the refusals the list holds %v, want %v", pl.path, after, before)
		}
	}
}

func TestVarKeyRoutes_AnswerNotFoundForAKeyTheScopeLacks(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	const col = "/workspaces/acme/vars"
	w.create(t, w.ownerKey, "/user/vars", "UserOnly")
	w.create(t, w.ownerKey, col, "Kept")
	w.seed(t, secret.ScopeWorkspace, "acme", "SecretOnly")
	before := w.allVars(t)
	w.advance(time.Minute)
	// A key never created, one of the owner's user scope, one no rule
	// allows and one the scope holds as a secret.
	for method, body := range map[string]any{http.MethodPatch: changed, http.MethodDelete: nil} {
		for _, key := range []string{"NeVer", "useronly", "a-b", "secretonly"} {
			wantError(t, method+" "+key, w.do(t, w.ownerKey, method, col+"/"+key, body), http.StatusNotFound, "not_found", fmt.Sprintf("key %q not found", key))
		}
	}
	if got := w.allVars(t); !slices.Equal(got, before) {
		t.Errorf("the variables are %v, want %v", got, before)
	}
	if v, found, err := w.secrets.Get(context.Background(), secret.ScopeWorkspace, "acme", "SecretOnly"); !found || err != nil || v != valueOf("SecretOnly") {
		t.Errorf("the secret SecretOnly = %q, %v, %v; want it unchanged", v, found, err)
	}
}

func TestDeleteVar_RemovesTheVariable(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	for _, pl := range w.varPlaces() {
		w.create(t, w.ownerKey, pl.path, "Region", "Other")
		if a := w.must(t, w.ownerKey, http.MethodDelete, pl.path+"/region", nil, http.StatusNoContent); len(a.body) != 0 {
			t.Errorf("%s: DELETE region answered the body %q, want none", pl.path, a.body)
		}
		if got := keysOf(w.list(t, w.ownerKey, pl.path)); !slices.Equal(got, []string{"Other"}) {
			t.Errorf("%s: after the delete the list holds %v, want only Other", pl.path, got)
		}
		wantError(t, pl.path+": a second DELETE", w.do(t, w.ownerKey, http.MethodDelete, pl.path+"/region", nil), http.StatusNotFound, "not_found", `key "region" not found`)
		w.create(t, w.ownerKey, pl.path, "REGION")
	}
}

func TestVarRoutes_RefuseAMethodTheirPathLacks(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	const resolved = "/api/v1/workspaces/acme/vars/resolved"
	w.create(t, w.ownerKey, "/workspaces/acme/vars", "resolved=kept")
	before := w.allVars(t)
	w.advance(time.Minute)
	refused := []struct {
		name, method, path, cred string
		allow                    string
	}{
		{"PATCH on the resolved path", http.MethodPatch, resolved, w.ownerKey, "GET"},
		{"DELETE on the resolved path", http.MethodDelete, resolved, w.ownerKey, "GET"},
		{"PATCH on the resolved path by a member", http.MethodPatch, resolved, w.memberKey, "GET"},
		{"DELETE on the resolved path with no credential", http.MethodDelete, resolved, "", "GET"},
		{"GET on a user variable", http.MethodGet, "/api/v1/user/vars/A", w.ownerKey, "DELETE, PATCH"},
		{"GET on an org variable", http.MethodGet, "/api/v1/orgs/acme/vars/A", w.ownerKey, "DELETE, PATCH"},
		{"GET on a workspace variable", http.MethodGet, "/api/v1/workspaces/acme/vars/A", w.ownerKey, "DELETE, PATCH"},
	}
	for _, r := range refused {
		var body any
		if r.method == http.MethodPatch {
			body = changed
		}
		status, header, b := w.Exchange(t, r.method, r.path, r.cred, body, nil)
		allow := strings.Split(header.Get("Allow"), ", ")
		slices.Sort(allow)
		wantError(t, r.name, answer{status, b}, http.StatusMethodNotAllowed, "method_not_allowed", "")
		if strings.Join(allow, ", ") != r.allow {
			t.Errorf("%s: Allow %q, want %s", r.name, header.Get("Allow"), r.allow)
		}
	}
	if got := w.allVars(t); !slices.Equal(got, before) {
		t.Errorf("after the refusals the store holds %v, want %v", got, before)
	}
	if v := varOf(t, w.do(t, w.ownerKey, http.MethodPatch, "/workspaces/acme/vars/RESOLVED", map[string]any{"value": "x"}), http.StatusOK); v.Key != "resolved" || v.Value != "x" {
		t.Errorf("PATCH RESOLVED changed %s to %q, want resolved to x", v.Key, v.Value)
	}
}

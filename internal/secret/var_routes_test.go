package secret_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/secret"
)

// short is v cut to its first 40 bytes, with its length, for a message
// about a long key or value.
func short(v string) string {
	if len(v) <= 40 {
		return v
	}
	return fmt.Sprintf("%s…(%d bytes)", v[:40], len(v))
}

func TestCreateVars_StoresEveryEntryInRequestOrder(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	at := w.clock().Truncate(time.Second).Format(time.RFC3339)
	long, big := strings.Repeat("k", secret.MaxKeyLen), strings.Repeat("x", secret.MaxValueSize)
	body := entries(map[string]any{"key": "Zed", "value": "1"}, map[string]any{"key": "alpha"}, map[string]any{"key": long, "value": big})
	want := []variable{{"Zed", "1", at, at}, {"alpha", "", at, at}, {long, big, at, at}}
	for _, pl := range w.varPlaces() {
		a := w.do(t, w.ownerKey, http.MethodPost, pl.path, body)
		got := varsOf(t, a, http.StatusCreated)
		var top map[string]json.RawMessage
		if err := json.Unmarshal(a.body, &top); err != nil || len(top) != 1 {
			t.Errorf("%s: the answer has members %v (%v), want only items", pl.path, len(top), err)
		}
		if !slices.Equal(got, want) {
			for i, v := range got {
				t.Errorf("%s: item %d = %s %s %s %s", pl.path, i, short(v.Key), short(v.Value), v.CreatedAt, v.UpdatedAt)
			}
			t.Errorf("%s: want Zed=1, alpha= and the long key with the largest value, each created and updated at %s", pl.path, at)
		}
		stored := w.storedVars(t, pl.scope, pl.id)
		if wantStored := []string{"alpha=", long + "=" + big, "Zed=1"}; !slices.Equal(stored, wantStored) {
			t.Errorf("%s: the scope holds %d variables %.80v, want alpha=, the long key and Zed=1", pl.path, len(stored), stored)
		}
	}
}

func TestCreateVars_RefusesABadRequestWhole(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	for _, pl := range w.varPlaces() {
		if a := w.do(t, w.ownerKey, http.MethodPost, pl.path, entries(map[string]any{"key": "Token", "value": "old"})); a.status != http.StatusCreated {
			t.Fatalf("create Token in %s = %d %s, want 201", pl.path, a.status, a.body)
		}
	}
	w.createVars(t, w.member2Key, "/user/vars", numbered("V", secret.MaxEntries)...)
	good := map[string]any{"key": "GOOD", "value": "v"}
	huge := strings.Repeat("x", secret.MaxValueSize+1)
	long := strings.Repeat("k", secret.MaxKeyLen+1)
	key := func(k any, v ...any) map[string]any {
		if len(v) == 0 {
			return map[string]any{"key": k}
		}
		return map[string]any{"key": k, "value": v[0]}
	}
	// A refusal of a body sent to every place owner reaches; has is what
	// its message holds, hasNot what it must not hold, and msg, when set,
	// the message exactly.
	type refusal struct {
		name        string
		body        any
		status      int
		typ         string
		has, hasNot string
		msg         string
	}
	bad := func(name string, body any, has string) refusal {
		return refusal{name: name, body: body, status: http.StatusBadRequest, typ: "invalid_request", has: has}
	}
	everywhere := []refusal{
		bad("no entries", map[string]any{}, ""),
		bad("empty entries", entries(), ""),
		bad("entries not an array", map[string]any{"entries": "x"}, ""),
		bad("an entry not an object", entries(good, 1), ""),
		bad("an entry without a key", entries(good, map[string]any{"value": "v"}), ""),
		bad("a key not a string", entries(good, key(7)), ""),
		bad("an empty key", entries(good, key("")), `""`),
		bad("a key of 256 letters", entries(good, key(long)), long),
		bad("a key beginning with a digit", entries(good, key("1A")), "1A"),
		bad("a key with a hyphen", entries(good, key("A-B")), "A-B"),
		bad("a key with a non-ASCII letter", entries(good, key("É")), "É"),
		bad("a value not a string", entries(good, key("NUM", 5)), "NUM"),
		bad("a value one byte too large", entries(good, key("HUGE", huge)), "HUGE"),
		bad("a value of two-byte characters too large", entries(good, key("WIDE", strings.Repeat("é", secret.MaxValueSize/2+1))), "WIDE"),
		bad("two keys equal ignoring case", entries(good, key("Foo"), key("FOO")), "FOO"),
		{name: "a key the scope holds in another casing", body: entries(key("NEW", "1"), key("TOKEN", "2")),
			status: http.StatusConflict, typ: "conflict", msg: `key "TOKEN" already exists`},
	}
	// The faults of one request on member2's full scope: the first in the
	// order of the checks is answered.
	firstFault := []refusal{
		{name: "an invalid key, then a value too large", body: entries(key("1A"), key("HUGE", huge)),
			status: http.StatusBadRequest, typ: "invalid_request", has: "1A", hasNot: "HUGE"},
		{name: "a value too large, then two keys equal ignoring case", body: entries(key("HUGE", huge), key("Dup"), key("DUP")),
			status: http.StatusBadRequest, typ: "invalid_request", has: "HUGE", hasNot: "DUP"},
		{name: "two keys equal ignoring case in a full scope", body: entries(key("Dup"), key("DUP")),
			status: http.StatusBadRequest, typ: "invalid_request", has: "DUP"},
		{name: "a key the full scope holds", body: keyed("v0"),
			status: http.StatusTooManyRequests, typ: "secret_limit_reached", msg: "maximum of 100 entries per scope exceeded"},
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
	const col = "/workspaces/acme/vars"
	w.createVars(t, w.ownerKey, col, numbered("W", 98)...)
	w.createVars(t, w.ownerKey, "/user/vars", numbered("U", secret.MaxEntries)...)
	w.seed(t, secret.ScopeWorkspace, "acme", numbered("S", secret.MaxEntries)...)
	// The steps run in order on the workspace acme, which holds 98
	// variables and a hundred secrets.
	steps := []struct {
		name   string
		keys   []string
		status int
	}{
		{"two new entries bring the scope to 100", []string{"N1", "N2"}, http.StatusCreated},
		{"one more new entry", []string{"N3"}, http.StatusTooManyRequests},
		{"an entry whose key the scope holds", []string{"w0"}, http.StatusTooManyRequests},
	}
	for _, s := range steps {
		a := w.do(t, w.ownerKey, http.MethodPost, col, keyed(s.keys...))
		if a.status != s.status {
			t.Errorf("%s: %d %.200s, want %d", s.name, a.status, a.body, s.status)
		}
		if s.status == http.StatusTooManyRequests {
			if typ, msg := errorOf(t, a); typ != "secret_limit_reached" || msg != "maximum of 100 entries per scope exceeded" {
				t.Errorf("%s: %s %q, want secret_limit_reached with the limit message", s.name, typ, msg)
			}
		}
		if n := len(w.storedVars(t, secret.ScopeWorkspace, "acme")); n != secret.MaxEntries {
			t.Errorf("%s: the scope holds %d variables, want %d", s.name, n, secret.MaxEntries)
		}
	}
	if a := w.do(t, w.memberKey, http.MethodPost, "/user/vars", keyed(numbered("N", secret.MaxEntries+1)...)); a.status != http.StatusTooManyRequests {
		t.Errorf("101 entries in an empty scope = %d %.200s, want 429", a.status, a.body)
	}
	if a := w.do(t, w.ownerKey, http.MethodPost, "/orgs/acme/vars", keyed("ORG")); a.status != http.StatusCreated {
		t.Errorf("an org create by a user whose own scope is full = %d %s, want 201", a.status, a.body)
	}
}

func TestCreateVars_ReplaysARetriedCreate(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	body := entries(map[string]any{"key": "K", "value": "v"})
	header := http.Header{"Idempotency-Key": {"k1"}}
	status, first := w.CallWith(t, http.MethodPost, "/api/v1/user/vars", w.ownerKey, body, header)
	if status != http.StatusCreated {
		t.Fatalf("create = %d %s, want 201", status, first)
	}
	w.advance(time.Minute)
	status, again := w.CallWith(t, http.MethodPost, "/api/v1/user/vars", w.ownerKey, body, header)
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
	if a := w.do(t, w.ownerKey, http.MethodPost, "/user/secrets", keyed("S")); a.status != http.StatusServiceUnavailable {
		t.Fatalf("a secret create on the keyless hub = %d %s, want 503", a.status, a.body)
	}
	for _, pl := range w.varPlaces() {
		a := w.do(t, w.ownerKey, http.MethodPost, pl.path, entries(map[string]any{"key": "K", "value": "readable"}))
		if got := varsOf(t, a, http.StatusCreated); len(got) != 1 || got[0].Value != "readable" {
			t.Errorf("%s: created %v, want K=readable", pl.path, got)
		}
		if got := w.storedVars(t, pl.scope, pl.id); !slices.Equal(got, []string{"K=readable"}) {
			t.Errorf("%s: the scope holds %v, want K=readable", pl.path, got)
		}
	}
}

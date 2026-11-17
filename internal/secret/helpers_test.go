package secret_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/fs"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/secret"
)

// newKey returns a random key in the raw form and in the base64 form an
// operator configures it in.
func newKey(t *testing.T) (raw []byte, encoded string) {
	t.Helper()
	raw = make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	return raw, base64.StdEncoding.EncodeToString(raw)
}

// route is one secret route as the route tests call it: its scope, its
// method, its path below /api/v1 with %s where the org or workspace goes
// (a user path has none), a body the route accepts, the PAT scope it needs
// and the status it answers a caller it serves.
type route struct {
	scope  secret.Scope
	method string
	path   string
	body   any
	need   string
	ok     int
}

// created is a create body the create routes accept once per scope.
var created = entries(map[string]any{"key": "ROUTE_KEY", "value": "v"})

// routes are every secret route. The access tests iterate them, so a route
// added here is held to the access rules of its scope. A scope's POST
// creates ROUTE_KEY, its PATCH changes it and its DELETE removes it, so a
// test that calls every route in this order sees each one succeed.
var routes = []route{
	{secret.ScopeUser, http.MethodGet, "/user/secrets", nil, "secrets:read", http.StatusOK},
	{secret.ScopeOrg, http.MethodGet, "/orgs/%s/secrets", nil, "secrets:read", http.StatusOK},
	{secret.ScopeWorkspace, http.MethodGet, "/workspaces/%s/secrets", nil, "secrets:read", http.StatusOK},
	{secret.ScopeUser, http.MethodPost, "/user/secrets", created, "secrets:write", http.StatusCreated},
	{secret.ScopeOrg, http.MethodPost, "/orgs/%s/secrets", created, "secrets:write", http.StatusCreated},
	{secret.ScopeWorkspace, http.MethodPost, "/workspaces/%s/secrets", created, "secrets:write", http.StatusCreated},
	{secret.ScopeUser, http.MethodPatch, "/user/secrets/ROUTE_KEY", changed, "secrets:write", http.StatusOK},
	{secret.ScopeOrg, http.MethodPatch, "/orgs/%s/secrets/ROUTE_KEY", changed, "secrets:write", http.StatusOK},
	{secret.ScopeWorkspace, http.MethodPatch, "/workspaces/%s/secrets/ROUTE_KEY", changed, "secrets:write", http.StatusOK},
	{secret.ScopeUser, http.MethodDelete, "/user/secrets/route_key", nil, "secrets:write", http.StatusNoContent},
	{secret.ScopeOrg, http.MethodDelete, "/orgs/%s/secrets/route_key", nil, "secrets:write", http.StatusNoContent},
	{secret.ScopeWorkspace, http.MethodDelete, "/workspaces/%s/secrets/route_key", nil, "secrets:write", http.StatusNoContent},
}

// changed is an update body.
var changed = map[string]any{"value": "changed"}

// entries is a create body of the entries given.
func entries(e ...any) map[string]any { return map[string]any{"entries": e} }

// keyed is a create body of entries with the keys given and no value.
func keyed(keys ...string) map[string]any {
	e := make([]any, len(keys))
	for i, k := range keys {
		e[i] = map[string]any{"key": k}
	}
	return entries(e...)
}

// at is the route's path for the org or workspace name.
func (r route) at(name string) string {
	if r.scope == secret.ScopeUser {
		return r.path
	}
	return fmt.Sprintf(r.path, name)
}

func (r route) String() string { return r.method + " " + r.path }

// world is a hub with a current secret key, a clock that stands still
// until a test advances it, and the callers the route tests ask with. The org acme has the members owner, member and member2;
// outsider belongs to no org but their own. The workspace acme is owner's,
// in the org acme, and acme-fork is a task fork of it that member created.
// The org frozen, of which member is a member, is blocked.
type world struct {
	*hubtest.Hub
	// secrets and events are the hub's secret and event components, taken
	// again at each restart.
	secrets *secret.Component
	events  *event.Component
	// mu guards now, the hub's time, which the hub reads while a test
	// advances it.
	mu  sync.Mutex
	now time.Time
	// ownerID, memberID, member2ID and outsiderID are the users' ids, and
	// the matching keys their API keys.
	ownerID, memberID, member2ID, outsiderID     string
	ownerKey, memberKey, member2Key, outsiderKey string
	// orgID is the id of the org acme.
	orgID string
}

// newWorld starts a hub with a current secret key and builds the world in
// it.
func newWorld(t *testing.T) *world {
	t.Helper()
	_, key := newKey(t)
	return newWorldEnv(t, map[string]string{secret.EnvCurrentKey: key})
}

// newKeylessWorld builds the world in a hub with no secret key configured.
func newKeylessWorld(t *testing.T) *world {
	t.Helper()
	return newWorldEnv(t, nil)
}

// newWorldEnv builds the world in a hub with env added to its environment.
func newWorldEnv(t *testing.T, env map[string]string) *world {
	t.Helper()
	// Halfway through a second, so that a stored time is seen cut to whole
	// seconds.
	w := &world{now: time.Now().UTC().Truncate(time.Second).Add(-time.Second / 2)}
	w.Hub = hubtest.StartWith(t, hubtest.Config{
		Env:     env,
		Now:     w.clock,
		Compose: func(c *hub.Composition) { w.secrets, w.events = c.Secrets(), c.Events() },
	})
	w.ownerID, w.memberID = w.CreateUser(t, "owner"), w.CreateUser(t, "member")
	w.member2ID, w.outsiderID = w.CreateUser(t, "member2"), w.CreateUser(t, "outsider")
	w.ownerKey, w.memberKey = w.IssueKey(t, w.ownerID), w.IssueKey(t, w.memberID)
	w.member2Key, w.outsiderKey = w.IssueKey(t, w.member2ID), w.IssueKey(t, w.outsiderID)
	w.orgID = w.CreateOrg(t, "acme")
	for _, id := range []string{w.ownerID, w.memberID, w.member2ID} {
		w.AddMember(t, w.orgID, id)
	}
	frozen := w.CreateOrg(t, "frozen")
	w.AddMember(t, frozen, w.memberID)
	if a := w.do(t, w.Token, http.MethodPost, "/orgs/frozen/block", nil); a.status != http.StatusOK {
		t.Fatalf("block the org frozen = %d %s, want 200", a.status, a.body)
	}
	w.AddWorkspace(t, access.Record{Slug: "acme", OwnerID: w.ownerID, OrgID: w.orgID,
		Kind: access.KindRepository, Status: access.StatusReady})
	w.AddWorkspace(t, access.Record{Slug: "acme-fork", OwnerID: w.memberID, OrgID: w.orgID,
		Kind: access.KindFork, ParentSlug: "acme", Status: access.StatusReady})
	return w
}

// clock is the hub's time.
func (w *world) clock() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.now
}

// advance moves the hub's time on by d and returns the new time.
func (w *world) advance(d time.Duration) time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.now = w.now.Add(d)
	return w.now
}

// place is a set of secrets that owner reaches: its scope, its owner's id
// and its collection path.
type place struct {
	scope    secret.Scope
	id, path string
}

// places are owner's user secrets, those of the org acme and those of the
// workspace acme.
func (w *world) places() []place {
	return []place{
		{secret.ScopeUser, w.ownerID, "/user/secrets"},
		{secret.ScopeOrg, w.orgID, "/orgs/acme/secrets"},
		{secret.ScopeWorkspace, "acme", "/workspaces/acme/secrets"},
	}
}

// answer is the status and the body of a hub's answer.
type answer struct {
	status int
	body   []byte
}

// do sends method path, below /api/v1, with cred as the bearer credential
// and body as JSON unless it is nil.
func (w *world) do(t *testing.T, cred, method, path string, body any) answer {
	t.Helper()
	status, b := w.Call(t, method, "/api/v1"+path, cred, body)
	return answer{status: status, body: b}
}

// seed stores the secrets keys in one scope through the component, each
// with the value valueOf gives it.
func (w *world) seed(t *testing.T, scope secret.Scope, id string, keys ...string) {
	t.Helper()
	ctx := context.Background()
	err := w.Store.Tx(ctx, func(tx *sql.Tx) error {
		for _, k := range keys {
			if err := w.secrets.Put(ctx, tx, scope, id, k, valueOf(k)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed %v in %s %s: %v", keys, scope, id, err)
	}
}

// count is the number of secrets a scope holds.
func (w *world) count(t *testing.T, scope secret.Scope, id string) int {
	t.Helper()
	n, err := w.secrets.Count(context.Background(), scope, id)
	if err != nil {
		t.Fatalf("count the secrets of %s %s: %v", scope, id, err)
	}
	return n
}

// eventsAfter returns the events of the log after seq and the log's last
// seq, from which a later call reads what a request wrote.
func (w *world) eventsAfter(t *testing.T, seq int64) ([]event.Record, int64) {
	t.Helper()
	recs, last, err := w.events.ReadAfter(context.Background(), seq, 0)
	if err != nil {
		t.Fatalf("read the events after %d: %v", seq, err)
	}
	return recs, last
}

// varEventsAfter returns the variable events of the log after seq and the
// log's last seq. It leaves out the events other components write in every
// hub, such as the overlaps the awareness worker finds on its own time.
func (w *world) varEventsAfter(t *testing.T, seq int64) ([]event.Record, int64) {
	t.Helper()
	recs, last := w.eventsAfter(t, seq)
	vars := []event.Record{}
	for _, ev := range recs {
		if strings.HasPrefix(ev.Type, "parley.variable.") {
			vars = append(vars, ev)
		}
	}
	return vars, last
}

// valueOf is the value seed stores under key: text no answer may hold.
func valueOf(key string) string { return "sv-" + key + "-6f1d0c9a" }

// errorOf decodes the error envelope of a.
func errorOf(t *testing.T, a answer) (errorType, message string) {
	t.Helper()
	var env struct {
		Error struct {
			ErrorType string `json:"error_type"`
			Message   string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(a.body, &env); err != nil {
		t.Fatalf("decode the error envelope %s: %v", a.body, err)
	}
	return env.Error.ErrorType, env.Error.Message
}

// page is a decoded list answer.
type page struct {
	Items      []map[string]json.RawMessage `json:"items"`
	NextCursor *string                      `json:"next_cursor"`
}

// pageOf decodes the answer a, which must be 200 or have the status
// given, as a page, and checks that each of its items has exactly the
// members key, created_at and updated_at: never a value.
func pageOf(t *testing.T, a answer, status ...int) page {
	t.Helper()
	var p page
	if want := append(status, http.StatusOK)[0]; a.status != want {
		t.Fatalf("answer = %d %s, want %d", a.status, a.body, want)
	}
	if err := json.Unmarshal(a.body, &p); err != nil {
		t.Fatalf("decode the page %s: %v", a.body, err)
	}
	for _, it := range p.Items {
		if members := slices.Sorted(maps.Keys(it)); !slices.Equal(members, []string{"created_at", "key", "updated_at"}) {
			t.Errorf("item members = %v, want exactly key, created_at, updated_at", members)
		}
	}
	return p
}

// keys are the keys of the items of p, in order.
func (p page) keys(t *testing.T) []string {
	t.Helper()
	out := []string{}
	for _, it := range p.Items {
		var k string
		if err := json.Unmarshal(it["key"], &k); err != nil {
			t.Fatalf("the item %v has no string key: %v", it, err)
		}
		out = append(out, k)
	}
	return out
}

// numbered are the n keys prefix0, prefix1 and so on.
func numbered(prefix string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s%d", prefix, i)
	}
	return out
}

// metadataOf decodes the metadata of the event ev.
func metadataOf(t *testing.T, ev event.Record) map[string]string {
	t.Helper()
	var meta map[string]string
	if err := json.Unmarshal(ev.Metadata, &meta); err != nil {
		t.Fatalf("decode the metadata %s of event %d: %v", ev.Metadata, ev.Seq, err)
	}
	return meta
}

// passEnd adds to c a maintenance task that marks the end of each pass on
// ended, a channel with room for one mark. Tasks added after the hub's own
// run last in a pass.
func passEnd(c *hub.Composition, ended chan struct{}) {
	c.AddMaintenanceTask("test-pass-end", func(context.Context, *hub.Runtime) error {
		select {
		case ended <- struct{}{}:
		default:
		}
		return nil
	})
}

// waitPass waits up to 5 s for the mark of a pass's end.
func waitPass(t *testing.T, ended chan struct{}) {
	t.Helper()
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the maintenance pass did not end within 5 s")
	}
}

// stopAndRead stops the hub and returns what it kept and wrote: every file
// of its data directory, the database and its -wal and -shm files among
// them, and its log, by where it was.
func stopAndRead(t *testing.T, h *hubtest.Hub) map[string][]byte {
	t.Helper()
	dir, _ := h.Listening["data_dir"].(string)
	h.Stop()
	if code := h.ExitCode(t, 10*time.Second); code != 0 {
		t.Fatalf("the hub stopped with %d, want 0; log = %s", code, h.Log.String())
	}
	sources := map[string][]byte{"the log": []byte(h.Log.String())}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		sources[path], err = os.ReadFile(path)
		return err
	})
	if err != nil || len(sources) == 1 {
		t.Fatalf("read the data directory %s: %d files, %v", dir, len(sources)-1, err)
	}
	return sources
}

// noneHolds fails the test for each source that holds one of the needles,
// each named by what it is.
func noneHolds(t *testing.T, sources, needles map[string][]byte) {
	t.Helper()
	for where, content := range sources {
		for what, needle := range needles {
			if bytes.Contains(content, needle) {
				t.Errorf("%s holds %s", where, what)
			}
		}
	}
}

func ptr(s string) *string { return &s }

// equalPtr reports whether a and b are both nil or point to equal strings.
func equalPtr(a, b *string) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }

// varPlaces are owner's user variables, those of the org acme and those of
// the workspace acme.
func (w *world) varPlaces() []place {
	return []place{
		{secret.ScopeUser, w.ownerID, "/user/vars"},
		{secret.ScopeOrg, w.orgID, "/orgs/acme/vars"},
		{secret.ScopeWorkspace, "acme", "/workspaces/acme/vars"},
	}
}

// variable is a variable as an answer gives it.
type variable struct {
	Key       string `json:"key"`
	Value     string `json:"value"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// varsOf decodes the answer a, which must have the status given, as
// {"items": [...]}, and checks that each item has exactly the members key,
// value, created_at and updated_at.
func varsOf(t *testing.T, a answer, status int) []variable {
	t.Helper()
	if a.status != status {
		t.Fatalf("answer = %d %.300s, want %d", a.status, a.body, status)
	}
	var raw struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	var p struct {
		Items []variable `json:"items"`
	}
	if err := json.Unmarshal(a.body, &raw); err != nil {
		t.Fatalf("decode the items %.300s: %v", a.body, err)
	}
	if err := json.Unmarshal(a.body, &p); err != nil {
		t.Fatalf("decode the items %.300s: %v", a.body, err)
	}
	for _, it := range raw.Items {
		if members := slices.Sorted(maps.Keys(it)); !slices.Equal(members, []string{"created_at", "key", "updated_at", "value"}) {
			t.Errorf("item members = %v, want exactly key, value, created_at, updated_at", members)
		}
	}
	return p.Items
}

// storedVars are the variables a scope holds, as key=value, in the order of
// their keys with A to Z lowered, read from the store.
func (w *world) storedVars(t *testing.T, scope secret.Scope, id string) []string {
	t.Helper()
	rows, err := w.Store.Main().QueryContext(context.Background(),
		`SELECT key, value FROM variables WHERE scope = ? AND scope_id = ? ORDER BY key_lower`, scope, id)
	if err != nil {
		t.Fatalf("read the variables of %s %s: %v", scope, id, err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			t.Fatalf("read the variables of %s %s: %v", scope, id, err)
		}
		out = append(out, k+"="+v)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the variables of %s %s: %v", scope, id, err)
	}
	return out
}

// createVars creates the variables keys, each with no value, through the
// collection path with cred, and fails the test unless it is answered 201.
func (w *world) createVars(t *testing.T, cred, path string, keys ...string) {
	t.Helper()
	if a := w.do(t, cred, http.MethodPost, path, keyed(keys...)); a.status != http.StatusCreated {
		t.Fatalf("create %d variables in %s = %d %.300s, want 201", len(keys), path, a.status, a.body)
	}
}

// varPageOf decodes the answer a, which must be 200, as a page of
// variables: its items, each checked as varsOf checks them, and its
// next_cursor, which must be present.
func varPageOf(t *testing.T, a answer) ([]variable, *string) {
	t.Helper()
	items := varsOf(t, a, http.StatusOK)
	var top map[string]json.RawMessage
	if err := json.Unmarshal(a.body, &top); err != nil || len(top) != 2 || top["items"] == nil || top["next_cursor"] == nil {
		t.Fatalf("the page %.300s (%v) has not exactly the members items and next_cursor", a.body, err)
	}
	if bytes.HasPrefix(top["items"], []byte("null")) {
		t.Errorf("the page %.300s has items null, want an array", a.body)
	}
	var next *string
	if err := json.Unmarshal(top["next_cursor"], &next); err != nil {
		t.Fatalf("next_cursor of %.300s: %v", a.body, err)
	}
	return items, next
}

// listVars are the variables the collection path answers cred with, as
// one page of up to 200, which must be the last.
func (w *world) listVars(t *testing.T, cred, path string) []variable {
	t.Helper()
	items, next := varPageOf(t, w.do(t, cred, http.MethodGet, path+"?limit=200", nil))
	if next != nil {
		t.Fatalf("GET %s: next_cursor %q, want null", path, *next)
	}
	return items
}

// varOf decodes the answer a, which must have the status given, as one
// variable with exactly the members key, value, created_at and updated_at.
func varOf(t *testing.T, a answer, status int) variable {
	t.Helper()
	if a.status != status {
		t.Fatalf("answer = %d %.300s, want %d", a.status, a.body, status)
	}
	var raw map[string]json.RawMessage
	var v variable
	if err := json.Unmarshal(a.body, &raw); err != nil {
		t.Fatalf("decode the variable %.300s: %v", a.body, err)
	}
	if err := json.Unmarshal(a.body, &v); err != nil {
		t.Fatalf("decode the variable %.300s: %v", a.body, err)
	}
	if members := slices.Sorted(maps.Keys(raw)); !slices.Equal(members, []string{"created_at", "key", "updated_at", "value"}) {
		t.Errorf("variable members = %v, want exactly key, value, created_at, updated_at", members)
	}
	return v
}

// keysOf are the keys of vars, in order.
func keysOf(vars []variable) []string {
	out := make([]string, len(vars))
	for i, v := range vars {
		out[i] = v.Key
	}
	return out
}

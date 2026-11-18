package secret_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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

// varRoutes are every variable route that an access check guards, in the
// order of routes: a scope's POST creates ROUTE_KEY, its PATCH changes it
// and its DELETE removes it. The access tests iterate them, so a route added
// here is held to the access rules of its scope. PATCH and DELETE on the
// resolved path answer 405 before any check and are not here.
var varRoutes = []route{
	{secret.ScopeUser, http.MethodGet, "/user/vars", nil, "vars:read", http.StatusOK},
	{secret.ScopeOrg, http.MethodGet, "/orgs/%s/vars", nil, "vars:read", http.StatusOK},
	{secret.ScopeWorkspace, http.MethodGet, "/workspaces/%s/vars", nil, "vars:read", http.StatusOK},
	{secret.ScopeWorkspace, http.MethodGet, "/workspaces/%s/vars/resolved", nil, "vars:read", http.StatusOK},
	{secret.ScopeUser, http.MethodPost, "/user/vars", created, "vars:write", http.StatusCreated},
	{secret.ScopeOrg, http.MethodPost, "/orgs/%s/vars", created, "vars:write", http.StatusCreated},
	{secret.ScopeWorkspace, http.MethodPost, "/workspaces/%s/vars", created, "vars:write", http.StatusCreated},
	{secret.ScopeUser, http.MethodPatch, "/user/vars/ROUTE_KEY", changed, "vars:write", http.StatusOK},
	{secret.ScopeOrg, http.MethodPatch, "/orgs/%s/vars/ROUTE_KEY", changed, "vars:write", http.StatusOK},
	{secret.ScopeWorkspace, http.MethodPatch, "/workspaces/%s/vars/ROUTE_KEY", changed, "vars:write", http.StatusOK},
	{secret.ScopeUser, http.MethodDelete, "/user/vars/route_key", nil, "vars:write", http.StatusNoContent},
	{secret.ScopeOrg, http.MethodDelete, "/orgs/%s/vars/route_key", nil, "vars:write", http.StatusNoContent},
	{secret.ScopeWorkspace, http.MethodDelete, "/workspaces/%s/vars/route_key", nil, "vars:write", http.StatusNoContent},
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

// post sends body as JSON to path, below /api/v1, with cred as the bearer
// credential, and returns the answer or the error of sending it, never
// failing the test, so that a goroutine of the test may call it.
func (w *world) post(cred, path string, body any) (answer, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return answer{}, err
	}
	req, err := http.NewRequest(http.MethodPost, w.URL("/api/v1"+path), bytes.NewReader(raw))
	if err != nil {
		return answer{}, err
	}
	req.Header.Set("Authorization", "Bearer "+cred)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return answer{}, err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	return answer{status: res.StatusCode, body: b}, err
}

// seed stores the secrets keys in one scope through the component, each
// with the value valueOf gives it.
func (w *world) seed(t *testing.T, scope secret.Scope, id string, keys ...string) {
	t.Helper()
	pairs := []string{}
	for _, k := range keys {
		pairs = append(pairs, k, valueOf(k))
	}
	w.putSecrets(t, scope, id, pairs...)
}

// putSecrets stores, through the component, the secrets pairs names in
// one scope as key, value, key, value and so on; a key the scope holds
// ignoring case gets the new value.
func (w *world) putSecrets(t *testing.T, scope secret.Scope, id string, pairs ...string) {
	t.Helper()
	ctx := context.Background()
	err := w.Store.Tx(ctx, func(tx *sql.Tx) error {
		for i := 0; i+1 < len(pairs); i += 2 {
			if err := w.secrets.Put(ctx, tx, scope, id, pairs[i], pairs[i+1]); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("store %v in %s %s: %v", pairs, scope, id, err)
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

// variable is a variable as an answer gives it; only a resolved variable
// has an origin.
type variable struct {
	Key       string `json:"key"`
	Value     string `json:"value"`
	Origin    string `json:"origin,omitempty"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// hasMembers fails the test unless the JSON object raw has exactly the
// members want, in sorted order.
func hasMembers(t *testing.T, raw []byte, want ...string) {
	t.Helper()
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("decode %.300s: %v", raw, err)
	}
	if got := slices.Sorted(maps.Keys(obj)); !slices.Equal(got, want) {
		t.Errorf("%.300s has the members %v, want %v", raw, got, want)
	}
}

// varOf decodes the answer a, which must have the status given, as one
// variable with exactly the members key, value, created_at and updated_at.
func varOf(t *testing.T, a answer, status int) variable {
	t.Helper()
	if a.status != status {
		t.Fatalf("answer = %d %.300s, want %d", a.status, a.body, status)
	}
	hasMembers(t, a.body, "created_at", "key", "updated_at", "value")
	var v variable
	if err := json.Unmarshal(a.body, &v); err != nil {
		t.Fatalf("decode the variable %.300s: %v", a.body, err)
	}
	return v
}

// varsOf decodes the answer a, which must have the status given, as its
// items and its next_cursor. A create's 201 has the member items alone, a
// page items and next_cursor; items is an array, and each item has exactly
// the members key, value, created_at and updated_at, and origin as well
// when resolved is set.
func varsOf(t *testing.T, a answer, status int, resolved bool) ([]variable, *string) {
	t.Helper()
	if a.status != status {
		t.Fatalf("answer = %d %.300s, want %d", a.status, a.body, status)
	}
	if status == http.StatusCreated {
		hasMembers(t, a.body, "items")
	} else {
		hasMembers(t, a.body, "items", "next_cursor")
	}
	var p struct {
		Items []json.RawMessage `json:"items"`
		Next  *string           `json:"next_cursor"`
	}
	if err := json.Unmarshal(a.body, &p); err != nil || p.Items == nil {
		t.Fatalf("the items of %.300s are not an array: %v", a.body, err)
	}
	members := []string{"created_at", "key", "updated_at", "value"}
	if resolved {
		members = []string{"created_at", "key", "origin", "updated_at", "value"}
	}
	items := make([]variable, len(p.Items))
	for i, raw := range p.Items {
		hasMembers(t, raw, members...)
		if err := json.Unmarshal(raw, &items[i]); err != nil {
			t.Fatalf("decode the item %.300s: %v", raw, err)
		}
	}
	return items, p.Next
}

// pages walks the list or resolved path as cred, limit items a page or the
// route's default with limit 0, and returns its pages, the last the one
// whose next_cursor is null.
func (w *world) pages(t *testing.T, cred, path string, limit int) [][]variable {
	t.Helper()
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var out [][]variable
	for range 10 {
		items, next := varsOf(t, w.do(t, cred, http.MethodGet, path+"?"+q.Encode(), nil), http.StatusOK, strings.HasSuffix(path, "/resolved"))
		if limit > 0 && len(items) > limit {
			t.Errorf("GET %s: a page of %d items, want at most %d", path, len(items), limit)
		}
		if out = append(out, items); next == nil {
			return out
		}
		q.Set("cursor", *next)
	}
	t.Fatalf("GET %s: more than 10 pages", path)
	return nil
}

// list is what the list or resolved path answers cred, which must be one
// page of up to 200.
func (w *world) list(t *testing.T, cred, path string) []variable {
	t.Helper()
	p := w.pages(t, cred, path, 200)
	if len(p) != 1 {
		t.Fatalf("GET %s: %d pages, want 1", path, len(p))
	}
	return p[0]
}

// resolve is the resolved variables of the workspace slug that cred is
// answered.
func (w *world) resolve(t *testing.T, cred, slug string) []variable {
	t.Helper()
	return w.list(t, cred, "/workspaces/"+slug+"/vars/resolved")
}

// keysOf are the keys of vars, in order.
func keysOf(vars []variable) []string {
	out := make([]string, len(vars))
	for i, v := range vars {
		out[i] = v.Key
	}
	return out
}

// allVars are every variable the store holds, as scope/scope id/key=value,
// in that order with A to Z lowered in the key.
func (w *world) allVars(t *testing.T) []string {
	t.Helper()
	rows, err := w.Store.Main().QueryContext(context.Background(),
		`SELECT scope || '/' || scope_id || '/' || key || '=' || value FROM variables ORDER BY scope, scope_id, key_lower`)
	if err != nil {
		t.Fatalf("read the variables: %v", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			t.Fatalf("read the variables: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the variables: %v", err)
	}
	return out
}

// storedVars are the variables one scope holds, as allVars gives them less
// the scope and its id.
func (w *world) storedVars(t *testing.T, scope secret.Scope, id string) []string {
	t.Helper()
	out := []string{}
	for _, r := range w.allVars(t) {
		if kv, ok := strings.CutPrefix(r, string(scope)+"/"+id+"/"); ok {
			out = append(out, kv)
		}
	}
	return out
}

// varEntries is a create body of an entry for each spec: "KEY=value", or
// "KEY" for an entry with no value.
func varEntries(specs ...string) map[string]any {
	e := make([]any, len(specs))
	for i, s := range specs {
		k, v, ok := strings.Cut(s, "=")
		if e[i] = map[string]any{"key": k}; ok {
			e[i] = map[string]any{"key": k, "value": v}
		}
	}
	return entries(e...)
}

// create creates, through the collection path with cred, the variables
// specs names as varEntries reads them, and fails the test unless it is
// answered 201.
func (w *world) create(t *testing.T, cred, path string, specs ...string) {
	t.Helper()
	w.must(t, cred, http.MethodPost, path, varEntries(specs...), http.StatusCreated)
}

// must sends a request as do does, and fails the test unless it is
// answered status.
func (w *world) must(t *testing.T, cred, method, path string, body any, status int) answer {
	t.Helper()
	a := w.do(t, cred, method, path, body)
	if a.status != status {
		t.Fatalf("%s %s = %d %.300s, want %d", method, path, a.status, a.body, status)
	}
	return a
}

// admin sends method path, below /api/v1, as the admin token, and fails
// the test unless it is answered 2xx.
func (w *world) admin(t *testing.T, method, path string) {
	t.Helper()
	if a := w.do(t, w.Token, method, path, nil); a.status/100 != 2 {
		t.Fatalf("%s %s as the admin token = %d %.300s, want 2xx", method, path, a.status, a.body)
	}
}

// wantError fails the test unless a is the error status with the type typ
// and, unless msg is empty, the message msg; what names the request.
func wantError(t *testing.T, what string, a answer, status int, typ, msg string) {
	t.Helper()
	if a.status != status {
		t.Errorf("%s = %d %.300s, want %d %s", what, a.status, a.body, status, typ)
		return
	}
	if gotType, got := errorOf(t, a); gotType != typ || msg != "" && got != msg {
		t.Errorf("%s = %s %q, want %s %q", what, gotType, got, typ, msg)
	}
}

// stamp is the time t as an answer gives it: whole seconds, RFC 3339.
func stamp(t time.Time) string { return t.Truncate(time.Second).Format(time.RFC3339) }

// exec runs query in a transaction of the hub's store.
func (w *world) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	ctx := context.Background()
	if err := w.Store.Tx(ctx, func(tx *sql.Tx) error { _, err := tx.ExecContext(ctx, query, args...); return err }); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

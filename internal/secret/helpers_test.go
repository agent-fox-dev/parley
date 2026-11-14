package secret_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
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

// logLines decodes every line of a hub's log whose msg is msg.
func logLines(t *testing.T, log, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for line := range strings.SplitSeq(log, "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) == nil && rec["msg"] == msg {
			out = append(out, rec)
		}
	}
	return out
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
// added here is held to the access rules of its scope.
var routes = []route{
	{secret.ScopeUser, http.MethodGet, "/user/secrets", nil, "secrets:read", http.StatusOK},
	{secret.ScopeOrg, http.MethodGet, "/orgs/%s/secrets", nil, "secrets:read", http.StatusOK},
	{secret.ScopeWorkspace, http.MethodGet, "/workspaces/%s/secrets", nil, "secrets:read", http.StatusOK},
	{secret.ScopeUser, http.MethodPost, "/user/secrets", created, "secrets:write", http.StatusCreated},
	{secret.ScopeOrg, http.MethodPost, "/orgs/%s/secrets", created, "secrets:write", http.StatusCreated},
	{secret.ScopeWorkspace, http.MethodPost, "/workspaces/%s/secrets", created, "secrets:write", http.StatusCreated},
}

// entries is a create body of the entries given.
func entries(e ...any) map[string]any { return map[string]any{"entries": e} }

// routesOf are the routes of scope.
func routesOf(scope secret.Scope) []route {
	var out []route
	for _, r := range routes {
		if r.scope == scope {
			out = append(out, r)
		}
	}
	return out
}

// at is the route's path for the org or workspace name.
func (r route) at(name string) string {
	if r.scope == secret.ScopeUser {
		return r.path
	}
	return fmt.Sprintf(r.path, name)
}

func (r route) String() string { return r.method + " " + r.path }

// world is a hub with a current secret key, a clock that stands still at
// now, and the callers the route tests
// ask with. The org acme has the members owner, member and member2;
// outsider belongs to no org but their own. The workspace acme is owner's,
// in the org acme, and acme-fork is a task fork of it that member created.
// The org frozen, of which member is a member, is blocked.
type world struct {
	*hubtest.Hub
	// secrets and events are the hub's secret and event components, taken
	// again at each restart.
	secrets *secret.Component
	events  *event.Component
	// now is the hub's time.
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
	// Halfway through a second, so that a stored time is seen cut to whole
	// seconds.
	w := &world{now: time.Now().UTC().Truncate(time.Second).Add(-time.Second / 2)}
	w.Hub = hubtest.StartWith(t, hubtest.Config{
		Env:     map[string]string{secret.EnvCurrentKey: key},
		Now:     func() time.Time { return w.now },
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

// pageOf decodes the list answer a, which must be 200.
func pageOf(t *testing.T, a answer) page { t.Helper(); return itemsOf(t, a, http.StatusOK) }

// itemsOf decodes the answer a, which must have the status want, as a page.
func itemsOf(t *testing.T, a answer, want int) page {
	t.Helper()
	var p page
	if a.status != want {
		t.Fatalf("answer = %d %s, want %d", a.status, a.body, want)
	}
	if err := json.Unmarshal(a.body, &p); err != nil {
		t.Fatalf("decode the page %s: %v", a.body, err)
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

func ptr(s string) *string { return &s }

// equalPtr reports whether a and b are both nil or point to equal strings.
func equalPtr(a, b *string) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }

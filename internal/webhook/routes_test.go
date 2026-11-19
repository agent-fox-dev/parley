package webhook_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/google/uuid"
)

// The scopes of a PAT the webhook routes ask for.
const (
	scopeReadName  = "workspaces:read"
	scopeWriteName = "workspaces:write"
)

// listPage is one page of the webhook list.
type listPage struct {
	Items      []map[string]any `json:"items"`
	NextCursor *string          `json:"next_cursor"`
}

// list reads one page of the webhook list of slug with the query query.
func (w *world) list(t *testing.T, slug string, query url.Values) (int, listPage, []byte) {
	t.Helper()
	path := hooksPath(slug)
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	status, raw := w.Call(t, http.MethodGet, path, w.ownerKey, nil)
	var page listPage
	if status == http.StatusOK {
		if err := json.Unmarshal(raw, &page); err != nil {
			t.Fatalf("decode the page %s: %v", raw, err)
		}
	}
	return status, page, raw
}

// ids are the ids of the items of a page.
func ids(items []map[string]any) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i], _ = it["id"].(string)
	}
	return out
}

func TestList_AnswersTheWorkspaceSubscriptionsInCreationOrder(t *testing.T) {
	t.Parallel()
	w := newWorld(t, options{})
	const url0 = "https://ci.example/x"
	// Five subscriptions of demo, the second and the third created in the
	// same second, so that their id orders them, and one of other.
	var want []string
	created := func() string {
		id, _ := w.mustCreate(t, demo, url0)["id"].(string)
		return id
	}
	want = append(want, created())
	w.clock.advance(time.Second)
	pair := []string{created(), created()}
	slices.Sort(pair)
	want = append(want, pair...)
	w.clock.advance(time.Second)
	want = append(want, created())
	w.clock.advance(time.Second)
	disabled := created()
	want = append(want, disabled)
	if _, err := w.Store.Main().Exec(`UPDATE webhook_subscriptions SET state = 'disabled' WHERE id = ?`, disabled); err != nil {
		t.Fatal(err)
	}
	otherID, _ := w.mustCreate(t, other, url0)["id"].(string)

	t.Run("one page holds every subscription in order", func(t *testing.T) {
		status, page, raw := w.list(t, demo, nil)
		if status != http.StatusOK {
			t.Fatalf("list = %d %s, want 200", status, raw)
		}
		if got := ids(page.Items); !reflect.DeepEqual(got, want) {
			t.Errorf("ids = %v, want %v", got, want)
		}
		if page.NextCursor != nil {
			t.Errorf("next_cursor = %q, want null", *page.NextCursor)
		}
	})

	t.Run("pages of two continue without repeating", func(t *testing.T) {
		var got []string
		var sizes []int
		query := url.Values{"limit": {"2"}}
		for range 4 {
			status, page, raw := w.list(t, demo, query)
			if status != http.StatusOK {
				t.Fatalf("list %v = %d %s, want 200", query, status, raw)
			}
			got = append(got, ids(page.Items)...)
			sizes = append(sizes, len(page.Items))
			if page.NextCursor == nil {
				break
			}
			query = url.Values{"limit": {"2"}, "cursor": {*page.NextCursor}}
		}
		if !reflect.DeepEqual(sizes, []int{2, 2, 1}) {
			t.Errorf("page sizes = %v, want [2 2 1]", sizes)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("ids over the pages = %v, want %v", got, want)
		}
	})

	t.Run("the state filter", func(t *testing.T) {
		for _, tc := range []struct {
			state string
			want  []string
		}{
			{"disabled", []string{disabled}},
			{"failing", []string{}},
			{"active", want[:4]},
		} {
			status, page, raw := w.list(t, demo, url.Values{"state": {tc.state}})
			if status != http.StatusOK {
				t.Fatalf("list state=%s = %d %s, want 200", tc.state, status, raw)
			}
			if page.Items == nil {
				t.Errorf("state=%s: items %s is not an array", tc.state, raw)
			}
			if got := ids(page.Items); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("state=%s: ids = %v, want %v", tc.state, got, tc.want)
			}
		}
	})

	t.Run("a state outside active, failing and disabled is refused", func(t *testing.T) {
		status, _, raw := w.list(t, demo, url.Values{"state": {"paused"}})
		if status != http.StatusBadRequest {
			t.Fatalf("list state=paused = %d %s, want 400", status, raw)
		}
		if e := errorOf(t, raw); e["error_type"] != "invalid_request" {
			t.Errorf("error_type = %v, want invalid_request", e["error_type"])
		}
	})

	t.Run("items hold no secret and no other workspace's subscription", func(t *testing.T) {
		_, page, _ := w.list(t, demo, nil)
		for _, it := range page.Items {
			if _, ok := it["secret"]; ok {
				t.Errorf("item %v has a member secret", it["id"])
			}
			if it["id"] == otherID || it["workspace"] != demo {
				t.Errorf("item %v of workspace %v is in demo's list", it["id"], it["workspace"])
			}
		}
		if _, page, _ := w.list(t, other, nil); !reflect.DeepEqual(ids(page.Items), []string{otherID}) {
			t.Errorf("other's list = %v, want [%s]", ids(page.Items), otherID)
		}
	})
}

func TestGet_AnswersTheSubscriptionWithItsPendingEvents(t *testing.T) {
	t.Parallel()
	w := newWorld(t, options{})
	// Each subscription is created before the events are written; the
	// delivery loop does not run, so nothing is examined.
	cases := []struct {
		name  string
		body  map[string]any
		want  int
		state string
	}{
		{"change events of demo alone", map[string]any{"types": []string{"parley.change.*"}, "include_forks": false}, 3, ""},
		{"change events of demo and its forks", map[string]any{"types": []string{"parley.change.*"}}, 4, ""},
		{"change and push events of demo alone", map[string]any{"types": []string{"parley.change.*", "parley.git.push"}, "include_forks": false}, 4, ""},
		{"a disabled subscription", map[string]any{"types": []string{"parley.change.*"}}, 0, "disabled"},
	}
	subs := make([]string, len(cases))
	for i, tc := range cases {
		body := map[string]any{"url": "https://ci.example/x"}
		for k, v := range tc.body {
			body[k] = v
		}
		status, raw := w.create(t, w.ownerKey, demo, body)
		if status != http.StatusCreated {
			t.Fatalf("create %s = %d %s, want 201", tc.name, status, raw)
		}
		subs[i], _ = decode(t, raw)["id"].(string)
	}
	pending := func(t *testing.T, id string) (any, map[string]any) {
		t.Helper()
		status, raw := w.Call(t, http.MethodGet, hookPath(demo, id), w.ownerKey, nil)
		if status != http.StatusOK {
			t.Fatalf("get %s = %d %s, want 200", id, status, raw)
		}
		got := decode(t, raw)
		if _, ok := got["secret"]; ok {
			t.Errorf("get %s answers a member secret", id)
		}
		return got["pending"], got
	}
	for i, id := range subs {
		if got, _ := pending(t, id); got != float64(0) {
			t.Errorf("%s: pending right after the creates = %v, want 0", cases[i].name, got)
		}
	}

	for range 3 {
		w.emit(t, demo, "parley.change.open", "")
	}
	w.emit(t, demo, "parley.git.push", "")
	w.emit(t, other, "parley.change.open", "")
	w.emit(t, "demo-f1", "parley.change.open", demo)

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.state == "disabled" {
				status, raw := w.Call(t, http.MethodPatch, hookPath(demo, subs[i]), w.ownerKey, map[string]any{"active": false})
				if status != http.StatusOK {
					t.Fatalf("PATCH {active: false} = %d %s, want 200", status, raw)
				}
			}
			got, obj := pending(t, subs[i])
			if got != float64(tc.want) {
				t.Errorf("pending = %v, want %d", got, tc.want)
			}
			want := []string{"created_at", "created_by", "description", "failing_since", "id", "include_forks",
				"last_delivery_at", "last_status", "pending", "state", "types", "updated_at", "url", "workspace"}
			var keys []string
			for k := range obj {
				keys = append(keys, k)
			}
			slices.Sort(keys)
			if !reflect.DeepEqual(keys, want) {
				t.Errorf("members = %v, want %v", keys, want)
			}
		})
	}
}

// routeCall is one of the webhook routes as a test sends it, with the status
// its handler answers when it succeeds and whether it changes something.
type routeCall struct {
	name, method, path string
	body               any
	ok                 int
	writes             bool
}

// routeCalls are the nine webhook routes of the workspace slug: those with
// :id on the subscription id, the redelivery on the delivery delivery, the
// create with body. The delete comes last.
func routeCalls(slug, id, delivery string, body any) []routeCall {
	item := hookPath(slug, id)
	return []routeCall{
		{"create", http.MethodPost, hooksPath(slug), body, http.StatusCreated, true},
		{"list", http.MethodGet, hooksPath(slug), nil, http.StatusOK, false},
		{"get", http.MethodGet, item, nil, http.StatusOK, false},
		{"update", http.MethodPatch, item, map[string]any{"description": "x"}, http.StatusOK, true},
		{"rotate-secret", http.MethodPost, item + "/rotate-secret", nil, http.StatusOK, true},
		{"ping", http.MethodPost, item + "/ping", nil, http.StatusOK, true},
		{"deliveries", http.MethodGet, item + "/deliveries", nil, http.StatusOK, false},
		{"redeliver", http.MethodPost, item + "/deliveries/" + delivery + "/redeliver", nil, http.StatusOK, true},
		{"delete", http.MethodDelete, item, nil, http.StatusNoContent, true},
	}
}

// deliveryRows are the delivery records of rows, the webhook rows.
func deliveryRows(rows map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range rows {
		if strings.HasPrefix(k, "delivery ") {
			out[k] = v
		}
	}
	return out
}

func TestAccess_LetsOnlyTheOwnerAndTheAdminTokenUseTheRoutes(t *testing.T) {
	t.Parallel()
	w := newWorld(t, options{toml: insecure})
	in := newInbox(t, answerOK)
	body := map[string]any{"url": in.url(), "types": testTypes}
	// One subscription of demo with one succeeded delivery, which every
	// route can address.
	sub := w.subscribe(t, demo, body)
	w.emit(t, demo, "parley.test.one", "")
	waitFor(t, "the subscription to deliver its event", func() bool { return w.settled(t, sub) })
	delivery := w.deliveries(t, sub)[0].id

	pat := func(permissions ...string) string {
		return w.IssuePAT(t, identity.PATSpec{UserID: w.owner, Permissions: permissions})
	}
	member := w.CreateUser(t, "member")
	w.AddMember(t, w.org, member)
	memberKey := w.IssueKey(t, member)
	outsiderKey := w.IssueKey(t, w.CreateUser(t, "outsider"))

	t.Run("the owner and the admin token reach every handler", func(t *testing.T) {
		for _, caller := range []struct {
			name, credential string
			readsOnly        bool
		}{
			{"owner API key", w.ownerKey, false},
			{"admin token", w.Token, false},
			{"owner PAT with workspaces:write", pat(scopeWriteName), false},
			{"owner PAT with workspaces:read", pat(scopeReadName), true},
		} {
			// Each caller deletes the subscription its create made.
			created := ""
			for _, r := range routeCalls(demo, sub, delivery, body) {
				if caller.readsOnly && r.writes {
					continue
				}
				path := r.path
				if r.name == "delete" {
					path = hookPath(demo, created)
				}
				status, raw := w.Call(t, r.method, path, caller.credential, r.body)
				if status != r.ok {
					t.Errorf("%s: %s = %d %s, want %d", caller.name, r.name, status, raw, r.ok)
				}
				if r.name == "create" && status == http.StatusCreated {
					created, _ = decode(t, raw)["id"].(string)
				}
			}
		}
	})

	t.Run("everyone else is refused before the handler", func(t *testing.T) {
		readOnly, unrelated := pat(scopeReadName), pat("changes:read")
		bound := w.IssuePAT(t, identity.PATSpec{UserID: w.owner, Permissions: []string{scopeWriteName}, Workspace: demo})
		waitFor(t, "the subscription to examine every event", func() bool { return w.settled(t, sub) })
		configured, deliveries := w.configured(t), deliveryRows(w.webhookRows(t))
		seq, sent := w.lastSeq(t), len(in.requests())
		for _, caller := range []struct {
			name, credential string
			status           int
			errorType        string
			writesOnly       bool
		}{
			{"org member", memberKey, http.StatusForbidden, "forbidden", false},
			{"user without access", outsiderKey, http.StatusNotFound, "not_found", false},
			{"owner PAT bound to demo", bound, http.StatusForbidden, "token_bound", false},
			{"owner PAT with workspaces:read alone", readOnly, http.StatusForbidden, "insufficient_scope", true},
			{"owner PAT with changes:read alone", unrelated, http.StatusForbidden, "insufficient_scope", false},
		} {
			for _, r := range routeCalls(demo, sub, delivery, body) {
				if caller.writesOnly && !r.writes {
					continue
				}
				status, raw := w.Call(t, r.method, r.path, caller.credential, r.body)
				if status != caller.status || errorOf(t, raw)["error_type"] != caller.errorType {
					t.Errorf("%s: %s = %d %s, want %d %s", caller.name, r.name, status, raw, caller.status, caller.errorType)
				}
			}
		}
		if after := w.configured(t); !reflect.DeepEqual(after, configured) {
			t.Errorf("after the refusals the subscriptions are\n%v\nwant\n%v", after, configured)
		}
		if after := deliveryRows(w.webhookRows(t)); !reflect.DeepEqual(after, deliveries) {
			t.Errorf("after the refusals the delivery records are\n%v\nwant\n%v", after, deliveries)
		}
		// The binding guard records each refusal of a bound token in the
		// log; nothing else is written.
		for _, typ := range w.cellsOf(t, fmt.Sprintf(`SELECT type FROM hub_events WHERE seq > %d`, seq)) {
			if typ != "parley.token.bound_refused" {
				t.Errorf("the refusals wrote the event %s", typ)
			}
		}
		if n := len(in.requests()); n != sent {
			t.Errorf("the receiver got %d requests during the refusals, want none", n-sent)
		}
	})

	t.Run("a task fork is refused whatever the body", func(t *testing.T) {
		const fork = "demo--t1"
		// The org member owns the fork, and has a level on its parent.
		w.AddWorkspace(t, access.Record{Slug: fork, Kind: access.KindFork, ParentSlug: demo, OwnerID: member, OrgID: w.org, Status: access.StatusReady})
		for _, caller := range []struct{ name, credential string }{
			{"the fork's owner", memberKey},
			{"admin token", w.Token},
		} {
			for _, r := range routeCalls(fork, "abc", "abc", map[string]any{"url": "ftp://x"}) {
				status, raw := w.Call(t, r.method, r.path, caller.credential, r.body)
				if status != http.StatusBadRequest || errorOf(t, raw)["error_type"] != "not_supported_on_fork" {
					t.Errorf("%s: %s = %d %s, want 400 not_supported_on_fork", caller.name, r.name, status, raw)
				}
			}
		}
		if n := w.count(t, fork); n != 0 {
			t.Errorf("the fork has %d subscriptions, want none", n)
		}
	})

	t.Run("an id that names no subscription of the workspace", func(t *testing.T) {
		elsewhere := w.subscribe(t, other, body)
		waitFor(t, "the subscription of other to examine every event", func() bool { return w.settled(t, elsewhere) })
		ofOther := func(rows map[string]string) map[string]string {
			out := map[string]string{}
			for k, v := range rows {
				if strings.Contains(k, elsewhere) {
					out[k] = v
				}
			}
			return out
		}
		before := ofOther(w.webhookRows(t))
		for _, r := range routeCalls(demo, "abc", delivery, nil)[2:] {
			status, raw := w.Call(t, r.method, r.path, w.ownerKey, r.body)
			if status != http.StatusBadRequest || errorOf(t, raw)["error_type"] != "invalid_request" {
				t.Errorf("%s with id abc = %d %s, want 400 invalid_request", r.name, status, raw)
			}
		}
		unknown := routeCalls(demo, uuid.NewString(), delivery, nil)[2:]
		foreign := routeCalls(demo, elsewhere, delivery, nil)[2:]
		for i := range unknown {
			s1, b1 := w.Call(t, unknown[i].method, unknown[i].path, w.ownerKey, unknown[i].body)
			s2, b2 := w.Call(t, foreign[i].method, foreign[i].path, w.ownerKey, foreign[i].body)
			if s1 != http.StatusNotFound || errorOf(t, b1)["error_type"] != "webhook_not_found" {
				t.Errorf("%s with an unknown id = %d %s, want 404 webhook_not_found", unknown[i].name, s1, b1)
			}
			if s2 != http.StatusNotFound || !bytes.Equal(b1, b2) {
				t.Errorf("%s with the id of other's subscription = %d %s, want the unknown id's answer %s", foreign[i].name, s2, b2, b1)
			}
		}
		if after := ofOther(w.webhookRows(t)); len(after) == 0 || !reflect.DeepEqual(after, before) {
			t.Errorf("the subscription of other is\n%v\nwant it unchanged\n%v", after, before)
		}
	})
}

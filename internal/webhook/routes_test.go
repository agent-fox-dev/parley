package webhook_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"testing"
	"time"
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

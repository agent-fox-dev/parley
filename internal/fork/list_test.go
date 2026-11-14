package fork_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/agent-fox-dev/parley/internal/fork"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// page decodes a, which must be 200 with items an array, as a page of the
// fork list: the items' slugs, the items by slug and next_cursor.
func page(t *testing.T, a answer) (slugs []string, items map[string]map[string]any, next *string) {
	t.Helper()
	var p struct {
		Items      *[]map[string]any `json:"items"`
		NextCursor *string           `json:"next_cursor"`
	}
	if a.status != http.StatusOK || json.Unmarshal(a.body, &p) != nil || p.Items == nil {
		t.Fatalf("answer = %d %s, want 200 with an array of items", a.status, a.body)
	}
	slugs, items = []string{}, map[string]map[string]any{}
	for _, it := range *p.Items {
		slug, _ := it["slug"].(string)
		slugs, items[slug] = append(slugs, slug), it
	}
	return slugs, items, p.NextCursor
}

// TestList runs on one hub whose demo has four forks: demo--a of dev, the
// oldest, demo--b of mia, created in the session sid, and demo--c and
// demo--d of dev, created at the same instant, the newest; other has none.
// The bound-token case creates forks of its own and runs last.
func TestList(t *testing.T) {
	t.Parallel()
	w := newWorld(t, nil)
	w.addParent(t, "other")
	sid := uuid.New().String()
	w.seedFork(t, "demo--a")
	w.advance(time.Second)
	w.seedFork(t, "demo--b", func(f *fork.Fork) { f.OwnerID, f.SessionID = w.mia, new(sid) })
	w.advance(time.Second)
	w.seedFork(t, "demo--d")
	w.seedFork(t, "demo--c")
	all := []string{"demo--c", "demo--d", "demo--b", "demo--a"}

	t.Run("order, paging and access", func(t *testing.T) {
		slugs, items, next := page(t, w.get(t, w.devKey, forksPath))
		if !slices.Equal(slugs, all) || next != nil {
			t.Fatalf("list = %v, next_cursor %v; want %v on one page", slugs, next, all)
		}
		for slug, it := range items {
			if it["access"] != "owner" || it["kind"] != "fork" || it["parent_slug"] != parentSlug {
				t.Errorf("%s: access %v, kind %v, parent_slug %v; want owner, fork, demo", slug, it["access"], it["kind"], it["parent_slug"])
			}
		}
		// Pages of 1 split demo--c and demo--d, created at the same instant.
		for _, limit := range []string{"1", "3"} {
			var walked []string
			for cursor := ""; ; {
				slugs, _, next := page(t, w.get(t, w.devKey, forksPath+"?limit="+limit+cursor))
				if walked = append(walked, slugs...); next == nil || len(walked) > len(all) {
					break
				}
				cursor = "&cursor=" + url.QueryEscape(*next)
			}
			if !slices.Equal(walked, all) {
				t.Errorf("pages of %s = %v, want %v, then a null next_cursor", limit, walked, all)
			}
		}
		_, items, _ = page(t, w.get(t, w.miaKey, forksPath))
		for slug, it := range items {
			if want := map[bool]string{true: "owner", false: "member"}[slug == "demo--b"]; it["access"] != want {
				t.Errorf("mia's access on %s = %v, want %s", slug, it["access"], want)
			}
		}
		writer, _ := w.pat(t, "", nil, "workspaces:write")
		if slugs, _, _ := page(t, w.get(t, writer, forksPath)); !slices.Equal(slugs, all) {
			t.Errorf("list with workspaces:write = %v, want %v", slugs, all)
		}
		gitReader, _ := w.pat(t, "", nil, "git:read")
		wantError(t, w.get(t, gitReader, forksPath), http.StatusForbidden, "insufficient_scope")
		wantError(t, w.get(t, w.IssueKey(t, w.CreateUser(t, "sam")), forksPath), http.StatusNotFound, "not_found")
		w.setParent(t, workspace.StatusArchived, workspace.MarkerNone)
		if slugs, _, _ := page(t, w.get(t, w.devKey, forksPath)); !slices.Equal(slugs, all) {
			t.Errorf("list of an archived parent = %v, want %v", slugs, all)
		}
	})

	t.Run("filters", func(t *testing.T) {
		for _, c := range []struct {
			name, cred, query string
			want              []string
		}{
			{"mia's own", w.miaKey, "owner=me", []string{"demo--b"}},
			{"dev's own", w.devKey, "owner=me", []string{"demo--c", "demo--d", "demo--a"}},
			{"session", w.devKey, "session_id=" + sid, []string{"demo--b"}},
			{"session in upper case", w.devKey, "session_id=" + strings.ToUpper(sid), []string{"demo--b"}},
			{"both", w.devKey, "owner=me&session_id=" + sid, []string{}},
			{"admin token's own", w.Token, "owner=me", []string{}},
			{"admin token", w.Token, "", all},
			{"another session", w.devKey, "session_id=" + uuid.New().String(), []string{}},
		} {
			t.Run(c.name, func(t *testing.T) {
				if got, _, _ := page(t, w.get(t, c.cred, forksPath+"?"+c.query)); !slices.Equal(got, c.want) {
					t.Errorf("list ?%s = %v, want %v", c.query, got, c.want)
				}
			})
		}
	})

	t.Run("empty for a fork and for a workspace without forks", func(t *testing.T) {
		for _, slug := range []string{"demo--a", "other"} {
			var body map[string]any
			a := w.get(t, w.devKey, "/workspaces/"+slug+"/forks")
			if a.status != http.StatusOK || json.Unmarshal(a.body, &body) != nil ||
				!reflect.DeepEqual(body, map[string]any{"items": []any{}, "next_cursor": nil}) {
				t.Errorf("list of %s = %d %s, want 200 with no items and a null next_cursor", slug, a.status, a.body)
			}
		}
	})

	t.Run("refuses bad filters", func(t *testing.T) {
		for _, query := range []string{"owner=you", "owner=", "owner=me&owner=me", "session_id=abc", "session_id="} {
			t.Run(query, func(t *testing.T) {
				wantError(t, w.get(t, w.devKey, forksPath+"?"+query), http.StatusBadRequest, "invalid_request")
			})
		}
	})

	t.Run("bound token", func(t *testing.T) {
		bound, _ := w.pat(t, parentSlug, nil, "workspaces:write")
		w.post(t, bound, forksPath, map[string]any{"name": "z"}).object(t, http.StatusCreated)
		if slugs, _, _ := page(t, w.get(t, bound, forksPath)); !slices.Equal(slugs, []string{"demo--z"}) {
			t.Errorf("list with a token bound to demo = %v, want the fork it created alone", slugs)
		}
		created := w.post(t, w.devKey, forksPath, map[string]any{"name": "y", "token": map[string]any{"permissions": []string{"workspaces:read"}}}).
			object(t, http.StatusCreated)
		secret, _ := created["token"].(map[string]any)["token"].(string)
		if slugs, _, _ := page(t, w.get(t, secret, "/workspaces/demo--y/forks")); len(slugs) != 0 {
			t.Errorf("list of its own fork with the fork's token = %v, want none", slugs)
		}
	})
}

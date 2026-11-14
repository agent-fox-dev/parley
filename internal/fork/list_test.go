package fork_test

import (
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"uuid"

	"github.com/agent-fox-dev/parley/internal/workspace"
)

func TestList_NewestFirstOnPages(t *testing.T) {
	t.Parallel()
	w, _ := newListWorld(t)
	slugs, items, next := page(t, w.get(t, w.devKey, forksPath))
	if !slices.Equal(slugs, listed) || next != nil {
		t.Fatalf("list = %v, next_cursor %v; want %v on one page", slugs, next, listed)
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
			if walked = append(walked, slugs...); next == nil || len(walked) > len(listed) {
				break
			}
			cursor = "&cursor=" + url.QueryEscape(*next)
		}
		if !slices.Equal(walked, listed) {
			t.Errorf("pages of %s = %v, want %v, then a null next_cursor", limit, walked, listed)
		}
	}
}

func TestList_Access(t *testing.T) {
	t.Parallel()
	w, _ := newListWorld(t)
	_, items, _ := page(t, w.get(t, w.miaKey, forksPath))
	for slug, it := range items {
		if want := map[bool]string{true: "owner", false: "member"}[slug == "demo--b"]; it["access"] != want {
			t.Errorf("mia's access on %s = %v, want %s", slug, it["access"], want)
		}
	}
	writer, _ := w.pat(t, "", nil, "workspaces:write")
	if slugs, _, _ := page(t, w.get(t, writer, forksPath)); !slices.Equal(slugs, listed) {
		t.Errorf("list with workspaces:write = %v, want %v", slugs, listed)
	}
	gitReader, _ := w.pat(t, "", nil, "git:read")
	wantError(t, w.get(t, gitReader, forksPath), http.StatusForbidden, "insufficient_scope")
	wantError(t, w.get(t, w.IssueKey(t, w.CreateUser(t, "sam")), forksPath), http.StatusNotFound, "not_found")
	w.setParent(t, workspace.StatusArchived, workspace.MarkerNone)
	if slugs, _, _ := page(t, w.get(t, w.devKey, forksPath)); !slices.Equal(slugs, listed) {
		t.Errorf("list of an archived parent = %v, want %v", slugs, listed)
	}
}

func TestList_Filters(t *testing.T) {
	t.Parallel()
	w, sid := newListWorld(t)
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
		{"admin token", w.Token, "", listed},
		{"another session", w.devKey, "session_id=" + uuid.New().String(), []string{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got, _, _ := page(t, w.get(t, c.cred, forksPath+"?"+c.query)); !slices.Equal(got, c.want) {
				t.Errorf("list ?%s = %v, want %v", c.query, got, c.want)
			}
		})
	}
}

func TestList_EmptyForForkAndWorkspaceWithoutForks(t *testing.T) {
	t.Parallel()
	w := newWorld(t, nil)
	w.addParent(t, "other")
	w.seedFork(t, "demo--a")
	for _, slug := range []string{"demo--a", "other"} {
		var body map[string]any
		a := w.get(t, w.devKey, "/workspaces/"+slug+"/forks")
		if a.status != http.StatusOK || json.Unmarshal(a.body, &body) != nil ||
			!reflect.DeepEqual(body, map[string]any{"items": []any{}, "next_cursor": nil}) {
			t.Errorf("list of %s = %d %s, want 200 with no items and a null next_cursor", slug, a.status, a.body)
		}
	}
}

func TestList_RefusesBadFilters(t *testing.T) {
	t.Parallel()
	w := newWorld(t, nil)
	for _, query := range []string{
		"owner=you", "owner=", "owner=me&owner=me", "session_id=abc", "session_id=",
		"session_id=" + strings.ReplaceAll(uuid.New().String(), "-", ""),
	} {
		t.Run(query, func(t *testing.T) {
			wantError(t, w.get(t, w.devKey, forksPath+"?"+query), http.StatusBadRequest, "invalid_request")
		})
	}
}

func TestList_BoundTokenSeesItsCoveredForks(t *testing.T) {
	t.Parallel()
	w, _ := newListWorld(t)
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
}

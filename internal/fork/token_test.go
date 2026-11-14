package fork_test

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/fork"
	"github.com/agent-fox-dev/parley/internal/server/timefmt"
)

func TestCreate_MintsBoundToken(t *testing.T) {
	t.Parallel()
	w := newWorld(t, nil)
	w.addParent(t, "other")
	body, _ := created(t, w.post(t, w.devKey, forksPath, map[string]any{"name": "t1",
		"token": map[string]any{"permissions": []string{"git:read", "workspaces:read"}, "paths": []string{"src/**"}}}))
	tok, _ := body["token"].(map[string]any)
	wantMembers(t, tok, map[string]any{
		"workspace": "demo--t1", "name": "fork:demo--t1", "user_id": w.dev, "permissions": []any{"git:read", "workspaces:read"},
		"paths": []any{"src/**"}, "expires_at": nil, "revoked_at": nil,
	})
	secret, _ := tok["token"].(string)
	if !strings.HasPrefix(secret, "parley_pat_") {
		t.Fatalf("token.token = %q, want a PAT's secret", secret)
	}
	if r := w.get(t, secret, "/workspaces/demo--t1"); r.status != http.StatusOK {
		t.Errorf("GET of the fork with its token = %d %s, want 200", r.status, r.body)
	}
	wantError(t, w.get(t, secret, "/workspaces/other"), http.StatusNotFound, "not_found")
	list := w.get(t, w.devKey, "/workspaces/demo--t1/tokens")
	items, _ := list.object(t, http.StatusOK)["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["token_id"] != tok["token_id"] || strings.Contains(string(list.body), secret) {
		t.Errorf("tokens bound to the fork = %s, want the minted one without its secret", list.body)
	}
	wantAbsent(t, items[0].(map[string]any), "token")

	t.Run("name and expires", func(t *testing.T) {
		body, _ := created(t, w.post(t, w.devKey, forksPath, map[string]any{"name": "t2",
			"token": map[string]any{"name": "ci", "permissions": []string{"git:read"}, "expires": "2h"}}))
		tok, _ := body["token"].(map[string]any)
		wantMembers(t, tok, map[string]any{"name": "ci", "expires_at": timefmt.Format(w.clock().Add(2 * time.Hour))})
	})
}

func TestCreate_RefusesInvalidToken(t *testing.T) {
	t.Parallel()
	w := newWorld(t, nil)
	read := []string{"git:read"}
	for name, token := range map[string]map[string]any{
		"no permissions":         {},
		"empty permissions":      {"permissions": []string{}},
		"string permissions":     {"permissions": "git:read"},
		"unregistered scope":     {"permissions": []string{"no:such"}},
		"invalid paths glob":     {"permissions": read, "paths": []string{"/abs"}},
		"expires not a duration": {"permissions": read, "expires": "soon"},
		"expires of zero":        {"permissions": read, "expires": "0h"},
		"number name":            {"permissions": read, "name": 5},
	} {
		t.Run(name, func(t *testing.T) {
			before := w.footprint(t)
			wantError(t, w.post(t, w.devKey, forksPath, map[string]any{"name": "bad", "token": token}), http.StatusBadRequest, "invalid_request")
			w.unchanged(t, before)
		})
	}
}

func TestCreate_RefusesTokenBeyondCaller(t *testing.T) {
	t.Parallel()
	w := newWorld(t, nil)
	noManage, _ := w.pat(t, "", nil, "workspaces:write", "git:read")
	readOnly, _ := w.pat(t, "", nil, "workspaces:write", "tokens:manage", "git:read")
	limited, _ := w.pat(t, "", []string{"src/**"}, "workspaces:write", "tokens:manage", "git:write")
	write := []string{"git:write"}
	for _, c := range []struct {
		name, pat string
		token     map[string]any
	}{
		{"without tokens:manage", noManage, map[string]any{"permissions": []string{"git:read"}}},
		{"a scope the caller lacks", readOnly, map[string]any{"permissions": write}},
		{"path-limited caller, no paths", limited, map[string]any{"permissions": write}},
		{"path-limited caller, a glob it does not cover", limited, map[string]any{"permissions": write, "paths": []string{"docs/**"}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			before := w.footprint(t)
			wantError(t, w.post(t, c.pat, forksPath, map[string]any{"name": "p", "token": c.token}), http.StatusForbidden, "insufficient_scope")
			w.unchanged(t, before)
		})
	}
	for _, pat := range []string{noManage, readOnly, limited} {
		created(t, w.post(t, pat, forksPath, map[string]any{}))
	}
	created(t, w.post(t, limited, forksPath, map[string]any{"token": map[string]any{"permissions": write, "paths": []string{"src/loader/**"}}}))
}

func TestCreate_RecordsCreatingToken(t *testing.T) {
	t.Parallel()
	w := newWorld(t, nil)
	pat, patID := w.pat(t, parentSlug, nil, "workspaces:write")
	created(t, w.post(t, pat, forksPath, map[string]any{"name": "mine"}))
	created(t, w.post(t, w.devKey, forksPath, map[string]any{"name": "theirs"}))
	if r := w.get(t, pat, "/workspaces/demo--mine"); r.status != http.StatusOK {
		t.Errorf("GET of the fork the PAT created = %d %s, want 200", r.status, r.body)
	}
	wantError(t, w.get(t, pat, "/workspaces/demo--theirs"), http.StatusNotFound, "not_found")
	var got []string
	for _, slug := range []string{"demo--mine", "demo--theirs"} {
		f, _, err := fork.BySlug(context.Background(), w.Store.Main(), slug)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, f.CreatingTokenID)
	}
	if want := []string{patID, ""}; !reflect.DeepEqual(got, want) {
		t.Errorf("creating tokens = %q, want %q", got, want)
	}
}

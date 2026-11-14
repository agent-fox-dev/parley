package fork_test

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/fork"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

func TestCreate_RefusesAdminToken(t *testing.T) {
	t.Parallel()
	w := newWorld(t, nil)
	for _, body := range []string{`{}`, `{"name":"BAD"}`, `not json`} {
		t.Run(body, func(t *testing.T) {
			before := w.footprint(t)
			wantError(t, w.post(t, w.Token, forksPath, body), http.StatusForbidden, "forbidden")
			w.unchanged(t, before)
		})
	}
}

func TestCreate_RefusesInvalidBody(t *testing.T) {
	t.Parallel()
	w := newWorldWith(t, hubtest.Config{TOML: "[forks]\nmax_ttl = \"30d\""})
	for name, body := range map[string]any{
		"array": `[]`, "string": `"x"`,
		"empty name": map[string]any{"name": ""}, "name of 33": map[string]any{"name": strings.Repeat("a", 33)},
		"upper-case name": map[string]any{"name": "Task"}, "leading hyphen": map[string]any{"name": "-a"},
		"trailing hyphen": map[string]any{"name": "a-"}, "double hyphen": map[string]any{"name": "a--b"},
		"number name": map[string]any{"name": 7}, "empty ref": map[string]any{"ref": ""}, "number ref": map[string]any{"ref": 5},
		"ttl 4m": map[string]any{"ttl": "4m"}, "ttl above max_ttl": map[string]any{"ttl": "31d"}, "ttl 0m": map[string]any{"ttl": "0m"},
		"ttl in seconds": map[string]any{"ttl": "90s"}, "fractional ttl": map[string]any{"ttl": "1.5h"}, "number ttl": map[string]any{"ttl": 60},
		"auto_change string": map[string]any{"auto_change": "yes"}, "auto_change number": map[string]any{"auto_change": 1},
		"purpose of 1025":             map[string]any{"purpose": strings.Repeat("x", 1025)},
		"purpose of 1025 code points": map[string]any{"purpose": strings.Repeat("é", 1025)},
		"number purpose":              map[string]any{"purpose": 3}, "string token": map[string]any{"token": "git:read"},
		"array sandbox": map[string]any{"sandbox": []any{}}, "session_id not a UUID": map[string]any{"session_id": "not-a-uuid"},
	} {
		t.Run(name, func(t *testing.T) {
			before := w.footprint(t)
			wantError(t, w.post(t, w.devKey, forksPath, body), http.StatusBadRequest, "invalid_request")
			w.unchanged(t, before)
		})
	}
	for name, body := range map[string]map[string]any{
		"name of 1": {"name": "a"}, "name of 32": {"name": strings.Repeat("a", 32)},
		"purpose of 1024": {"purpose": strings.Repeat("x", 1024)}, "purpose of 1024 code points": {"purpose": strings.Repeat("é", 1024)},
	} {
		t.Run(name, func(t *testing.T) { created(t, w.post(t, w.devKey, forksPath, body)) })
	}
}

func TestCreate_RefusesForkOfFork(t *testing.T) {
	t.Parallel()
	w := newWorld(t, nil)
	created(t, w.post(t, w.devKey, forksPath, map[string]any{"name": "f1"}))
	before := w.footprint(t)
	wantError(t, w.post(t, w.devKey, "/workspaces/demo--f1/forks", map[string]any{}), http.StatusBadRequest, "not_supported_on_fork")
	w.unchanged(t, before)
	var n int
	if err := w.Store.Main().QueryRow(`SELECT count(*) FROM workspaces WHERE kind = 'fork'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("forks = %d (%v), want 1", n, err)
	}
}

func TestCreate_RefusesByParentState(t *testing.T) {
	t.Parallel()
	w := newWorld(t, nil)
	for _, c := range []struct {
		name   string
		status workspace.Status
		marker workspace.Marker
		want   string
	}{
		{"pending", workspace.StatusPending, workspace.MarkerNone, "workspace_busy"},
		{"cloning", workspace.StatusCloning, workspace.MarkerNone, "workspace_busy"},
		{"ready with a maintenance marker", workspace.StatusReady, workspace.MarkerReclone, "workspace_busy"},
		{"failed", workspace.StatusFailed, workspace.MarkerNone, "workspace_not_ready"},
		{"archived", workspace.StatusArchived, workspace.MarkerNone, "workspace_archived"},
	} {
		t.Run(c.name, func(t *testing.T) {
			const set = `UPDATE workspaces SET status = ?, maintenance = ? WHERE slug = ?`
			w.exec(t, set, c.status, c.marker, parentSlug)
			defer w.exec(t, set, workspace.StatusReady, workspace.MarkerNone, parentSlug)
			before := w.footprint(t)
			wantError(t, w.post(t, w.devKey, forksPath, map[string]any{}), http.StatusConflict, c.want)
			w.unchanged(t, before)
		})
	}
}

func TestCreate_RefusesUnknownRef(t *testing.T) {
	t.Parallel()
	w := newWorld(t, nil)
	for _, ref := range []string{"nope", "v1", "refs/heads/main", w.head(t, w.parent, "main"), "refs/tags/v1"} {
		t.Run(ref, func(t *testing.T) {
			before := w.footprint(t)
			wantError(t, w.post(t, w.devKey, forksPath, map[string]any{"ref": ref}), http.StatusBadRequest, "fork_ref_not_found")
			w.unchanged(t, before)
		})
	}
}

func TestCreate_RefusesNameInUse(t *testing.T) {
	t.Parallel()
	w := newWorld(t, nil)
	ctx := context.Background()
	created(t, w.post(t, w.devKey, forksPath, map[string]any{"name": "dup"}))
	was, _, err := fork.BySlug(ctx, w.Store.Main(), "demo--dup")
	if err != nil {
		t.Fatal(err)
	}
	wantError(t, w.post(t, w.devKey, forksPath, map[string]any{"name": "dup"}), http.StatusConflict, "conflict")
	if is, _, err := fork.BySlug(ctx, w.Store.Main(), "demo--dup"); err != nil || !reflect.DeepEqual(is, was) {
		t.Errorf("the existing fork = %+v (%v), want it unchanged from %+v", is, err, was)
	}

	if got := w.postAll(t, 8, map[string]any{"name": "race"}); got[http.StatusCreated] != 1 || got[http.StatusConflict] != 7 {
		t.Errorf("8 creates of one name answered %v, want one 201 and seven 409", got)
	}
	if _, found, err := fork.BySlug(ctx, w.Store.Main(), "demo--race"); err != nil || !found {
		t.Errorf("the fork demo--race: found %v (%v), want it recorded", found, err)
	}
	if _, err := (gitrepo.Git{}).Open(ctx, w.workspaces.RepoPath("demo--race")); err != nil {
		t.Errorf("the repository of demo--race does not open: %v", err)
	}
}

func TestCreate_RefusesAtForkLimit(t *testing.T) {
	t.Parallel()
	w := newWorldWith(t, hubtest.Config{TOML: "[forks]\nmax_per_workspace = 3"})
	w.addParent(t, "other")
	for range 3 {
		created(t, w.post(t, w.devKey, forksPath, map[string]any{}))
	}
	before := w.footprint(t)
	wantError(t, w.post(t, w.devKey, forksPath, map[string]any{}), http.StatusTooManyRequests, "fork_limit_reached")
	w.unchanged(t, before)

	const removeOne = `DELETE FROM workspaces WHERE slug = (SELECT slug FROM workspaces WHERE parent_slug = ? LIMIT 1)`
	w.exec(t, removeOne, parentSlug)
	created(t, w.post(t, w.devKey, forksPath, map[string]any{}))
	w.exec(t, removeOne, parentSlug)
	if got := w.postAll(t, 6, map[string]any{}); got[http.StatusCreated] != 1 || got[http.StatusTooManyRequests] != 5 {
		t.Errorf("6 creates with one free place answered %v, want one 201 and five 429", got)
	}
	if n := w.footprint(t).forks; n != 3 {
		t.Errorf("forks of demo = %d, want 3", n)
	}
	created(t, w.post(t, w.devKey, "/workspaces/other/forks", map[string]any{}))
}

func TestCreate_RefusalOrder(t *testing.T) {
	t.Parallel()
	w := newWorldWith(t, hubtest.Config{TOML: "[forks]\nmax_per_workspace = 2"})
	parentOf := func(slug string, status workspace.Status) func(*fork.Fork) {
		return func(f *fork.Fork) { f.ParentSlug, f.Status = &slug, status }
	}
	// demo has one fork and one free place; arch is archived, and so is its
	// fork; full has no free place.
	w.seedFork(t, "demo--f1")
	w.addParent(t, "arch", func(rec *workspace.Workspace) { rec.Status = workspace.StatusArchived })
	w.seedFork(t, "arch--f", parentOf("arch", workspace.StatusArchived))
	w.addParent(t, "full")
	w.seedFork(t, "full--x", parentOf("full", workspace.StatusReady))
	w.seedFork(t, "full--y", parentOf("full", workspace.StatusReady))

	// w has no session linker, so no session exists, and no sandbox
	// component, so every sandbox is refused; stubbed has a linker refusing
	// every session and a sandbox component refusing every sandbox.
	stubbed := newWorld(t, collaborate(&linker{refuse: true},
		&sandboxer{err: server.NewError("credential_missing", http.StatusUnprocessableEntity, "no credential")}))
	stubbed.seedFork(t, "demo--f1")
	const session = "0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d"
	badToken := map[string]any{"permissions": []string{"no:such"}}
	for _, c := range []struct {
		name, slug  string
		admin, stub bool
		body        map[string]any
		status      int
		want        string
	}{
		{"admin token, then the body", "demo", true, false, map[string]any{"name": "BAD"}, http.StatusForbidden, "forbidden"},
		{"body, then the session", "demo", false, false, map[string]any{"name": "BAD", "session_id": session}, http.StatusBadRequest, "invalid_request"},
		{"session, then a fork as parent", "demo--f1", false, false, map[string]any{"session_id": session}, http.StatusBadRequest, "unknown_session"},
		{"a fork as parent, then its state", "arch--f", false, false, map[string]any{}, http.StatusBadRequest, "not_supported_on_fork"},
		{"parent state, then ref", "arch", false, false, map[string]any{"ref": "nope"}, http.StatusConflict, "workspace_archived"},
		{"ref, then the limit", "full", false, false, map[string]any{"ref": "nope"}, http.StatusBadRequest, "fork_ref_not_found"},
		{"limit, then the name in use", "full", false, false, map[string]any{"name": "x"}, http.StatusTooManyRequests, "fork_limit_reached"},
		{"name in use, then the token", "demo", false, false, map[string]any{"name": "f1", "token": badToken}, http.StatusConflict, "conflict"},
		{"token, then the sandbox", "demo", false, false, map[string]any{"name": "t1", "token": badToken, "sandbox": map[string]any{}}, http.StatusBadRequest, "invalid_request"},
		{"body, then the session, with a linker", "demo", false, true, map[string]any{"name": "BAD", "session_id": session}, http.StatusBadRequest, "invalid_request"},
		{"session, then a fork as parent, with a linker", "demo--f1", false, true, map[string]any{"session_id": session}, http.StatusBadRequest, "unknown_session"},
		{"token, then the sandbox, with a component", "demo", false, true, map[string]any{"name": "t1", "token": badToken, "sandbox": map[string]any{}}, http.StatusBadRequest, "invalid_request"},
	} {
		t.Run(c.name, func(t *testing.T) {
			on := w
			if c.stub {
				on = stubbed
			}
			cred := on.devKey
			if c.admin {
				cred = on.Token
			}
			before := on.footprint(t)
			wantError(t, on.post(t, cred, "/workspaces/"+c.slug+"/forks", c.body), c.status, c.want)
			on.unchanged(t, before)
		})
	}
}

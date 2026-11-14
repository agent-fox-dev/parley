package fork_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/fork"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server/timefmt"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

func TestCreate_AnswersReadyFork(t *testing.T) {
	t.Parallel()
	w := newWorldWith(t, hubtest.Config{Server: `external_url = "https://hub.test"`})
	body, ws := created(t, w.post(t, w.devKey, forksPath, map[string]any{"name": "task1"}))
	wantMembers(t, ws, map[string]any{
		"slug": "demo--task1", "kind": "fork", "status": "ready", "workspace_mode": "standard", "parent_slug": parentSlug,
		"org_id": w.acme, "owner_id": w.dev, "access": "owner", "display_name": "demo--task1", "description": "",
		"git_url": nil, "origin_head_sha": nil, "upstream_url": nil, "integration_branch": nil, "upstream_head_sha": nil,
		"head_sha": w.head(t, w.parent, "main"), "hub_url": "https://hub.test/git/acme/demo--task1.git",
	})
	if body["remote"] != ws["hub_url"] {
		t.Errorf("remote = %v, want the hub_url %v", body["remote"], ws["hub_url"])
	}
	wantAbsent(t, body, "token", "sandbox")
	if r := w.get(t, w.devKey, "/workspaces/demo--task1"); r.status != http.StatusOK {
		t.Errorf("GET of the fork = %d %s, want 200", r.status, r.body)
	}

	t.Run("carry-patch parent", func(t *testing.T) {
		w.addParent(t, "cp", func(rec *workspace.Workspace) {
			rec.Mode, rec.UpstreamURL, rec.IntegrationBranch = workspace.ModeCarryPatch, new("https://up.test/cp.git"), new("main")
		})
		_, ws := created(t, w.post(t, w.devKey, "/workspaces/cp/forks", map[string]any{"name": "t"}))
		wantMembers(t, ws, map[string]any{"workspace_mode": "standard", "upstream_url": nil, "integration_branch": nil})
	})
	t.Run("no external_url", func(t *testing.T) {
		t.Parallel()
		w := newWorld(t, nil)
		body, ws := created(t, w.post(t, w.devKey, forksPath, map[string]any{"name": "task1"}))
		if v, ok := body["remote"]; !ok || v != nil || ws["hub_url"] != nil {
			t.Errorf("remote = %v (present %v), hub_url = %v; want both null", v, ok, ws["hub_url"])
		}
	})
}

func TestCreate_Defaults(t *testing.T) {
	t.Parallel()
	w := newWorld(t, nil)
	slug := regexp.MustCompile(`^demo--[a-z0-9]{8}$`)
	expires := timefmt.Format(w.clock().Add(7 * 24 * time.Hour))
	for name, body := range map[string]any{
		"absent": nil, "empty without a length": io.MultiReader(), "empty": "{}", "unknown member": `{"colour":"red"}`,
		"null members": `{"name":null,"ref":null,"ttl":null,"auto_change":null,"token":null,"purpose":null,"session_id":null,"sandbox":null}`,
	} {
		t.Run(name, func(t *testing.T) {
			body, ws := created(t, w.post(t, w.devKey, forksPath, body))
			if s, _ := ws["slug"].(string); !slug.MatchString(s) {
				t.Errorf("slug = %q, want demo-- and 8 lower-case letters or digits", s)
			}
			wantMembers(t, ws, map[string]any{"default_branch": "main", "auto_change": true, "purpose": nil, "session_id": nil, "expires_at": expires})
			wantAbsent(t, body, "token", "sandbox")
		})
	}
}

func TestCreate_MemberOwnsFork(t *testing.T) {
	t.Parallel()
	w := newWorld(t, nil)
	pat := w.IssuePAT(t, identity.PATSpec{UserID: w.dev, Permissions: []string{"workspaces:write"}, Workspace: parentSlug})
	for _, c := range []struct{ name, cred, user string }{{"member with an API key", w.miaKey, w.mia}, {"owner with a bound PAT", pat, w.dev}} {
		t.Run(c.name, func(t *testing.T) {
			_, ws := created(t, w.post(t, c.cred, forksPath, map[string]any{}))
			wantMembers(t, ws, map[string]any{"owner_id": c.user, "org_id": w.acme, "access": "owner"})
		})
	}
	wantError(t, w.patch(t, w.miaKey, "/workspaces/demo", map[string]any{"description": "x"}), http.StatusForbidden, "forbidden")
}

func TestCreate_ForkPoint(t *testing.T) {
	t.Parallel()
	w := newWorld(t, nil)
	_, ws := created(t, w.post(t, w.devKey, forksPath, map[string]any{"name": "fp", "ref": "release"}))
	sha := w.head(t, w.parent, "release")
	wantMembers(t, ws, map[string]any{
		"default_branch": "release", "fork_point": map[string]any{"ref": "release", "sha": sha}, "head_sha": sha,
	})
	if len(sha) != 40 {
		t.Errorf("the fork point's sha %q is not 40 characters", sha)
	}
}

func TestCreate_TTL(t *testing.T) {
	t.Parallel()
	w := newWorld(t, nil)
	for _, c := range []struct {
		ttl  string
		want time.Duration
	}{{"5m", 5 * time.Minute}, {"90m", 90 * time.Minute}, {"24h", 24 * time.Hour}, {"30d", 30 * 24 * time.Hour}} {
		t.Run(c.ttl, func(t *testing.T) {
			_, ws := created(t, w.post(t, w.devKey, forksPath, map[string]any{"name": "ttl" + c.ttl, "ttl": c.ttl}))
			at, err1 := time.Parse(time.RFC3339, ws["expires_at"].(string))
			from, err2 := time.Parse(time.RFC3339, ws["created_at"].(string))
			if err := errors.Join(err1, err2); err != nil || at.Sub(from) != c.want {
				t.Errorf("expires_at - created_at = %v (%v), want %v", at.Sub(from), err, c.want)
			}
		})
	}
}

func TestCreate_EmitsForkEvent(t *testing.T) {
	t.Parallel()
	w := newWorld(t, nil)
	_, ws := created(t, w.post(t, w.devKey, forksPath, map[string]any{"name": "e1", "purpose": "fix loader"}))
	ev := w.forkEvents(t)
	if len(ev) != 1 {
		t.Fatalf("fork events = %d, want 1", len(ev))
	}
	if *ev[0].Workspace != "demo--e1" || ev[0].ResourceID != "demo--e1" || ev[0].ActorType != identity.TypeAPIKey || *ev[0].ActorID != w.dev {
		t.Errorf("event = %+v, want workspace and resource demo--e1 by dev's API key", ev[0])
	}
	var meta map[string]any
	if err := json.Unmarshal(ev[0].Metadata, &meta); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"parent_slug": parentSlug, "fork_point": ws["fork_point"], "expires_at": ws["expires_at"], "auto_change": true, "purpose": "fix loader"}
	if !reflect.DeepEqual(meta, want) {
		t.Errorf("metadata = %v, want %v", meta, want)
	}

	body, _ := created(t, w.post(t, w.devKey, forksPath, map[string]any{"name": "e2", "token": map[string]any{"permissions": []string{"git:read"}}}))
	token, _ := body["token"].(map[string]any)
	ev = w.forkEvents(t)
	if err := json.Unmarshal(ev[len(ev)-1].Metadata, &meta); err != nil || meta["token_id"] != token["token_id"] || token["token_id"] == nil {
		t.Errorf("token_id = %v, want the minted token's %v", meta["token_id"], token["token_id"])
	}
	raw, _ := json.Marshal(ev[len(ev)-1])
	if secret, _ := token["token"].(string); secret == "" || strings.Contains(string(raw), secret) {
		t.Errorf("the event %s holds the token's secret %q, or the answer has none", raw, secret)
	}
	if r := w.post(t, w.devKey, forksPath, map[string]any{"name": "BAD"}); r.status != http.StatusBadRequest || len(w.forkEvents(t)) != 2 {
		t.Errorf("a refused create = %d with %d fork events, want 400 and 2", r.status, len(w.forkEvents(t)))
	}
}

func TestCreate_IdempotentReplayOmitsSecret(t *testing.T) {
	t.Parallel()
	w := newWorld(t, nil)
	req := map[string]any{"name": "idem", "token": map[string]any{"permissions": []string{"git:read"}}}
	a := w.postKey(t, w.devKey, forksPath, "k1", req).object(t, http.StatusCreated)
	b := w.postKey(t, w.devKey, forksPath, "k1", req).object(t, http.StatusOK)
	at, _ := a["token"].(map[string]any)
	bt, _ := b["token"].(map[string]any)
	if s, _ := at["token"].(string); s == "" {
		t.Errorf("the first answer's token = %v, want its secret", at)
	}
	wantAbsent(t, bt, "token")
	if !reflect.DeepEqual(a["workspace"], b["workspace"]) || bt["token_id"] != at["token_id"] {
		t.Errorf("replay = %v, want the first answer %v without the secret", b, a)
	}
	var tokens int
	if err := w.Store.Main().QueryRow(`SELECT count(*) FROM pats WHERE workspace = 'demo--idem'`).Scan(&tokens); err != nil {
		t.Fatal(err)
	}
	if n := w.footprint(t).forks; n != 1 || tokens != 1 {
		t.Errorf("forks = %d, tokens bound to the fork = %d; want 1 and 1", n, tokens)
	}
}

func TestCreate_GeneratedNameCollision(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex // guards names and calls
	var names []string
	calls := 0
	set := func(n []string) {
		mu.Lock()
		defer mu.Unlock()
		names, calls = n, 0
	}
	called := func() int {
		mu.Lock()
		defer mu.Unlock()
		return calls
	}
	w := newWorld(t, func(c *hub.Composition) {
		c.AdjustForks(func(d *fork.Deps) {
			d.NewName = func() string {
				mu.Lock()
				defer mu.Unlock()
				calls++
				n := names[0]
				if len(names) > 1 {
					names = names[1:]
				}
				return n
			}
		})
	})
	w.seedFork(t, "demo--aaaaaaaa")
	set([]string{"aaaaaaaa", "aaaaaaaa", "bbbbbbbb"})
	if _, ws := created(t, w.post(t, w.devKey, forksPath, map[string]any{})); ws["slug"] != "demo--bbbbbbbb" || called() != 3 {
		t.Errorf("slug = %v after %d names, want demo--bbbbbbbb after 3", ws["slug"], called())
	}
	set([]string{"aaaaaaaa"})
	before := w.footprint(t)
	wantError(t, w.post(t, w.devKey, forksPath, map[string]any{}), http.StatusInternalServerError, "internal")
	if called() != 8 {
		t.Errorf("names drawn = %d, want 8", called())
	}
	w.unchanged(t, before)
}

func TestCreate_CleansUpOnFailure(t *testing.T) {
	t.Parallel()
	var fault atomic.Value // the step that fails: "repository", "commit", "event" or ""
	fault.Store("")
	errFault := errors.New("injected fault")
	w := newWorld(t, func(c *hub.Composition) {
		c.AdjustForks(func(d *fork.Deps) {
			create, transact, events := d.CreateRepo, d.Transact, d.Events
			d.CreateRepo = func(ctx context.Context, parentDir, forkDir string) error {
				if err := create(ctx, parentDir, forkDir); err != nil || fault.Load() != "repository" {
					return err
				}
				return errFault // after the directory was made
			}
			d.Transact = func(ctx context.Context, fn func(*sql.Tx) error) error {
				return transact(ctx, func(tx *sql.Tx) error {
					if err := fn(tx); err != nil || fault.Load() != "commit" {
						return err
					}
					return errFault
				})
			}
			d.Events = faultyEvents{events, &fault}
		})
	})
	for _, f := range []string{"repository", "commit", "event"} {
		fault.Store(f)
		t.Run(f, func(t *testing.T) {
			before := w.footprint(t)
			r := w.post(t, w.devKey, forksPath, map[string]any{"name": "x1"})
			if e := r.object(t, http.StatusInternalServerError)["error"].(map[string]any); e["error_type"] != "internal" {
				t.Errorf("error = %v, want internal", e)
			}
			w.unchanged(t, before)
			if _, err := os.Stat(w.workspaces.RepoPath("demo--x1")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the fork's directory after the failure: %v, want it absent", err)
			}
		})
	}
	fault.Store("")

	t.Run("leftover directory", func(t *testing.T) {
		dir := w.workspaces.RepoPath("demo--x2")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "stray"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		created(t, w.post(t, w.devKey, forksPath, map[string]any{"name": "x2"}))
		if _, err := os.Stat(filepath.Join(dir, "stray")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the stray file: %v, want it gone", err)
		}
		if out := w.git(t, "demo--x2", "count-objects", "-v"); !strings.Contains(out, "count: 0\n") || !strings.Contains(out, "in-pack: 0") {
			t.Errorf("count-objects = %q, want no object", out)
		}
	})
}

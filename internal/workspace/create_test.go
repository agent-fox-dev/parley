package workspace_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server/timefmt"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// valid is a create body the route accepts for the slug given.
func valid(slug string) map[string]any {
	return map[string]any{"slug": slug, "git_url": "https://example.com/a.git"}
}

func TestCreate_AnswersScopeAndAdminTokenRefusalsBeforeTheBody(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	refused := []struct {
		name, cred, errorType string
	}{
		{"a PAT with no scopes", w.pat(t, w.u), "insufficient_scope"},
		{"a PAT with only workspaces:read", w.pat(t, w.u, "workspaces:read"), "insufficient_scope"},
		{"the admin token", w.Token, "forbidden"},
	}
	for _, body := range []struct {
		name string
		body any
	}{{"a valid body", valid("refused")}, {"an empty body", map[string]any{}}} {
		for _, c := range refused {
			a := w.create(t, c.cred, body.body)
			if a.status != http.StatusForbidden || a.errorType(t) != c.errorType {
				t.Errorf("%s with %s = %d %s, want 403 %s", c.name, body.name, a.status, a.body, c.errorType)
			}
			if n, ev := w.workspaces(t), len(w.createEvents(t)); n != 0 || ev != 0 {
				t.Errorf("after %s with %s: %d workspaces and %d create events, want none", c.name, body.name, n, ev)
			}
		}
	}
	for i, c := range []struct{ name, cred string }{
		{"a PAT with workspaces:write", w.pat(t, w.u, "workspaces:write")},
		{"an API key", w.uKey},
	} {
		if a := w.create(t, c.cred, valid(fmt.Sprintf("allowed-%d", i))); a.status != http.StatusCreated {
			t.Errorf("%s = %d %s, want 201", c.name, a.status, a.body)
		}
	}
}

func TestCreate_RecordsAPendingRepositoryAndAnswersTheWholeObject(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	obj := w.create(t, w.uKey, valid("demo")).object(t)

	members := []string{"slug", "kind", "workspace_mode", "git_url", "default_branch", "owner_id", "org_id", "status",
		"display_name", "description", "head_sha", "origin_head_sha", "clone_error", "last_sync_at", "sync_error",
		"upstream_url", "integration_branch", "upstream_head_sha", "parent_slug", "fork_point", "expires_at",
		"auto_change", "purpose", "session_id", "settings", "created_at", "updated_at", "org_slug", "hub_url", "access"}
	if got := slices.Sorted(maps.Keys(obj)); !slices.Equal(got, slices.Sorted(slices.Values(members))) {
		t.Errorf("members = %v, want exactly %v", got, members)
	}
	at := timefmt.Format(w.clock())
	for member, want := range map[string]any{
		"slug": "demo", "kind": "repository", "workspace_mode": "standard", "status": "pending", "access": "owner",
		"owner_id": w.u, "org_id": w.uOrg.ID, "display_name": "demo", "description": "",
		"git_url": "https://example.com/a.git", "created_at": at, "updated_at": at,
	} {
		if obj[member] != want {
			t.Errorf("%s = %#v, want %#v", member, obj[member], want)
		}
	}
	for _, member := range []string{"default_branch", "head_sha", "origin_head_sha", "clone_error", "last_sync_at",
		"sync_error", "upstream_url", "integration_branch", "upstream_head_sha", "parent_slug", "fork_point",
		"expires_at", "auto_change", "purpose", "session_id"} {
		if v, present := obj[member]; !present || v != nil {
			t.Errorf("%s = %#v (present %v), want null", member, v, present)
		}
	}
	if rec, found := w.workspace(t, "demo"); !found || rec.Status != workspace.StatusPending || rec.InstanceID == "" {
		t.Errorf("stored record = %+v (found %v), want pending with an instance id", rec, found)
	}
	if got := w.notifications(); !slices.Equal(got, []string{"demo"}) {
		t.Errorf("clone notifications = %v, want [demo]", got)
	}

	for _, c := range []struct {
		name   string
		extra  map[string]any
		member string
		want   string
	}{
		{"an empty display_name", map[string]any{"display_name": ""}, "display_name", "demo-2"},
		{"a given display_name", map[string]any{"display_name": "Demo"}, "display_name", "Demo"},
		{"a given description", map[string]any{"description": "the demo"}, "description", "the demo"},
		{"a given default_branch", map[string]any{"default_branch": "trunk"}, "default_branch", "trunk"},
	} {
		body := valid("demo-2")
		maps.Copy(body, c.extra)
		if got := w.create(t, w.uKey, body).object(t)[c.member]; got != c.want {
			t.Errorf("with %s: %s = %#v, want %q", c.name, c.member, got, c.want)
		}
		w.deleteRecord(t, "demo-2")
	}
}

func TestCreate_AnswersHubURLOrgSlugAndEffectiveSettings(t *testing.T) {
	t.Parallel()
	registered := withCompose(func(c *hub.Composition) {
		for _, k := range []workspace.SettingKey{
			{Name: "flag", Default: func(*string) any { return true }},
			{Name: "branches", Default: func(b *string) any {
				if b == nil {
					return []string{}
				}
				return []string{*b}
			}},
		} {
			k.Validate = func(json.RawMessage) error { return nil }
			if err := c.Workspaces().Settings().RegisterSetting(k); err != nil {
				panic(err)
			}
		}
	})
	for _, c := range []struct {
		name     string
		opts     []option
		body     map[string]any
		hubURL   string // "" for null
		settings string
	}{
		{"an external URL", []option{withExternalURL("https://hub.example")}, valid("demo"), "https://hub.example/git/%s/demo.git", `{}`},
		{"one with a trailing slash", []option{withExternalURL("https://hub.example/")}, valid("demo"), "https://hub.example/git/%s/demo.git", `{}`},
		{"one with a path", []option{withExternalURL("https://hub.example/base/")}, valid("demo"), "https://hub.example/base/git/%s/demo.git", `{}`},
		{"none", nil, valid("demo"), "", `{}`},
		{"registered settings", []option{registered}, map[string]any{"slug": "demo", "git_url": "https://example.com/a.git", "default_branch": "trunk"},
			"", `{"branches":["trunk"],"flag":true}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t, c.opts...)
			obj := w.create(t, w.uKey, c.body).object(t)
			var want any
			if c.hubURL != "" {
				want = fmt.Sprintf(c.hubURL, w.uOrg.Slug)
			}
			if obj["hub_url"] != want {
				t.Errorf("hub_url = %#v, want %#v", obj["hub_url"], want)
			}
			if obj["org_slug"] != w.uOrg.Slug {
				t.Errorf("org_slug = %#v, want %q", obj["org_slug"], w.uOrg.Slug)
			}
			if got, _ := json.Marshal(obj["settings"]); string(got) != c.settings {
				t.Errorf("settings = %s, want %s", got, c.settings)
			}
		})
	}
}

func TestCreate_WritesOneCreateEventInTheTransaction(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	for _, c := range []struct {
		slug          string
		extra         map[string]any
		defaultBranch any
	}{
		{"demo", nil, nil},
		{"demo-trunk", map[string]any{"default_branch": "trunk"}, "trunk"},
	} {
		before := len(w.createEvents(t))
		body := valid(c.slug)
		maps.Copy(body, c.extra)
		w.create(t, w.uKey, body).object(t)
		evs := w.createEvents(t)
		if len(evs) != before+1 {
			t.Fatalf("%s: %d create events, want %d", c.slug, len(evs), before+1)
		}
		ev := evs[len(evs)-1]
		if ev.Workspace == nil || *ev.Workspace != c.slug || ev.ResourceType != "workspace" || ev.ResourceID != c.slug ||
			ev.ActorType != identity.TypeAPIKey || ev.ActorID == nil || *ev.ActorID != w.u {
			t.Errorf("%s: event = %+v, want workspace and resource %s, actor api_key %s", c.slug, ev, c.slug, w.u)
		}
		var meta map[string]any
		if err := json.Unmarshal(ev.Metadata, &meta); err != nil {
			t.Fatal(err)
		}
		want := map[string]any{"kind": "repository", "git_url": "https://example.com/a.git", "workspace_mode": "standard", "default_branch": c.defaultBranch}
		if !reflect.DeepEqual(meta, want) {
			t.Errorf("%s: metadata = %v, want %v", c.slug, meta, want)
		}
	}
}

func TestCreate_RefusesASlugInUseWithoutTouchingTheWorkspace(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.create(t, w.uKey, valid("demo")).object(t)
	stored, _ := w.workspace(t, "demo")
	events := len(w.createEvents(t))
	w.advance(time.Minute)

	again := func(when string) {
		t.Helper()
		for _, c := range []struct{ name, cred string }{{"its owner", w.uKey}, {"another user", w.vKey}} {
			body := valid("demo")
			body["git_pat"] = "tok-123"
			if a := w.create(t, c.cred, body); a.status != http.StatusConflict || a.errorType(t) != "conflict" {
				t.Errorf("%s: %s = %d %s, want 409 conflict", when, c.name, a.status, a.body)
			}
		}
		if n := w.origin.called(); n != 0 {
			t.Errorf("%s: origin was called %d times, want never", when, n)
		}
		if n := w.workspaceSecrets(t, "demo"); n != 0 {
			t.Errorf("%s: the workspace holds %d secrets, want none", when, n)
		}
		if n := len(w.createEvents(t)); n != events {
			t.Errorf("%s: %d create events, want %d", when, n, events)
		}
	}
	again("pending")
	if rec, _ := w.workspace(t, "demo"); !reflect.DeepEqual(rec, stored) {
		t.Errorf("stored record = %+v, want it unchanged: %+v", rec, stored)
	}
	if _, err := w.Store.Main().Exec(`UPDATE workspaces SET status = 'archived' WHERE slug = 'demo'`); err != nil {
		t.Fatal(err)
	}
	again("archived")
}

func TestCreate_CreatesExactlyOneOfConcurrentRequestsForOneSlug(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	body, _ := json.Marshal(valid("demo"))
	type result struct {
		status int
		owner  string
		err    error
	}
	results := make([]result, 8)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range results {
		key, owner := w.uKey, w.u
		if i%2 == 1 {
			key, owner = w.vKey, w.v
		}
		wg.Go(func() {
			<-start
			req, err := http.NewRequest(http.MethodPost, w.URL("/api/v1/workspaces"), bytes.NewReader(body))
			if err != nil {
				results[i].err = err
				return
			}
			req.Header.Set("Authorization", "Bearer "+key)
			req.Header.Set("Content-Type", "application/json")
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				results[i].err = err
				return
			}
			_ = res.Body.Close()
			results[i] = result{status: res.StatusCode, owner: owner}
		})
	}
	close(start)
	wg.Wait()

	var created, conflicts int
	var winner string
	for _, r := range results {
		switch {
		case r.err != nil:
			t.Fatal(r.err)
		case r.status == http.StatusCreated:
			created, winner = created+1, r.owner
		case r.status == http.StatusConflict:
			conflicts++
		}
	}
	if created != 1 || conflicts != 7 {
		t.Fatalf("%d created and %d conflicts, want 1 and 7", created, conflicts)
	}
	if rec, found := w.workspace(t, "demo"); !found || rec.OwnerID != winner || w.workspaces(t) != 1 {
		t.Errorf("stored = %+v (found %v), %d workspaces; want one, owned by %s", rec, found, w.workspaces(t), winner)
	}
	if n := len(w.createEvents(t)); n != 1 {
		t.Errorf("%d create events, want 1", n)
	}
}

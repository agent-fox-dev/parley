package workspace_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
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

// withBoolSetting registers the setting k: true by default, and only a JSON
// boolean is a value of it.
var withBoolSetting = withCompose(func(c *hub.Composition) {
	err := c.Workspaces().Settings().RegisterSetting(workspace.SettingKey{
		Name:    "k",
		Default: func(*string) any { return true },
		Validate: func(v json.RawMessage) error {
			var b bool
			return json.Unmarshal(v, &b)
		},
	})
	if err != nil {
		panic(err)
	}
})

func TestCreate_RefusesAnInvalidBodyNamingTheFirstMemberRefused(t *testing.T) {
	t.Parallel()
	w := newWorld(t, withBoolSetting)
	// Every body names a slug in use, so a refusal shows that the member
	// checks come before the slug-in-use check.
	w.create(t, w.uKey, valid("taken")).object(t)
	const sshURL = "git@example.com:a/b.git"
	carry := func(extra map[string]any) map[string]any {
		extra["workspace_mode"], extra["upstream_url"] = "carry_patch", "https://example.com/up.git"
		return extra
	}
	for _, c := range []struct {
		name   string
		extra  map[string]any
		drop   string
		prefix string
	}{
		{"no slug", nil, "slug", "slug:"},
		{"slug ab", map[string]any{"slug": "ab"}, "", "slug:"},
		{"slug A-b", map[string]any{"slug": "A-b"}, "", "slug:"},
		{"slug 7", map[string]any{"slug": 7}, "", "slug:"},
		{"no git_url", nil, "git_url", "git_url:"},
		{"git_url over http", map[string]any{"git_url": "http://example.com/a"}, "", "git_url:"},
		{"git_url with user information", map[string]any{"git_url": "https://u:p@example.com/a"}, "", "git_url:"},
		{"git_url null", map[string]any{"git_url": nil}, "", "git_url:"},
		{"slug and git_url both bad", map[string]any{"slug": "ab", "git_url": "file:///x"}, "", "slug:"},

		{"default_branch a..b", map[string]any{"default_branch": "a..b"}, "", "default_branch:"},
		{"default_branch empty", map[string]any{"default_branch": ""}, "", "default_branch:"},
		{"default_branch 5", map[string]any{"default_branch": 5}, "", "default_branch:"},
		{"display_name of 129 characters", map[string]any{"display_name": strings.Repeat("a", 129)}, "", "display_name:"},
		{"display_name []", map[string]any{"display_name": []any{}}, "", "display_name:"},
		{"description of 1025 characters", map[string]any{"description": strings.Repeat("a", 1025)}, "", "description:"},
		{"description 1", map[string]any{"description": 1}, "", "description:"},

		{"pat and username", map[string]any{"git_pat": "t", "git_username": "u"}, "", "git_username:"},
		{"pat and password", map[string]any{"git_pat": "t", "git_password": "p"}, "", "git_password:"},
		{"pat, username and password", map[string]any{"git_pat": "t", "git_username": "u", "git_password": "p"}, "", "git_username:"},
		{"username only", map[string]any{"git_username": "u"}, "", "git_password:"},
		{"password only", map[string]any{"git_password": "p"}, "", "git_password:"},
		{"pat empty", map[string]any{"git_pat": ""}, "", "git_pat:"},
		{"username empty", map[string]any{"git_username": "", "git_password": "p"}, "", "git_username:"},
		{"password empty", map[string]any{"git_username": "u", "git_password": ""}, "", "git_password:"},
		{"pat 5", map[string]any{"git_pat": 5}, "", "git_pat:"},
		{"pat null", map[string]any{"git_pat": nil}, "", "git_pat:"},
		{"pat with a git@ URL", map[string]any{"git_url": sshURL, "git_pat": "t"}, "", "git_pat:"},
		{"username and password with a git@ URL", map[string]any{"git_url": sshURL, "git_username": "u", "git_password": "p"}, "", "git_username:"},

		{"mode mirror", map[string]any{"workspace_mode": "mirror"}, "", "workspace_mode:"},
		{"mode 3", map[string]any{"workspace_mode": 3}, "", "workspace_mode:"},
		{"standard with upstream_url", map[string]any{"workspace_mode": "standard", "upstream_url": "https://example.com/up.git"}, "", "upstream_url:"},
		{"no mode with integration_branch", map[string]any{"integration_branch": "deploy"}, "", "integration_branch:"},
		{"carry_patch without upstream_url", map[string]any{"workspace_mode": "carry_patch"}, "", "upstream_url:"},
		{"carry_patch with upstream_url over http", map[string]any{"workspace_mode": "carry_patch", "upstream_url": "http://x/y"}, "", "upstream_url:"},
		{"carry_patch with integration_branch a..b", carry(map[string]any{"integration_branch": "a..b"}), "", "integration_branch:"},

		{"settings []", map[string]any{"settings": []any{}}, "", "settings:"},
		{"settings x", map[string]any{"settings": "x"}, "", "settings:"},
		{"settings 1", map[string]any{"settings": 1}, "", "settings:"},
		{"an unknown setting", map[string]any{"settings": map[string]any{"nope": true}}, "", `unknown setting "nope"`},
		{"a setting value its rule refuses", map[string]any{"settings": map[string]any{"k": "x"}}, "", "settings.k:"},
		{"bad settings before a bad credential", map[string]any{"settings": 1, "git_pat": 5}, "", "settings:"},
	} {
		body := valid("taken")
		maps.Copy(body, c.extra)
		delete(body, c.drop)
		before := w.footprint(t)
		a := w.create(t, w.uKey, body)
		if errorType, message := a.refusal(t); a.status != http.StatusBadRequest || errorType != "invalid_request" || !strings.HasPrefix(message, c.prefix) {
			t.Errorf("%s = %d %s, want 400 invalid_request with a message starting %q", c.name, a.status, a.body, c.prefix)
		}
		if after := w.footprint(t); after != before {
			t.Errorf("%s: the hub went from %+v to %+v, want nothing recorded and origin never called", c.name, before, after)
		}
	}
}

func TestCreate_StoresAndAnswersTheOptionalMembers(t *testing.T) {
	t.Parallel()
	w := newWorld(t, withBoolSetting)
	twoByte := strings.Repeat("é", 128)
	long := strings.Repeat("d", 1024)
	upstream := "https://example.com/up.git"
	carry := map[string]any{"workspace_mode": "carry_patch", "upstream_url": upstream}
	for i, c := range []struct {
		name  string
		extra map[string]any
		want  map[string]any // members of the answer; a display_name of "" stands for the slug
	}{
		{"display_name empty", map[string]any{"display_name": ""}, map[string]any{"display_name": ""}},
		{"display_name of 128 two-byte characters", map[string]any{"display_name": twoByte}, map[string]any{"display_name": twoByte}},
		{"default_branch given", map[string]any{"default_branch": "trunk"}, map[string]any{"default_branch": "trunk"}},
		{"description of 1024 characters", map[string]any{"description": long}, map[string]any{"description": long}},
		{"default_branch, display_name and description null", map[string]any{"default_branch": nil, "display_name": nil, "description": nil},
			map[string]any{"default_branch": nil, "display_name": "", "description": ""}},

		{"mode empty", map[string]any{"workspace_mode": ""}, map[string]any{"workspace_mode": "standard"}},
		{"mode null", map[string]any{"workspace_mode": nil}, map[string]any{"workspace_mode": "standard"}},
		{"standard with upstream_url null", map[string]any{"workspace_mode": "standard", "upstream_url": nil},
			map[string]any{"workspace_mode": "standard", "upstream_url": nil}},
		{"carry_patch with upstream_url", carry,
			map[string]any{"workspace_mode": "carry_patch", "upstream_url": upstream, "integration_branch": nil}},
		{"carry_patch with integration_branch", map[string]any{"workspace_mode": "carry_patch", "upstream_url": upstream, "integration_branch": "deploy"},
			map[string]any{"workspace_mode": "carry_patch", "upstream_url": upstream, "integration_branch": "deploy"}},

		{"settings null", map[string]any{"settings": nil}, map[string]any{"settings": map[string]any{"k": true}}},
		{"settings {}", map[string]any{"settings": map[string]any{}}, map[string]any{"settings": map[string]any{"k": true}}},
		{"settings k false", map[string]any{"settings": map[string]any{"k": false}}, map[string]any{"settings": map[string]any{"k": false}}},
		{"settings k null", map[string]any{"settings": map[string]any{"k": nil}}, map[string]any{"settings": map[string]any{"k": true}}},

		{"members the route does not define", map[string]any{"status": "ready", "owner_id": "someone", "kind": "fork", "zzz": []any{1}},
			map[string]any{"status": "pending", "owner_id": w.u, "kind": "repository"}},
	} {
		slug := fmt.Sprintf("demo-%d", i)
		body := valid(slug)
		maps.Copy(body, c.extra)
		obj := w.create(t, w.uKey, body).object(t)
		for member, want := range c.want {
			if member == "display_name" && want == "" {
				want = slug
			}
			if !reflect.DeepEqual(obj[member], want) {
				t.Errorf("%s: %s = %#v, want %#v", c.name, member, obj[member], want)
			}
		}
		// What is stored answers the same object as the create did.
		rec, _ := w.workspace(t, slug)
		stored, err := w.svc.RenderWorkspace(context.Background(), rec, access.LevelOwner)
		if err != nil {
			t.Fatal(err)
		}
		var storedObj map[string]any
		if b, err := json.Marshal(stored); err != nil || json.Unmarshal(b, &storedObj) != nil {
			t.Fatalf("encode the stored object: %v", err)
		}
		if !reflect.DeepEqual(storedObj, obj) {
			t.Errorf("%s: the stored workspace renders %v, the create answered %v", c.name, storedObj, obj)
		}
	}
}

func TestCreate_RecordsTheWorkspaceInTheOrgItNames(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	beta := w.CreateOrg(t, "beta")
	w.create(t, w.uKey, valid("taken")).object(t)
	post := func(orgID any, slug string) answer {
		t.Helper()
		body := valid(slug)
		body["org_id"] = orgID
		return w.create(t, w.uKey, body)
	}
	for _, c := range []struct {
		name      string
		orgID     any
		status    int
		errorType string
	}{
		{"an unknown org", "no-such-org", http.StatusBadRequest, "invalid_request"},
		{"an unknown org id", "00000000-0000-4000-8000-000000000000", http.StatusBadRequest, "invalid_request"},
		{"a number", 7, http.StatusBadRequest, "invalid_request"},
		{"the id of an org U is not in", beta, http.StatusForbidden, "forbidden"},
		{"the slug of an org U is not in", "beta", http.StatusForbidden, "forbidden"},
	} {
		before := w.footprint(t)
		if a := post(c.orgID, "taken"); a.status != c.status || a.errorType(t) != c.errorType {
			t.Errorf("%s = %d %s, want %d %s", c.name, a.status, a.body, c.status, c.errorType)
		}
		if after := w.footprint(t); after != before {
			t.Errorf("%s: the hub went from %+v to %+v, want nothing recorded", c.name, before, after)
		}
	}

	w.AddMember(t, beta, w.u)
	for _, c := range []struct {
		orgID         any
		slug          string
		wantID, wantS string
	}{
		{"beta", "in-beta", beta, "beta"},
		{beta, "in-beta-by-id", beta, "beta"},
		{nil, "personal", w.uOrg.ID, w.uOrg.Slug},
	} {
		obj := post(c.orgID, c.slug).object(t)
		if obj["org_id"] != c.wantID || obj["org_slug"] != c.wantS {
			t.Errorf("org_id %v: org %v (%v), want %s (%s)", c.orgID, obj["org_id"], obj["org_slug"], c.wantID, c.wantS)
		}
	}
}

func TestCreate_RollsBackEveryWriteWhenOneFails(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	for i, fault := range []string{faultInsert, faultSecret, faultEvent} {
		w.fail(fault)
		before := w.footprint(t)
		slug := fmt.Sprintf("demo-%d", i)
		body := valid(slug)
		body["git_pat"] = "tok-123"
		if a := w.create(t, w.uKey, body); a.status != http.StatusInternalServerError || a.errorType(t) != "internal" {
			t.Errorf("a failed %s = %d %s, want 500 internal", fault, a.status, a.body)
		}
		before.originCalls++
		if after := w.footprint(t); after != before {
			t.Errorf("a failed %s: the hub went from %+v to %+v, want nothing recorded", fault, before, after)
		}
		w.fail("")
		w.create(t, w.uKey, body).object(t)
		if n := w.workspaceSecrets(t, slug); n != 1 {
			t.Errorf("after a failed %s the create stored %d secrets, want 1", fault, n)
		}
	}
}

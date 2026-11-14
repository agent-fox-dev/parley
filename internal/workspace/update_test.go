package workspace_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// settingsJSON is settings as JSON, its keys in a fixed order, so that two
// settings objects compare whatever Go types hold them.
func settingsJSON(t *testing.T, settings any) string {
	t.Helper()
	b, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// withDefaults is settingDefaults with the keys of changes changed.
func withDefaults(changes map[string]any) map[string]any {
	out := maps.Clone(settingDefaults)
	maps.Copy(out, changes)
	return out
}

func TestUpdate_StoresTheGivenSettingsAndKeepsTheOthers(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.put(t, workspace.Workspace{Slug: "demo", Status: workspace.StatusReady, DefaultBranch: new("main")})
	w.patch(t, w.uKey, "demo", map[string]any{"settings": map[string]any{"land.strategy": "squash"}}).decode(t, http.StatusOK)
	r := w.patch(t, w.uKey, "demo", map[string]any{"settings": map[string]any{
		"sync_enabled": false, "protected_branches": []string{"main", "release/**"}}})
	want := withDefaults(map[string]any{"land.strategy": "squash", "sync_enabled": false,
		"protected_branches": []string{"main", "release/**"}})
	if got := settingsJSON(t, r.decode(t, http.StatusOK)["settings"]); got != settingsJSON(t, want) {
		t.Errorf("settings = %s, want %s", got, settingsJSON(t, want))
	}
	if after := w.get(t, w.uKey, "demo"); !bytes.Equal(after.body, r.body) {
		t.Errorf("GET after the update = %s, want the update's answer %s", after.body, r.body)
	}
}

func TestUpdate_ReturnsSettingsGivenNullToTheirDefaults(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	stored := map[string]json.RawMessage{"land.strategy": json.RawMessage(`"squash"`),
		"protected_branches": json.RawMessage(`[]`), "audit_fetches": json.RawMessage(`true`)}
	for _, c := range []struct {
		slug     string
		settings any
		want     map[string]any
	}{
		{"null-keys", map[string]any{"land.strategy": nil, "protected_branches": nil}, withDefaults(map[string]any{"audit_fetches": true})},
		{"null-settings", nil, settingDefaults},
	} {
		w.put(t, workspace.Workspace{Slug: c.slug, Status: workspace.StatusReady, DefaultBranch: new("main"), Settings: stored})
		r := w.patch(t, w.uKey, c.slug, map[string]any{"settings": c.settings})
		if got := settingsJSON(t, r.decode(t, http.StatusOK)["settings"]); got != settingsJSON(t, c.want) {
			t.Errorf("%s: settings = %s, want %s", c.slug, got, settingsJSON(t, c.want))
		}
	}
}

func TestUpdate_RefusesAnInvalidSettingsValueChangingNothing(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.put(t, workspace.Workspace{Slug: "demo", Status: workspace.StatusReady, DefaultBranch: new("main")})
	for _, c := range []struct {
		body any
		want string // what the message names
	}{
		{map[string]any{"description": "new", "settings": map[string]any{"sync_enabled": false, "land.strategy": "merge"}}, "land.strategy"},
		{map[string]any{"settings": []any{}}, "settings"},
		{map[string]any{"settings": map[string]any{"nope": 1}}, `unknown setting "nope"`},
	} {
		before := w.get(t, w.uKey, "demo")
		w.advance(time.Minute)
		r := w.patch(t, w.uKey, "demo", c.body)
		if errorType, msg := r.refusal(t); r.status != http.StatusBadRequest || errorType != "invalid_request" || !strings.Contains(msg, c.want) {
			t.Errorf("%v = %d %s, want 400 invalid_request naming %s", c.body, r.status, r.body, c.want)
		}
		if after := w.get(t, w.uKey, "demo"); !bytes.Equal(after.body, before.body) {
			t.Errorf("%v: the workspace = %s, want it unchanged %s", c.body, after.body, before.body)
		}
	}
}

// refused sends body to PATCH /workspaces/<slug> as cred, an hour of the
// hub's time after the workspace was last read, and checks that the answer
// is status with errorType, that the workspace, updated_at included, is as
// it was and that the event log holds no new event.
func (w *world) refused(t *testing.T, cred, slug string, body any, status int, errorType string) {
	t.Helper()
	before, events := w.get(t, w.Token, slug), w.eventCount(t)
	w.advance(time.Hour)
	if r := w.patch(t, cred, slug, body); r.status != status || r.errorType(t) != errorType {
		t.Errorf("PATCH %s %v = %d %s, want %d %s", slug, body, r.status, r.body, status, errorType)
	}
	if after := w.get(t, w.Token, slug); !bytes.Equal(after.body, before.body) {
		t.Errorf("PATCH %s %v: the workspace = %s, want it unchanged %s", slug, body, after.body, before.body)
	}
	if n := w.eventCount(t); n != events {
		t.Errorf("PATCH %s %v: %d events, want the %d before it", slug, body, n, events)
	}
}

func TestUpdate_ChangesOnlyTheGivenFieldsAndUpdatedAt(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.put(t, workspace.Workspace{Slug: "demo", Status: workspace.StatusReady, DefaultBranch: new("main"),
		DisplayName: "demo", Description: "old"})
	for _, cred := range []string{w.uKey, w.Token} {
		for _, c := range []struct {
			body  map[string]any
			given []string
		}{
			{map[string]any{"display_name": "Demo"}, []string{"display_name"}},
			{map[string]any{"description": "payments"}, []string{"description"}},
			{map[string]any{"display_name": "D", "description": "p", "unknown_member": 1}, []string{"display_name", "description"}},
		} {
			before := w.get(t, cred, "demo").decode(t, http.StatusOK)
			w.advance(time.Minute)
			r := w.patch(t, cred, "demo", c.body)
			got := r.decode(t, http.StatusOK)
			if want := w.clock().Truncate(time.Second).Format(time.RFC3339); got["updated_at"] != want {
				t.Errorf("%v: updated_at = %v, want the hub's time %s", c.body, got["updated_at"], want)
			}
			for _, f := range c.given {
				if got[f] != c.body[f] {
					t.Errorf("%v: %s = %v, want %v", c.body, f, got[f], c.body[f])
				}
			}
			for _, m := range append(c.given, "updated_at") {
				delete(got, m)
				delete(before, m)
			}
			if !reflect.DeepEqual(got, before) {
				t.Errorf("%v: the other members = %v, want them as before %v", c.body, got, before)
			}
			if after := w.get(t, cred, "demo"); !bytes.Equal(after.body, r.body) {
				t.Errorf("%v: GET after the update = %s, want the update's answer %s", c.body, after.body, r.body)
			}
		}
	}
	for _, status := range []workspace.Status{workspace.StatusPending, workspace.StatusCloning, workspace.StatusReady, workspace.StatusFailed} {
		w.put(t, workspace.Workspace{Slug: "in-" + string(status), Status: status})
		if r := w.patch(t, w.uKey, "in-"+string(status), map[string]any{"description": "s"}); r.status != http.StatusOK {
			t.Errorf("an update in status %s = %d %s, want 200", status, r.status, r.body)
		}
	}
}

func TestUpdate_ResetsANullOrEmptyDisplayNameToTheSlugAndANullDescriptionToEmpty(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	for _, c := range []struct {
		slug, field string
		value       any
		want        string
	}{
		{"name-null", "display_name", nil, "name-null"},
		{"name-empty", "display_name", "", "name-empty"},
		{"text-null", "description", nil, ""},
		{"text-empty", "description", "", ""},
	} {
		w.put(t, workspace.Workspace{Slug: c.slug, DisplayName: "Demo", Description: "payments"})
		if got := w.patch(t, w.uKey, c.slug, map[string]any{c.field: c.value}).decode(t, http.StatusOK)[c.field]; got != c.want {
			t.Errorf("%s %v: %s = %q, want %q", c.field, c.value, c.field, got, c.want)
		}
	}
}

func TestUpdate_RefusesABodyThatIsNoObjectOrCarriesNoMutableField(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.put(t, workspace.Workspace{Slug: "demo", Status: workspace.StatusReady})
	for _, body := range []string{`[]`, `"x"`, `5`, `null`, `{}`, `{"foo": 1}`, `{"access": "owner"}`, `{not json`} {
		w.refused(t, w.uKey, "demo", body, http.StatusBadRequest, "invalid_request")
	}
}

func TestUpdate_RefusesAnImmutableOrForkOnlyFieldOnARepositoryWhateverItsValue(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.put(t, workspace.Workspace{Slug: "demo", Status: workspace.StatusReady, DefaultBranch: new("main"), GitURL: new("https://git.example/demo.git")})
	current := w.get(t, w.uKey, "demo").decode(t, http.StatusOK)
	for _, field := range []string{"slug", "kind", "git_url", "default_branch", "owner_id", "workspace_mode", "upstream_url",
		"integration_branch", "parent_slug", "fork_point", "ttl", "purpose", "auto_change"} {
		for _, value := range []any{current[field], "other"} {
			w.refused(t, w.uKey, "demo", map[string]any{field: value, "description": "x"}, http.StatusBadRequest, "invalid_request")
		}
	}
}

func TestUpdate_RefusesADisplayNameOrDescriptionOfTheWrongTypeOrLength(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.put(t, workspace.Workspace{Slug: "demo", Status: workspace.StatusReady})
	for _, c := range []struct {
		field string
		value any
		ok    bool
	}{
		{"display_name", strings.Repeat("a", 128), true},
		{"display_name", strings.Repeat("a", 129), false},
		{"display_name", strings.Repeat("é", 128), true},
		{"display_name", 5, false},
		{"display_name", []any{}, false},
		{"description", strings.Repeat("a", 1024), true},
		{"description", strings.Repeat("a", 1025), false},
		{"description", false, false},
	} {
		body := map[string]any{c.field: c.value}
		if !c.ok {
			w.refused(t, w.uKey, "demo", body, http.StatusBadRequest, "invalid_request")
		} else if got := w.patch(t, w.uKey, "demo", body).decode(t, http.StatusOK)[c.field]; got != c.value {
			t.Errorf("%s of %d characters = %v, want it stored", c.field, len([]rune(c.value.(string))), got)
		}
	}
}

func TestUpdate_RefusesAValidUpdateOfAnArchivedWorkspaceAfterTheBodyChecks(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.put(t, workspace.Workspace{Slug: "old", Status: workspace.StatusArchived})
	for _, c := range []struct {
		body      map[string]any
		status    int
		errorType string
	}{
		{map[string]any{"description": "x"}, http.StatusConflict, "workspace_archived"},
		{map[string]any{"settings": map[string]any{"sync_enabled": false}}, http.StatusConflict, "workspace_archived"},
		{map[string]any{"description": 5}, http.StatusBadRequest, "invalid_request"},
		{map[string]any{"slug": "other"}, http.StatusBadRequest, "invalid_request"},
	} {
		w.refused(t, w.uKey, "old", c.body, c.status, c.errorType)
	}
}

func TestUpdate_RefusesAMemberAPATWithoutWriteScopeAndACallerWhoCannotRead(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	acme := w.acme(t)
	w.put(t, workspace.Workspace{Slug: "demo", OrgID: acme, Status: workspace.StatusReady})
	w.put(t, workspace.Workspace{Slug: "old", OrgID: acme, Status: workspace.StatusArchived})
	readOnly, stranger := w.pat(t, w.u, "workspaces:read"), w.IssueKey(t, w.CreateUser(t, "wanda"))
	body := map[string]any{"description": "x"}
	for _, c := range []struct {
		cred, slug string
		status     int
		errorType  string
	}{
		{w.vKey, "demo", http.StatusForbidden, "forbidden"},
		{w.vKey, "old", http.StatusForbidden, "forbidden"},
		{readOnly, "demo", http.StatusForbidden, "insufficient_scope"},
		{readOnly, "no-such-slug", http.StatusForbidden, "insufficient_scope"},
		{stranger, "demo", http.StatusNotFound, "not_found"},
	} {
		w.refused(t, c.cred, c.slug, body, c.status, c.errorType)
	}
	if hidden, unknown := w.patch(t, stranger, "demo", body), w.patch(t, stranger, "no-such-slug", body); !bytes.Equal(hidden.body, unknown.body) {
		t.Errorf("the 404 of an unreadable workspace = %s, want the one of an unknown slug %s", hidden.body, unknown.body)
	}
	if got := w.get(t, w.uKey, "demo").decode(t, http.StatusOK)["description"]; got != "" {
		t.Errorf("description of demo = %q, want it unchanged", got)
	}
}

func TestUpdate_ChangesTheNameAndDescriptionOfAForkAndRefusesItsOrgAndSettings(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.put(t, workspace.Workspace{Slug: "demo", Status: workspace.StatusReady})
	w.put(t, workspace.Workspace{Slug: "demo--t1", Kind: access.KindFork, ParentSlug: new("demo"), Status: workspace.StatusReady})
	got := w.patch(t, w.uKey, "demo--t1", map[string]any{"display_name": "task 1", "description": "fix"}).decode(t, http.StatusOK)
	if got["display_name"] != "task 1" || got["description"] != "fix" {
		t.Errorf("the fork after the update = %v / %v, want task 1 / fix", got["display_name"], got["description"])
	}
	if got := w.patch(t, w.uKey, "demo--t1", map[string]any{"display_name": nil}).decode(t, http.StatusOK)["display_name"]; got != "demo--t1" {
		t.Errorf("display_name after null = %v, want the slug", got)
	}
	for _, c := range []struct {
		body      map[string]any
		errorType string
	}{
		{map[string]any{"org_id": "acme"}, "not_supported_on_fork"},
		{map[string]any{"org_id": nil}, "not_supported_on_fork"},
		{map[string]any{"settings": map[string]any{"sync_enabled": false}}, "not_supported_on_fork"},
		{map[string]any{"settings": nil}, "not_supported_on_fork"},
		{map[string]any{"description": "y", "settings": map[string]any{}}, "not_supported_on_fork"},
		// A fork-only field nobody registered a handler for.
		{map[string]any{"description": "y", "ttl": 3600}, "invalid_request"},
	} {
		w.refused(t, w.uKey, "demo--t1", c.body, http.StatusBadRequest, c.errorType)
	}
}

func TestUpdate_AppliesTheRegisteredForkFieldsAndGuard(t *testing.T) {
	t.Parallel()
	w := newWorld(t, withCompose(func(c *hub.Composition) {
		c.Workspaces().RegisterForkField("purpose", func(_ workspace.Workspace, raw json.RawMessage) (workspace.ForkFieldWrite, error) {
			var purpose string
			if err := json.Unmarshal(raw, &purpose); err != nil {
				return nil, errors.New("must be a string")
			}
			return func(ctx context.Context, tx *sql.Tx, fork workspace.Workspace) error {
				_, err := tx.ExecContext(ctx, `UPDATE workspaces SET purpose = ? WHERE slug = ?`, purpose, fork.Slug)
				return err
			}, nil
		})
		c.Workspaces().RegisterForkUpdateGuard(func(_ context.Context, _ *sql.Tx, fork workspace.Workspace) error {
			if fork.Slug == "blocked--t1" {
				return server.NewError("parent_archived", http.StatusConflict, "the parent is archived")
			}
			return nil
		})
	}))
	w.put(t, workspace.Workspace{Slug: "demo", Status: workspace.StatusReady})
	for _, fork := range []string{"demo--t1", "blocked--t1"} {
		w.put(t, workspace.Workspace{Slug: fork, Kind: access.KindFork, ParentSlug: new("demo"), Status: workspace.StatusReady})
	}
	if got := w.patch(t, w.uKey, "demo--t1", map[string]any{"purpose": "review"}).decode(t, http.StatusOK)["purpose"]; got != "review" {
		t.Errorf("purpose after the update = %v, want review", got)
	}
	for _, c := range []struct {
		slug      string
		body      map[string]any
		status    int
		errorType string
	}{
		{"demo--t1", map[string]any{"purpose": 5}, http.StatusBadRequest, "invalid_request"},
		{"demo", map[string]any{"purpose": "review"}, http.StatusBadRequest, "invalid_request"},
		{"blocked--t1", map[string]any{"description": "x"}, http.StatusConflict, "parent_archived"},
		{"blocked--t1", map[string]any{"description": 5}, http.StatusBadRequest, "invalid_request"},
	} {
		w.refused(t, w.uKey, c.slug, c.body, c.status, c.errorType)
	}
}

func TestUpdate_MovesARepositoryWorkspaceToAnOrgOfItsOwner(t *testing.T) {
	t.Parallel()
	w := newWorld(t, withExternalURL("https://hub.example"))
	acme, gamma := w.acme(t), w.CreateOrg(t, "gamma")
	w.AddMember(t, gamma, w.u)
	for i, cred := range []string{w.uKey, w.Token} {
		for j, c := range []struct {
			from, wantID, wantSlug string
			value                  any
		}{
			{w.uOrg.ID, acme, "acme", acme},
			{w.uOrg.ID, acme, "acme", "acme"},
			{acme, w.uOrg.ID, w.uOrg.Slug, nil},
		} {
			slug := fmt.Sprintf("demo-%d-%d", i, j)
			w.put(t, workspace.Workspace{Slug: slug, OrgID: c.from, Status: workspace.StatusReady})
			got := w.patch(t, cred, slug, map[string]any{"org_id": c.value}).decode(t, http.StatusOK)
			hubURL := "https://hub.example/git/" + c.wantSlug + "/" + slug + ".git"
			if got["org_id"] != c.wantID || got["org_slug"] != c.wantSlug || got["hub_url"] != hubURL {
				t.Errorf("%s moved by %v: org_id, org_slug, hub_url = %v, %v, %v; want %s, %s, %s",
					slug, c.value, got["org_id"], got["org_slug"], got["hub_url"], c.wantID, c.wantSlug, hubURL)
			}
			// v is a member of acme only: the workspace's org decides
			// whether v may read it.
			r := w.get(t, w.vKey, slug)
			if c.wantID == acme && (r.status != http.StatusOK || r.decode(t, http.StatusOK)["access"] != "member") {
				t.Errorf("%s in acme read by v = %d %s, want 200 with access member", slug, r.status, r.body)
			} else if c.wantID != acme && r.status != http.StatusNotFound {
				t.Errorf("%s out of acme read by v = %d %s, want 404", slug, r.status, r.body)
			}
			w.patch(t, cred, slug, map[string]any{"org_id": "gamma"}).decode(t, http.StatusOK)
			if r := w.get(t, w.vKey, slug); r.status != http.StatusNotFound {
				t.Errorf("%s in gamma read by v = %d %s, want 404", slug, r.status, r.body)
			}
		}
	}
}

func TestUpdate_MovesTheForksWithTheWorkspaceInOneTransaction(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	acme := w.acme(t)
	forks := []string{"demo--t1", "demo--t2"}
	w.put(t, workspace.Workspace{Slug: "demo", Status: workspace.StatusReady})
	for _, fork := range forks {
		w.put(t, workspace.Workspace{Slug: fork, Kind: access.KindFork, ParentSlug: new("demo"), Status: workspace.StatusReady})
	}
	w.patch(t, w.uKey, "demo", map[string]any{"org_id": "acme"}).decode(t, http.StatusOK)
	for _, fork := range forks {
		if got := w.get(t, w.uKey, fork).decode(t, http.StatusOK); got["org_id"] != acme || got["org_slug"] != "acme" {
			t.Errorf("%s after the move: org_id, org_slug = %v, %v; want %s, acme", fork, got["org_id"], got["org_slug"], acme)
		}
		if r := w.get(t, w.vKey, fork); r.status != http.StatusOK {
			t.Errorf("%s read by v, a member of acme = %d %s, want 200", fork, r.status, r.body)
		}
	}
	// The write of the second fork fails: nothing of the move is kept.
	if _, err := w.Store.Main().Exec(`CREATE TRIGGER fail_fork BEFORE UPDATE OF org_id ON workspaces
		WHEN NEW.slug = 'demo--t2' BEGIN SELECT RAISE(ABORT, 'boom'); END`); err != nil {
		t.Fatal(err)
	}
	if r := w.patch(t, w.uKey, "demo", map[string]any{"org_id": nil}); r.status != http.StatusInternalServerError {
		t.Errorf("a move whose fork write fails = %d %s, want 500", r.status, r.body)
	}
	for _, slug := range append([]string{"demo"}, forks...) {
		if rec, _ := w.workspace(t, slug); rec.OrgID != acme {
			t.Errorf("%s after the failed move: org_id = %s, want acme's %s", slug, rec.OrgID, acme)
		}
	}
}

func TestUpdate_RefusesAnOrgThatIsNoneOrNotOneOfTheOwnersChangingNothing(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	acme, beta := w.acme(t), w.CreateOrg(t, "beta")
	w.AddMember(t, beta, w.v)
	w.put(t, workspace.Workspace{Slug: "demo", Status: workspace.StatusReady})
	w.put(t, workspace.Workspace{Slug: "demo--t1", Kind: access.KindFork, ParentSlug: new("demo"), Status: workspace.StatusReady})
	w.put(t, workspace.Workspace{Slug: "old", Status: workspace.StatusArchived})
	type row struct {
		slug      string
		body      map[string]any
		status    int
		errorType string
	}
	cases := []row{
		// The owner, not the caller, must be a member: v is one of beta.
		{"demo", map[string]any{"org_id": beta, "description": "x"}, http.StatusForbidden, "forbidden"},
		{"demo", map[string]any{"org_id": "beta", "description": "x"}, http.StatusForbidden, "forbidden"},
		// The values are judged first, the archived status last.
		{"demo", map[string]any{"org_id": "beta", "description": 5}, http.StatusBadRequest, "invalid_request"},
		{"old", map[string]any{"org_id": "beta"}, http.StatusForbidden, "forbidden"},
		{"old", map[string]any{"org_id": acme}, http.StatusConflict, "workspace_archived"},
	}
	for _, value := range []any{"no-such-org", "00000000-0000-0000-0000-000000000000", "", 5, true, map[string]any{}} {
		cases = append(cases, row{"demo", map[string]any{"org_id": value}, http.StatusBadRequest, "invalid_request"})
	}
	for _, cred := range []string{w.uKey, w.Token} {
		for _, c := range cases {
			w.refused(t, cred, c.slug, c.body, c.status, c.errorType)
		}
	}
	if rec, _ := w.workspace(t, "demo--t1"); rec.OrgID != w.uOrg.ID {
		t.Errorf("the fork after the refusals: org_id = %s, want u's personal org %s", rec.OrgID, w.uOrg.ID)
	}
}

// lastUpdateEvent checks that the event log holds exactly one update event
// more than before and returns it with its metadata decoded.
func (w *world) lastUpdateEvent(t *testing.T, before int) (event.Record, map[string]any) {
	t.Helper()
	evs := w.eventsOf(t, updateEvent)
	if len(evs) != before+1 {
		t.Fatalf("%d update events, want %d", len(evs), before+1)
	}
	ev := evs[len(evs)-1]
	var meta map[string]any
	if err := json.Unmarshal(ev.Metadata, &meta); err != nil {
		t.Fatal(err)
	}
	return ev, meta
}

func TestUpdate_RecordsOneEventNamingTheCallerAndTheFieldsTheBodyCarried(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.put(t, workspace.Workspace{Slug: "demo", Status: workspace.StatusReady, DefaultBranch: new("main")})
	for _, caller := range []struct {
		cred, actorType string
		actorID         *string
	}{
		{w.uKey, event.ActorAPIKey, &w.u},
		{w.pat(t, w.u, "workspaces:write"), event.ActorPAT, &w.u},
		{w.Token, event.ActorAdminToken, nil},
	} {
		for _, c := range []struct {
			body   map[string]any
			fields []any
		}{
			{map[string]any{"description": "x"}, []any{"description"}},
			{map[string]any{"settings": map[string]any{"sync_enabled": false}, "display_name": "D", "foo": 1}, []any{"display_name", "settings"}},
			{map[string]any{"org_id": nil, "description": nil}, []any{"description", "org_id"}},
			// Changes no value, and is recorded all the same.
			{map[string]any{"description": "x"}, []any{"description"}},
		} {
			before, events := len(w.eventsOf(t, updateEvent)), w.eventCount(t)
			w.patch(t, caller.cred, "demo", c.body).decode(t, http.StatusOK)
			ev, meta := w.lastUpdateEvent(t, before)
			if n := w.eventCount(t); n != events+1 {
				t.Errorf("%s %v: the log grew by %d events, want 1", caller.actorType, c.body, n-events)
			}
			b, _ := json.Marshal(ev)
			if !reflect.DeepEqual(ev.Workspace, new("demo")) || ev.ResourceType != "workspace" || ev.ResourceID != "demo" ||
				ev.ActorType != caller.actorType || !reflect.DeepEqual(ev.ActorID, caller.actorID) {
				t.Errorf("%s %v: event = %s, want workspace demo, resource workspace demo, the caller as actor", caller.actorType, c.body, b)
			}
			// The fields given, never a value of one.
			if want := map[string]any{"fields": c.fields}; !reflect.DeepEqual(meta, want) {
				t.Errorf("%s %v: metadata = %v, want %v", caller.actorType, c.body, meta, want)
			}
			if bytes.Contains(b, []byte("sync_enabled")) {
				t.Errorf("%s %v: the event %s names a setting", caller.actorType, c.body, b)
			}
		}
	}
}

func TestUpdate_RecordsTheParentOfAForkInItsEvent(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.put(t, workspace.Workspace{Slug: "demo", Status: workspace.StatusReady})
	w.put(t, workspace.Workspace{Slug: "demo--t1", Kind: access.KindFork, ParentSlug: new("demo"), Status: workspace.StatusReady})
	w.patch(t, w.uKey, "demo--t1", map[string]any{"description": "fix"}).decode(t, http.StatusOK)
	ev, meta := w.lastUpdateEvent(t, 0)
	if want := map[string]any{"fields": []any{"description"}, "parent_slug": "demo"}; !reflect.DeepEqual(ev.Workspace, new("demo--t1")) || !reflect.DeepEqual(meta, want) {
		b, _ := json.Marshal(ev)
		t.Errorf("the fork's event = %s, want workspace demo--t1 and metadata %v", b, want)
	}
}

func TestUpdate_WritesNoEventWhenRefusedOrWhenTheEventCannotBeWritten(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	acme, beta := w.acme(t), w.CreateOrg(t, "beta")
	w.AddMember(t, beta, w.v)
	w.put(t, workspace.Workspace{Slug: "demo", OrgID: acme, Status: workspace.StatusReady})
	w.put(t, workspace.Workspace{Slug: "old", Status: workspace.StatusArchived})
	for _, c := range []struct {
		cred, slug string
		body       map[string]any
		fault      string
		status     int
		errorType  string
	}{
		{w.uKey, "demo", map[string]any{"slug": "x"}, "", http.StatusBadRequest, "invalid_request"},
		{w.uKey, "demo", map[string]any{"settings": map[string]any{"nope": 1}}, "", http.StatusBadRequest, "invalid_request"},
		{w.vKey, "demo", map[string]any{"description": "x"}, "", http.StatusForbidden, "forbidden"},
		{w.uKey, "demo", map[string]any{"org_id": "beta"}, "", http.StatusForbidden, "forbidden"},
		{w.uKey, "old", map[string]any{"description": "x"}, "", http.StatusConflict, "workspace_archived"},
		{w.uKey, "no-such-slug", map[string]any{"description": "x"}, "", http.StatusNotFound, "not_found"},
		{w.uKey, "demo", map[string]any{"description": "x", "org_id": nil}, faultEvent, http.StatusInternalServerError, "internal"},
	} {
		w.fail(c.fault)
		w.refused(t, c.cred, c.slug, c.body, c.status, c.errorType)
	}
}

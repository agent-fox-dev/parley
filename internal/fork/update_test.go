package fork_test

import (
	"encoding/json"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/fork"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

const forkPath = "/workspaces/demo--f"

// lastUpdate is the last parley.workspace.update event of the log, with
// its metadata decoded.
func (w *world) lastUpdate(t *testing.T) (sessionID *string, fields []string, parent string) {
	t.Helper()
	evs := w.eventsOf(t, "parley.workspace.update")
	if len(evs) == 0 {
		t.Fatal("no parley.workspace.update event")
	}
	ev := evs[len(evs)-1]
	var meta struct {
		Fields []string `json:"fields"`
		Parent string   `json:"parent_slug"`
	}
	if err := json.Unmarshal(ev.Metadata, &meta); err != nil {
		t.Fatal(err)
	}
	return ev.SessionID, meta.Fields, meta.Parent
}

func TestUpdate_ForkFields(t *testing.T) {
	t.Parallel()
	w := newWorld(t, nil)
	ann := w.CreateUser(t, "ann")
	w.AddMember(t, w.acme, ann)
	session := "5b3c1f9e-8d2a-4c6b-9e7f-0a1b2c3d4e5f"
	w.seedFork(t, "demo--f", func(f *fork.Fork) { f.OwnerID, f.SessionID, f.AutoChange = w.mia, &session, new(true) })
	before := w.get(t, w.miaKey, forkPath).object(t, http.StatusOK)
	w.advance(time.Second)

	got := w.patch(t, w.miaKey, forkPath, map[string]any{"auto_change": false, "purpose": "new aim"}).object(t, http.StatusOK)
	wantMembers(t, got, map[string]any{"auto_change": false, "purpose": "new aim", "expires_at": before["expires_at"],
		"display_name": before["display_name"], "description": before["description"]})
	if got["updated_at"].(string) <= before["updated_at"].(string) {
		t.Errorf("updated_at = %v, want later than %v", got["updated_at"], before["updated_at"])
	}
	sid, fields, parent := w.lastUpdate(t)
	if sid == nil || *sid != session || !slices.Equal(fields, []string{"purpose", "auto_change"}) || parent != parentSlug {
		t.Errorf("event session %v, fields %v, parent %q; want %s, [purpose auto_change], %s", sid, fields, parent, session, parentSlug)
	}

	got = w.patch(t, w.miaKey, forkPath, map[string]any{"purpose": nil}).object(t, http.StatusOK)
	wantMembers(t, got, map[string]any{"purpose": nil, "auto_change": false})
	w.patch(t, w.miaKey, forkPath, map[string]any{"description": "d", "purpose": "p"}).object(t, http.StatusOK)
	if _, fields, _ := w.lastUpdate(t); !slices.Equal(fields, []string{"description", "purpose"}) {
		t.Errorf("fields = %v, want [description purpose]", fields)
	}

	w.patch(t, w.devKey, forkPath, map[string]any{"purpose": "by parent owner"}).object(t, http.StatusOK)
	if r := w.patch(t, w.IssueKey(t, ann), forkPath, map[string]any{"purpose": "by a member"}); r.status != http.StatusForbidden {
		t.Errorf("a member's PATCH = %d %s, want 403", r.status, r.body)
	}
}

func TestUpdate_RefusesInvalidForkFields(t *testing.T) {
	t.Parallel()
	w := newWorld(t, nil)
	w.seedFork(t, "demo--f")
	for name, body := range map[string]map[string]any{
		"null auto_change": {"auto_change": nil}, "string auto_change": {"auto_change": "no"}, "number auto_change": {"auto_change": 0},
		"number purpose": {"purpose": 7}, "purpose of 1025": {"purpose": strings.Repeat("x", 1025)},
		"valid purpose, string auto_change": {"purpose": "ok", "auto_change": "no"},
	} {
		t.Run(name, func(t *testing.T) {
			before, n := w.record(t, "demo--f"), len(w.eventsOf(t, "parley.workspace.update"))
			wantError(t, w.patch(t, w.devKey, forkPath, body), http.StatusBadRequest, "invalid_request")
			if got := w.record(t, "demo--f"); !reflect.DeepEqual(got, before) {
				t.Errorf("fork = %+v, want it unchanged from %+v", got, before)
			}
			if got := len(w.eventsOf(t, "parley.workspace.update")); got != n {
				t.Errorf("update events = %d, want %d", got, n)
			}
		})
	}
	w.patch(t, w.devKey, forkPath, map[string]any{"purpose": strings.Repeat("x", 1024)}).object(t, http.StatusOK)
}

func TestUpdate_RefusedByParentState(t *testing.T) {
	t.Parallel()
	w := newWorld(t, nil)
	w.seedFork(t, "demo--f")
	for _, c := range []struct {
		name   string
		status workspace.Status
		marker workspace.Marker
		want   string
	}{
		{"archived", workspace.StatusArchived, workspace.MarkerNone, "parent_archived"},
		{"pending", workspace.StatusPending, workspace.MarkerNone, "workspace_busy"},
		{"maintenance marker set", workspace.StatusReady, workspace.MarkerDelete, "workspace_busy"},
		{"failed", workspace.StatusFailed, workspace.MarkerNone, "workspace_not_ready"},
	} {
		t.Run(c.name, func(t *testing.T) {
			w.setParent(t, c.status, c.marker)
			before := w.record(t, "demo--f")
			for _, body := range []map[string]any{{"purpose": "p"}, {"auto_change": false}, {"display_name": "n"}} {
				wantError(t, w.patch(t, w.devKey, forkPath, body), http.StatusConflict, c.want)
			}
			if got := w.record(t, "demo--f"); !reflect.DeepEqual(got, before) {
				t.Errorf("fork = %+v, want it unchanged from %+v", got, before)
			}
			if c.status == workspace.StatusArchived {
				w.get(t, w.devKey, forkPath).object(t, http.StatusOK)
				wantError(t, w.patch(t, w.devKey, forkPath, map[string]any{"auto_change": nil}), http.StatusBadRequest, "invalid_request")
			}
		})
	}
}

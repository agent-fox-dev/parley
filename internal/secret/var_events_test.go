package secret_test

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/secret"
)

func TestVarChanges_WriteOneEventPerChangeWithoutTheValue(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	pat := w.IssuePAT(t, identity.PATSpec{UserID: w.ownerID, Permissions: []string{"vars:write"}})
	// Every value a step stores is one of the markers, which no event may
	// hold.
	const marker1, marker2 = "VALUE-MARKER-1", "VALUE-MARKER-2"
	two, update := varEntries("Z_one="+marker1, "A_two="+marker2), map[string]any{"value": marker2}
	created, acme := []string{"Z_one", "A_two"}, ptr("acme")
	// Each step is a change and the events it writes, one per key, in order.
	steps := []struct {
		cred, method, path     string
		body                   any
		action, scope, scopeID string
		keys                   []string
		actorType              string
		actorID, slug          *string
	}{
		{w.ownerKey, http.MethodPost, "/user/vars", two, "create", "user", w.ownerID, created, event.ActorAPIKey, &w.ownerID, nil},
		{w.Token, http.MethodPost, "/orgs/acme/vars", two, "create", "org", w.orgID, created, event.ActorAdminToken, nil, nil},
		{pat, http.MethodPost, "/workspaces/acme/vars", two, "create", "workspace", "acme", created, event.ActorPAT, &w.ownerID, acme},
		{w.ownerKey, http.MethodPost, "/orgs/acme/vars", varEntries("ApiUrl=" + marker1), "create", "org", w.orgID, []string{"ApiUrl"}, event.ActorAPIKey, &w.ownerID, nil},
		{w.ownerKey, http.MethodPatch, "/orgs/acme/vars/APIURL", update, "update", "org", w.orgID, []string{"ApiUrl"}, event.ActorAPIKey, &w.ownerID, nil},
		{w.ownerKey, http.MethodPost, "/workspaces/acme/vars", varEntries("Region=" + marker1), "create", "workspace", "acme", []string{"Region"}, event.ActorAPIKey, &w.ownerID, acme},
		{pat, http.MethodPatch, "/workspaces/acme/vars/region", update, "update", "workspace", "acme", []string{"Region"}, event.ActorPAT, &w.ownerID, acme},
		{w.ownerKey, http.MethodDelete, "/workspaces/acme/vars/REGION", nil, "delete", "workspace", "acme", []string{"Region"}, event.ActorAPIKey, &w.ownerID, acme},
		{w.Token, http.MethodDelete, "/orgs/acme/vars/a_TWO", nil, "delete", "org", w.orgID, []string{"A_two"}, event.ActorAdminToken, nil, nil},
	}
	for _, s := range steps {
		_, seq := w.varEventsAfter(t, 0)
		if a := w.do(t, s.cred, s.method, s.path, s.body); a.status >= 300 {
			t.Fatalf("%s %s = %d %s", s.method, s.path, a.status, a.body)
		}
		recs, _ := w.varEventsAfter(t, seq)
		for _, ev := range recs {
			if b, err := json.Marshal(ev); err != nil || strings.Contains(string(b), "VALUE-MARKER") {
				t.Errorf("event %s (%v) holds a value", b, err)
			}
		}
		if len(recs) != len(s.keys) {
			t.Errorf("%s %s: %d new variable events, want %d", s.method, s.path, len(recs), len(s.keys))
			continue
		}
		for i, key := range s.keys {
			ev, meta := recs[i], map[string]string{"scope": s.scope, "scope_id": s.scopeID, "key": key}
			if ev.Type != "parley.variable."+s.action || ev.Action != s.action || ev.ResourceType != "variable" || ev.ResourceID != key ||
				!maps.Equal(metadataOf(t, ev), meta) || !equalPtr(ev.Workspace, s.slug) || ev.ActorType != s.actorType || !equalPtr(ev.ActorID, s.actorID) {
				t.Errorf("%s %s: event %d = %+v, metadata %s; want the %s of %s by %s", s.method, s.path, i, ev, ev.Metadata, s.action, key, s.actorType)
			}
		}
	}
}

func TestVarRefusals_WriteNoEvent(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	const col = "/workspaces/acme/vars"
	w.create(t, w.ownerKey, col, "Held")
	w.create(t, w.member2Key, "/user/vars", numbered("V", secret.MaxEntries)...)
	retried, header := varEntries("Retried=v"), http.Header{"Idempotency-Key": {"retried"}}
	_, seq := w.varEventsAfter(t, 0)
	if status, body := w.CallWith(t, http.MethodPost, "/api/v1"+col, w.ownerKey, retried, header); status != http.StatusCreated {
		t.Fatalf("the first create = %d %s, want 201", status, body)
	}
	if recs, _ := w.varEventsAfter(t, seq); len(recs) != 1 {
		t.Fatalf("the first create wrote %d variable events, want the 1 its replay must not write again", len(recs))
	}
	for _, c := range []struct {
		name, cred, method, path string
		body                     any
		header                   http.Header
		status                   int
	}{
		{"create with an invalid key", w.ownerKey, http.MethodPost, col, keyed("1A"), nil, http.StatusBadRequest},
		{"create of a key the scope holds", w.ownerKey, http.MethodPost, col, keyed("HELD"), nil, http.StatusConflict},
		{"create into a full scope", w.member2Key, http.MethodPost, "/user/vars", keyed("MORE"), nil, http.StatusTooManyRequests},
		{"create by an org member on the workspace", w.memberKey, http.MethodPost, col, keyed("NEW"), nil, http.StatusForbidden},
		{"update of a key the scope lacks", w.ownerKey, http.MethodPatch, col + "/MISSING", changed, nil, http.StatusNotFound},
		{"update with a null value", w.ownerKey, http.MethodPatch, col + "/Held", map[string]any{"value": nil}, nil, http.StatusBadRequest},
		{"delete of a key the scope lacks", w.ownerKey, http.MethodDelete, col + "/MISSING", nil, nil, http.StatusNotFound},
		{"replay of a create", w.ownerKey, http.MethodPost, col, retried, header, http.StatusOK},
	} {
		_, seq := w.varEventsAfter(t, 0)
		status, body := w.CallWith(t, c.method, "/api/v1"+c.path, c.cred, c.body, c.header)
		if recs, _ := w.varEventsAfter(t, seq); status != c.status || len(recs) != 0 {
			t.Errorf("%s: %s %s = %d %.200s and %d new variable events; want %d and none", c.name, c.method, c.path, status, body, len(recs), c.status)
		}
	}
}

func TestVarChanges_RollBackWhenTheirEventCannotBeWritten(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	const col = "/workspaces/acme/vars"
	w.create(t, w.ownerKey, col, "KEPT")
	w.exec(t, `CREATE TRIGGER refuse_events BEFORE INSERT ON hub_events BEGIN SELECT RAISE(ABORT, 'event refused'); END`)
	rows := func() (s string) {
		if err := w.Store.Main().QueryRowContext(context.Background(), `SELECT coalesce(group_concat(r, ','), '') FROM
(SELECT scope || ':' || scope_id || ':' || key || ':' || value || ':' || updated_at AS r FROM variables ORDER BY id)`).Scan(&s); err != nil {
			t.Fatalf("read the variable rows: %v", err)
		}
		return s
	}
	before := rows()
	for _, r := range []route{
		{method: http.MethodPost, path: col, body: varEntries("NEW=v")},
		{method: http.MethodPatch, path: col + "/KEPT", body: changed},
		{method: http.MethodDelete, path: col + "/KEPT"},
	} {
		w.advance(time.Second)
		a := w.do(t, w.ownerKey, r.method, r.path, r.body)
		wantError(t, r.String(), a, http.StatusInternalServerError, "internal", "")
		if strings.Contains(string(a.body), "refused") {
			t.Errorf("%v = %s, want no cause", r, a.body)
		}
		if got := rows(); got != before {
			t.Errorf("%v changed the rows to %s, want %s", r, got, before)
		}
	}
}

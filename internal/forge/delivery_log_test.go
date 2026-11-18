package forge_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/forge"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

const (
	deliveriesPath = "/forge/deliveries"
	day            = 24 * time.Hour
	// passInterval is the time between two maintenance passes of the hubs
	// whose test waits for a pass.
	passInterval = 20 * time.Millisecond
)

// logClock is a hub's clock that the test moves.
type logClock struct{ ns atomic.Int64 }

func (c *logClock) Now() time.Time   { return time.Unix(0, c.ns.Load()).UTC() }
func (c *logClock) set(at time.Time) { c.ns.Store(at.UnixNano()) }

// logWorld is a world whose hub's clock starts at deliveryTime and moves
// as the test sets it, with the workspaces demo, on acme/widgets, and
// other, on acme/other.
type logWorld struct {
	*world
	clock      *logClock
	workspaces *workspace.Service
	// passes counts the maintenance passes that have ended.
	passes atomic.Int64
}

// newLogWorld starts a log world with what cfg adds to the world's hub; a
// test that waits for a maintenance pass gives it a MaintenanceInterval.
func newLogWorld(t *testing.T, cfg hubtest.Config) *logWorld {
	t.Helper()
	lw := &logWorld{clock: &logClock{}}
	lw.clock.set(deliveryTime)
	cfg.Now = lw.clock.Now
	cfg.Compose = func(c *hub.Composition) {
		lw.workspaces = c.Workspaces()
		// A task under a name the maintenance order does not know runs
		// after those it knows, forge_delivery_pruning among them.
		c.AddMaintenanceTask("delivery_log_test_pass", func(context.Context, *hub.Runtime) error {
			lw.passes.Add(1)
			return nil
		})
	}
	lw.world = newWorldWith(t, cfg, nil)
	lw.addWorkspace(t, workspace.Workspace{Slug: "demo", GitURL: new("https://github.com/acme/widgets.git")})
	lw.addWorkspace(t, workspace.Workspace{Slug: "other", GitURL: new("https://github.com/acme/other.git")})
	return lw
}

// recordAt records the GitHub delivery id of slug with the event name and
// body while the hub's clock reads at, and is its outcome.
func (lw *logWorld) recordAt(t *testing.T, at time.Time, slug, id, eventName string, body []byte) forge.Outcome {
	t.Helper()
	lw.clock.set(at)
	return lw.record(t, slug, forge.KindGitHub, id, eventName, body)
}

// pass waits for a maintenance pass that started after the call, so that
// its tasks read the hub's clock and the store as they are now.
func (lw *logWorld) pass(t *testing.T) {
	t.Helper()
	from := lw.passes.Load()
	waitUntil(t, "a maintenance pass", func() bool { return lw.passes.Load() >= from+2 })
}

// deliveries sends GET on the delivery log of slug with the query and
// credential, failing the test unless it answers 200 with a list.
func (lw *logWorld) deliveries(t *testing.T, slug, query, credential string) list {
	t.Helper()
	status, body, _ := lw.get(t, slug, deliveriesPath+query, credential)
	var l list
	if status != http.StatusOK || json.Unmarshal(body, &l) != nil || l.Items == nil {
		t.Fatalf("GET %s%s%s = %d %.300s, want 200 with a list", slug, deliveriesPath, query, status, body)
	}
	return l
}

// deliveryIDs are the delivery ids of the items, in their order.
func deliveryIDs(items []map[string]any) []string {
	out := []string{}
	for _, it := range items {
		id, _ := it["delivery_id"].(string)
		out = append(out, id)
	}
	return out
}

// logged reports whether the hub's log holds a line each of whose members
// in want is text holding the value want gives it.
func (lw *logWorld) logged(want map[string]string) bool {
	for line := range strings.Lines(lw.Log.String()) {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		matches := true
		for k, v := range want {
			if text, _ := rec[k].(string); !strings.Contains(text, v) {
				matches = false
			}
		}
		if matches {
			return true
		}
	}
	return false
}

func TestDeliveryLog_ListsTheDeliveriesOfAWorkspaceNewestFirstInPages(t *testing.T) {
	t.Parallel()
	lw := newLogWorld(t, hubtest.Config{})
	lw.addWorkspace(t, workspace.Workspace{Slug: "old", GitURL: new("https://github.com/acme/old.git"), Status: workspace.StatusArchived})
	lw.addWorkspace(t, workspace.Workspace{Slug: "quiet", GitURL: new("https://github.com/acme/quiet.git")})
	at := func(s int) time.Time { return deliveryTime.Add(time.Duration(s) * time.Second) }
	lw.recordAt(t, at(0), "demo", "d1", "issues", ghIssue("labeled", map[string]any{"label": map[string]any{"name": "bug"}}))
	lw.recordAt(t, at(1), "demo", "d2", "ping", []byte(`{"zen":"keep it simple"}`))
	lw.recordAt(t, at(1), "other", "o1", "issues", ghPayload("opened", "acme/other", map[string]any{"issue": map[string]any{"number": 1}}))
	lw.recordAt(t, at(2), "demo", "d3", "issues", ghIssue("opened", nil))
	lw.recordAt(t, at(2), "demo", "d4", "issues", ghIssue("closed", nil))
	lw.recordAt(t, at(3), "demo", "d5", "issues", ghIssue("reopened", nil))
	lw.recordAt(t, at(3), "old", "a1", "ping", []byte(`{}`))
	want := []string{"d5", "d4", "d3", "d2", "d1"}

	// every follows the cursors of the list of demo from query on, with
	// credential, and is the items of all pages.
	every := func(query, credential string) []map[string]any {
		t.Helper()
		var items []map[string]any
		l := lw.deliveries(t, "demo", query, credential)
		for pages := 1; ; pages++ {
			items = append(items, l.Items...)
			if l.NextCursor == nil || pages > 10 {
				break
			}
			l = lw.deliveries(t, "demo", query+"&cursor="+url.QueryEscape(*l.NextCursor), credential)
		}
		return items
	}

	first := lw.deliveries(t, "demo", "?limit=2", lw.memberKey)
	if len(first.Items) != 2 || first.NextCursor == nil {
		t.Fatalf("the first page of 2 = %v, next %v; want 2 items and a cursor", first.Items, first.NextCursor)
	}
	items := every("?limit=2", lw.memberKey)
	if got := deliveryIDs(items); !slices.Equal(got, want) {
		t.Fatalf("the pages of demo = %v, want %v", got, want)
	}
	byID := map[string]map[string]any{}
	for _, it := range items {
		if keys := slices.Sorted(maps.Keys(it)); !slices.Equal(keys, []string{"action", "delivery_id", "event", "forge",
			"outcome", "received_at", "relayed", "target"}) {
			t.Errorf("item %v has the members %v, want the eight of a delivery", it["delivery_id"], keys)
		}
		byID[it["delivery_id"].(string)] = it
	}
	for id, wantJSON := range map[string]string{
		"d1": `{"delivery_id":"d1","forge":"github","event":"issues","action":"labeled","target":"origin",
			"outcome":"relayed","relayed":["parley.forge.issue.labeled"],"received_at":"2026-10-21T09:00:00Z"}`,
		"d2": `{"delivery_id":"d2","forge":"github","event":"ping","action":"","target":null,
			"outcome":"ignored","relayed":[],"received_at":"2026-10-21T09:00:01Z"}`,
	} {
		if got, _ := json.Marshal(byID[id]); !sameJSON(got, wantJSON) {
			t.Errorf("item %s = %s, want %s", id, got, wantJSON)
		}
	}

	if l := lw.deliveries(t, "quiet", "", lw.memberKey); len(l.Items) != 0 || l.NextCursor != nil {
		t.Errorf("the log of a workspace without deliveries = %v, next %v; want [] and null", l.Items, l.NextCursor)
	}
	if got := deliveryIDs(lw.deliveries(t, "old", "", lw.memberKey).Items); !slices.Equal(got, []string{"a1"}) {
		t.Errorf("the log of the archived workspace = %v, want [a1]", got)
	}
	if got := deliveryIDs(every("?limit=2&target=upstream", lw.ownerKey)); !slices.Equal(got, want) {
		t.Errorf("the pages of demo with a target = %v, want %v", got, want)
	}
	lw.putSecret(t, "demo", forge.SecretWebhook, "the-webhook-secret-0123")
	if status, body, _ := lw.send(t, http.MethodDelete, "demo", webhookPath, lw.ownerKey, nil); status != http.StatusNoContent {
		t.Fatalf("DELETE the webhook of demo = %d %s, want 204", status, body)
	}
	if got := deliveryIDs(every("?limit=2", lw.memberKey)); !slices.Equal(got, want) {
		t.Errorf("the pages of demo after its secret was removed = %v, want %v", got, want)
	}
}

func TestDeliveryLog_ListsOnlyTheDeliveriesReceivedSince(t *testing.T) {
	t.Parallel()
	lw := newLogWorld(t, hubtest.Config{})
	for i, id := range []string{"s0", "s5", "s10"} {
		lw.recordAt(t, deliveryTime.Add(time.Duration(5*i)*time.Second), "demo", id, "ping", []byte(`{}`))
	}
	cases := []struct {
		since string
		want  []string
	}{
		{"2026-10-21T09:00:05Z", []string{"s10", "s5"}},
		{"2026-10-21T11:00:05+02:00", []string{"s10", "s5"}},
		{"2026-10-21T09:00:04.5Z", []string{"s10", "s5"}},
		{"2026-10-21T09:00:11Z", []string{}},
	}
	for _, c := range cases {
		if got := deliveryIDs(lw.deliveries(t, "demo", "?since="+url.QueryEscape(c.since), lw.ownerKey).Items); !slices.Equal(got, c.want) {
			t.Errorf("since %s = %v, want %v", c.since, got, c.want)
		}
	}
}

func TestDeliveryLog_RefusesAForkABadSinceAndACallerWithoutReadAccess(t *testing.T) {
	t.Parallel()
	lw := newLogWorld(t, hubtest.Config{})
	lw.addWorkspace(t, workspace.Workspace{Slug: "demo--t1", ParentSlug: new("demo")})
	readOnly := lw.pat(t, lw.owner, "", "workspaces:read")
	forgeWrite := lw.pat(t, lw.owner, "", forge.ScopeWrite)
	cases := []struct {
		name, slug, query, credential string
		status                        int
		errorType                     string
	}{
		{"a task fork", "demo--t1", "", lw.ownerKey, http.StatusBadRequest, "fork_has_no_origin"},
		{"since a word", "demo", "?since=yesterday", lw.ownerKey, http.StatusBadRequest, "invalid_request"},
		{"since a date", "demo", "?since=2026-10-21", lw.ownerKey, http.StatusBadRequest, "invalid_request"},
		{"since empty", "demo", "?since=", lw.ownerKey, http.StatusBadRequest, "invalid_request"},
		{"a PAT without forge:read", "demo", "", readOnly, http.StatusForbidden, "insufficient_scope"},
		{"a caller without read access", "demo", "", lw.outKey, http.StatusNotFound, "not_found"},
		{"a PAT with forge:write", "demo", "", forgeWrite, http.StatusOK, ""},
	}
	for _, c := range cases {
		status, body, _ := lw.get(t, c.slug, deliveriesPath+c.query, c.credential)
		if status != c.status {
			t.Errorf("%s: GET = %d %s, want %d", c.name, status, body, c.status)
			continue
		}
		if c.errorType != "" {
			if e := errorOf(t, body); e.Type != c.errorType {
				t.Errorf("%s: error_type %q, want %q", c.name, e.Type, c.errorType)
			}
		}
	}
}

func TestDeliveryPruning_DeletesTheDeliveriesMoreThanSevenDaysOldAtAPass(t *testing.T) {
	t.Parallel()
	lw := newLogWorld(t, hubtest.Config{MaintenanceInterval: passInterval})
	now := deliveryTime.Add(8 * day)
	week := 7 * day
	// The clock only moves forward, and the passes meanwhile prune nothing
	// the pass at now keeps.
	lw.recordAt(t, now.Add(-8*day), "demo", "eight-days", "ping", []byte(`{}`))
	lw.recordAt(t, now.Add(-week-time.Second), "other", "a-second-over", "ping", []byte(`{}`))
	lw.recordAt(t, now.Add(-week), "demo", "exactly-seven-days", "ping", []byte(`{}`))
	lw.recordAt(t, now.Add(-time.Hour), "other", "an-hour", "ping", []byte(`{}`))
	lw.clock.set(now)
	lw.pass(t)

	for slug, want := range map[string][]string{"demo": {"exactly-seven-days"}, "other": {"an-hour"}} {
		if got := deliveryIDs(lw.deliveries(t, slug, "", lw.Token).Items); !slices.Equal(got, want) {
			t.Errorf("the log of %s after a pass = %v, want %v", slug, got, want)
		}
	}
	if out := lw.record(t, "demo", forge.KindGitHub, "eight-days", "ping", []byte(`{}`)); out != forge.OutcomeIgnored {
		t.Errorf("the pruned id sent again = %q, want ignored, a new delivery", out)
	}
}

func TestDeliveryPruning_ReportsTheStoresErrorAndPrunesAtTheNextPass(t *testing.T) {
	t.Parallel()
	lw := newLogWorld(t, hubtest.Config{MaintenanceInterval: passInterval})
	now := deliveryTime.Add(8 * day)
	lw.recordAt(t, deliveryTime, "demo", "old", "ping", []byte(`{}`))
	lw.recordAt(t, now.Add(-time.Hour), "demo", "recent", "ping", []byte(`{}`))
	exec := func(stmt string) {
		t.Helper()
		if err := lw.Store.Tx(context.Background(), func(tx *sql.Tx) error {
			_, err := tx.Exec(stmt)
			return err
		}); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	// The trigger makes every delete of a delivery fail.
	exec(`CREATE TRIGGER deliveries_locked BEFORE DELETE ON forge_deliveries BEGIN SELECT RAISE(ABORT, 'deliveries are locked'); END`)
	lw.clock.set(now)

	waitUntil(t, "the pruning task's failure in the log", func() bool {
		return lw.logged(map[string]string{"msg": "maintenance task failed", "task": "forge_delivery_pruning", "error": "deliveries are locked"})
	})
	if got := deliveryIDs(lw.deliveries(t, "demo", "", lw.Token).Items); !slices.Equal(got, []string{"recent", "old"}) {
		t.Fatalf("the log of demo after a failed pass = %v, want [recent old]", got)
	}
	exec(`DROP TRIGGER deliveries_locked`)
	lw.pass(t)
	if got := deliveryIDs(lw.deliveries(t, "demo", "", lw.Token).Items); !slices.Equal(got, []string{"recent"}) {
		t.Errorf("the log of demo after the next pass = %v, want [recent]", got)
	}
}

func TestRemoveOwned_DeletesTheWorkspacesDeliveriesWithItsTransaction(t *testing.T) {
	t.Parallel()
	lw := newLogWorld(t, hubtest.Config{})
	ctx := context.Background()
	for _, id := range []string{"d1", "d2", "d3"} {
		lw.record(t, "demo", forge.KindGitHub, id, "ping", []byte(`{}`))
	}
	lw.record(t, "other", forge.KindGitHub, "o1", "ping", []byte(`{}`))
	count := func(slug string) int {
		t.Helper()
		list, err := forge.ListDeliveries(ctx, lw.Store.Main(), slug, forge.DeliveryQuery{})
		if err != nil {
			t.Fatal(err)
		}
		return len(list)
	}
	errRollBack := errors.New("roll the delete back")
	// The rolled back delete comes first, since the committed one leaves
	// nothing to remove.
	for _, c := range []struct {
		name       string
		end        error
		demo, rest int
	}{
		{"rolled back", errRollBack, 3, 1},
		{"committed", nil, 0, 1},
	} {
		err := lw.Store.Tx(ctx, func(tx *sql.Tx) error {
			if _, err := lw.workspaces.RemoveOwned(ctx, tx, "demo", "workspace_deleted"); err != nil {
				return err
			}
			return c.end
		})
		if !errors.Is(err, c.end) {
			t.Fatalf("%s: the delete transaction = %v, want %v", c.name, err, c.end)
		}
		if got, rest := count("demo"), count("other"); got != c.demo || rest != c.rest {
			t.Errorf("%s: deliveries of demo %d and of other %d, want %d and %d", c.name, got, rest, c.demo, c.rest)
		}
	}
	if out := lw.record(t, "demo", forge.KindGitHub, "d1", "ping", []byte(`{}`)); out != forge.OutcomeIgnored {
		t.Errorf("an earlier id after the delete = %q, want ignored, a new delivery", out)
	}
}

func TestForgeStatus_ReportsTheNewestDeliveryAsTheWebhooksLastDelivery(t *testing.T) {
	t.Parallel()
	lw := newLogWorld(t, hubtest.Config{Server: externalURL(hubURL), MaintenanceInterval: passInterval})
	webhook := func(slug string) string {
		t.Helper()
		status, body, _ := lw.get(t, slug, "/forge", lw.ownerKey)
		var doc struct {
			Webhook json.RawMessage `json:"webhook"`
		}
		if status != http.StatusOK || json.Unmarshal(body, &doc) != nil {
			t.Fatalf("GET %s/forge = %d %s, want 200 with the diagnostic document", slug, status, body)
		}
		return string(doc.Webhook)
	}
	check := func(when, slug, want string) {
		t.Helper()
		if got := webhook(slug); !sameJSON([]byte(got), want) {
			t.Errorf("%s: the webhook of %s = %s, want %s", when, slug, got, want)
		}
	}
	const hookURL = `"url":"https://hub.example.com/hooks/forge/demo"`
	check("before a secret", "demo", `{"configured":false,`+hookURL+`,"last_delivery_at":null}`)
	if status, body, _ := lw.putWebhook(t, "demo", lw.ownerKey, nil); status != http.StatusOK {
		t.Fatalf("PUT the webhook of demo = %d %s, want 200", status, body)
	}
	check("after the PUT", "demo", `{"configured":true,`+hookURL+`,"last_delivery_at":null}`)
	lw.recordAt(t, deliveryTime, "demo", "d1", "ping", []byte(`{}`))
	lw.recordAt(t, deliveryTime.Add(12*time.Minute+3*time.Second), "demo", "d2", "ping", []byte(`{}`))
	check("after two deliveries", "demo", `{"configured":true,`+hookURL+`,"last_delivery_at":"2026-10-21T09:12:03Z"}`)
	if status, body, _ := lw.send(t, http.MethodDelete, "demo", webhookPath, lw.ownerKey, nil); status != http.StatusNoContent {
		t.Fatalf("DELETE the webhook of demo = %d %s, want 204", status, body)
	}
	check("after the DELETE", "demo", `{"configured":false,`+hookURL+`,"last_delivery_at":"2026-10-21T09:12:03Z"}`)
	check("without a delivery", "other",
		`{"configured":false,"url":"https://hub.example.com/hooks/forge/other","last_delivery_at":null}`)
	lw.clock.set(deliveryTime.Add(8 * day))
	lw.pass(t)
	check("once every delivery is pruned", "demo", `{"configured":false,`+hookURL+`,"last_delivery_at":null}`)
}
